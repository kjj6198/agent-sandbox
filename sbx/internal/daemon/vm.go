//go:build linux

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// vm is one jailed Firecracker process. The jailer chroots it into dir/root, where every path
// in its config and snapshots is relative to that root. That is what lets thousands of clones
// share one snapshot: each clone's root holds hard links to the template's files under the
// same names, plus its own copy of the writable disk.
type vm struct {
	id   string
	slot int
	uid  int
	dir  string
	pid  int
	// start is the process start time, so a recycled PID is never mistaken for this VM.
	start uint64
}

func (d *Daemon) newVM(id string, slot int) *vm {
	return &vm{id: id, slot: slot, uid: d.pool.uid(slot), dir: filepath.Join(d.jailBase(), "firecracker", id)}
}

func (d *Daemon) jailBase() string { return filepath.Join(d.cfg.DataDir, "jail") }

func (v *vm) root() string    { return filepath.Join(v.dir, "root") }
func (v *vm) apiSock() string { return filepath.Join(v.root(), "api.sock") }
func (v *vm) vsock() string   { return filepath.Join(v.root(), "v.sock") }
func (v *vm) console() string { return filepath.Join(v.dir, "console.log") }

// bootArgs boots straight into sbx-agent with eth0 configured by the kernel.
const bootArgs = "console=ttyS0 reboot=k panic=1 pci=off quiet random.trust_cpu=on " + guestNet +
	" init=/usr/local/sbin/sbx-agent ro root=/dev/vda"

// launch starts Firecracker through the jailer and waits for its API socket.
func (d *Daemon) launch(ctx context.Context, v *vm, fcArgs ...string) error {
	logf, err := os.OpenFile(v.console(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	args := []string{
		"--id", v.id, "--exec-file", d.cfg.Firecracker,
		"--uid", strconv.Itoa(v.uid), "--gid", strconv.Itoa(v.uid),
		"--chroot-base-dir", d.jailBase(), "--netns", "/var/run/netns/" + netnsName(v.slot),
		"--new-pid-ns", "--cgroup-version", "2",
		"--", "--api-sock", "/api.sock", "--level", "Warning",
	}
	cmd := exec.Command(d.cfg.Jailer, append(args, fcArgs...)...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	logf.Close()
	if err != nil {
		return err
	}
	// With --new-pid-ns the jailer forks Firecracker, writes its host PID, and exits.
	jailer := make(chan error, 1)
	go func() { jailer <- cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if v.pid == 0 {
			if b, err := os.ReadFile(filepath.Join(v.root(), "firecracker.pid")); err == nil {
				v.pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			}
		}
		if v.pid > 0 {
			if _, err := os.Stat(v.apiSock()); err == nil {
				v.start = procStart(v.pid)
				return nil
			}
		}
		select {
		case err := <-jailer:
			if err != nil {
				return fmt.Errorf("jailer: %v: %s", err, tail(v.console(), 2048))
			}
			jailer = nil
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("firecracker did not start: %s", tail(v.console(), 2048))
		}
	}
}

// api calls Firecracker's REST API on its socket.
func (v *vm) api(ctx context.Context, method, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	hc := &http.Client{Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", v.apiSock())
		},
	}}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("firecracker %s %s: %s", method, path, strings.TrimSpace(string(msg)))
	}
	return nil
}

func (v *vm) pause(ctx context.Context) error {
	return v.api(ctx, http.MethodPatch, "/vm", map[string]string{"state": "Paused"})
}

func (v *vm) resume(ctx context.Context) error {
	return v.api(ctx, http.MethodPatch, "/vm", map[string]string{"state": "Resumed"})
}

// snapshot writes a full snapshot to vmstate and mem, paths inside the jail.
func (v *vm) snapshot(ctx context.Context, vmstate, mem string) error {
	return v.api(ctx, http.MethodPut, "/snapshot/create", map[string]string{
		"snapshot_type": "Full", "snapshot_path": vmstate, "mem_file_path": mem,
	})
}

// restore maps the snapshot memory MAP_PRIVATE, so pages load lazily from the shared page
// cache and only pages the clone writes cost it memory.
func (v *vm) restore(ctx context.Context) error {
	return v.api(ctx, http.MethodPut, "/snapshot/load", map[string]any{
		"snapshot_path": "/vmstate",
		"mem_backend":   map[string]string{"backend_type": "File", "backend_path": "/mem"},
		"resume_vm":     true,
	})
}

func (v *vm) alive() bool {
	return v.pid > 0 && v.start != 0 && procStart(v.pid) == v.start
}

// kill stops the VM and waits until the process is gone. Its tap stays busy until then, and
// the next sandbox in this slot would fail to open it.
func (v *vm) kill() {
	if !v.alive() {
		return
	}
	syscall.Kill(v.pid, syscall.SIGKILL)
	for range 500 {
		if procStart(v.pid) != v.start {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// privateMiB is the VMM's anonymous memory: guest pages the sandbox wrote plus Firecracker's
// own. Guest pages it only read stay in the template's page cache, shared and reclaimable.
func (v *vm) privateMiB() float64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps_rollup", v.pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "Anonymous:" {
			kib, _ := strconv.ParseFloat(f[1], 64)
			return kib / 1024
		}
	}
	return 0
}

// procStart returns field 22 of /proc/PID/stat, or 0 if the process is gone or a zombie.
func procStart(pid int) uint64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	// comm may contain spaces and parentheses, so split after its closing parenthesis.
	f := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+1:]))
	if len(f) < 20 || f[0] == "Z" {
		return 0
	}
	n, _ := strconv.ParseUint(f[19], 10, 64)
	return n
}

// cloneFile makes dst a copy-on-write copy of src (XFS, Btrfs), falling back to a sparse copy.
func cloneFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	err = unix.IoctlFileClone(int(out.Fd()), int(in.Fd()))
	out.Close()
	if err == nil {
		return nil
	}
	os.Remove(dst)
	if err := run("cp", "--sparse=always", src, dst); err != nil {
		return err
	}
	// cp copies the mode of src, and template files are read-only.
	return os.Chmod(dst, 0o600)
}

func tail(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > n {
		f.Seek(-n, io.SeekEnd)
	}
	b, _ := io.ReadAll(f)
	return strings.TrimSpace(string(b))
}
