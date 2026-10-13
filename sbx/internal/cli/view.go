package cli

import (
	"bufio"
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"
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

func viewCmd() *cobra.Command {
	var foreground, noOpen bool
	cmd := &cobra.Command{
		Use:   "view ID",
		Short: "Show the sandbox's screen in a small window",
		Long: "Open a small browser window at the top right of your screen with a live view of the " +
			"sandbox's desktop. It starts in watch mode; click \"Watching\" to take over the mouse " +
			"and keyboard. GUI programs started in the sandbox appear there, including the browser " +
			"template's Chromium.\n\n" +
			"The viewer runs in the background and exits a minute after its window closes or its " +
			"sandbox is removed. Watching does not keep a sandbox alive.",
		Example: "  id=$(sbx create browser --view)\n  sbx view $id\n  sbx view --no-open $id   # print the URL only",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if foreground {
				c, err := newClient()
				if err != nil {
					return err
				}
				return serveViewer(cmd.Context(), c, args[0], !noOpen)
			}
			url, err := startViewer(args[0], !noOpen)
			if err != nil {
				return err
			}
			fmt.Println(url)
			return nil
		},
	}
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "print the viewer URL without opening a window")
	cmd.Flags().BoolVar(&foreground, "foreground", false, "serve the viewer in this process")
	cmd.Flags().MarkHidden("foreground")
	return cmd
}

// startViewer runs `sbx view --foreground` as a detached process, so the caller, often an
// agent's shell command, returns at once. The child reports its URL or error on fd 3.
func startViewer(id string, open bool) (string, error) {
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
	active atomic.Int32
	gone   atomic.Bool
}

func serveViewer(ctx context.Context, c *client, id string, open bool) error {
	v := &viewer{c: c, id: id}
	if _, err := v.exists(ctx); err != nil {
		report("", err)
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		report("", err)
		return err
	}
	files, _ := fs.Sub(viewerFiles, "viewer")
	mux := http.NewServeMux()
	mux.Handle("GET /novnc/", http.FileServerFS(files))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		page, _ := fs.ReadFile(files, "index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(strings.ReplaceAll(string(page), "{{ID}}", id)))
	})
	mux.HandleFunc("GET /alive", func(w http.ResponseWriter, r *http.Request) {
		if v.gone.Load() {
			w.WriteHeader(http.StatusGone)
		}
	})
	mux.HandleFunc("GET /ws", v.serveScreen)
	go http.Serve(ln, mux)

	url := "http://" + ln.Addr().String() + "/"
	report(url, nil)
	if open {
		openWindow(url)
	}
	return v.wait(ctx)
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
func (v *viewer) wait(ctx context.Context) error {
	idleSince, goneSince := time.Now(), time.Time{}
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
			idleSince = now
		}
		if goneSince.IsZero() {
			if ok, err := v.exists(ctx); !ok && isNotFound(err) {
				v.gone.Store(true)
				goneSince = now
			}
		}
		if now.Sub(idleSince) > viewerLinger || (!goneSince.IsZero() && now.Sub(goneSince) > viewerLinger) {
			return nil
		}
	}
}

// serveScreen bridges the page's WebSocket to the sandbox's VNC port through the sbx API.
func (v *viewer) serveScreen(w http.ResponseWriter, r *http.Request) {
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
