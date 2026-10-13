#!/usr/bin/env python3
"""Cold boot and snapshot restore of Firecracker and Cloud Hypervisor, run the same way on
the bare-metal host (L0) and inside the lab VM (L1).

Run inside a network namespace that has tap `sbxtap0` at 172.16.0.1/24 (see run.sh):
    nested_compare.py --level L0 --dir /root/sbx-l0 --trials 20 --out l0.jsonl
"""
import argparse
import asyncio
import http.client
import json
import os
import shutil
import signal
import socket
import statistics
import subprocess
import time

HOST_IP, GUEST_IP, TAP = "172.16.0.1", "172.16.0.2", "sbxtap0"
MAC = "06:00:ac:10:00:02"
BASE_ARGS = (
    "reboot=k panic=1 quiet random.trust_cpu=on "
    f"ip={GUEST_IP}::{HOST_IP}:255.255.255.0::eth0:off "
    f"init=/sbin/sbx-init ro root=/dev/vda sbx.host={HOST_IP} sbx.slot=0"
)
# "console" is the configuration of the main harness run; "noconsole" matches Firecracker's
# own defaults (no serial console, no i8042 aux/mux probing) and its SPECIFICATION conditions.
CONSOLE_ARGS = {
    "console": "console=ttyS0 ",
    "noconsole": "8250.nr_uarts=0 i8042.noaux i8042.nomux i8042.dumbkbd ",
}


def now_ms(t0):
    return (time.perf_counter_ns() - t0) / 1e6


class UnixHTTP(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("localhost")
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.connect(self.path)


def fc_api(sock_path, method, url, body):
    conn = UnixHTTP(sock_path)
    conn.request(method, url, json.dumps(body), {"Content-Type": "application/json"})
    resp = conn.getresponse()
    data = resp.read()
    conn.close()
    if resp.status >= 300:
        raise RuntimeError(f"{method} {url}: {resp.status} {data!r}")


class Lab:
    def __init__(self, d, fc, ch, ch_remote):
        self.d, self.fc, self.ch, self.ch_remote = d, fc, ch, ch_remote
        self.kernel = os.path.join(d, "vmlinux")
        self.rootfs = os.path.join(d, "rootfs.ext4")
        self.run = os.path.join(d, "run")
        os.makedirs(self.run, exist_ok=True)
        self.hello = asyncio.Queue()

    async def serve_hello(self):
        async def on_conn(reader, writer):
            line = await reader.readline()
            await self.hello.put((time.perf_counter_ns(), line.decode().strip()))
            writer.close()

        return await asyncio.start_server(on_conn, HOST_IP, 7001, reuse_address=True)

    def drain_hello(self):
        while not self.hello.empty():
            self.hello.get_nowait()

    def fc_config(self, console):
        path = os.path.join(self.run, f"fc-{console}.json")
        with open(path, "w") as f:
            json.dump({
                "boot-source": {"kernel_image_path": self.kernel,
                                "boot_args": CONSOLE_ARGS[console] + "pci=off " + BASE_ARGS},
                "drives": [{"drive_id": "rootfs", "path_on_host": self.rootfs, "is_root_device": True, "is_read_only": True}],
                "network-interfaces": [{"iface_id": "eth0", "host_dev_name": TAP, "guest_mac": MAC}],
                "machine-config": {"vcpu_count": 2, "mem_size_mib": 1024},
                "entropy": {},
            }, f)
        return path

    def fc_argv(self, console, api_sock=None, boot=True):
        argv = [self.fc, "--id", "l0"]
        argv += ["--api-sock", api_sock] if api_sock else ["--no-api"]
        return argv + (["--config-file", self.fc_config(console)] if boot else [])

    def ch_argv(self, console, api_sock, restore=None):
        base = [self.ch, "--api-socket", f"path={api_sock}"]
        if restore:
            return base + ["--restore", restore]
        return base + [
            "--kernel", self.kernel, "--cmdline", CONSOLE_ARGS[console] + BASE_ARGS,
            "--disk", f"path={self.rootfs},readonly=on",
            "--net", f"tap={TAP},mac={MAC}",
            "--cpus", "boot=2", "--memory", "size=1024M",
            "--rng", "src=/dev/urandom", "--serial", "tty" if console == "console" else "off",
            "--console", "off",
        ]

    def spawn(self, argv):
        log = open(os.path.join(self.run, "console.log"), "wb")
        return subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT,
                                start_new_session=True)


def kill(proc):
    if proc.poll() is None:
        os.killpg(proc.pid, signal.SIGKILL)
    proc.wait()


async def exec_once(timeout_s=10.0, poll=False):
    deadline = time.monotonic() + timeout_s
    while True:
        try:
            reader, writer = await asyncio.wait_for(asyncio.open_connection(GUEST_IP, 7000), 0.05)
            break
        except (OSError, asyncio.TimeoutError):
            if not poll or time.monotonic() > deadline:
                raise
            await asyncio.sleep(0.001)
    writer.write(b"echo ok; echo __RC=$?__\n")
    await writer.drain()
    out = b""
    while b"__RC=" not in out:
        chunk = await asyncio.wait_for(reader.read(4096), max(0.1, deadline - time.monotonic()))
        if not chunk:
            raise RuntimeError(f"agent closed early: {out!r}")
        out += chunk
    writer.close()
    return out


async def wait_socket(path, t0, timeout_s=5.0):
    while not os.path.exists(path):
        if now_ms(t0) > timeout_s * 1000:
            raise TimeoutError(path)
        await asyncio.sleep(0.0005)


async def cold(lab, vmm, console):
    lab.drain_hello()
    api = os.path.join(lab.run, f"{vmm}.sock")
    if os.path.exists(api):
        os.unlink(api)
    argv = lab.fc_argv(console) if vmm == "firecracker" else lab.ch_argv(console, api)
    t0 = time.perf_counter_ns()
    proc = lab.spawn(argv)
    try:
        t_hello, line = await asyncio.wait_for(lab.hello.get(), 10)
        t_ready = (t_hello - t0) / 1e6
        # sbx-init sends its hello in parallel with starting the agent, so the agent port may lag by a moment.
        await exec_once(poll=True)
        return {"t_ready_ms": t_ready, "t_exec_ms": now_ms(t0), "hello": line}
    finally:
        kill(proc)


async def snapshot(lab, vmm, out):
    shutil.rmtree(out, ignore_errors=True)
    os.makedirs(out)
    lab.drain_hello()
    api = os.path.join(lab.run, f"{vmm}.sock")
    if os.path.exists(api):
        os.unlink(api)
    if vmm == "firecracker":
        proc = lab.spawn(lab.fc_argv("noconsole", api))
    else:
        proc = lab.spawn(lab.ch_argv("noconsole", api))
    try:
        await asyncio.wait_for(lab.hello.get(), 10)
        await exec_once()
        await asyncio.sleep(2)
        t0 = time.perf_counter_ns()
        if vmm == "firecracker":
            fc_api(api, "PATCH", "/vm", {"state": "Paused"})
            fc_api(api, "PUT", "/snapshot/create", {"snapshot_type": "Full",
                   "snapshot_path": f"{out}/vmstate", "mem_file_path": f"{out}/mem"})
        else:
            subprocess.run([lab.ch_remote, "--api-socket", api, "pause"], check=True)
            subprocess.run([lab.ch_remote, "--api-socket", api, "snapshot", f"file://{out}"], check=True)
        return {"t_create_ms": now_ms(t0),
                "bytes": sum(os.path.getsize(os.path.join(out, f)) for f in os.listdir(out))}
    finally:
        kill(proc)


async def restore(lab, vmm, snap, variant):
    api = os.path.join(lab.run, f"{vmm}.sock")
    if os.path.exists(api):
        os.unlink(api)
    t0 = time.perf_counter_ns()
    if vmm == "firecracker":
        proc = lab.spawn(lab.fc_argv("noconsole", api, boot=False))
        await wait_socket(api, t0)
        fc_api(api, "PUT", "/snapshot/load", {
            "snapshot_path": f"{snap}/vmstate",
            "mem_backend": {"backend_type": "File", "backend_path": f"{snap}/mem"},
            "network_overrides": [{"iface_id": "eth0", "host_dev_name": TAP}],
            "resume_vm": True,
        })
    else:
        opts = f"source_url=file://{snap},resume=true"
        if variant == "ondemand":
            opts += ",memory_restore_mode=ondemand"
        proc = lab.spawn(lab.ch_argv("noconsole", api, restore=opts))
    try:
        t_loaded = now_ms(t0)
        await exec_once(poll=True)
        return {"t_loaded_ms": t_loaded, "t_exec_ms": now_ms(t0)}
    finally:
        kill(proc)


def summarize(rows):
    groups = {}
    for r in rows:
        if r.get("ok") and "t_exec_ms" in r:
            groups.setdefault((r["level"], r["case"]), []).append(r["t_exec_ms"])
    for (level, case), xs in sorted(groups.items()):
        xs.sort()
        p95 = xs[min(len(xs) - 1, round(0.95 * (len(xs) - 1)))]
        print(f"{level} {case:28s} n={len(xs):3d} p50={statistics.median(xs):8.1f} "
              f"p95={p95:8.1f} min={xs[0]:8.1f} max={xs[-1]:8.1f} ms")


async def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--level", required=True)
    ap.add_argument("--dir", required=True)
    ap.add_argument("--fc", default="firecracker")
    ap.add_argument("--ch", default="cloud-hypervisor")
    ap.add_argument("--ch-remote", default="ch-remote")
    ap.add_argument("--trials", type=int, default=20)
    ap.add_argument("--warmup", type=int, default=2)
    ap.add_argument("--out", required=True)
    a = ap.parse_args()

    lab = Lab(a.dir, a.fc, a.ch, a.ch_remote)
    server = await lab.serve_hello()
    rows = []

    def record(case, trial, fn_result=None, error=None):
        row = {"level": a.level, "case": case, "trial": trial, "ok": error is None, "error": error}
        row.update(fn_result or {})
        rows.append(row)
        if trial >= 0:
            with open(a.out, "a") as f:
                f.write(json.dumps(row) + "\n")

    snaps = {}
    for vmm in ("firecracker", "cloud-hypervisor"):
        snaps[vmm] = os.path.join(a.dir, "snap", vmm)
        res = await snapshot(lab, vmm, snaps[vmm])
        record(f"{vmm}/snapshot-create", 0, res)

    cases = [
        ("firecracker/cold-console", lambda: cold(lab, "firecracker", "console")),
        ("cloud-hypervisor/cold-console", lambda: cold(lab, "cloud-hypervisor", "console")),
        ("firecracker/cold-noconsole", lambda: cold(lab, "firecracker", "noconsole")),
        ("cloud-hypervisor/cold-noconsole", lambda: cold(lab, "cloud-hypervisor", "noconsole")),
        ("firecracker/restore-file", lambda: restore(lab, "firecracker", snaps["firecracker"], "file")),
        ("cloud-hypervisor/restore-copy", lambda: restore(lab, "cloud-hypervisor", snaps["cloud-hypervisor"], "copy")),
        ("cloud-hypervisor/restore-ondemand", lambda: restore(lab, "cloud-hypervisor", snaps["cloud-hypervisor"], "ondemand")),
    ]
    for trial in range(-a.warmup, a.trials):
        for case, fn in cases:
            try:
                record(case, trial, await fn())
            except Exception as e:
                record(case, trial, error=f"{type(e).__name__}: {e}")
            await asyncio.sleep(0.2)
    server.close()
    summarize([r for r in rows if r["trial"] >= 0])


if __name__ == "__main__":
    asyncio.run(main())
