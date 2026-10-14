package cli

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/kjj6198/agent-sandbox-eval/sbx/internal/proto"
)

type execFlags struct {
	env         []string
	cwd         string
	user        string
	timeout     string
	interactive bool
	tty         bool
	detach      bool
}

func (f *execFlags) register(cmd *cobra.Command) {
	fl := cmd.Flags()
	// Everything after the sandbox ID belongs to the command, as in `docker exec`.
	fl.SetInterspersed(false)
	fl.StringArrayVarP(&f.env, "env", "e", nil, "NAME=VALUE, or NAME to copy it from your environment")
	fl.StringVarP(&f.cwd, "workdir", "w", "", "working directory; relative paths are under the user's home")
	fl.StringVarP(&f.user, "user", "u", "", `user to run as (default "user"; "root" works too)`)
	fl.StringVar(&f.timeout, "timeout", "", "kill the command after this long, such as 10m; exit code 124")
	fl.BoolVarP(&f.interactive, "interactive", "i", false, "pass stdin to the command")
	fl.BoolVarP(&f.tty, "tty", "t", false, "allocate a terminal, for shells and interactive programs")
	fl.BoolVarP(&f.detach, "detach", "d", false, "run in the background and print its PID and log file")
}

func (f *execFlags) request(command []string) (proto.ExecRequest, error) {
	if len(command) == 0 {
		return proto.ExecRequest{}, errors.New("no command given")
	}
	env, err := parseEnv(f.env)
	if err != nil {
		return proto.ExecRequest{}, err
	}
	timeout, err := parseTTL(f.timeout)
	if err != nil {
		return proto.ExecRequest{}, fmt.Errorf("--timeout: %w", err)
	}
	req := proto.ExecRequest{
		Script: script(command), Env: env, Cwd: f.cwd, User: f.user,
		Stdin: f.interactive, TTY: f.tty, Detach: f.detach,
	}
	if timeout != nil {
		req.Timeout = *timeout
	}
	if req.Detach && req.Interactive() {
		return req, errors.New("--detach cannot be combined with -i or -t")
	}
	return req, nil
}

// script turns the command into bash source. A single argument already is a script, so
// `sbx exec ID 'cd app && make'` works; several arguments are quoted word by word.
func script(command []string) string {
	if len(command) == 1 {
		return command[0]
	}
	quoted := make([]string, len(command))
	for i, a := range command {
		quoted[i] = shellQuote(a)
	}
	return strings.Join(quoted, " ")
}

func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./=:,+@%") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func execCmd() *cobra.Command {
	var f execFlags
	cmd := &cobra.Command{
		Use:   "exec [flags] ID [--] COMMAND [ARG...]",
		Short: "Run a command in a sandbox",
		Long: "Run a command in a sandbox through bash as a login shell. Output streams live and " +
			"sbx exits with the command's exit code.\n\n" +
			"A single argument is a bash script; several arguments are a command and its arguments. " +
			"Background jobs (`cmd &`) keep running after exec returns; use --detach to also keep their output.",
		Example: "  sbx exec $id -- ls -la\n" +
			"  sbx exec $id 'cd app && pnpm install && pnpm build'\n" +
			"  sbx exec -d $id 'cd app && pnpm dev'\n" +
			"  sbx exec --timeout 5m -w app $id -- go test ./...\n" +
			"  echo 'print(42)' | sbx exec -i $id -- python3\n" +
			"  sbx exec -it $id bash",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			req, err := f.request(stripDashes(args[1:]))
			if err != nil {
				return err
			}
			return runExec(cmd.Context(), c, args[0], req)
		},
	}
	f.register(cmd)
	return cmd
}

func stripDashes(args []string) []string {
	if len(args) > 0 && args[0] == "--" {
		return args[1:]
	}
	return args
}

func runCmd() *cobra.Command {
	var f execFlags
	var template, ttl string
	var keep bool
	cmd := &cobra.Command{
		Use:   "run [flags] [--] COMMAND [ARG...]",
		Short: "Run one command in a new sandbox, then remove it",
		Long: "Create a sandbox, run the command, and remove the sandbox, even if the command fails " +
			"or you press Ctrl-C. sbx exits with the command's exit code.",
		Example: "  sbx run -- python3 -c 'import sys; print(sys.version)'\n" +
			"  curl -sL https://example.com/script.sh | sbx run -i -- bash\n" +
			"  sbx run --template browser --keep -- 'curl -s localhost:9222/json/version'",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			req, err := f.request(stripDashes(args))
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			sb, err := create(ctx, c, template, ttl, nil)
			if err != nil {
				return err
			}
			if keep {
				fmt.Fprintln(os.Stderr, "sbx: sandbox", sb.ID)
			} else {
				defer c.call(context.Background(), "DELETE", "/v1/sandboxes/"+sb.ID, nil, nil)
			}
			return runExec(ctx, c, sb.ID, req)
		},
	}
	f.register(cmd)
	cmd.Flags().StringVar(&template, "template", "", `template to start from (default "default")`)
	cmd.Flags().StringVar(&ttl, "ttl", "", "idle timeout of the sandbox, with --keep")
	cmd.Flags().BoolVar(&keep, "keep", false, "keep the sandbox and print its ID to stderr")
	return cmd
}

func runExec(ctx context.Context, c *client, id string, req proto.ExecRequest) error {
	path := "/v1/sandboxes/" + id + "/exec"
	var res proto.ExecResult
	var err error
	if req.Interactive() {
		res, err = execInteractive(ctx, c, path, req)
	} else {
		res, err = execStream(ctx, c, path, req, os.Stdout, os.Stderr)
	}
	if err != nil {
		return err
	}
	if req.Detach {
		if res.Error != "" {
			return errors.New(res.Error)
		}
		fmt.Printf("pid %d, output in %s\n", res.PID, res.Log)
		return nil
	}
	if res.Error != "" {
		fmt.Fprintln(os.Stderr, "sbx:", res.Error)
	}
	if res.ExitCode != 0 {
		return exitError{res.ExitCode}
	}
	return nil
}

func execStream(ctx context.Context, c *client, path string, req proto.ExecRequest, stdout, stderr io.Writer) (proto.ExecResult, error) {
	b, _ := json.Marshal(req)
	hreq, err := c.request(ctx, "POST", path, strings.NewReader(string(b)))
	if err != nil {
		return proto.ExecResult{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := c.send(hreq)
	if err != nil {
		return proto.ExecResult{}, err
	}
	defer resp.Body.Close()
	return readFrames(resp.Body, stdout, stderr)
}

func execInteractive(ctx context.Context, c *client, path string, req proto.ExecRequest) (proto.ExecResult, error) {
	stdinFd := int(os.Stdin.Fd())
	if req.TTY {
		if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
			req.Cols, req.Rows = uint16(w), uint16(h)
		}
		if term.IsTerminal(stdinFd) {
			old, err := term.MakeRaw(stdinFd)
			if err != nil {
				return proto.ExecResult{}, err
			}
			defer term.Restore(stdinFd, old)
		}
	}
	b, _ := json.Marshal(req)
	conn, err := c.upgrade(ctx, "POST", path, proto.ExecUpgrade, b)
	if err != nil {
		return proto.ExecResult{}, err
	}
	defer conn.Close()

	var mu sync.Mutex
	send := func(typ byte, p []byte) error {
		mu.Lock()
		defer mu.Unlock()
		return proto.WriteFrame(conn, typ, p)
	}
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 && send(proto.FrameStdin, buf[:n]) != nil {
				return
			}
			if err != nil {
				send(proto.FrameStdinEOF, nil)
				return
			}
		}
	}()
	if req.TTY {
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)
		go func() {
			for range winch {
				if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
					p := make([]byte, 4)
					binary.BigEndian.PutUint16(p, uint16(h))
					binary.BigEndian.PutUint16(p[2:], uint16(w))
					send(proto.FrameResize, p)
				}
			}
		}()
	}
	return readFrames(conn, os.Stdout, os.Stderr)
}
