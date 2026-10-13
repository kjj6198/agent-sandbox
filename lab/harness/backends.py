"""Backend adapters. Each launches its runtime directly; prepare_* runs outside the timed window."""
import ctypes
import http.client
import json
import os
import shutil
import socket
import subprocess
import time
from dataclasses import dataclass, field
from datetime import datetime
from pathlib import Path

from .probe import LineWatcher, descendants, now_ns, pids_matching, wait_pids_gone

SBX = Path("/opt/sbx")
RUN = Path("/run/sbx")
KERNEL = SBX / "kernel/vmlinux"
ROOTFS_EXT4 = SBX / "images/rootfs.ext4"
OCI_ROOTFS = SBX / "oci/rootfs"
OCI_TEMPLATE = SBX / "oci/config.json"
VCPUS = 2
MNT_DETACH = 2

libc = ctypes.CDLL("libc.so.6", use_errno=True)


def mount(source: str, target: str, fstype: str, data: str = "") -> None:
    if libc.mount(source.encode(), target.encode(), fstype.encode(), 0, data.encode() or None) != 0:
        err = ctypes.get_errno()
        raise OSError(err, f"mount {fstype} on {target}: {os.strerror(err)}")


def run(argv: list[str], check: bool = True, timeout: float = 60) -> subprocess.CompletedProcess:
    p = subprocess.run(argv, capture_output=True, text=True, timeout=timeout)
    if check and p.returncode:
        raise RuntimeError(f"exit {p.returncode}: {' '.join(argv)}: {(p.stdout + p.stderr)[-800:]}")
    return p


class UnixHTTPConnection(http.client.HTTPConnection):
    def __init__(self, path: str, timeout: float = 30):
        super().__init__("localhost", timeout=timeout)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


def unix_api(sock: str, method: str, path: str, body: dict | None = None) -> str:
    conn = UnixHTTPConnection(sock)
    try:
        conn.request(method, path, body=json.dumps(body) if body is not None else None,
                     headers={"Content-Type": "application/json"})
        resp = conn.getresponse()
        text = resp.read().decode(errors="replace")
        if resp.status >= 300:
            raise RuntimeError(f"{method} {path} -> {resp.status}: {text}")
        return text
    finally:
        conn.close()


def wait_path(path: str, timeout_s: float = 5) -> None:
    deadline = time.monotonic() + timeout_s
    while not os.path.exists(path):
        if time.monotonic() > deadline:
            raise TimeoutError(f"{path} did not appear")
        time.sleep(0.0005)


def file_sizes(d: Path) -> dict[str, int]:
    return {p.name: p.stat().st_size for p in sorted(d.iterdir()) if p.is_file()}


def warm_page_cache(d: Path) -> None:
    for p in d.iterdir():
        if p.is_file():
            with p.open("rb") as f:
                while f.read(1 << 24):
                    pass


@dataclass
class Prep:
    slot: int
    argv: list[str]
    mem_mib: int = 1024
    extra: dict = field(default_factory=dict)


@dataclass
class Sandbox:
    backend: str
    slot: int
    guest_ip: str
    host_ip: str
    proc: subprocess.Popen | None = None
    watcher: LineWatcher | None = None
    t0_ns: int = 0
    wall0_ns: int = 0
    extra: dict = field(default_factory=dict)

    def console(self) -> str:
        return self.watcher.text() if self.watcher else ""


def spawn_piped(argv: list[str]) -> tuple[subprocess.Popen, LineWatcher]:
    proc = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, start_new_session=True)
    return proc, LineWatcher(proc.stdout)


def wait_launcher(proc: subprocess.Popen, watcher: LineWatcher, what: str) -> None:
    """For detach-style launchers (runsc/runc run --detach) the launch call is the launcher's lifetime."""
    if proc.wait(timeout=60) != 0:
        time.sleep(0.2)
        raise RuntimeError(f"{what} exited {proc.returncode}: {watcher.text()[-800:]}")


class Backend:
    id = ""
    chrome_sandbox = True
    restore_variants: tuple[str, ...] = ()

    def guest_ip(self, slot: int) -> str: ...
    def host_ip(self, slot: int) -> str: ...
    def cleanup(self, slot: int) -> None: ...
    def prepare(self, slot: int, mem_mib: int = 1024) -> Prep: ...
    def start(self, prep: Prep) -> Sandbox: ...
    def stop(self, sb: Sandbox) -> None: ...
    def pids(self, sb: Sandbox) -> set[int]: ...

    def init_ns(self, sb: Sandbox) -> int | None:
        return sb.watcher.t_marker if sb.watcher else None

    def console(self, sb: Sandbox) -> str:
        return sb.console()

    def sandbox(self, slot: int, **kw) -> Sandbox:
        return Sandbox(self.id, slot, self.guest_ip(slot), self.host_ip(slot), **kw)

    def snapshot(self, sb: Sandbox, out: Path, variant: str) -> dict: ...
    def prepare_restore(self, slot: int, src: Path, variant: str) -> Prep: ...
    def restore(self, prep: Prep) -> Sandbox: ...


class VMBackend(Backend):
    prefix = ""

    def guest_ip(self, slot):
        return f"172.16.{slot}.2"

    def host_ip(self, slot):
        return f"172.16.{slot}.1"

    def mac(self, slot):
        return f"06:00:ac:10:{slot:02x}:02"

    def boot_args(self, slot: int) -> str:
        return (f"console=ttyS0 reboot=k panic=1 quiet random.trust_cpu=on "
                f"ip={self.guest_ip(slot)}::{self.host_ip(slot)}:255.255.255.0::eth0:off "
                f"init=/sbin/sbx-init ro root=/dev/vda sbx.host={self.host_ip(slot)} sbx.slot={slot}")

    def dir(self, slot) -> Path:
        return RUN / f"{self.prefix}-{slot}"

    def cleanup(self, slot):
        run([str(SBX / f"bin/{self.prefix}-stop"), str(slot)], check=False)
        self.dir(slot).mkdir(parents=True, exist_ok=True)

    def start(self, prep):
        proc, watcher = spawn_piped(prep.argv)
        return self.sandbox(prep.slot, proc=proc, watcher=watcher)

    def stop(self, sb):
        if sb.proc:
            sb.proc.kill()
            sb.proc.wait()
        shutil.rmtree(self.dir(sb.slot), ignore_errors=True)

    def pids(self, sb):
        return {sb.proc.pid} if sb.proc and sb.proc.poll() is None else set()


class Firecracker(VMBackend):
    id = "firecracker"
    prefix = "fc"
    restore_variants = ("file",)
    clock_realtime = False

    def boot_args(self, slot):
        # Firecracker has no PCI bus unless --enable-pci; Cloud Hypervisor needs it.
        return super().boot_args(slot).replace("quiet", "pci=off quiet")

    def config(self, slot: int, mem_mib: int) -> dict:
        return {
            "boot-source": {"kernel_image_path": str(KERNEL), "boot_args": self.boot_args(slot)},
            "drives": [{"drive_id": "rootfs", "path_on_host": str(ROOTFS_EXT4),
                        "is_root_device": True, "is_read_only": True}],
            "network-interfaces": [{"iface_id": "eth0", "host_dev_name": f"sbxtap{slot}",
                                    "guest_mac": self.mac(slot)}],
            "machine-config": {"vcpu_count": VCPUS, "mem_size_mib": mem_mib},
            "entropy": {},
        }

    def prepare(self, slot, mem_mib=1024, api=False):
        self.cleanup(slot)
        d = self.dir(slot)
        (d / "config.json").write_text(json.dumps(self.config(slot, mem_mib)))
        mode = ["--api-sock", str(d / "api.sock")] if api else ["--no-api"]
        return Prep(slot, ["firecracker", "--id", f"sbx-fc-{slot}", *mode,
                           "--config-file", str(d / "config.json")], mem_mib)

    def snapshot(self, sb, out, variant):
        sock = str(self.dir(sb.slot) / "api.sock")
        shutil.rmtree(out, ignore_errors=True)
        out.mkdir(parents=True)
        t = now_ns()
        unix_api(sock, "PATCH", "/vm", {"state": "Paused"})
        t_paused = now_ns()
        unix_api(sock, "PUT", "/snapshot/create", {"snapshot_type": "Full", "snapshot_path": str(out / "vmstate"),
                                                   "mem_file_path": str(out / "mem")})
        t_done = now_ns()
        (out / "slot").write_text(str(sb.slot))
        sizes = file_sizes(out)
        return {"t_create_ms": (t_done - t_paused) / 1e6, "t_pause_ms": (t_paused - t) / 1e6,
                "mem_file_bytes": sizes["mem"], "state_bytes": sizes["vmstate"], "files": sizes}

    def prepare_restore(self, slot, src, variant):
        self.cleanup(slot)
        sock = str(self.dir(slot) / "api.sock")
        return Prep(slot, ["firecracker", "--id", f"sbx-fc-{slot}", "--api-sock", sock],
                    extra={"sock": sock, "src": str(src)})

    def load_body(self, prep: Prep) -> dict:
        src = prep.extra["src"]
        return {"snapshot_path": f"{src}/vmstate",
                "mem_backend": {"backend_type": "File", "backend_path": f"{src}/mem"},
                "network_overrides": [{"iface_id": "eth0", "host_dev_name": f"sbxtap{prep.slot}"}],
                "clock_realtime": self.clock_realtime, "resume_vm": True}

    def restore(self, prep):
        sb = self.start(prep)
        wait_path(prep.extra["sock"])
        try:
            unix_api(prep.extra["sock"], "PUT", "/snapshot/load", self.load_body(prep))
        except Exception as e:
            raise RuntimeError(f"{e}; console: {sb.console()[-500:]}") from e
        return sb


class FirecrackerHugePages(Firecracker):
    """Firecracker with guest RAM on 2 MiB hugetlbfs pages. Needs vm.nr_hugepages reserved on the host."""
    id = "firecracker-hp"
    restore_variants = ()

    def config(self, slot: int, mem_mib: int) -> dict:
        cfg = super().config(slot, mem_mib)
        cfg["machine-config"]["huge_pages"] = "2M"
        return cfg


class FirecrackerJailer(Firecracker):
    """Firecracker through the jailer: chroot, cgroup v2, unprivileged uid (the production launcher)."""
    id = "firecracker-jailer"
    restore_variants = ()
    uid = gid = 1000  # owner of the persistent sbxtapN devices
    jail_base = Path("/srv/jailer")

    def jail_id(self, slot) -> str:
        return f"sbx-fcj-{slot}"

    def jail_root(self, slot) -> Path:
        return self.jail_base / "firecracker" / self.jail_id(slot) / "root"

    def cgroup(self, slot) -> Path:
        return Path("/sys/fs/cgroup/firecracker") / self.jail_id(slot)

    def cleanup(self, slot):
        for pid in pids_matching(f"--id\0{self.jail_id(slot)}\0"):
            os.kill(pid, 9)
            wait_pids_gone({pid})
        shutil.rmtree(self.jail_root(slot).parent, ignore_errors=True)
        if self.cgroup(slot).exists():
            self.cgroup(slot).rmdir()

    def prepare(self, slot, mem_mib=1024):
        self.cleanup(slot)
        root = self.jail_root(slot)
        root.mkdir(parents=True)
        os.link(KERNEL, root / "vmlinux")
        os.link(ROOTFS_EXT4, root / "rootfs.ext4")
        cfg = self.config(slot, mem_mib)
        cfg["boot-source"]["kernel_image_path"] = "vmlinux"
        cfg["drives"][0]["path_on_host"] = "rootfs.ext4"
        (root / "config.json").write_text(json.dumps(cfg))
        for p in (root, root / "config.json"):
            os.chown(p, self.uid, self.gid)
        return Prep(slot, ["jailer", "--id", self.jail_id(slot), "--exec-file", shutil.which("firecracker"),
                           "--uid", str(self.uid), "--gid", str(self.gid), "--cgroup-version", "2",
                           "--cgroup", f"cpu.max={VCPUS * 100000} 100000",
                           "--", "--no-api", "--config-file", "config.json"], mem_mib)

    def stop(self, sb):
        super().stop(sb)
        self.cleanup(sb.slot)


class CloudHypervisor(VMBackend):
    id = "cloud-hypervisor"
    prefix = "ch"
    restore_variants = ("copy", "ondemand")

    def sock(self, slot) -> str:
        return str(self.dir(slot) / "api.sock")

    def prepare(self, slot, mem_mib=1024):
        self.cleanup(slot)
        return Prep(slot, ["cloud-hypervisor", "--api-socket", f"path={self.sock(slot)}",
                           "--kernel", str(KERNEL), "--cmdline", self.boot_args(slot),
                           "--disk", f"path={ROOTFS_EXT4},readonly=on",
                           "--net", f"tap=sbxtap{slot},mac={self.mac(slot)}",
                           "--cpus", f"boot={VCPUS}", "--memory", f"size={mem_mib}M",
                           "--rng", "src=/dev/urandom", "--serial", "tty", "--console", "off"], mem_mib)

    def snapshot(self, sb, out, variant):
        sock = self.sock(sb.slot)
        shutil.rmtree(out, ignore_errors=True)
        out.mkdir(parents=True)
        t = now_ns()
        unix_api(sock, "PUT", "/api/v1/vm.pause")
        t_paused = now_ns()
        unix_api(sock, "PUT", "/api/v1/vm.snapshot", {"destination_url": f"file://{out}"})
        t_done = now_ns()
        (out / "slot").write_text(str(sb.slot))
        sizes = file_sizes(out)
        return {"t_create_ms": (t_done - t_paused) / 1e6, "t_pause_ms": (t_paused - t) / 1e6,
                "mem_file_bytes": sizes["memory-ranges"],
                "state_bytes": sizes["state.json"] + sizes["config.json"], "files": sizes}

    def prepare_restore(self, slot, src, variant):
        self.cleanup(slot)
        opts = f"source_url=file://{src},resume=true"
        if variant == "ondemand":
            opts += ",memory_restore_mode=ondemand"
        return Prep(slot, ["cloud-hypervisor", "--api-socket", f"path={self.sock(slot)}", "--restore", opts])

    def restore(self, prep):
        return self.start(prep)


class OCIBackend(Backend):
    runtime = ""

    def guest_ip(self, slot):
        return f"172.18.{slot}.2"

    def host_ip(self, slot):
        return f"172.18.{slot}.1"

    def cid(self, slot) -> str:
        return f"sbx-{self.runtime}-{slot}"

    def dir(self, slot) -> Path:
        return RUN / f"{self.runtime}-{slot}"

    def bundle_config(self, slot: int, rootfs: Path) -> dict:
        cfg = json.loads(OCI_TEMPLATE.read_text())
        cfg["root"]["path"] = str(rootfs)
        cfg["hostname"] = f"sbx-{slot}"
        cfg["process"]["env"] += [f"SBX_HOST={self.host_ip(slot)}", f"SBX_SLOT={slot}"]
        for ns in cfg["linux"]["namespaces"]:
            if ns["type"] == "network":
                ns["path"] = f"/run/netns/sbxns{slot}"
        cfg["linux"]["cgroupsPath"] = f"/sbx/{self.cid(slot)}"
        return cfg

    def write_bundle(self, slot: int, rootfs: Path, mem_mib: int = 1024) -> Path:
        bundle = self.dir(slot) / "bundle"
        bundle.mkdir(parents=True, exist_ok=True)
        cfg = self.bundle_config(slot, rootfs)
        cfg["linux"]["resources"]["memory"]["limit"] = mem_mib << 20
        (bundle / "config.json").write_text(json.dumps(cfg))
        return bundle

    def cleanup(self, slot):
        run([str(SBX / f"bin/{self.runtime}-stop"), str(slot)], check=False)
        # runsc strips eth0's address from the netns and never puts it back.
        run([str(SBX / "bin/net-setup"), str(slot)])

    def start(self, prep):
        proc, watcher = spawn_piped(prep.argv)
        sb = self.sandbox(prep.slot, proc=proc, watcher=watcher)
        wait_launcher(proc, watcher, prep.argv[0])
        return sb

    def pids(self, sb):
        roots = pids_matching(f"{self.cid(sb.slot)}\0") | pids_matching(f"{self.dir(sb.slot)}/")
        return descendants(roots)


class Runsc(OCIBackend):
    id = "runsc"
    runtime = "runsc"
    restore_variants = ("plain", "background")
    platform = "systrap"

    def base(self) -> list[str]:
        return ["runsc", "--root", str(RUN / "runsc-root"), f"--platform={self.platform}",
                "--network=sandbox", "--overlay2=root:memory"]

    def prepare(self, slot, mem_mib=1024):
        self.cleanup(slot)
        bundle = self.write_bundle(slot, OCI_ROOTFS, mem_mib)
        return Prep(slot, [*self.base(), "run", "--detach", "--bundle", str(bundle),
                           "--pid-file", str(self.dir(slot) / "pid"), self.cid(slot)], mem_mib)

    def stop(self, sb):
        pids = self.pids(sb)
        run([*self.base(), "delete", "-force", self.cid(sb.slot)], check=False)
        for pid in pids:
            try:
                os.kill(pid, 9)
            except ProcessLookupError:
                pass
        wait_pids_gone(pids)
        shutil.rmtree(self.dir(sb.slot), ignore_errors=True)

    def snapshot(self, sb, out, variant):
        shutil.rmtree(out, ignore_errors=True)
        out.mkdir(parents=True)
        opts = ["--compression=none", "--exclude-committed-zero-pages"] if variant == "background" else []
        t = now_ns()
        run([*self.base(), "checkpoint", "--image-path", str(out), *opts, self.cid(sb.slot)], timeout=300)
        t_done = now_ns()
        (out / "slot").write_text(str(sb.slot))
        sizes = file_sizes(out)
        return {"t_create_ms": (t_done - t) / 1e6, "t_pause_ms": None,
                "mem_file_bytes": sizes.get("pages.img", 0),
                "state_bytes": sizes.get("checkpoint.img", 0) + sizes.get("pages_meta.img", 0), "files": sizes}

    def prepare_restore(self, slot, src, variant):
        self.cleanup(slot)
        bundle = self.write_bundle(slot, OCI_ROOTFS)
        opts = ["--background"] if variant == "background" else []
        return Prep(slot, [*self.base(), "restore", "--detach", "--image-path", str(src), *opts,
                           "--bundle", str(bundle), "--pid-file", str(self.dir(slot) / "pid"), self.cid(slot)])

    def restore(self, prep):
        return self.start(prep)


class Runc(OCIBackend):
    id = "runc"
    runtime = "runc"

    def base(self) -> list[str]:
        return ["runc", "--root", str(RUN / "runc-root")]

    def prepare(self, slot, mem_mib=1024):
        self.cleanup(slot)
        ovl = self.dir(slot) / "ovl"
        ovl.mkdir(parents=True)
        bundle = self.write_bundle(slot, ovl / "merged", mem_mib)
        return Prep(slot, [*self.base(), "run", "--detach", "--bundle", str(bundle),
                           "--pid-file", str(self.dir(slot) / "pid"), self.cid(slot)], mem_mib,
                    extra={"ovl": ovl})

    def start(self, prep):
        # runc has no rootfs overlay of its own; runsc and the VM init do this work inside their timed path.
        ovl = prep.extra["ovl"]
        mount("tmpfs", str(ovl), "tmpfs")
        for d in ("upper", "work", "merged"):
            (ovl / d).mkdir()
        mount("overlay", str(ovl / "merged"), "overlay",
              f"lowerdir={OCI_ROOTFS},upperdir={ovl}/upper,workdir={ovl}/work")
        return super().start(prep)

    def pids(self, sb):
        procs = Path(f"/sys/fs/cgroup/sbx/{self.cid(sb.slot)}/cgroup.procs")
        try:
            return {int(p) for p in procs.read_text().split()}
        except OSError:
            return set()

    def stop(self, sb):
        pids = self.pids(sb)
        run([*self.base(), "delete", "--force", self.cid(sb.slot)], check=False)
        wait_pids_gone(pids)
        ovl = self.dir(sb.slot) / "ovl"
        libc.umount2(str(ovl / "merged").encode(), MNT_DETACH)
        libc.umount2(str(ovl).encode(), MNT_DETACH)
        shutil.rmtree(self.dir(sb.slot), ignore_errors=True)


class Docker(Backend):
    def __init__(self, runtime: str, chrome_sandbox: bool):
        self.runtime = runtime
        self.id = f"docker-{runtime}"
        self.chrome_sandbox = chrome_sandbox

    def guest_ip(self, slot):
        return f"172.30.{slot}.2"

    def host_ip(self, slot):
        return "172.30.0.1"

    def name(self, slot) -> str:
        return f"sbx-docker-{slot}"

    def cleanup(self, slot):
        run(["docker", "rm", "-f", self.name(slot)], check=False)

    def prepare(self, slot, mem_mib=1024):
        self.cleanup(slot)
        return Prep(slot, ["docker", "run", "-d", "--name", self.name(slot), "--runtime", self.runtime,
                           "--network", "sbxnet", "--ip", self.guest_ip(slot), "--hostname", f"sbx-{slot}",
                           "--cpus", str(VCPUS), "--memory", f"{mem_mib}m", "--memory-swap", f"{mem_mib}m",
                           "--tmpfs", "/work:mode=1777",
                           "-v", "/opt/sbx/guest/resolv.conf:/etc/resolv.conf:ro",
                           "-e", f"SBX_HOST={self.host_ip(slot)}", "-e", f"SBX_SLOT={slot}",
                           "sbx-guest:latest"], mem_mib)

    def start(self, prep):
        p = run(prep.argv)
        return self.sandbox(prep.slot, extra={"cid": p.stdout.strip()})

    def init_ns(self, sb):
        """dockerd's receive timestamp of the SBX_INIT line, mapped from wall clock onto the T0 timeline."""
        log = Path(f"/var/lib/docker/containers/{sb.extra['cid']}/{sb.extra['cid']}-json.log")
        try:
            for line in log.read_text().splitlines():
                rec = json.loads(line)
                if rec["log"].startswith("SBX_INIT"):
                    ts = rec["time"]
                    base, frac = ts.rstrip("Z").split(".")
                    wall = int(datetime.fromisoformat(base + "+00:00").timestamp()) * 10**9 + int(frac.ljust(9, "0")[:9])
                    return sb.t0_ns + (wall - sb.wall0_ns)
        except (OSError, ValueError, KeyError):
            pass
        return None

    def console(self, sb) -> str:
        return run(["docker", "logs", "--tail", "30", self.name(sb.slot)], check=False).stdout

    def pids(self, sb):
        state = run(["docker", "inspect", "-f", "{{.State.Pid}}", sb.extra["cid"]], check=False).stdout.strip()
        if not state.isdigit() or state == "0":
            return set()
        pid = int(state)
        # Walk up to the containerd shim so runsc's sandbox, gofer and shim are all counted.
        while pid > 1:
            with open(f"/proc/{pid}/comm") as f:
                if f.read().startswith("containerd-shim"):
                    return descendants({pid})
            with open(f"/proc/{pid}/stat") as f:
                pid = int(f.read().rsplit(")", 1)[1].split()[1])
        return descendants({int(state)})

    def stop(self, sb):
        run(["docker", "rm", "-f", self.name(sb.slot)], check=False)


BACKENDS: dict[str, Backend] = {
    "firecracker": Firecracker(),
    "firecracker-jailer": FirecrackerJailer(),
    "firecracker-hp": FirecrackerHugePages(),
    "cloud-hypervisor": CloudHypervisor(),
    "runsc": Runsc(),
    "runc": Runc(),
    "docker-runsc": Docker("runsc", chrome_sandbox=True),
    "docker-runc": Docker("runc", chrome_sandbox=False),
    "docker-runsc-kvm": Docker("runsc-kvm", chrome_sandbox=True),
}

# Fixed per-backend slots for sequential scenarios; families never share a slot concurrently.
SLOTS = {"firecracker": 1, "cloud-hypervisor": 2, "runsc": 3, "runc": 4,
         "docker-runsc": 5, "docker-runc": 6, "docker-runsc-kvm": 7, "firecracker-jailer": 8,
         "firecracker-hp": 9}
