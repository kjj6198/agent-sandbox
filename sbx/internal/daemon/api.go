//go:build linux

package daemon

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"

	"github.com/kjj6198/agent-sandbox-eval/sbx/internal/proto"
)

// Handler serves the sbx API. Every route needs the bearer token.
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", d.handleStatus)
	mux.HandleFunc("GET /v1/sandboxes", d.handleList)
	mux.HandleFunc("POST /v1/sandboxes", d.handleCreate)
	mux.HandleFunc("GET /v1/sandboxes/{id}", d.withSandbox(d.handleGet))
	mux.HandleFunc("PATCH /v1/sandboxes/{id}", d.withSandbox(d.handleUpdate))
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", d.handleDelete)
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", d.withSandbox(proxyTo("/exec")))
	mux.HandleFunc("GET /v1/sandboxes/{id}/files", d.withSandbox(proxyTo("/archive")))
	mux.HandleFunc("PUT /v1/sandboxes/{id}/files", d.withSandbox(proxyTo("/archive")))
	mux.HandleFunc("GET /v1/sandboxes/{id}/tcp", d.withSandbox(proxyTo("/tcp")))
	mux.HandleFunc("GET /v1/sandboxes/{id}/screenshot", d.withSandbox(proxyTo("/screenshot")))
	mux.HandleFunc("POST /v1/sandboxes/{id}/ports", d.withSandbox(d.handleExpose))
	mux.HandleFunc("DELETE /v1/sandboxes/{id}/ports/{port}", d.withSandbox(d.handleUnexpose))
	mux.HandleFunc("POST /v1/sandboxes/{id}/snapshot", d.withSandbox(d.handleSnapshot))
	mux.HandleFunc("POST /v1/sandboxes/{id}/fork", d.withSandbox(d.handleFork))
	mux.HandleFunc("GET /v1/templates", d.handleTemplates)
	mux.HandleFunc("POST /v1/templates/{name}", d.handleBuild)
	mux.HandleFunc("DELETE /v1/templates/{name}", d.handleDeleteTemplate)
	return d.auth(mux)
}

func (d *Daemon) auth(next http.Handler) http.Handler {
	want := []byte("Bearer " + d.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			writeError(w, http.StatusUnauthorized, errors.New("missing or wrong API token"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withSandbox looks up {id} and marks the sandbox active for the whole request, so a long
// exec or an open port forward never lets it expire. Requests with passive=1 do not count,
// so a screen viewer left open cannot keep an abandoned sandbox alive.
func (d *Daemon) withSandbox(h func(http.ResponseWriter, *http.Request, *sandbox)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sb, err := d.get(r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		if r.URL.Query().Get("passive") != "1" {
			sb.begin()
			defer sb.end()
		}
		h(w, r, sb)
	}
}

func proxyTo(path string) func(http.ResponseWriter, *http.Request, *sandbox) {
	return func(w http.ResponseWriter, r *http.Request, sb *sandbox) {
		sb.agent.proxy(w, r, path)
	}
}

func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, proto.Status{
		Version: proto.Version, Firecracker: d.fcVersion,
		Sandboxes: len(d.list()), MaxSandboxes: d.cfg.MaxSandboxes,
		MemAvailMiB: memInfoMiB("MemAvailable"), MemTotalMiB: memInfoMiB("MemTotal"),
		DiskFreeGiB: diskFreeGiB(d.cfg.DataDir), Templates: len(d.Templates()),
		PublicHost: d.publicHost(r), DefaultTTL: d.cfg.DefaultTTL, PoolReady: d.pool.readyCount(),
	})
}

func (d *Daemon) handleList(w http.ResponseWriter, r *http.Request) {
	out := []proto.Sandbox{}
	for _, sb := range d.list() {
		out = append(out, d.view(sb, d.publicHost(r)))
	}
	writeJSON(w, http.StatusOK, out)
}

func (d *Daemon) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req proto.CreateRequest
	if !decode(w, r, &req) {
		return
	}
	sb, err := d.Create(r.Context(), req)
	if err != nil {
		writeDaemonError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, d.view(sb, d.publicHost(r)))
}

func (d *Daemon) handleGet(w http.ResponseWriter, r *http.Request, sb *sandbox) {
	writeJSON(w, http.StatusOK, d.view(sb, d.publicHost(r)))
}

func (d *Daemon) handleUpdate(w http.ResponseWriter, r *http.Request, sb *sandbox) {
	var req proto.UpdateRequest
	if !decode(w, r, &req) {
		return
	}
	if req.TTL != nil {
		if _, err := d.SetTTL(sb.state.ID, *req.TTL); err != nil {
			writeDaemonError(w, err)
			return
		}
	}
	if req.Egress != nil {
		if _, err := d.SetEgress(sb.state.ID, req.Egress); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, d.view(sb, d.publicHost(r)))
}

func (d *Daemon) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := d.Destroy(r.PathValue("id")); err != nil {
		writeDaemonError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d *Daemon) handleExpose(w http.ResponseWriter, r *http.Request, sb *sandbox) {
	var req proto.ExposeRequest
	if !decode(w, r, &req) {
		return
	}
	p, err := d.Expose(sb.state.ID, req.Port, d.publicHost(r))
	if err != nil {
		writeDaemonError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (d *Daemon) handleUnexpose(w http.ResponseWriter, r *http.Request, sb *sandbox) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := d.Unexpose(sb.state.ID, port); err != nil {
		writeDaemonError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d *Daemon) handleSnapshot(w http.ResponseWriter, r *http.Request, sb *sandbox) {
	var req proto.SnapshotRequest
	if !decode(w, r, &req) {
		return
	}
	tpl, err := d.Snapshot(r.Context(), sb.state.ID, req.Name)
	if err != nil {
		writeDaemonError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, tpl)
}

func (d *Daemon) handleFork(w http.ResponseWriter, r *http.Request, sb *sandbox) {
	clone, err := d.Fork(r.Context(), sb.state.ID)
	if err != nil {
		writeDaemonError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, d.view(clone, d.publicHost(r)))
}

func (d *Daemon) handleTemplates(w http.ResponseWriter, r *http.Request) {
	out := d.Templates()
	if out == nil {
		out = []proto.Template{}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleBuild takes the build context as a tar body and answers with a frame stream: the
// build log as FrameStdout, then FrameExit.
func (d *Daemon) handleBuild(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opt := BuildOptions{
		Name: r.PathValue("name"), CPUs: atoiOr(q.Get("cpus"), 2),
		MemoryMiB: atoiOr(q.Get("memory_mib"), 2048), DiskGiB: atoiOr(q.Get("disk_gib"), 16),
		Warmup: q.Get("warmup"),
	}
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "application/x-sbx-stream")
	w.WriteHeader(http.StatusOK)
	var mu sync.Mutex
	logw := &proto.FrameWriter{W: w, Type: proto.FrameStdout, Mu: &mu, Flush: func() { rc.Flush() }}
	res := proto.ExecResult{}
	if _, err := d.BuildTemplate(r.Context(), opt, r.Body, logw); err != nil {
		res = proto.ExecResult{ExitCode: 1, Error: err.Error()}
	}
	b, _ := json.Marshal(res)
	mu.Lock()
	proto.WriteFrame(w, proto.FrameExit, b)
	mu.Unlock()
}

func (d *Daemon) handleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	if err := d.DeleteTemplate(r.PathValue("name")); err != nil {
		writeDaemonError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// publicHost is the host name the client used for the API, which also reaches exposed ports
// when the API is behind a port forward.
func (d *Daemon) publicHost(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.Host); err == nil {
		return host
	}
	return r.Host
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

func writeDaemonError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, errNotFound) {
		status = http.StatusNotFound
	}
	writeError(w, status, err)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, proto.Error{Error: err.Error()})
}
