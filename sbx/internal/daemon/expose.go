//go:build linux

package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/kjj6198/agent-sandbox-eval/sbx/internal/proto"
)

// An exposure publishes a guest TCP port on a host port. Each accepted connection is tunneled
// to the guest over vsock, so the sandbox network stays closed to inbound traffic.
type exposure struct {
	hostPort int
	ln       net.Listener
}

// Expose publishes a guest port and returns it. Exposing a published port again returns it.
func (d *Daemon) Expose(id string, port int, publicHost string) (proto.Port, error) {
	if port < 1 || port > 65535 {
		return proto.Port{}, errors.New("port must be 1-65535")
	}
	sb, err := d.get(id)
	if err != nil {
		return proto.Port{}, err
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if sb.destroyed {
		return proto.Port{}, fmt.Errorf("sandbox %s %w", id, errNotFound)
	}
	if hostPort, ok := sb.state.Ports[port]; ok {
		return d.portView(port, hostPort, publicHost), nil
	}
	if err := d.listen(sb, port, 0); err != nil {
		return proto.Port{}, err
	}
	if err := d.save(sb); err != nil {
		d.unlistenLocked(sb, port)
		return proto.Port{}, err
	}
	return d.portView(port, sb.state.Ports[port], publicHost), nil
}

func (d *Daemon) Unexpose(id string, port int) error {
	sb, err := d.get(id)
	if err != nil {
		return err
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if _, ok := sb.exposures[port]; !ok {
		return fmt.Errorf("port %d of sandbox %s is not exposed: %w", port, id, errNotFound)
	}
	d.unlistenLocked(sb, port)
	return d.save(sb)
}

// listen binds hostPort, or the first free port in the configured range when it is 0, and
// serves connections to the guest port. The caller holds sb.mu.
func (d *Daemon) listen(sb *sandbox, port, hostPort int) error {
	var ln net.Listener
	var err error
	if hostPort != 0 {
		ln, err = d.bindPort(hostPort)
	} else {
		for p := d.cfg.PortMin; p <= d.cfg.PortMax; p++ {
			if ln, err = d.bindPort(p); err == nil {
				hostPort = p
				break
			}
		}
		if ln == nil {
			return fmt.Errorf("no free host port in %d-%d", d.cfg.PortMin, d.cfg.PortMax)
		}
	}
	if err != nil {
		return err
	}
	sb.exposures[port] = &exposure{hostPort: hostPort, ln: ln}
	sb.state.Ports[port] = hostPort
	go d.serveExposure(sb, port, ln)
	return nil
}

func (d *Daemon) bindPort(p int) (net.Listener, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.hostPorts[p] {
		return nil, fmt.Errorf("host port %d is in use", p)
	}
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(p))
	if err != nil {
		return nil, err
	}
	d.hostPorts[p] = true
	return ln, nil
}

// unlistenLocked closes the exposure of port. The caller holds sb.mu.
func (d *Daemon) unlistenLocked(sb *sandbox, port int) {
	e, ok := sb.exposures[port]
	if !ok {
		return
	}
	e.ln.Close()
	delete(sb.exposures, port)
	delete(sb.state.Ports, port)
	d.mu.Lock()
	delete(d.hostPorts, e.hostPort)
	d.mu.Unlock()
}

func (d *Daemon) serveExposure(sb *sandbox, port int, ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				slog.Warn("accept on exposed port", "id", sb.state.ID, "port", port, "err", err)
			}
			return
		}
		go func() {
			sb.begin()
			defer sb.end()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			guest, gr, err := sb.agent.dialTCP(ctx, port)
			cancel()
			if err != nil {
				c.Close()
				return
			}
			proto.Splice(c, c, guest, gr)
		}()
	}
}

func (d *Daemon) portView(port, hostPort int, publicHost string) proto.Port {
	if d.cfg.PublicHost != "" {
		publicHost = d.cfg.PublicHost
	}
	return proto.Port{
		Port: port, HostPort: hostPort,
		URL: "http://" + net.JoinHostPort(publicHost, strconv.Itoa(hostPort)),
	}
}

// dialTCP opens a byte stream to a TCP port inside the guest. Reads must go through the
// returned reader, which may hold bytes buffered with the upgrade response.
func (a *agentClient) dialTCP(ctx context.Context, port int) (net.Conn, io.Reader, error) {
	conn, err := dialVsock(ctx, a.uds, proto.AgentPort)
	if err != nil {
		return nil, nil, err
	}
	fmt.Fprintf(conn, "GET /tcp?port=%d HTTP/1.1\r\nHost: agent\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n", port)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		resp.Body.Close()
		conn.Close()
		return nil, nil, fmt.Errorf("guest port %d: %s", port, resp.Status)
	}
	return conn, br, nil
}
