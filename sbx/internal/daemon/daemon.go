//go:build linux

package daemon

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/kjj6198/agent-sandbox-eval/sbx/internal/proto"
)

type Daemon struct {
	cfg        *Config
	token      string
	pool       *netPool
	fcVersion  string
	buildMu    sync.Mutex // one template build at a time
	templateMu sync.Mutex // template directory changes
	mu         sync.Mutex
	sandboxes  map[string]*sandbox
	pending    int // creates in progress, counted against MaxSandboxes
	hostPorts  map[int]bool
}

type sandbox struct {
	mu        sync.Mutex // serializes snapshot, expose, and destroy
	state     sandboxState
	vm        *vm
	agent     *agentClient
	exposures map[int]*exposure
	destroyed bool

	egressMu sync.Mutex // guards state.Egress and blocked; writers also hold mu
	blocked  []proto.Blocked

	lastActive atomic.Int64 // unix nanoseconds
	inflight   atomic.Int32 // open requests and forwarded connections
}

// sandboxState is persisted to sandboxes/<id>.json so a restarted sbxd can adopt its VMs.
type sandboxState struct {
	ID        string            `json:"id"`
	Template  string            `json:"template"`
	Slot      int               `json:"slot"`
	PID       int               `json:"pid"`
	PIDStart  uint64            `json:"pid_start"`
	CPUs      int               `json:"cpus"`
	MemoryMiB int               `json:"memory_mib"`
	CreatedAt time.Time         `json:"created_at"`
	TTL       proto.Duration    `json:"ttl"`
	CreateMs  float64           `json:"create_ms"`
	Env       map[string]string `json:"env,omitempty"`
	Ports     map[int]int       `json:"ports,omitempty"` // guest port -> host port
	Egress    *proto.Egress     `json:"egress,omitempty"`
}

func New(cfg *Config) (*Daemon, error) {
	token, err := os.ReadFile(cfg.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("read token: %w", err)
	}
	d := &Daemon{
		cfg:       cfg,
		token:     strings.TrimSpace(string(token)),
		pool:      newNetPool(cfg.MaxSandboxes+cfg.PoolSize+4, cfg.PoolSize, cfg.UIDBase),
		sandboxes: map[string]*sandbox{},
		hostPorts: map[int]bool{},
	}
	if d.token == "" {
		return nil, errors.New("token file is empty")
	}
	out, err := exec.Command(cfg.Firecracker, "--version").Output()
	if err != nil {
		return nil, fmt.Errorf("firecracker: %w", err)
	}
	d.fcVersion = strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	return d, nil
}

// Start prepares the host, adopts VMs that survived a restart, and removes everything else
// left over from a previous run.
func (d *Daemon) Start(ctx context.Context) error {
	for _, dir := range []string{"sandboxes", "templates", "jail", "build"} {
		if err := os.MkdirAll(filepath.Join(d.cfg.DataDir, dir), 0o755); err != nil {
			return err
		}
	}
	if err := setupHostNetwork(d.cfg.EgressPort); err != nil {
		return fmt.Errorf("host network: %w", err)
	}
	if err := d.serveEgress(ctx); err != nil {
		return fmt.Errorf("egress listener: %w", err)
	}
	d.adopt()
	go d.pool.run(ctx)
	go d.reap(ctx)
	return nil
}

func (d *Daemon) adopt() {
	files, _ := filepath.Glob(filepath.Join(d.cfg.DataDir, "sandboxes", "*.json"))
	keep := map[string]bool{}
	for _, f := range files {
		var st sandboxState
		b, err := os.ReadFile(f)
		if err == nil {
			err = json.Unmarshal(b, &st)
		}
		if err != nil {
			os.Remove(f)
			continue
		}
		v := d.newVM(st.ID, st.Slot)
		v.pid, v.start = st.PID, st.PIDStart
		if !v.alive() {
			slog.Info("sandbox did not survive restart", "id", st.ID)
			os.Remove(f)
			continue
		}
		sb := &sandbox{state: st, vm: v, agent: newAgentClient(v.vsock()), exposures: map[int]*exposure{}}
		sb.touch()
		d.pool.claim(st.Slot)
		d.sandboxes[st.ID] = sb
		keep[st.ID] = true
		for port, hostPort := range st.Ports {
			if err := d.listen(sb, port, hostPort); err != nil {
				slog.Warn("re-expose port", "id", st.ID, "port", port, "err", err)
				delete(sb.state.Ports, port)
			}
		}
		slog.Info("adopted sandbox", "id", st.ID, "pid", st.PID)
	}
	// Jails and namespaces nothing claims belong to VMs that died with the old daemon.
	jails, _ := os.ReadDir(filepath.Join(d.jailBase(), "firecracker"))
	for _, j := range jails {
		if keep[j.Name()] {
			continue
		}
		v := &vm{dir: filepath.Join(d.jailBase(), "firecracker", j.Name())}
		if b, err := os.ReadFile(filepath.Join(v.root(), "firecracker.pid")); err == nil {
			v.pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			if v.start = procStart(v.pid); v.start != 0 && isFirecracker(v.pid) {
				v.kill()
			}
		}
		os.RemoveAll(v.dir)
	}
	claimed := map[int]bool{}
	for _, sb := range d.sandboxes {
		claimed[sb.state.Slot] = true
	}
	for _, slot := range existingSlots() {
		if !claimed[slot] {
			teardownSlot(slot)
		}
	}
}

func isFirecracker(pid int) bool {
	b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return strings.Contains(string(b), "firecracker")
}

// reap destroys sandboxes that have been idle longer than their TTL or whose VM died.
func (d *Daemon) reap(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, sb := range d.list() {
			switch {
			case !sb.vm.alive():
				slog.Warn("sandbox VM exited", "id", sb.state.ID, "console", tail(sb.vm.console(), 512))
				d.destroy(sb)
			case sb.expired(time.Now()):
				slog.Info("sandbox idle past its TTL", "id", sb.state.ID, "ttl", time.Duration(sb.state.TTL))
				d.destroy(sb)
			}
		}
	}
}

func (sb *sandbox) touch() { sb.lastActive.Store(time.Now().UnixNano()) }

// begin and end bracket a request or a forwarded connection, which keeps the sandbox alive.
func (sb *sandbox) begin() { sb.inflight.Add(1); sb.touch() }
func (sb *sandbox) end()   { sb.touch(); sb.inflight.Add(-1) }

func (sb *sandbox) expiresAt() *time.Time {
	if sb.state.TTL == 0 {
		return nil
	}
	t := time.Unix(0, sb.lastActive.Load()).Add(time.Duration(sb.state.TTL))
	return &t
}

func (sb *sandbox) expired(now time.Time) bool {
	exp := sb.expiresAt()
	return exp != nil && sb.inflight.Load() == 0 && now.After(*exp)
}

func (d *Daemon) list() []*sandbox {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*sandbox, 0, len(d.sandboxes))
	for _, sb := range d.sandboxes {
		out = append(out, sb)
	}
	return out
}

var errNotFound = errors.New("not found")

func (d *Daemon) get(id string) (*sandbox, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if sb, ok := d.sandboxes[id]; ok {
		return sb, nil
	}
	return nil, fmt.Errorf("sandbox %s %w (it may have expired after its idle TTL; see `sbx ls`)", id, errNotFound)
}

// Create restores a sandbox from a template snapshot.
func (d *Daemon) Create(ctx context.Context, req proto.CreateRequest) (*sandbox, error) {
	name := req.Template
	if name == "" {
		name = "default"
	}
	tpl, err := d.template(name)
	if err != nil {
		return nil, err
	}
	ttl := d.cfg.DefaultTTL
	if req.TTL != nil {
		ttl = *req.TTL
	}
	return d.createFrom(ctx, d.templateDir(name), tpl, ttl, req.Env, req.Egress)
}

func (d *Daemon) createFrom(ctx context.Context, tplDir string, tpl *proto.Template, ttl proto.Duration, env map[string]string, egress *proto.Egress) (*sandbox, error) {
	d.mu.Lock()
	if len(d.sandboxes)+d.pending >= d.cfg.MaxSandboxes {
		d.mu.Unlock()
		return nil, fmt.Errorf("sandbox limit of %d reached; remove some with `sbx rm`", d.cfg.MaxSandboxes)
	}
	d.pending++
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.pending--
		d.mu.Unlock()
	}()
	if avail := memInfoMiB("MemAvailable"); avail < d.cfg.MinFreeMiB {
		return nil, fmt.Errorf("host is low on memory (%d MiB available); remove some sandboxes", avail)
	}

	t0 := time.Now()
	slot, err := d.pool.acquire()
	if err != nil {
		return nil, err
	}
	v := d.newVM(newID("sb"), slot)
	ok := false
	defer func() {
		if !ok {
			v.kill()
			os.RemoveAll(v.dir)
			d.pool.release(slot)
		}
	}()
	if egress != nil {
		if err := restrictSlot(slot, d.cfg.EgressPort); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(v.root(), 0o700); err != nil {
		return nil, err
	}
	for _, f := range []string{"vmstate", "mem", "rootfs.ext4"} {
		if err := os.Link(filepath.Join(tplDir, f), filepath.Join(v.root(), f)); err != nil {
			return nil, err
		}
	}
	upper := filepath.Join(v.root(), "upper.ext4")
	if err := cloneFile(filepath.Join(tplDir, "upper.ext4"), upper); err != nil {
		return nil, fmt.Errorf("clone disk: %w", err)
	}
	if err := os.Chown(upper, v.uid, v.uid); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := d.launch(ctx, v); err != nil {
		return nil, err
	}
	if err := v.restore(ctx); err != nil {
		return nil, fmt.Errorf("%w: %s", err, tail(v.console(), 1024))
	}
	agent := newAgentClient(v.vsock())
	if err := agent.init(ctx, v.id, env); err != nil {
		return nil, err
	}
	sb := &sandbox{
		state: sandboxState{
			ID: v.id, Template: tpl.Name, Slot: slot, PID: v.pid, PIDStart: v.start,
			CPUs: tpl.CPUs, MemoryMiB: tpl.MemoryMiB, CreatedAt: time.Now().UTC(), TTL: ttl,
			CreateMs: float64(time.Since(t0).Microseconds()) / 1000, Env: env, Ports: map[int]int{},
			Egress: egress,
		},
		vm: v, agent: agent, exposures: map[int]*exposure{},
	}
	sb.touch()
	if err := d.save(sb); err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.sandboxes[v.id] = sb
	d.mu.Unlock()
	ok = true
	slog.Info("created sandbox", "id", v.id, "template", tpl.Name, "ms", sb.state.CreateMs)
	return sb, nil
}

func (d *Daemon) save(sb *sandbox) error {
	b, err := json.MarshalIndent(sb.state, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(d.cfg.DataDir, "sandboxes", sb.state.ID+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (d *Daemon) destroy(sb *sandbox) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if sb.destroyed {
		return
	}
	sb.destroyed = true
	d.mu.Lock()
	delete(d.sandboxes, sb.state.ID)
	d.mu.Unlock()
	for port := range sb.exposures {
		d.unlistenLocked(sb, port)
	}
	sb.vm.kill()
	os.RemoveAll(sb.vm.dir)
	os.Remove(filepath.Join(d.cfg.DataDir, "sandboxes", sb.state.ID+".json"))
	d.pool.release(sb.state.Slot)
	slog.Info("destroyed sandbox", "id", sb.state.ID)
}

func (d *Daemon) Destroy(id string) error {
	sb, err := d.get(id)
	if err != nil {
		return err
	}
	d.destroy(sb)
	return nil
}

// Snapshot saves a running sandbox as a new template. The sandbox pauses for about a second
// per GiB of guest memory and then continues.
func (d *Daemon) Snapshot(ctx context.Context, id, name string) (*proto.Template, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	return d.snapshot(ctx, id, name)
}

func (d *Daemon) snapshot(ctx context.Context, id, name string) (*proto.Template, error) {
	sb, err := d.get(id)
	if err != nil {
		return nil, err
	}
	sb.begin()
	defer sb.end()
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if sb.destroyed {
		return nil, fmt.Errorf("sandbox %s %w", id, errNotFound)
	}
	d.templateMu.Lock()
	defer d.templateMu.Unlock()
	if _, err := os.Stat(d.templateDir(name)); err == nil {
		return nil, fmt.Errorf("template %q already exists; remove it first with `sbx template rm %s`", name, name)
	}
	tmp, err := os.MkdirTemp(filepath.Join(d.cfg.DataDir, "templates"), ".tmp-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	if err := sb.agent.sync(ctx); err != nil {
		return nil, err
	}
	root := sb.vm.root()
	if err := sb.vm.pause(ctx); err != nil {
		return nil, err
	}
	err = errors.Join(
		sb.vm.snapshot(ctx, "/snap.vmstate", "/snap.mem"),
		// The disk copy must be taken while the guest is paused to match its memory.
		cloneFile(filepath.Join(root, "upper.ext4"), filepath.Join(tmp, "upper.ext4")),
	)
	if rerr := sb.vm.resume(ctx); rerr != nil {
		err = errors.Join(err, rerr)
	}
	// The guest clock stood still while paused.
	ictx, cancel := context.WithTimeout(ctx, 5*time.Second)
	sb.agent.init(ictx, id, sb.state.Env)
	cancel()
	if err != nil {
		os.Remove(filepath.Join(root, "snap.vmstate"))
		os.Remove(filepath.Join(root, "snap.mem"))
		return nil, err
	}
	if err := errors.Join(
		os.Rename(filepath.Join(root, "snap.vmstate"), filepath.Join(tmp, "vmstate")),
		os.Rename(filepath.Join(root, "snap.mem"), filepath.Join(tmp, "mem")),
		os.Link(filepath.Join(root, "rootfs.ext4"), filepath.Join(tmp, "rootfs.ext4")),
	); err != nil {
		return nil, err
	}
	parent, _ := d.template(sb.state.Template)
	tpl := &proto.Template{
		Name: name, CPUs: sb.state.CPUs, MemoryMiB: sb.state.MemoryMiB, Parent: sb.state.Template,
		CreatedAt: time.Now().UTC(),
	}
	if parent != nil {
		tpl.DiskGiB = parent.DiskGiB
	}
	if err := sealTemplate(tmp, tpl); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, d.templateDir(name)); err != nil {
		return nil, err
	}
	return tpl, nil
}

// Fork clones a running sandbox, including its memory, processes, and files.
func (d *Daemon) Fork(ctx context.Context, id string) (*sandbox, error) {
	src, err := d.get(id)
	if err != nil {
		return nil, err
	}
	name := ".fork-" + newID("")
	tpl, err := d.snapshot(ctx, id, name)
	if err != nil {
		return nil, err
	}
	dir := d.templateDir(name)
	// The clone hard-links what it needs, so the temporary template can go right away.
	defer os.RemoveAll(dir)
	tpl.Name = src.state.Template
	return d.createFrom(ctx, dir, tpl, src.state.TTL, src.state.Env, src.state.Egress)
}

func (d *Daemon) SetTTL(id string, ttl proto.Duration) (*sandbox, error) {
	sb, err := d.get(id)
	if err != nil {
		return nil, err
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	sb.state.TTL = ttl
	sb.touch()
	return sb, d.save(sb)
}

func (d *Daemon) view(sb *sandbox, publicHost string) proto.Sandbox {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	s := proto.Sandbox{
		ID: sb.state.ID, Template: sb.state.Template, CPUs: sb.state.CPUs, MemoryMiB: sb.state.MemoryMiB,
		CreatedAt: sb.state.CreatedAt, TTL: sb.state.TTL, ExpiresAt: sb.expiresAt(),
		CreateMs: sb.state.CreateMs, MemoryUsedMiB: sb.vm.privateMiB(),
	}
	sb.egressMu.Lock()
	s.Egress, s.Blocked = sb.state.Egress, slices.Clone(sb.blocked)
	sb.egressMu.Unlock()
	for port, hostPort := range sb.state.Ports {
		s.Ports = append(s.Ports, d.portView(port, hostPort, publicHost))
	}
	return s
}

func newID(prefix string) string {
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789"
	b := make([]byte, 8)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	if prefix == "" {
		return string(b)
	}
	return prefix + "-" + string(b)
}

func memInfoMiB(key string) int {
	b, _ := os.ReadFile("/proc/meminfo")
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == key+":" {
			kib, _ := strconv.Atoi(f[1])
			return kib / 1024
		}
	}
	return 0
}

func diskFreeGiB(path string) int {
	var st unix.Statfs_t
	if unix.Statfs(path, &st) != nil {
		return 0
	}
	return int(st.Bavail * uint64(st.Bsize) >> 30)
}
