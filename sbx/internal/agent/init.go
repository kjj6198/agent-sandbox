//go:build linux

// Package agent is sbx-agent, the process that runs inside every microVM. As PID 1 it builds
// the root filesystem and reaps orphans; its child serves the host over vsock.
package agent

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// The template's root image (/dev/vda) is shared read-only by every sandbox. Each sandbox has
// its own sparse disk (/dev/vdb) that holds the overlay's upper layer, so writes cost host
// disk instead of guest RAM.
const (
	upperDisk = "/dev/vdb"
	overlay   = "/overlay"
)

// RunInit is PID 1. It never returns.
func RunInit() {
	if err := setupRoot(); err != nil {
		fatal("root filesystem: %v", err)
	}
	if err := setupSystem(); err != nil {
		fatal("system mounts: %v", err)
	}
	for {
		pid, err := startServer()
		if err != nil {
			fatal("start server: %v", err)
		}
		reapUntil(pid)
		fmt.Fprintln(os.Stderr, "sbx-agent: server exited, restarting")
		time.Sleep(200 * time.Millisecond)
	}
}

func setupRoot() error {
	for _, m := range []struct{ src, dst, fs string }{
		{"proc", "/proc", "proc"},
		{"sysfs", "/sys", "sysfs"},
		{"devtmpfs", "/dev", "devtmpfs"},
	} {
		// The kernel mounts devtmpfs itself when CONFIG_DEVTMPFS_MOUNT is set.
		if err := unix.Mount(m.src, m.dst, m.fs, unix.MS_NOSUID, ""); err != nil && !errors.Is(err, unix.EBUSY) {
			return fmt.Errorf("mount %s: %w", m.dst, err)
		}
	}
	if err := unix.Mount(upperDisk, overlay, "ext4", unix.MS_NOATIME, ""); err != nil {
		return fmt.Errorf("mount %s: %w", upperDisk, err)
	}
	for _, d := range []string{"upper", "work", "root"} {
		if err := os.MkdirAll(overlay+"/"+d, 0o755); err != nil {
			return err
		}
	}
	root := overlay + "/root"
	opts := "lowerdir=/,upperdir=" + overlay + "/upper,workdir=" + overlay + "/work"
	if err := unix.Mount("overlay", root, "overlay", 0, opts); err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}
	for _, m := range []string{"/proc", "/sys", "/dev"} {
		if err := unix.Mount(m, root+m, "", unix.MS_MOVE, ""); err != nil {
			return fmt.Errorf("move %s: %w", m, err)
		}
	}
	if err := os.Chdir(root); err != nil {
		return err
	}
	if err := os.MkdirAll("oldroot", 0o700); err != nil {
		return err
	}
	if err := unix.PivotRoot(".", "oldroot"); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	if err := unix.Unmount("/oldroot", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount old root: %w", err)
	}
	return os.Remove("/oldroot")
}

func setupSystem() error {
	for _, d := range []string{"/dev/pts", "/dev/shm", "/run", "/sys/fs/cgroup"} {
		os.MkdirAll(d, 0o755)
	}
	for _, m := range []struct {
		src, dst, fs string
		flags        uintptr
		data         string
	}{
		{"devpts", "/dev/pts", "devpts", unix.MS_NOSUID | unix.MS_NOEXEC, "gid=5,mode=620,ptmxmode=666"},
		{"tmpfs", "/dev/shm", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=1777"},
		{"tmpfs", "/run", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=0755"},
		{"cgroup2", "/sys/fs/cgroup", "cgroup2", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, ""},
	} {
		if err := unix.Mount(m.src, m.dst, m.fs, m.flags, m.data); err != nil {
			return fmt.Errorf("mount %s: %w", m.dst, err)
		}
	}
	os.Symlink("/proc/self/fd", "/dev/fd")
	os.Chmod("/tmp", 0o1777)
	if err := setHostname("sbx"); err != nil {
		return err
	}
	// eth0 is configured by the kernel's ip= parameter; only loopback is left.
	return linkUp("lo")
}

func linkUp(name string) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

func startServer() (int, error) {
	p, err := os.StartProcess("/proc/self/exe", []string{"sbx-agent", "serve"}, &os.ProcAttr{
		Env:   []string{"PATH=" + defaultPath, "LANG=C.UTF-8"},
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	if err != nil {
		return 0, err
	}
	return p.Pid, nil
}

// reapUntil reaps every child, including orphans reparented to PID 1, until pid exits.
func reapUntil(pid int) {
	for {
		var ws unix.WaitStatus
		got, err := unix.Wait4(-1, &ws, 0, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil || got == pid {
			return
		}
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sbx-agent: "+format+"\n", args...)
	// A guest reboot makes Firecracker exit, so a broken sandbox stops instead of hanging.
	unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART)
	select {}
}
