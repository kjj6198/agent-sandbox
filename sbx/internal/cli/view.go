package cli

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/rand"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"

	"github.com/kjj6198/agent-sandbox-eval/sbx/internal/proto"
)

//go:embed viewer
var viewerFiles embed.FS

// screenPort is where the templates' X server serves VNC inside the sandbox.
const screenPort = 5900

const (
	viewerWidth  = 520
	viewerHeight = 360
	// A viewer exits this long after its window closed, or after its sandbox was removed.
	viewerLinger = time.Minute
)

const defaultViewerListen = "127.0.0.1:0"

func viewCmd() *cobra.Command {
	var foreground, noOpen bool
	var listen string
	cmd := &cobra.Command{
		Use:   "view ID",
		Short: "Show the sandbox's screen in a small window",
		Long: "Open a small browser window at the top right of your screen with a live view of the " +
			"sandbox's desktop. It starts in watch mode; click \"Watching\" to take over the mouse " +
			"and keyboard. GUI programs started in the sandbox appear there, including the browser " +
			"template's Chromium.\n\n" +
			"The viewer runs in the background and exits a minute after its window closes or its " +
			"sandbox is removed. Watching does not keep a sandbox alive.\n\n" +
			"For external rendering, the viewer also serves the screen as an image: GET " +
			"screenshot.png under the printed URL (or ?format=jpeg&width=640) returns the " +
			"current desktop as one image any page can put in an <img>, and screen.json beside " +
			"it reports its size. The viewer page itself can be embedded in an <iframe>; add " +
			"?controls=0 to hide its button. The URL carries a random token, which is the " +
			"viewer's only key. --listen serves beyond this machine, for example --listen " +
			"0.0.0.0:7681; then anyone who learns the URL, which travels as plain HTTP, can " +
			"watch and take over with one click. Prefer `sbx screenshot` or the API for " +
			"untrusted networks.",
		Example: "  id=$(sbx create browser --view)\n  sbx view $id\n  sbx view --no-open $id   # print the URL only\n" +
			"  sbx view --no-open --listen 0.0.0.0:7681 $id   # render it from another machine",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if foreground {
				c, err := newClient()
				if err != nil {
					return err
				}
				return serveViewer(cmd.Context(), c, args[0], !noOpen, listen)
			}
			url, err := startViewer(args[0], !noOpen, listen)
			if err != nil {
				return err
			}
			if !isLoopbackListen(listen) {
				fmt.Fprintln(os.Stderr, "sbx: warning: the viewer listens on", listen,
					"over plain HTTP; anyone with its URL can watch and control the sandbox")
			}
			fmt.Println(url)
			return nil
		},
	}
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "print the viewer URL without opening a window")
	cmd.Flags().StringVar(&listen, "listen", defaultViewerListen, "address the viewer serves on")
	cmd.Flags().BoolVar(&foreground, "foreground", false, "serve the viewer in this process")
	cmd.Flags().MarkHidden("foreground")
	return cmd
}

// isLoopbackListen reports whether only this machine can reach a listen address.
func isLoopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// startViewer runs `sbx view --foreground` as a detached process, so the caller, often an
// agent's shell command, returns at once. The child reports its URL or error on fd 3.
func startViewer(id string, open bool, listen string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return "", err
	}
	args := []string{"view", "--foreground", id}
	if !open {
		args = append(args, "--no-open")
	}
	if listen != "" && listen != defaultViewerListen {
		args = append(args, "--listen", listen)
	}
	cmd := exec.Command(exe, args...)
	cmd.ExtraFiles = []*os.File{w}
	cmd.Env = append(os.Environ(), "SBX_VIEWER_REPORT_FD=3")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	w.Close()
	if err != nil {
		r.Close()
		return "", err
	}
	line, _ := bufio.NewReader(r).ReadString('\n')
	r.Close()
	cmd.Process.Release()
	line = strings.TrimSpace(line)
	if msg, ok := strings.CutPrefix(line, "error: "); ok {
		return "", errors.New(msg)
	}
	if line == "" {
		return "", errors.New("the viewer exited before it started")
	}
	return line, nil
}

// report tells the parent of a detached viewer how the start went.
func report(url string, err error) {
	fd, _ := strconv.Atoi(os.Getenv("SBX_VIEWER_REPORT_FD"))
	if fd == 0 {
		if err == nil {
			fmt.Println(url)
		}
		return
	}
	f := os.NewFile(uintptr(fd), "report")
	if err != nil {
		fmt.Fprintf(f, "error: %v\n", err)
	} else {
		fmt.Fprintln(f, url)
	}
	f.Close()
}

type viewer struct {
	c      *client
	id     string
	url    string
	prefix string // path of the viewer's pages: a slash and the URL's random token
	active atomic.Int32
	gone   atomic.Bool
	// lastSeen is the last time anyone used the viewer, by screen or by screenshot,
	// as Unix nanoseconds. It decides when a viewer with no window exits.
	lastSeen atomic.Int64
	// watchers counts open windows, which follow the secret requests.
	watchers atomic.Int32
	// secrets is set when the viewer runs inside `sbx mcp`, which asks the user for secrets.
	secrets *secretStore
	// dropped hears about files the user dropped on the window, by their path in the sandbox.
	dropped func(paths []string)
	srv     *http.Server

	mu     sync.Mutex
	shot   proto.Screenshot // last full-size screenshot, for screen.json
	shotAt time.Time
}

var sandboxID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// checkID rejects IDs that are not plain names before they go into a URL or the viewer page.
func checkID(id string) error {
	if !sandboxID.MatchString(id) {
		return fmt.Errorf("%q is not a sandbox ID; see sbx ls", id)
	}
	return nil
}

// dropDir is where files dropped on the viewer land, relative to the user's home.
const dropDir = "Downloads"

func (v *viewer) touch() { v.lastSeen.Store(time.Now().UnixNano()) }

// startViewerServer serves the viewer for sandbox id on listen, a random loopback port by
// default. The URL carries a random token, because any web page the user opens could reach
// the port, and the page can drive the sandbox and answer its secret prompts.
func startViewerServer(ctx context.Context, v *viewer, listen string) error {
	if err := checkID(v.id); err != nil {
		return err
	}
	if _, err := v.exists(ctx); err != nil {
		return err
	}
	if listen == "" {
		listen = defaultViewerListen
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	token := rand.Text()
	files, _ := fs.Sub(viewerFiles, "viewer")
	mux := http.NewServeMux()
	prefix := "/" + token
	v.prefix = prefix
	mux.Handle("GET "+prefix+"/novnc/", http.StripPrefix(prefix, http.FileServerFS(files)))
	mux.HandleFunc("GET "+prefix+"/{$}", func(w http.ResponseWriter, r *http.Request) {
		page, _ := fs.ReadFile(files, "index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(strings.ReplaceAll(string(page), "{{ID}}", html.EscapeString(v.id))))
	})
	mux.HandleFunc("GET "+prefix+"/alive", func(w http.ResponseWriter, r *http.Request) {
		if v.gone.Load() {
			w.WriteHeader(http.StatusGone)
		}
	})
	mux.HandleFunc("GET "+prefix+"/ws", v.serveScreen)
	mux.HandleFunc("GET "+prefix+"/screenshot.png", v.serveScreenshot)
	mux.HandleFunc("GET "+prefix+"/screen.json", v.serveScreenJSON)
	mux.HandleFunc("POST "+prefix+"/files", v.serveDrop)
	mux.HandleFunc("GET "+prefix+"/secrets", v.serveSecretRequests)
	mux.HandleFunc("POST "+prefix+"/secrets/{request}", v.serveSecretAnswer)
	v.srv = &http.Server{Handler: mux}
	go v.srv.Serve(ln)
	v.url = viewerURL(ln) + token + "/"
	return nil
}

func serveViewer(ctx context.Context, c *client, id string, open bool, listen string) error {
	v := &viewer{c: c, id: id}
	v.touch()
	if err := startViewerServer(ctx, v, listen); err != nil {
		report("", err)
		return err
	}
	report(v.url, nil)
	if open {
		openWindow(v.url)
	}
	return v.wait(ctx)
}

// viewerURL is the server's root to open locally: a wildcard listen still opens on loopback.
func viewerURL(ln net.Listener) string {
	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return "http://" + ln.Addr().String() + "/"
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

// exists reports whether the sandbox is still there, without counting as activity.
func (v *viewer) exists(ctx context.Context) (bool, error) {
	err := v.c.call(ctx, "GET", "/v1/sandboxes/"+v.id+"?passive=1", nil, nil)
	if isNotFound(err) {
		return false, err
	}
	return err == nil, err
}

// wait returns once the window has been closed, or the sandbox removed, for viewerLinger.
// Screenshot and screen.json requests count as use, so an external page polling the image
// keeps its viewer, and nothing else needs to stay open.
func (v *viewer) wait(ctx context.Context) error {
	goneSince := time.Time{}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		now := time.Now()
		if v.active.Load() > 0 {
			v.touch()
		}
		last := time.Unix(0, v.lastSeen.Load())
		if goneSince.IsZero() {
			if ok, err := v.exists(ctx); !ok && isNotFound(err) {
				v.gone.Store(true)
				goneSince = now
			}
		}
		if now.Sub(last) > viewerLinger || (!goneSince.IsZero() && now.Sub(goneSince) > viewerLinger) {
			return nil
		}
	}
}

// serveScreenshot returns the desktop as one image for external renderers: a page can
// poll it from an <img> without speaking VNC. Query: format=png|jpeg, width=<px>.
func (v *viewer) serveScreenshot(w http.ResponseWriter, r *http.Request) {
	v.touch()
	width := 0
	if s := r.URL.Query().Get("width"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			v.writeError(w, http.StatusBadRequest, fmt.Errorf("width %q must be a pixel count", s))
			return
		}
		width = n
	}
	data, shot, err := fetchScreenshot(r.Context(), v.c, v.id, r.URL.Query().Get("format"), width, true)
	if err != nil {
		status := http.StatusBadGateway
		if isNotFound(err) {
			status = http.StatusNotFound
		}
		v.writeError(w, status, err)
		return
	}
	if width == 0 {
		v.mu.Lock()
		v.shot, v.shotAt = shot, time.Now()
		v.mu.Unlock()
	}
	contentType := "image/png"
	if shot.Format == "jpeg" {
		contentType = "image/jpeg"
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Screen-Width", strconv.Itoa(shot.Width))
	w.Header().Set("X-Screen-Height", strconv.Itoa(shot.Height))
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
}

// serveScreenJSON reports the screen's size and where to render it, for pages that lay
// out around the image before loading it.
func (v *viewer) serveScreenJSON(w http.ResponseWriter, r *http.Request) {
	v.touch()
	v.mu.Lock()
	shot, at := v.shot, v.shotAt
	v.mu.Unlock()
	if shot.Width == 0 || time.Since(at) > 5*time.Second {
		_, fresh, err := fetchScreenshot(r.Context(), v.c, v.id, "png", 0, true)
		if err != nil {
			status := http.StatusBadGateway
			if isNotFound(err) {
				status = http.StatusNotFound
			}
			v.writeError(w, status, err)
			return
		}
		shot = fresh
		v.mu.Lock()
		v.shot, v.shotAt = shot, time.Now()
		v.mu.Unlock()
	}
	shot.Path = ""
	shot.ScreenshotURL = v.prefix + "/screenshot.png"
	shot.ViewerURL = v.prefix + "/"
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(shot)
}

func (v *viewer) writeError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(proto.Error{Error: err.Error()})
}

// serveScreen bridges the page's WebSocket to the sandbox's VNC port through the sbx API.
func (v *viewer) serveScreen(w http.ResponseWriter, r *http.Request) {
	v.touch()
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	v.active.Add(1)
	defer v.active.Add(-1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := fmt.Sprintf("/v1/sandboxes/%s/tcp?port=%d&passive=1", v.id, screenPort)
	remote, err := v.c.upgrade(ctx, "GET", path, "tcp", nil)
	if err != nil {
		ws.Close(websocket.StatusInternalError, truncate(err.Error(), 100))
		return
	}
	defer remote.Close()
	local := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	defer local.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(remote, local); done <- struct{}{} }()
	go func() { io.Copy(local, remote); done <- struct{}{} }()
	<-done
}

// serveDrop copies a file the user dropped on the window into the sandbox's ~/Downloads.
func (v *viewer) serveDrop(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.URL.Query().Get("name"))
	if name == "." || name == "/" || name == ".." || r.ContentLength < 0 {
		http.Error(w, "a file name and Content-Length are required", http.StatusBadRequest)
		return
	}
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: r.ContentLength, ModTime: time.Now()})
		if err == nil {
			_, err = io.Copy(tw, r.Body)
		}
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
	}()
	dst := path.Join(dropDir, name)
	req, err := v.c.request(r.Context(), "PUT", "/v1/sandboxes/"+v.id+"/files?path="+url.QueryEscape(dst), pr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := v.c.send(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	resp.Body.Close()
	if v.dropped != nil {
		v.dropped([]string{"/home/" + proto.DefaultUser + "/" + dst})
	}
	fmt.Fprintf(w, "~/%s", dst)
}

// serveSecretRequests streams this sandbox's open secret requests as server-sent events,
// sending the whole list each time it changes.
func (v *viewer) serveSecretRequests(w http.ResponseWriter, r *http.Request) {
	v.watchers.Add(1)
	defer v.watchers.Add(-1)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	flusher, _ := w.(http.Flusher)
	for {
		var pending []secretRequest
		var changed <-chan struct{}
		if v.secrets != nil {
			pending, changed = v.secrets.pendingFor(v.id)
		}
		b, _ := json.Marshal(pending)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case <-changed:
		}
	}
}

// serveSecretAnswer takes the user's answer to a secret request. An empty body declines.
func (v *viewer) serveSecretAnswer(w http.ResponseWriter, r *http.Request) {
	var answer struct {
		Value *string `json:"value"`
	}
	if v.secrets == nil || json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&answer) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !v.secrets.answer(r.PathValue("request"), answer.Value) {
		http.Error(w, "the request is no longer open", http.StatusGone)
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// openWindow shows url as a small app window at the top right, using a Chromium-based
// browser with its own profile so the size and position flags apply. Without one it falls
// back to the default browser.
func openWindow(url string) {
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
	}
	args := []string{
		"--app=" + url, "--user-data-dir=" + filepath.Join(cache, "sbx", "viewer-browser"),
		"--no-first-run", "--no-default-browser-check", "--disable-features=Translate",
		fmt.Sprintf("--window-size=%d,%d", viewerWidth, viewerHeight),
	}
	if width, ok := screenWidth(); ok {
		args = append(args, fmt.Sprintf("--window-position=%d,%d", width-viewerWidth-12, 40))
	}
	for _, browser := range chromiumBrowsers() {
		cmd := exec.Command(browser, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if cmd.Start() == nil {
			cmd.Process.Release()
			return
		}
	}
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	exec.Command(opener, url).Start()
}

func chromiumBrowsers() []string {
	if runtime.GOOS == "darwin" {
		var found []string
		for _, app := range []string{"Google Chrome", "Chromium", "Microsoft Edge", "Brave Browser"} {
			p := filepath.Join("/Applications", app+".app", "Contents", "MacOS", app)
			if _, err := os.Stat(p); err == nil {
				found = append(found, p)
			}
		}
		return found
	}
	var found []string
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge", "brave-browser"} {
		if p, err := exec.LookPath(name); err == nil {
			found = append(found, p)
		}
	}
	return found
}

// screenWidth returns the main display's width in points on macOS.
func screenWidth() (int, bool) {
	if runtime.GOOS != "darwin" {
		return 0, false
	}
	out, err := exec.Command("osascript", "-e", `tell application "Finder" to get bounds of window of desktop`).Output()
	if err != nil {
		return 0, false
	}
	f := strings.Split(strings.TrimSpace(string(out)), ", ")
	if len(f) != 4 {
		return 0, false
	}
	w, err := strconv.Atoi(f[2])
	return w, err == nil
}
