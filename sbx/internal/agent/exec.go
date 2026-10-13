//go:build linux

package agent

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/kjj6198/agent-sandbox-eval/sbx/internal/proto"
)

// outputGrace is how long output may keep arriving after the command exits. Background jobs
// such as `npm run dev &` inherit the output pipes; once the grace period ends their output is
// discarded, so exec returns while they keep running.
const outputGrace = 200 * time.Millisecond

func (s *server) exec(w http.ResponseWriter, r *http.Request) {
	var req proto.ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	u, err := lookupUser(req.User)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	cmd := s.command(req, u)
	switch {
	case req.Detach:
		s.execDetached(w, cmd, u)
	case req.Interactive():
		s.execInteractive(w, cmd, req)
	default:
		s.execStream(w, r, cmd, req)
	}
}

func (s *server) command(req proto.ExecRequest, u *account) *exec.Cmd {
	shell := "/bin/bash"
	if _, err := os.Stat(shell); err != nil {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell, "-lc", req.Script)
	cmd.Dir = u.resolve(req.Cwd)
	env := map[string]string{
		"PATH": defaultPath, "HOME": u.home, "USER": u.name, "LOGNAME": u.name,
		"SHELL": shell, "LANG": "C.UTF-8",
	}
	if req.TTY {
		env["TERM"] = "xterm-256color"
	}
	s.mu.Lock()
	maps.Copy(env, s.env)
	s.mu.Unlock()
	maps.Copy(env, req.Env)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: u.uid, Gid: u.gid, Groups: u.groups},
	}
	return cmd
}

func (s *server) execStream(w http.ResponseWriter, r *http.Request, cmd *exec.Cmd, req proto.ExecRequest) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "application/x-sbx-stream")
	w.WriteHeader(http.StatusOK)
	rc.Flush()
	var mu sync.Mutex
	flush := func() { rc.Flush() }
	stdout := &proto.FrameWriter{W: w, Type: proto.FrameStdout, Mu: &mu, Flush: flush}
	stderr := &proto.FrameWriter{W: w, Type: proto.FrameStderr, Mu: &mu, Flush: flush}
	res := run(r.Context(), cmd, time.Duration(req.Timeout), nil, stdout, stderr)
	writeExit(w, &mu, res)
	flush()
}

// execInteractive switches the connection to frames in both directions. Closing the
// connection kills the command.
func (s *server) execInteractive(w http.ResponseWriter, cmd *exec.Cmd, req proto.ExecRequest) {
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + proto.ExecUpgrade + "\r\n\r\n")
	if brw.Flush() != nil {
		return
	}
	var mu sync.Mutex
	stdout := &proto.FrameWriter{W: conn, Type: proto.FrameStdout, Mu: &mu}
	stderr := &proto.FrameWriter{W: conn, Type: proto.FrameStderr, Mu: &mu}
	stdinR, stdinW := io.Pipe()
	resize := make(chan *pty.Winsize, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer cancel()
		for {
			typ, p, err := proto.ReadFrame(brw.Reader)
			if err != nil {
				stdinW.Close()
				return
			}
			switch typ {
			case proto.FrameStdin:
				if _, err := stdinW.Write(p); err != nil {
					return
				}
			case proto.FrameStdinEOF:
				stdinW.Close()
			case proto.FrameResize:
				if len(p) == 4 {
					ws := &pty.Winsize{Rows: binary.BigEndian.Uint16(p), Cols: binary.BigEndian.Uint16(p[2:])}
					select {
					case resize <- ws:
					default:
					}
				}
			}
		}
	}()
	var res proto.ExecResult
	if req.TTY {
		res = runTTY(ctx, cmd, req, stdinR, stdout, resize)
	} else {
		res = run(ctx, cmd, time.Duration(req.Timeout), stdinR, stdout, stderr)
	}
	stdinR.Close()
	writeExit(conn, &mu, res)
}

// execDetached starts the command in its own session with output in a log file.
func (s *server) execDetached(w http.ResponseWriter, cmd *exec.Cmd, u *account) {
	f, err := os.CreateTemp("/tmp", "sbx-*.log")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	os.Chown(f.Name(), int(u.uid), int(u.gid))
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr.Setsid = true
	err = cmd.Start()
	f.Close()
	res := proto.ExecResult{}
	if err != nil {
		os.Remove(f.Name())
		res = proto.ExecResult{ExitCode: 127, Error: err.Error()}
	} else {
		res.PID, res.Log = cmd.Process.Pid, f.Name()
		if log := fmt.Sprintf("/tmp/sbx-%d.log", res.PID); os.Rename(f.Name(), log) == nil {
			res.Log = log
		}
		go cmd.Wait()
	}
	w.Header().Set("Content-Type", "application/x-sbx-stream")
	writeExit(w, &sync.Mutex{}, res)
}

// run starts cmd in its own process group and copies its output until it exits. A timeout or
// a cancelled ctx kills the whole group.
func run(ctx context.Context, cmd *exec.Cmd, timeout time.Duration, stdin io.Reader, stdout, stderr io.Writer) proto.ExecResult {
	cmd.SysProcAttr.Setpgid = true
	outR, outW, err := os.Pipe()
	if err != nil {
		return failed(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		return failed(err)
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	var inR, inW *os.File
	if stdin != nil {
		if inR, inW, err = os.Pipe(); err != nil {
			return failed(err)
		}
		cmd.Stdin = inR
	}
	err = cmd.Start()
	outW.Close()
	errW.Close()
	if inR != nil {
		inR.Close()
	}
	if err != nil {
		outR.Close()
		errR.Close()
		if inW != nil {
			inW.Close()
		}
		return failed(err)
	}
	if inW != nil {
		go func() {
			io.Copy(inW, stdin)
			inW.Close()
		}()
	}

	gates := []*gate{{w: stdout}, {w: stderr}}
	done := make(chan struct{}, 2)
	for i, r := range []*os.File{outR, errR} {
		go func() {
			io.Copy(gates[i], r)
			r.Close()
			done <- struct{}{}
		}()
	}
	res := wait(ctx, cmd, timeout)
	drain(done, 2)
	for _, g := range gates {
		g.close()
	}
	return res
}

func runTTY(ctx context.Context, cmd *exec.Cmd, req proto.ExecRequest, stdin io.Reader, out io.Writer, resize <-chan *pty.Winsize) proto.ExecResult {
	attrs := &syscall.SysProcAttr{Setsid: true, Setctty: true, Credential: cmd.SysProcAttr.Credential}
	size := &pty.Winsize{Rows: req.Rows, Cols: req.Cols}
	if size.Rows == 0 || size.Cols == 0 {
		size = &pty.Winsize{Rows: 24, Cols: 80}
	}
	ptmx, err := pty.StartWithAttrs(cmd, size, attrs)
	if err != nil {
		return failed(err)
	}
	defer ptmx.Close()
	go io.Copy(ptmx, stdin)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case ws := <-resize:
				pty.Setsize(ptmx, ws)
			case <-stop:
				return
			}
		}
	}()
	g := &gate{w: out}
	done := make(chan struct{}, 1)
	go func() {
		io.Copy(g, ptmx)
		done <- struct{}{}
	}()
	res := wait(ctx, cmd, time.Duration(req.Timeout))
	drain(done, 1)
	g.close()
	return res
}

func wait(ctx context.Context, cmd *exec.Cmd, timeout time.Duration) proto.ExecResult {
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}
	select {
	case err := <-waitErr:
		return exitResult(err)
	case <-timer:
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-waitErr
		return proto.ExecResult{ExitCode: 124, Error: fmt.Sprintf("timed out after %s", timeout)}
	case <-ctx.Done():
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-waitErr
		return proto.ExecResult{ExitCode: 137, Error: "client disconnected"}
	}
}

// drain waits for n output copiers to finish, but no longer than outputGrace.
func drain(done <-chan struct{}, n int) {
	grace := time.NewTimer(outputGrace)
	defer grace.Stop()
	for ; n > 0; n-- {
		select {
		case <-done:
		case <-grace.C:
			return
		}
	}
}

func exitResult(err error) proto.ExecResult {
	if err == nil {
		return proto.ExecResult{}
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return proto.ExecResult{ExitCode: 128 + int(ws.Signal())}
		}
		return proto.ExecResult{ExitCode: ee.ExitCode()}
	}
	return failed(err)
}

func failed(err error) proto.ExecResult {
	return proto.ExecResult{ExitCode: 127, Error: err.Error()}
}

func writeExit(w io.Writer, mu *sync.Mutex, res proto.ExecResult) {
	b, _ := json.Marshal(res)
	mu.Lock()
	defer mu.Unlock()
	proto.WriteFrame(w, proto.FrameExit, b)
}

// gate forwards writes until closed or until the destination fails, then discards them so
// the process writing to the pipe never blocks or gets SIGPIPE.
type gate struct {
	mu     sync.Mutex
	w      io.Writer
	closed bool
}

func (g *gate) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.closed {
		if _, err := g.w.Write(p); err != nil {
			g.closed = true
		}
	}
	return len(p), nil
}

func (g *gate) close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}
