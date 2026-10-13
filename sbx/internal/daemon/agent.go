//go:build linux

package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/kjj6198/agent-sandbox-eval/sbx/internal/proto"
)

// dialVsock opens a connection to a guest vsock port through Firecracker's Unix socket.
func dialVsock(ctx context.Context, uds string, port int) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", uds)
	if err != nil {
		return nil, err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	c.SetDeadline(deadline)
	if _, err := fmt.Fprintf(c, "CONNECT %d\n", port); err != nil {
		c.Close()
		return nil, err
	}
	// Firecracker drops anything sent before its "OK <port>\n", and the ack is read byte by
	// byte so no stream data is consumed with it.
	var line []byte
	b := make([]byte, 1)
	for {
		if _, err := c.Read(b); err != nil {
			c.Close()
			return nil, fmt.Errorf("vsock port %d: %w", port, err)
		}
		if b[0] == '\n' {
			break
		}
		if line = append(line, b[0]); len(line) > 64 {
			c.Close()
			return nil, errors.New("vsock: malformed handshake")
		}
	}
	if !bytes.HasPrefix(line, []byte("OK ")) {
		c.Close()
		return nil, fmt.Errorf("vsock port %d: %q", port, line)
	}
	c.SetDeadline(time.Time{})
	return c, nil
}

// agentClient talks HTTP to sbx-agent. Every request opens a new vsock connection: they cost
// microseconds, and a snapshot resets all open ones.
type agentClient struct {
	uds string
	hc  *http.Client
}

func newAgentClient(uds string) *agentClient {
	return &agentClient{uds: uds, hc: &http.Client{Transport: &http.Transport{
		DisableKeepAlives:  true,
		DisableCompression: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialVsock(ctx, uds, proto.AgentPort)
		},
	}}}
}

func (a *agentClient) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://agent"+path, body)
	if err != nil {
		return err
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e proto.Error
		json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("agent %s: %s", path, e.Error)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// init sets the guest clock and hostname, retrying until the agent answers or ctx ends.
func (a *agentClient) init(ctx context.Context, hostname string, env map[string]string) error {
	for {
		req := proto.InitRequest{UnixNano: time.Now().UnixNano(), Hostname: hostname, Env: env}
		err := a.call(ctx, http.MethodPost, "/init", req, nil)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("guest agent did not answer: %w", err)
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func (a *agentClient) sync(ctx context.Context) error {
	return a.call(ctx, http.MethodPost, "/sync", nil, nil)
}

// exec runs a script and copies its output to w, returning the exit status.
func (a *agentClient) exec(ctx context.Context, req proto.ExecRequest, w io.Writer) (proto.ExecResult, error) {
	b, _ := json.Marshal(req)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent/exec", bytes.NewReader(b))
	if err != nil {
		return proto.ExecResult{}, err
	}
	resp, err := a.hc.Do(hreq)
	if err != nil {
		return proto.ExecResult{}, err
	}
	defer resp.Body.Close()
	for {
		typ, p, err := proto.ReadFrame(resp.Body)
		if err != nil {
			return proto.ExecResult{}, fmt.Errorf("exec stream: %w", err)
		}
		if typ == proto.FrameExit {
			var res proto.ExecResult
			return res, json.Unmarshal(p, &res)
		}
		w.Write(p)
	}
}

// proxy forwards r to the agent at path and the answer back to w. A 101 answer turns both
// connections into one byte pipe; that carries interactive exec and TCP forwarding.
func (a *agentClient) proxy(w http.ResponseWriter, r *http.Request, path string) {
	conn, err := dialVsock(r.Context(), a.uds, proto.AgentPort)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("guest agent unreachable: %w", err))
		return
	}
	req, err := http.NewRequest(r.Method, "http://agent"+path+"?"+r.URL.RawQuery, r.Body)
	if err != nil {
		conn.Close()
		writeError(w, http.StatusBadRequest, err)
		return
	}
	req.ContentLength = r.ContentLength
	for _, h := range []string{"Content-Type", "Connection", "Upgrade"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	if err := req.Write(conn); err != nil {
		conn.Close()
		writeError(w, http.StatusBadGateway, err)
		return
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if resp.StatusCode == http.StatusSwitchingProtocols {
		cc, cbrw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			conn.Close()
			return
		}
		fmt.Fprintf(cbrw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", resp.Header.Get("Upgrade"))
		if err := cbrw.Flush(); err != nil {
			cc.Close()
			conn.Close()
			return
		}
		proto.Splice(cc, cbrw.Reader, conn, br)
		return
	}
	defer conn.Close()
	defer resp.Body.Close()
	// A command with no output never writes, so a vanished client is only noticed through
	// the request context. Closing the guest connection makes the agent kill the command.
	stop := context.AfterFunc(r.Context(), func() { conn.Close() })
	defer stop()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	copyFlush(w, resp.Body)
}

// copyFlush streams r to w, flushing after every read so exec output arrives live.
func copyFlush(w http.ResponseWriter, r io.Reader) {
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			rc.Flush()
		}
		if err != nil {
			return
		}
	}
}
