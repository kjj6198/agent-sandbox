#!/usr/bin/env python3
"""Restore k clones of one snapshot at the same moment, each clone in its own network namespace
with identical addressing (Firecracker's network-for-clones pattern), and measure time to first exec.

Run as root on the lab:  python3 parallel_restore.py --k 1,5,10 --rounds 3 --out par.jsonl
"""
import argparse
import http.client
import json
import os
import shutil
import signal
import socket
import statistics
import subprocess
import threading
import time
from pathlib import Path

SBX = Path("/opt/sbx")
WORK = SBX / "snapshots" / "par"
KERNEL, ROOTFS = SBX / "kernel/vmlinux", SBX / "images/rootfs.ext4"
OCI_ROOTFS, OCI_TEMPLATE = SBX / "oci/rootfs", SBX / "oci/config.json"
MEM_MIB = 512
VM_GUEST, VM_HOST, MAC, HOST_MAC = "172.16.0.2", "172.16.0.1", "06:00:ac:10:00:02", "06:00:ac:10:00:01"
OCI_GUEST, OCI_HOST, OCI_MAC, OCI_HOST_MAC = "172.18.0.2", "172.18.0.1", "06:00:ac:12:00:02", "06:00:ac:12:00:01"
VM_ARGS = ("8250.nr_uarts=0 i8042.noaux i8042.nomux i8042.dumbkbd reboot=k panic=1 quiet random.trust_cpu=on "
           f"ip={VM_GUEST}::{VM_HOST}:255.255.255.0::eth0:off init=/sbin/sbx-init ro root=/dev/vda "
           f"sbx.host={VM_HOST} sbx.slot=0")
RUNSC = ["runsc", "--root", "/run/sbx/par-runsc", "--platform=systrap", "--network=sandbox", "--overlay2=root:memory"]


def sh(*args, check=True):
    return subprocess.run(args, check=check, capture_output=True, text=True)


def ns_fd(name):
    return os.open(f"/run/netns/{name}", os.O_RDONLY)


def make_vm_ns(name):
    sh("ip", "netns", "del", name, check=False)
    sh("ip", "netns", "add", name)
    sh("ip", "-n", name, "link", "set", "lo", "up")
    sh("ip", "-n", name, "tuntap", "add", "dev", "sbxtap0", "mode", "tap", "vnet_hdr")
    # The restored guest still caches the gateway MAC it saw at snapshot time; a Firecracker guest's
    # clock does not move on restore here, so a different tap MAC breaks replies for up to ~30 s.
    sh("ip", "-n", name, "link", "set", "sbxtap0", "address", HOST_MAC)
    sh("ip", "-n", name, "addr", "add", f"{VM_HOST}/24", "dev", "sbxtap0")
    sh("ip", "-n", name, "link", "set", "sbxtap0", "up")
    # A static neighbor entry: without it, the first SYN can race the restore and wait out a 1 s ARP retry.
    sh("ip", "-n", name, "neigh", "replace", VM_GUEST, "lladdr", MAC, "dev", "sbxtap0", "nud", "permanent")
    return name


def make_oci_ns(name):
    outer, inner = f"{name}o", f"{name}i"
    for n in (outer, inner):
        sh("ip", "netns", "del", n, check=False)
        sh("ip", "netns", "add", n)
        sh("ip", "-n", n, "link", "set", "lo", "up")
    sh("ip", "-n", outer, "link", "add", "v0", "type", "veth", "peer", "name", "eth0", "netns", inner)
    sh("ip", "-n", inner, "link", "set", "eth0", "address", OCI_MAC)
    sh("ip", "-n", outer, "link", "set", "v0", "address", OCI_HOST_MAC)
    sh("ip", "-n", outer, "addr", "add", f"{OCI_HOST}/24", "dev", "v0")
    sh("ip", "-n", inner, "addr", "add", f"{OCI_GUEST}/24", "dev", "eth0")
    sh("ip", "-n", outer, "link", "set", "v0", "up")
    sh("ip", "-n", inner, "link", "set", "eth0", "up")
    sh("ip", "-n", inner, "route", "add", "default", "via", OCI_HOST)
    sh("ip", "-n", outer, "neigh", "replace", OCI_GUEST, "lladdr", OCI_MAC, "dev", "v0", "nud", "permanent")
    return outer


def drop_ns(name):
    for n in (name, f"{name}o", f"{name}i"):
        sh("ip", "netns", "del", n, check=False)


def exec_ok(ip, deadline):
    while True:
        try:
            s = socket.create_connection((ip, 7000), timeout=0.05)
            break
        except OSError:
            if time.monotonic() > deadline:
                raise
            time.sleep(0.001)
    with s:
        s.settimeout(max(0.1, deadline - time.monotonic()))
        s.sendall(b"echo ok; echo __RC=$?__\n")
        out = b""
        while b"__RC=" not in out:
            chunk = s.recv(4096)
            if not chunk:
                raise RuntimeError(f"agent closed: {out!r}")
            out += chunk


def exec_out(ip, cmd, deadline):
    while True:
        try:
            s = socket.create_connection((ip, 7000), timeout=0.05)
            break
        except OSError:
            if time.monotonic() > deadline:
                raise
            time.sleep(0.001)
    with s:
        s.settimeout(max(0.1, deadline - time.monotonic()))
        s.sendall(cmd.encode() + b"; echo __RC=$?__\n")
        out = b""
        while b"__RC=" not in out:
            chunk = s.recv(4096)
            if not chunk:
                break
            out += chunk
    return out.decode(errors="replace").split("__RC=")[0].strip()


def rng_clone_test(backend, snap, out):
    """Restores one fresh snapshot twice and compares the first 32 bytes each clone reads from /dev/urandom."""
    reads = []
    for i in range(2):
        c = Clone(backend, 50 + i, snap)
        box = {}

        def go():
            c.run(time.perf_counter_ns(), box)
            ip = OCI_GUEST if backend == "runsc" else VM_GUEST
            box["urandom"] = exec_out(ip, "head -c32 /dev/urandom | od -An -tx1 | tr -d ' \\n'",
                                      time.monotonic() + 10)
        in_netns(c.ns, go).join()
        reads.append(box.get("urandom", box.get("error")))
        c.stop()
    snap_age = time.time() - os.path.getmtime(snap)
    rec = {"backend": backend, "check": "urandom-fresh-snapshot", "snapshot_age_s": round(snap_age, 1),
           "identical": reads[0] == reads[1], "reads": reads}
    with open(out, "a") as f:
        f.write(json.dumps(rec) + "\n")
    print(f"{backend:17s} urandom identical across 2 restores of a {snap_age:.0f} s old snapshot: {rec['identical']}",
          flush=True)


def wait_path(p, deadline):
    while not os.path.exists(p):
        if time.monotonic() > deadline:
            raise TimeoutError(p)
        time.sleep(0.0005)


class UnixHTTP(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("localhost", timeout=30)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(30)
        self.sock.connect(self.path)


def api(sock, method, url, body):
    conn = UnixHTTP(sock)
    conn.request(method, url, json.dumps(body), {"Content-Type": "application/json"})
    resp = conn.getresponse()
    data = resp.read()
    conn.close()
    if resp.status >= 300:
        raise RuntimeError(f"{url}: {resp.status} {data[:300]!r}")


class Clone:
    """One sandbox restore inside its own netns; run() is called on a thread that has entered that netns."""

    def __init__(self, backend, idx, snap):
        self.backend, self.idx, self.snap = backend, idx, snap
        self.dir = Path(f"/run/sbx/par/{backend}-{idx}")
        shutil.rmtree(self.dir, ignore_errors=True)
        self.dir.mkdir(parents=True)
        self.procs = []
        self.ns = (make_oci_ns if backend == "runsc" else make_vm_ns)(f"pc{backend[:2]}{idx}")
        if backend == "runsc":
            self.cid = f"par-{idx}"
            self.bundle = write_bundle(self.dir, f"pc{backend[:2]}{idx}i")

    def spawn(self, argv):
        log = open(self.dir / "console.log", "wb")
        p = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT,
                             start_new_session=True)
        self.procs.append(p)
        return p

    def run(self, t0, result):
        deadline = time.monotonic() + 20
        try:
            if self.backend == "firecracker":
                sock = str(self.dir / "api.sock")
                self.spawn(["firecracker", "--id", f"par{self.idx}", "--api-sock", sock])
                wait_path(sock, deadline)
                api(sock, "PUT", "/snapshot/load", {
                    "snapshot_path": f"{self.snap}/vmstate",
                    "mem_backend": {"backend_type": "File", "backend_path": f"{self.snap}/mem"},
                    "resume_vm": True})
                ip = VM_GUEST
            elif self.backend == "cloud-hypervisor":
                self.spawn(["cloud-hypervisor", "--api-socket", f"path={self.dir}/api.sock", "--restore",
                            f"source_url=file://{self.snap},resume=true,memory_restore_mode=ondemand"])
                ip = VM_GUEST
            else:
                launcher = self.spawn([*RUNSC, "restore", "--detach", "--image-path", str(self.snap),
                                       "--background", "--bundle", str(self.bundle), self.cid])
                launcher.wait(timeout=20)
                ip = OCI_GUEST
            exec_ok(ip, deadline)
            result.update(ok=True, t_exec_ms=(time.perf_counter_ns() - t0) / 1e6)
        except Exception as e:
            result.update(ok=False, error=f"{type(e).__name__}: {e}"[:300])

    def pids(self):
        if self.backend != "runsc":
            return [p.pid for p in self.procs]
        out = sh(*RUNSC, "ps", "--format=json", self.cid, check=False).stdout
        state = sh(*RUNSC, "state", self.cid, check=False).stdout
        return [json.loads(state)["pid"]] if state.strip().startswith("{") else []

    def stop(self):
        if self.backend == "runsc":
            sh(*RUNSC, "delete", "-force", self.cid, check=False)
        for p in self.procs:
            if p.poll() is None:
                os.killpg(p.pid, signal.SIGKILL)
            p.wait()
        drop_ns(self.ns.rstrip("o") if self.backend == "runsc" else self.ns)
        shutil.rmtree(self.dir, ignore_errors=True)


def write_bundle(d, inner_ns):
    cfg = json.loads(OCI_TEMPLATE.read_text())
    cfg["root"]["path"] = str(OCI_ROOTFS)
    cfg["process"]["env"] += [f"SBX_HOST={OCI_HOST}", "SBX_SLOT=0"]
    for ns in cfg["linux"]["namespaces"]:
        if ns["type"] == "network":
            ns["path"] = f"/run/netns/{inner_ns}"
    cfg["linux"]["resources"]["memory"]["limit"] = MEM_MIB << 20
    cfg["linux"].pop("cgroupsPath", None)
    bundle = d / "bundle"
    bundle.mkdir(parents=True, exist_ok=True)
    (bundle / "config.json").write_text(json.dumps(cfg))
    return bundle


def in_netns(ns, fn, *args):
    def target():
        os.setns(ns_fd(ns), os.CLONE_NEWNET)
        fn(*args)
    t = threading.Thread(target=target)
    t.start()
    return t


def make_snapshot(backend):
    out = WORK / backend
    shutil.rmtree(out, ignore_errors=True)
    out.mkdir(parents=True)
    c = Clone(backend, 99, out)
    deadline = time.monotonic() + 30
    try:
        if backend == "firecracker":
            cfg = c.dir / "fc.json"
            cfg.write_text(json.dumps({
                "boot-source": {"kernel_image_path": str(KERNEL), "boot_args": "pci=off " + VM_ARGS},
                "drives": [{"drive_id": "rootfs", "path_on_host": str(ROOTFS), "is_root_device": True,
                            "is_read_only": True}],
                "network-interfaces": [{"iface_id": "eth0", "host_dev_name": "sbxtap0", "guest_mac": MAC}],
                "machine-config": {"vcpu_count": 2, "mem_size_mib": MEM_MIB}, "entropy": {}}))
            sock = str(c.dir / "api.sock")
            in_netns(c.ns, c.spawn, ["firecracker", "--id", "par-snap", "--api-sock", sock,
                                     "--config-file", str(cfg)]).join()
        elif backend == "cloud-hypervisor":
            sock = str(c.dir / "api.sock")
            in_netns(c.ns, c.spawn, ["cloud-hypervisor", "--api-socket", f"path={sock}",
                                     "--kernel", str(KERNEL), "--cmdline", VM_ARGS,
                                     "--disk", f"path={ROOTFS},readonly=on", "--net", f"tap=sbxtap0,mac={MAC}",
                                     "--cpus", "boot=2", "--memory", f"size={MEM_MIB}M", "--rng", "src=/dev/urandom",
                                     "--serial", "off", "--console", "off"]).join()
        else:
            p = c.spawn([*RUNSC, "run", "--detach", "--bundle", str(c.bundle), c.cid])
            p.wait(timeout=30)
        ip = OCI_GUEST if backend == "runsc" else VM_GUEST
        in_netns(c.ns, exec_ok, ip, deadline).join()
        time.sleep(2)
        if backend == "firecracker":
            api(sock, "PATCH", "/vm", {"state": "Paused"})
            api(sock, "PUT", "/snapshot/create", {"snapshot_type": "Full", "snapshot_path": f"{out}/vmstate",
                                                  "mem_file_path": f"{out}/mem"})
        elif backend == "cloud-hypervisor":
            sh("ch-remote", "--api-socket", sock, "pause")
            sh("ch-remote", "--api-socket", sock, "snapshot", f"file://{out}")
        else:
            sh(*RUNSC, "checkpoint", "--image-path", str(out), "--compression=none",
               "--exclude-committed-zero-pages", c.cid)
    finally:
        c.stop()
    sh("sh", "-c", f"cat {out}/* > /dev/null")
    return out


def mem_available_kib():
    for line in open("/proc/meminfo"):
        if line.startswith("MemAvailable:"):
            return int(line.split()[1])


def pss_kib(pid):
    try:
        for line in open(f"/proc/{pid}/smaps_rollup"):
            if line.startswith("Pss:"):
                return int(line.split()[1])
    except OSError:
        return 0
    return 0


def round_once(backend, snap, k, rnd, out):
    clones = [Clone(backend, i, snap) for i in range(k)]
    results = [{} for _ in clones]
    poll_ns = [c.ns for c in clones]
    mem_before = mem_available_kib()
    barrier = threading.Barrier(k + 1)
    t0_box = []

    def worker(i):
        os.setns(ns_fd(poll_ns[i]), os.CLONE_NEWNET)
        barrier.wait()
        clones[i].run(t0_box[0], results[i])

    threads = [threading.Thread(target=worker, args=(i,)) for i in range(k)]
    for t in threads:
        t.start()
    time.sleep(0.2)
    t0_box.append(time.perf_counter_ns())
    barrier.wait()
    for t in threads:
        t.join()
    # Cloud Hypervisor's ondemand mode keeps prefaulting in the background after the guest answers.
    time.sleep(5)
    mem_drop = (mem_before - mem_available_kib()) / 1024
    pss = sum(pss_kib(pid) for c in clones if c.backend != "runsc" for pid in c.pids()) / 1024
    for c in clones:
        c.stop()
    times = sorted(r["t_exec_ms"] for r in results if r.get("ok"))
    rec = {"backend": backend, "k": k, "round": rnd, "t_exec_ms": times,
           "failures": [r.get("error") for r in results if not r.get("ok")],
           "p50": statistics.median(times) if times else None, "max": times[-1] if times else None,
           "mem_available_drop_mib": round(mem_drop, 1), "vmm_pss_mib_total": round(pss, 1),
           "mem_mib": MEM_MIB}
    with open(out, "a") as f:
        f.write(json.dumps(rec) + "\n")
    print(f"{backend:17s} k={k:2d} r={rnd} p50={rec['p50']} max={rec['max']} fail={len(rec['failures'])} "
          f"memdrop={rec['mem_available_drop_mib']} MiB pss={rec['vmm_pss_mib_total']} MiB", flush=True)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--k", default="1,5,10")
    ap.add_argument("--rounds", type=int, default=3)
    ap.add_argument("--backends", default="firecracker,cloud-hypervisor,runsc")
    ap.add_argument("--out", required=True)
    a = ap.parse_args()
    Path("/run/sbx/par").mkdir(parents=True, exist_ok=True)
    snaps = {}
    for b in a.backends.split(","):
        snaps[b] = make_snapshot(b)
        rng_clone_test(b, snaps[b], a.out)
    for k in [int(x) for x in a.k.split(",")]:
        for rnd in range(a.rounds):
            for b, snap in snaps.items():
                round_once(b, snap, k, rnd, a.out)
                time.sleep(1)


if __name__ == "__main__":
    main()
