//go:build linux

package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"

	"github.com/kjj6198/agent-sandbox-eval/sbx/internal/proto"
)

const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

type server struct {
	mu  sync.Mutex
	env map[string]string // sandbox-wide environment from the last /init
}

// RunServer serves sbxd on vsock. Only the host can reach this port, so there is no auth.
func RunServer() {
	l, err := vsock.Listen(proto.AgentPort, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sbx-agent: listen on vsock %d: %v\n", proto.AgentPort, err)
		os.Exit(1)
	}
	s := &server{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("POST /init", s.init)
	mux.HandleFunc("POST /sync", func(w http.ResponseWriter, r *http.Request) {
		unix.Sync()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /exec", s.exec)
	mux.HandleFunc("GET /archive", s.getArchive)
	mux.HandleFunc("PUT /archive", s.putArchive)
	mux.HandleFunc("GET /tcp", s.tcp)
	fmt.Fprintf(os.Stderr, "sbx-agent %s: serving on vsock port %d\n", proto.Version, proto.AgentPort)
	if err := (&http.Server{Handler: mux}).Serve(l); err != nil {
		fmt.Fprintf(os.Stderr, "sbx-agent: %v\n", err)
		os.Exit(1)
	}
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, proto.Health{Version: proto.Version, Uptime: uptime()})
}

func (s *server) init(w http.ResponseWriter, r *http.Request) {
	var req proto.InitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.UnixNano != 0 {
		ts := unix.NsecToTimespec(req.UnixNano)
		if err := unix.ClockSettime(unix.CLOCK_REALTIME, &ts); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("set clock: %w", err))
			return
		}
	}
	if req.Hostname != "" {
		if err := setHostname(req.Hostname); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	s.mu.Lock()
	s.env = req.Env
	s.mu.Unlock()
	s.health(w, r)
}

// setHostname also maps the name in /etc/hosts, because sudo warns on every call otherwise.
func setHostname(name string) error {
	if err := unix.Sethostname([]byte(name)); err != nil {
		return fmt.Errorf("sethostname: %w", err)
	}
	hosts := "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n127.0.1.1\t" + name + "\n"
	os.WriteFile("/etc/hostname", []byte(name+"\n"), 0o644)
	return os.WriteFile("/etc/hosts", []byte(hosts), 0o644)
}

func (s *server) getArchive(w http.ResponseWriter, r *http.Request) {
	u, err := lookupUser(r.URL.Query().Get("user"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p := u.resolve(r.URL.Query().Get("path"))
	if _, err := os.Lstat(p); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	if err := proto.WriteTar(w, p); err != nil {
		// Headers are gone; abort so the client sees a truncated stream, not a valid archive.
		panic(http.ErrAbortHandler)
	}
}

func (s *server) putArchive(w http.ResponseWriter, r *http.Request) {
	u, err := lookupUser(r.URL.Query().Get("user"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	owner := &proto.TarOwner{UID: int(u.uid), GID: int(u.gid)}
	if err := proto.ReadTar(r.Body, u.resolve(r.URL.Query().Get("path")), owner); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// tcp connects the caller to a TCP port inside the guest. It tries loopback first, so a dev
// server bound to localhost is reachable without changing its bind address.
func (s *server) tcp(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.URL.Query().Get("port"))
	if err != nil || port < 1 || port > 65535 {
		writeError(w, http.StatusBadRequest, errors.New("port must be 1-65535"))
		return
	}
	var target net.Conn
	for _, host := range []string{"127.0.0.1", "::1", guestIP} {
		target, err = net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 2*time.Second)
		if err == nil {
			break
		}
	}
	if target == nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("nothing is listening on port %d inside the sandbox", port))
		return
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		target.Close()
		return
	}
	brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	if err := brw.Flush(); err != nil {
		conn.Close()
		target.Close()
		return
	}
	proto.Splice(conn, brw.Reader, target, target)
}

// guestIP is eth0's address, identical in every sandbox (see daemon/network.go).
const guestIP = "172.16.0.2"

func uptime() float64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseFloat(strings.Fields(string(b))[0], 64)
	return v
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, proto.Error{Error: err.Error()})
}
