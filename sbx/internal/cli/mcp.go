package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/kjj6198/agent-sandbox-eval/sbx/internal/proto"
)

const mcpInstructions = `sbx gives you disposable Linux machines (Firecracker microVMs) with a 1280x800 desktop.
Create one with sandbox_create, run shell commands with exec, and operate its screen with
click, type_text, press_key, scroll, and drag. Every screen action waits for the screen to
settle and returns the new screen, so you rarely need screenshot. The "browser" template
starts with Chromium open.

To sign in somewhere for the user, call request_secret for each value they must not paste
into the chat, such as a password, then type it with type_text {"secret": NAME}. The user
watches in a small window and can drop files on it; they land in ~/Downloads.`

// Output beyond this many bytes per stream is cut from exec results, keeping the end.
const execOutputLimit = 30 << 10

// Timing of the wait after a screen action: how long the screen gets to start reacting, how
// long it must then stay still, and the most the wait may take.
const (
	settleReact = time.Second
	settleQuiet = 300 * time.Millisecond
	settleLimit = 5 * time.Second
)

const secretTimeout = 10 * time.Minute

func mcpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Serve sandboxes to an agent over MCP on stdin and stdout",
		Long: "Run an MCP server for agents that use tools rather than a shell. It creates sandboxes, " +
			"runs commands, and operates their desktops like computer use: every click and keystroke " +
			"returns the screen once it has settled. request_secret asks the user for a password in " +
			"the viewer window without the agent ever seeing it, and files the user drops on that " +
			"window are copied to ~/Downloads in the sandbox.",
		Example: "  claude mcp add sbx -- sbx mcp\n" +
			"  # or in .mcp.json: {\"mcpServers\": {\"sbx\": {\"command\": \"sbx\", \"args\": [\"mcp\"]}}}",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			m := &mcpServer{c: c, screens: map[string]*screen{}, viewers: map[string]*viewer{},
				dropped: map[string][]string{}, secrets: newSecretStore()}
			return m.server().Run(cmd.Context(), &mcp.StdioTransport{})
		},
	}
}

type mcpServer struct {
	c       *client
	secrets *secretStore

	mu      sync.Mutex
	screens map[string]*screen
	viewers map[string]*viewer
	dropped map[string][]string // files the user dropped, not yet reported to the agent
}

type idArg struct {
	ID string `json:"id" jsonschema:"sandbox ID, from sandbox_create or sandbox_list"`
}

type pointArg struct {
	idArg
	X int `json:"x" jsonschema:"pixels from the left edge of the screen"`
	Y int `json:"y" jsonschema:"pixels from the top edge of the screen"`
}

type createArgs struct {
	Template string `json:"template,omitempty" jsonschema:"template name; default is \"default\". \"browser\" starts with Chromium open"`
	TTL      string `json:"ttl,omitempty" jsonschema:"idle timeout such as 30m or 2d; 0 never expires"`
	View     bool   `json:"view,omitempty" jsonschema:"open a window on the user's screen showing the sandbox's desktop"`
}

type execArgs struct {
	idArg
	Command string `json:"command" jsonschema:"bash script, run as a login shell by user in /home/user"`
	Timeout string `json:"timeout,omitempty" jsonschema:"kill the command after this long, such as 2m; exit code 124"`
	Detach  bool   `json:"detach,omitempty" jsonschema:"run in the background and return its PID and log file, for servers"`
}

type clickArgs struct {
	pointArg
	Button string `json:"button,omitempty" jsonschema:"left (default), middle, or right"`
	Double bool   `json:"double,omitempty" jsonschema:"double-click"`
}

type dragArgs struct {
	idArg
	FromX int `json:"from_x"`
	FromY int `json:"from_y"`
	ToX   int `json:"to_x"`
	ToY   int `json:"to_y"`
}

type scrollArgs struct {
	pointArg
	Direction string `json:"direction" jsonschema:"up, down, left, or right"`
	Amount    int    `json:"amount,omitempty" jsonschema:"wheel clicks, default 3"`
}

type typeArgs struct {
	idArg
	Text   string `json:"text,omitempty" jsonschema:"text to type; \\n presses Return"`
	Secret string `json:"secret,omitempty" jsonschema:"name of a secret stored with request_secret, typed instead of text"`
}

type keyArgs struct {
	idArg
	Keys string `json:"keys" jsonschema:"one key or chord, such as Return, Escape, Tab, ctrl+l, ctrl+shift+t, alt+F4, Page_Down"`
}

type secretArgs struct {
	idArg
	Name        string `json:"name" jsonschema:"name to store the value under, such as google.password"`
	Description string `json:"description" jsonschema:"what the value is and why you need it, shown to the user"`
}

func (m *mcpServer) server() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "sbx", Version: proto.Version}, &mcp.ServerOptions{Instructions: mcpInstructions})
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}

	mcp.AddTool(s, &mcp.Tool{Name: "sandbox_create", Description: "Create a sandbox: a fresh Debian machine with " +
		"Node.js, Python, Go, Chromium, and a 1280x800 desktop, ready in well under a second. Returns its ID. " +
		"Set view when the user wants to watch."}, m.create)
	mcp.AddTool(s, &mcp.Tool{Name: "sandbox_list", Description: "List running sandboxes.", Annotations: readOnly}, m.list)
	mcp.AddTool(s, &mcp.Tool{Name: "sandbox_remove", Description: "Destroy a sandbox and everything in it."}, m.remove)
	mcp.AddTool(s, &mcp.Tool{Name: "exec", Description: "Run a bash script in the sandbox and return its exit " +
		"code, stdout, and stderr. Prefer it over the screen for anything a command can do. GUI programs started " +
		"here appear on the desktop; start them with detach."}, m.exec)
	mcp.AddTool(s, &mcp.Tool{Name: "screenshot", Description: "Return the sandbox's screen. Screen actions already " +
		"return it, so use this only to look again after waiting.", Annotations: readOnly}, m.screenshot)
	mcp.AddTool(s, &mcp.Tool{Name: "click", Description: "Click at a point of the screen, then return the screen " +
		"once it settles. Coordinates are pixels of the returned screenshots."}, m.click)
	mcp.AddTool(s, &mcp.Tool{Name: "move_mouse", Description: "Move the pointer, for hover effects, then return the " +
		"screen once it settles."}, m.move)
	mcp.AddTool(s, &mcp.Tool{Name: "drag", Description: "Press the left button at one point, move to another, and " +
		"release, then return the screen once it settles."}, m.drag)
	mcp.AddTool(s, &mcp.Tool{Name: "scroll", Description: "Turn the mouse wheel over a point, then return the screen " +
		"once it settles."}, m.scroll)
	mcp.AddTool(s, &mcp.Tool{Name: "type_text", Description: "Type text into the focused field as key presses, then " +
		"return the screen once it settles. Pass secret instead of text to type a value from request_secret; " +
		"type secrets only into password or login fields, never where the page shows them."}, m.typeText)
	mcp.AddTool(s, &mcp.Tool{Name: "press_key", Description: "Press a key or chord, such as Return or ctrl+l, then " +
		"return the screen once it settles."}, m.pressKey)
	mcp.AddTool(s, &mcp.Tool{Name: "request_secret", Description: "Ask the user for a secret, such as a password or " +
		"a one-time code, in the sandbox's viewer window, which opens if needed. The value is stored in this " +
		"server under name and never returned to you; type it with type_text {\"secret\": name}. Never ask the " +
		"user to paste a secret into the chat. Blocks until the user answers, up to 10 minutes. A name stays " +
		"usable until the server exits, so request it once."}, m.requestSecret)
	mcp.AddTool(s, &mcp.Tool{Name: "view", Description: "Open a window on the user's screen showing the sandbox's " +
		"desktop live. The user can take over the mouse and keyboard there, and drop files on it to copy them " +
		"to ~/Downloads in the sandbox."}, m.view)
	return s
}

func (m *mcpServer) create(ctx context.Context, _ *mcp.CallToolRequest, in createArgs) (*mcp.CallToolResult, any, error) {
	sb, err := create(ctx, m.c, in.Template, in.TTL, nil)
	if err != nil {
		return nil, nil, err
	}
	msg := fmt.Sprintf("Created sandbox %s from template %s.", sb.ID, sb.Template)
	if in.View {
		if v, err := m.viewer(ctx, sb.ID); err != nil {
			msg += " The viewer did not open: " + err.Error()
		} else {
			openWindow(v.url)
		}
	}
	return text(msg), nil, nil
}

func (m *mcpServer) list(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	var list []proto.Sandbox
	if err := m.c.call(ctx, "GET", "/v1/sandboxes", nil, &list); err != nil {
		return nil, nil, err
	}
	b, _ := json.MarshalIndent(list, "", "  ")
	return text(string(b)), nil, nil
}

func (m *mcpServer) remove(ctx context.Context, _ *mcp.CallToolRequest, in idArg) (*mcp.CallToolResult, any, error) {
	if err := checkID(in.ID); err != nil {
		return nil, nil, err
	}
	if err := m.c.call(ctx, "DELETE", "/v1/sandboxes/"+in.ID, nil, nil); err != nil {
		return nil, nil, err
	}
	m.forget(in.ID)
	return text("Removed " + in.ID + "."), nil, nil
}

// forget drops what this server holds for a sandbox that is gone. Its viewer window shows
// "sandbox removed" for viewerLinger, then the viewer server stops.
func (m *mcpServer) forget(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.screens[id]; s != nil {
		s.Close()
	}
	if v := m.viewers[id]; v != nil {
		v.gone.Store(true)
		time.AfterFunc(viewerLinger, func() { v.srv.Close() })
	}
	delete(m.screens, id)
	delete(m.viewers, id)
	delete(m.dropped, id)
}

// touch checks that the sandbox exists, which also counts as activity on it.
func (m *mcpServer) touch(ctx context.Context, id string) error {
	if err := checkID(id); err != nil {
		return err
	}
	err := m.c.call(ctx, "GET", "/v1/sandboxes/"+id, nil, nil)
	if isNotFound(err) {
		m.forget(id)
	}
	return err
}

func (m *mcpServer) exec(ctx context.Context, _ *mcp.CallToolRequest, in execArgs) (*mcp.CallToolResult, any, error) {
	req := proto.ExecRequest{Script: in.Command, Detach: in.Detach}
	if in.Timeout != "" {
		d, err := proto.ParseDuration(in.Timeout)
		if err != nil {
			return nil, nil, fmt.Errorf("timeout: %w", err)
		}
		req.Timeout = proto.Duration(d)
	}
	if err := checkID(in.ID); err != nil {
		return nil, nil, err
	}
	var stdout, stderr tailBuffer
	res, err := execStream(ctx, m.c, "/v1/sandboxes/"+in.ID+"/exec", req, &stdout, &stderr)
	if err != nil {
		return nil, nil, err
	}
	var b strings.Builder
	if in.Detach && res.Error == "" {
		fmt.Fprintf(&b, "Started in the background: pid %d, output in %s.\n", res.PID, res.Log)
	} else {
		fmt.Fprintf(&b, "exit code %d\n", res.ExitCode)
	}
	if res.Error != "" {
		fmt.Fprintf(&b, "error: %s\n", res.Error)
	}
	for _, out := range []struct {
		name string
		buf  *tailBuffer
	}{{"stdout", &stdout}, {"stderr", &stderr}} {
		if out.buf.Len() > 0 {
			fmt.Fprintf(&b, "%s:\n%s\n", out.name, out.buf.String())
		}
	}
	return m.withNotices(in.ID, text(b.String())), nil, nil
}

// tailBuffer keeps the last execOutputLimit bytes written to it.
type tailBuffer struct {
	bytes.Buffer
	cut int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.Buffer.Write(p)
	if over := t.Len() - execOutputLimit; over > 0 {
		t.Next(over)
		t.cut += over
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	if t.cut == 0 {
		return t.Buffer.String()
	}
	return fmt.Sprintf("[first %d bytes cut]\n%s", t.cut, t.Buffer.String())
}

func (m *mcpServer) screenshot(ctx context.Context, _ *mcp.CallToolRequest, in idArg) (*mcp.CallToolResult, any, error) {
	s, err := m.screen(ctx, in.ID)
	if err != nil {
		return nil, nil, err
	}
	return m.screenResult(in.ID, s, "")
}

func (m *mcpServer) click(ctx context.Context, _ *mcp.CallToolRequest, in clickArgs) (*mcp.CallToolResult, any, error) {
	button := map[string]byte{"": buttonLeft, "left": buttonLeft, "middle": buttonMiddle, "right": buttonRight}[in.Button]
	if button == 0 {
		return nil, nil, fmt.Errorf("button %q must be left, middle, or right", in.Button)
	}
	clicks := 1
	if in.Double {
		clicks = 2
	}
	return m.act(ctx, in.ID, func(s *screen) error {
		if err := s.Pointer(in.X, in.Y, 0); err != nil {
			return err
		}
		for range clicks {
			if err := s.Pointer(in.X, in.Y, button); err != nil {
				return err
			}
			if err := s.Pointer(in.X, in.Y, 0); err != nil {
				return err
			}
		}
		return nil
	})
}

func (m *mcpServer) move(ctx context.Context, _ *mcp.CallToolRequest, in pointArg) (*mcp.CallToolResult, any, error) {
	return m.act(ctx, in.ID, func(s *screen) error { return s.Pointer(in.X, in.Y, 0) })
}

func (m *mcpServer) drag(ctx context.Context, _ *mcp.CallToolRequest, in dragArgs) (*mcp.CallToolResult, any, error) {
	return m.act(ctx, in.ID, func(s *screen) error {
		if err := s.Pointer(in.FromX, in.FromY, 0); err != nil {
			return err
		}
		if err := s.Pointer(in.FromX, in.FromY, buttonLeft); err != nil {
			return err
		}
		// Intermediate moves, because many apps start a drag only after the pointer travels.
		const steps = 10
		for i := 1; i <= steps; i++ {
			x := in.FromX + (in.ToX-in.FromX)*i/steps
			y := in.FromY + (in.ToY-in.FromY)*i/steps
			if err := s.Pointer(x, y, buttonLeft); err != nil {
				return err
			}
			time.Sleep(10 * time.Millisecond)
		}
		return s.Pointer(in.ToX, in.ToY, 0)
	})
}

func (m *mcpServer) scroll(ctx context.Context, _ *mcp.CallToolRequest, in scrollArgs) (*mcp.CallToolResult, any, error) {
	wheel := map[string]byte{"up": wheelUp, "down": wheelDown, "left": wheelLeft, "right": wheelRight}[in.Direction]
	if wheel == 0 {
		return nil, nil, fmt.Errorf("direction %q must be up, down, left, or right", in.Direction)
	}
	amount := in.Amount
	if amount <= 0 {
		amount = 3
	}
	return m.act(ctx, in.ID, func(s *screen) error {
		for range amount {
			if err := s.Pointer(in.X, in.Y, wheel); err != nil {
				return err
			}
			if err := s.Pointer(in.X, in.Y, 0); err != nil {
				return err
			}
		}
		return nil
	})
}

func (m *mcpServer) typeText(ctx context.Context, _ *mcp.CallToolRequest, in typeArgs) (*mcp.CallToolResult, any, error) {
	value := in.Text
	switch {
	case in.Text != "" && in.Secret != "":
		return nil, nil, errors.New("pass text or secret, not both")
	case in.Secret != "":
		var ok bool
		if value, ok = m.secrets.get(in.Secret); !ok {
			return nil, nil, fmt.Errorf("no secret named %q; ask the user for it with request_secret", in.Secret)
		}
	case in.Text == "":
		return nil, nil, errors.New("pass text or secret")
	}
	return m.act(ctx, in.ID, func(s *screen) error { return s.typeText(ctx, value) })
}

func (m *mcpServer) pressKey(ctx context.Context, _ *mcp.CallToolRequest, in keyArgs) (*mcp.CallToolResult, any, error) {
	keys, err := parseChord(in.Keys)
	if err != nil {
		return nil, nil, err
	}
	return m.act(ctx, in.ID, func(s *screen) error { return s.pressChord(keys) })
}

func (m *mcpServer) requestSecret(ctx context.Context, _ *mcp.CallToolRequest, in secretArgs) (*mcp.CallToolResult, any, error) {
	v, err := m.viewer(ctx, in.ID)
	if err != nil {
		return nil, nil, err
	}
	if v.watchers.Load() == 0 {
		openWindow(v.url)
	}
	ctx, cancel := context.WithTimeout(ctx, secretTimeout)
	defer cancel()
	switch err := m.secrets.request(ctx, in.ID, in.Name, in.Description); {
	case errors.Is(err, context.DeadlineExceeded):
		return nil, nil, fmt.Errorf("the user did not answer within %s", secretTimeout)
	case err != nil:
		return nil, nil, err
	}
	return text(fmt.Sprintf("Stored secret %q. Type it with type_text {\"secret\": %q}. "+
		"The value is never returned to you.", in.Name, in.Name)), nil, nil
}

func (m *mcpServer) view(ctx context.Context, _ *mcp.CallToolRequest, in idArg) (*mcp.CallToolResult, any, error) {
	v, err := m.viewer(ctx, in.ID)
	if err != nil {
		return nil, nil, err
	}
	openWindow(v.url)
	return text(fmt.Sprintf("Opened the viewer of %s on the user's screen. If no window appeared, "+
		"the user can open %s in a browser.", in.ID, v.url)), nil, nil
}

// viewer returns the in-process viewer server for a sandbox, starting it if needed. It
// serves the window for request_secret and drops, which share this process's memory.
func (m *mcpServer) viewer(ctx context.Context, id string) (*viewer, error) {
	if err := m.touch(ctx, id); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if v := m.viewers[id]; v != nil {
		return v, nil
	}
	v := &viewer{c: m.c, id: id, secrets: m.secrets, dropped: func(paths []string) {
		m.mu.Lock()
		m.dropped[id] = append(m.dropped[id], paths...)
		m.mu.Unlock()
	}}
	if err := startViewerServer(ctx, v); err != nil {
		return nil, err
	}
	m.viewers[id] = v
	return v, nil
}

// screen returns a live VNC connection to the sandbox's desktop. Its tunnel is passive, so
// the request in touch is what keeps a sandbox in use from expiring.
func (m *mcpServer) screen(ctx context.Context, id string) (*screen, error) {
	if err := m.touch(ctx, id); err != nil {
		return nil, err
	}
	m.mu.Lock()
	s := m.screens[id]
	m.mu.Unlock()
	if s != nil {
		if _, err := s.Image(); err == nil {
			return s, nil
		}
		s.Close()
	}
	s, err := dialScreen(ctx, m.c, id)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if prev := m.screens[id]; prev != nil && prev != s {
		if _, err := prev.Image(); err == nil {
			s.Close()
			return prev, nil
		}
	}
	m.screens[id] = s
	return s, nil
}

// act performs a screen action and returns the screen after it settles.
func (m *mcpServer) act(ctx context.Context, id string, action func(*screen) error) (*mcp.CallToolResult, any, error) {
	s, err := m.screen(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	// The SDK runs tool calls concurrently. Two type_text calls must not interleave keys or
	// swap clipboards, or one field could receive the other's secret.
	s.actMu.Lock()
	defer s.actMu.Unlock()
	before := s.Version()
	if err := action(s); err != nil {
		return nil, nil, err
	}
	changed, err := s.Settle(ctx, before, settleReact, settleQuiet, settleLimit)
	if err != nil {
		return nil, nil, err
	}
	if !changed {
		return m.withNotices(id, text(fmt.Sprintf("The screen did not change within %s.", settleReact))), nil, nil
	}
	return m.screenResult(id, s, "The screen after the action.")
}

func (m *mcpServer) screenResult(id string, s *screen, note string) (*mcp.CallToolResult, any, error) {
	img, err := s.Image()
	if err != nil {
		return nil, nil, err
	}
	var b bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&b, img); err != nil {
		return nil, nil, err
	}
	caption := strings.TrimSpace(fmt.Sprintf("%s Screen %dx%d.", note, img.Rect.Dx(), img.Rect.Dy()))
	res := &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: caption},
		&mcp.ImageContent{Data: b.Bytes(), MIMEType: "image/png"},
	}}
	return m.withNotices(id, res), nil, nil
}

// withNotices tells the agent about files the user dropped since its last call.
func (m *mcpServer) withNotices(id string, res *mcp.CallToolResult) *mcp.CallToolResult {
	m.mu.Lock()
	paths := m.dropped[id]
	delete(m.dropped, id)
	m.mu.Unlock()
	if len(paths) > 0 {
		res.Content = append(res.Content, &mcp.TextContent{
			Text: "The user dropped files into the sandbox: " + strings.Join(paths, ", "),
		})
	}
	return res
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}
