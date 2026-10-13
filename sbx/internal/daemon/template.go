//go:build linux

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kjj6198/agent-sandbox-eval/sbx/internal/proto"
)

// A template directory holds what every clone needs:
//
//	rootfs.ext4  the image, shared read-only by all clones
//	upper.ext4   the writable disk at snapshot time; each clone gets a reflink copy
//	vmstate, mem the Firecracker snapshot of the booted guest
//	template.json
func (d *Daemon) templateDir(name string) string {
	return filepath.Join(d.cfg.DataDir, "templates", name)
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func validName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("template name %q must be 1-32 lowercase letters, digits, or dashes", name)
	}
	return nil
}

func (d *Daemon) template(name string) (*proto.Template, error) {
	b, err := os.ReadFile(filepath.Join(d.templateDir(name), "template.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("template %q %w; see `sbx template ls`", name, errNotFound)
	}
	if err != nil {
		return nil, err
	}
	var t proto.Template
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	t.DiskBytes = diskUsage(d.templateDir(name))
	return &t, nil
}

func (d *Daemon) Templates() []proto.Template {
	entries, _ := os.ReadDir(filepath.Join(d.cfg.DataDir, "templates"))
	var out []proto.Template
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if t, err := d.template(e.Name()); err == nil {
			out = append(out, *t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// DeleteTemplate removes a template. Running clones keep working: they hold hard links.
func (d *Daemon) DeleteTemplate(name string) error {
	if err := validName(name); err != nil {
		return err
	}
	if _, err := d.template(name); err != nil {
		return err
	}
	d.templateMu.Lock()
	defer d.templateMu.Unlock()
	exec.Command("podman", "rmi", "--force", imageName(name)).Run()
	return os.RemoveAll(d.templateDir(name))
}

type BuildOptions struct {
	Name      string
	CPUs      int
	MemoryMiB int
	DiskGiB   int
	// Warmup runs in the booted guest before the snapshot, e.g. to start a browser.
	Warmup string
}

func imageName(name string) string { return "localhost/sbx-template/" + name + ":latest" }

// BuildTemplate builds a Dockerfile context into a template: podman builds the image, it
// becomes an ext4 root disk, a microVM boots it once, and the booted VM is snapshotted.
// Progress goes to logw.
func (d *Daemon) BuildTemplate(ctx context.Context, opt BuildOptions, contextTar io.Reader, logw io.Writer) (*proto.Template, error) {
	if err := validName(opt.Name); err != nil {
		return nil, err
	}
	if opt.CPUs != 1 && (opt.CPUs < 2 || opt.CPUs > 32 || opt.CPUs%2 != 0) {
		return nil, errors.New("cpus must be 1 or an even number up to 32")
	}
	if opt.MemoryMiB < 256 || opt.DiskGiB < 1 {
		return nil, errors.New("memory must be at least 256 MiB and disk at least 1 GiB")
	}
	d.buildMu.Lock()
	defer d.buildMu.Unlock()
	logf := func(format string, args ...any) { fmt.Fprintf(logw, "==> "+format+"\n", args...) }

	work, err := os.MkdirTemp(filepath.Join(d.cfg.DataDir, "build"), opt.Name+"-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	ctxDir := filepath.Join(work, "context")
	if err := proto.ReadTar(contextTar, ctxDir, nil); err != nil {
		return nil, fmt.Errorf("build context: %w", err)
	}

	logf("building image %s", imageName(opt.Name))
	if err := runLogged(ctx, logw, "podman", "build", "--network=host", "-t", imageName(opt.Name), ctxDir); err != nil {
		return nil, err
	}
	logf("exporting the image to an ext4 disk")
	rootfs := filepath.Join(work, "rootfs")
	if err := exportImage(ctx, imageName(opt.Name), rootfs); err != nil {
		return nil, err
	}
	if err := d.prepareRootfs(rootfs); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(filepath.Join(d.cfg.DataDir, "templates"), ".tmp-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	sizeMiB := diskUsage(rootfs)>>20*115/100 + 256
	if err := run("mkfs.ext4", "-q", "-F", "-O", "^has_journal", "-L", "sbx-root", "-d", rootfs,
		filepath.Join(tmp, "rootfs.ext4"), strconv.FormatInt(sizeMiB, 10)+"M"); err != nil {
		return nil, err
	}
	os.RemoveAll(rootfs)
	// The writable disk is sparse; mkfs on a regular file marks inode tables zeroed, so the
	// guest never spends I/O initializing them.
	upper := filepath.Join(tmp, "upper.ext4")
	if err := errors.Join(
		os.Truncate(createEmpty(upper), int64(opt.DiskGiB)<<30),
		run("mkfs.ext4", "-q", "-F", "-O", "^has_journal", "-m", "0", "-L", "sbx-upper", upper),
	); err != nil {
		return nil, err
	}

	logf("booting the template VM (%d vCPU, %d MiB)", opt.CPUs, opt.MemoryMiB)
	if err := d.bootAndSnapshot(ctx, opt, tmp, logw); err != nil {
		return nil, err
	}
	tpl := &proto.Template{Name: opt.Name, CPUs: opt.CPUs, MemoryMiB: opt.MemoryMiB, DiskGiB: opt.DiskGiB, CreatedAt: time.Now().UTC()}
	if err := sealTemplate(tmp, tpl); err != nil {
		return nil, err
	}
	d.templateMu.Lock()
	defer d.templateMu.Unlock()
	dst := d.templateDir(opt.Name)
	old := dst + ".old"
	os.RemoveAll(old)
	if _, err := os.Stat(dst); err == nil {
		if err := os.Rename(dst, old); err != nil {
			return nil, err
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		return nil, err
	}
	os.RemoveAll(old)
	tpl.DiskBytes = diskUsage(dst)
	logf("template %s ready", opt.Name)
	return tpl, nil
}

func (d *Daemon) bootAndSnapshot(ctx context.Context, opt BuildOptions, tmp string, logw io.Writer) error {
	slot, err := d.pool.acquire()
	if err != nil {
		return err
	}
	defer d.pool.release(slot)
	v := d.newVM(newID("build"), slot)
	defer os.RemoveAll(v.dir)
	defer v.kill()
	root := v.root()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	kernel, err := filepath.EvalSymlinks(d.cfg.Kernel)
	if err != nil {
		return fmt.Errorf("guest kernel: %w", err)
	}
	cfg, _ := json.Marshal(map[string]any{
		"boot-source": map[string]any{"kernel_image_path": "/vmlinux", "boot_args": bootArgs},
		"drives": []map[string]any{
			{"drive_id": "rootfs", "path_on_host": "/rootfs.ext4", "is_root_device": true, "is_read_only": true},
			{"drive_id": "upper", "path_on_host": "/upper.ext4", "is_root_device": false, "is_read_only": false},
		},
		"network-interfaces": []map[string]any{{"iface_id": "eth0", "host_dev_name": "tap0", "guest_mac": guestMAC}},
		"vsock":              map[string]any{"guest_cid": 3, "uds_path": "/v.sock"},
		"machine-config":     map[string]any{"vcpu_count": opt.CPUs, "mem_size_mib": opt.MemoryMiB},
		"entropy":            map[string]any{},
	})
	if err := errors.Join(
		os.Link(kernel, filepath.Join(root, "vmlinux")),
		os.Link(filepath.Join(tmp, "rootfs.ext4"), filepath.Join(root, "rootfs.ext4")),
		os.Rename(filepath.Join(tmp, "upper.ext4"), filepath.Join(root, "upper.ext4")),
		os.Chown(filepath.Join(root, "upper.ext4"), v.uid, v.uid),
		os.WriteFile(filepath.Join(root, "config.json"), cfg, 0o644),
	); err != nil {
		return err
	}
	t0 := time.Now()
	if err := d.launch(ctx, v, "--config-file", "/config.json"); err != nil {
		return err
	}
	agent := newAgentClient(v.vsock())
	bootCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := agent.init(bootCtx, "sbx", nil); err != nil {
		return fmt.Errorf("%w\n%s", err, tail(v.console(), 4096))
	}
	fmt.Fprintf(logw, "==> guest agent answered %d ms after launch\n", time.Since(t0).Milliseconds())
	if opt.Warmup != "" {
		fmt.Fprintf(logw, "==> warmup: %s\n", opt.Warmup)
		res, err := agent.exec(ctx, proto.ExecRequest{Script: opt.Warmup}, logw)
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("warmup exited with %d %s", res.ExitCode, res.Error)
		}
	}
	t1 := time.Now()
	if _, err := agent.exec(ctx, proto.ExecRequest{Script: settleScript, User: "root"}, io.Discard); err != nil {
		return err
	}
	fmt.Fprintf(logw, "==> guest settled after %.1f s\n", time.Since(t1).Seconds())
	if err := errors.Join(agent.sync(ctx), v.pause(ctx), v.snapshot(ctx, "/vmstate", "/mem")); err != nil {
		return err
	}
	v.kill()
	return errors.Join(
		os.Rename(filepath.Join(root, "vmstate"), filepath.Join(tmp, "vmstate")),
		os.Rename(filepath.Join(root, "mem"), filepath.Join(tmp, "mem")),
		os.Rename(filepath.Join(root, "upper.ext4"), filepath.Join(tmp, "upper.ext4")),
	)
}

// settleScript waits, up to 30 s, until the guest's free memory has not moved by 1 MiB for
// 2 s. Programs a warmup leaves running keep loading after it returns, and every page they
// touch after the snapshot is copied into each clone: the desktop alone cost every sandbox
// 70 MiB when it was snapshotted mid-startup.
const settleScript = `prev=0 stable=0
for _ in $(seq 120); do
  cur=$(awk '/^MemFree:/ {print $2}' /proc/meminfo)
  delta=$((cur - prev)); delta=${delta#-}
  if [ "$delta" -lt 1024 ]; then stable=$((stable + 1)); else stable=0; fi
  [ "$stable" -ge 8 ] && exit 0
  prev=$cur
  sleep 0.25
done`

// sealTemplate makes the template's files root-owned and read-only and writes its metadata.
func sealTemplate(dir string, tpl *proto.Template) error {
	for _, f := range []string{"rootfs.ext4", "upper.ext4", "vmstate", "mem"} {
		p := filepath.Join(dir, f)
		if err := errors.Join(os.Chown(p, 0, 0), os.Chmod(p, 0o444)); err != nil {
			return err
		}
	}
	b, _ := json.MarshalIndent(tpl, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "template.json"), b, 0o644); err != nil {
		return err
	}
	return os.Chmod(dir, 0o755)
}

func exportImage(ctx context.Context, image, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, "podman", "create", "--entrypoint", "/usr/local/sbin/sbx-agent", image).Output()
	if err != nil {
		return fmt.Errorf("podman create: %w", err)
	}
	cid := strings.TrimSpace(string(out))
	defer exec.Command("podman", "rm", "--force", cid).Run()
	pipe := fmt.Sprintf("podman export %s | tar -x --numeric-owner -C %s", cid, dst)
	if out, err := exec.CommandContext(ctx, "sh", "-c", pipe).CombinedOutput(); err != nil {
		return fmt.Errorf("export image: %v: %s", err, out)
	}
	return nil
}

// resolvConf: glibc otherwise sends A and AAAA queries from one socket in parallel, and the
// lab lost one reply behind the double NAT, stalling every lookup for 5 s.
const resolvConf = "nameserver 1.1.1.1\nnameserver 8.8.8.8\noptions single-request-reopen\n"

// prepareRootfs adds what sbx needs to any image: the agent as init, the overlay mount point,
// and network files that container runtimes normally bind-mount.
func (d *Daemon) prepareRootfs(root string) error {
	agent, err := os.ReadFile(d.cfg.AgentBinary)
	if err != nil {
		return fmt.Errorf("agent binary: %w", err)
	}
	os.Remove(filepath.Join(root, "etc/resolv.conf"))
	os.Remove(filepath.Join(root, ".containerenv"))
	os.Remove(filepath.Join(root, ".dockerenv"))
	return errors.Join(
		os.MkdirAll(filepath.Join(root, "usr/local/sbin"), 0o755),
		os.WriteFile(filepath.Join(root, "usr/local/sbin/sbx-agent"), agent, 0o755),
		os.MkdirAll(filepath.Join(root, "overlay"), 0o755),
		os.WriteFile(filepath.Join(root, "etc/resolv.conf"), []byte(resolvConf), 0o644),
		os.WriteFile(filepath.Join(root, "etc/hostname"), []byte("sbx\n"), 0o644),
		os.WriteFile(filepath.Join(root, "etc/hosts"), []byte("127.0.0.1\tlocalhost\n::1\tlocalhost\n"), 0o644),
	)
}

func createEmpty(path string) string {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err == nil {
		f.Close()
	}
	return path
}

// diskUsage returns the bytes allocated under path, so sparse files count as what they use.
func diskUsage(path string) int64 {
	var total int64
	filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err == nil {
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				total += st.Blocks * 512
			}
		}
		return nil
	})
	return total
}

func runLogged(ctx context.Context, w io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}
