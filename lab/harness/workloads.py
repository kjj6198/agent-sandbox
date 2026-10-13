"""workloads and network: one long-lived sandbox per backend, runs interleaved across backends."""
import json
import re
import statistics
import subprocess
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Callable

from .backends import SLOTS, Backend, Sandbox
from .probe import agent, agent_ok, now_ns, serve_www
from .records import WorkloadRecord, append
from .spawn import boot, short_error

ARTIFACTS = Path("/opt/sbx/results/artifacts")

FILES_PY = r"""
import os
d = "/work/files"
os.makedirs(d, exist_ok=True)
data = b"x" * 1024
for i in range(20000):
    with open(f"{d}/{i}", "wb") as f:
        f.write(data)
for i in range(20000):
    os.stat(f"{d}/{i}")
for i in range(20000):
    os.unlink(f"{d}/{i}")
"""

# typescript 7 (the Go port) no longer ships a JS compiler module, so load a set of heavy CommonJS packages instead.
NODE_REQUIRE_JS = r"""
const r = require("module").createRequire("/work/untar/");
for (const m of ["eslint", "@babel/core", "@babel/preset-env", "webpack/lib/webpack.js", "jest"]) r(m);
console.log(Object.keys(require.cache).length, "modules");
"""

UNTAR_READY = "test -d /work/untar/node_modules || (mkdir -p /work/untar && tar -xf /opt/work/many-files.tar -C /work/untar)"


@dataclass
class Workload:
    name: str
    cmd: str
    setup: str = "true"
    unit: str = "s"
    parse: Callable[[str], float] | None = None
    timeout: float = 300


def events_per_s(out: str) -> float:
    return float(re.search(r"events per second:\s*([\d.]+)", out).group(1))


def fio_iops(out: str) -> float:
    return json.loads(out[out.index("{"):])["jobs"][0]["write"]["iops"]


WORKLOADS = [
    Workload("cpu-1t", "sysbench cpu --threads=1 --time=10 run", unit="events/s", parse=events_per_s),
    Workload("cpu-2t", "sysbench cpu --threads=2 --time=10 run", unit="events/s", parse=events_per_s),
    Workload("spawn", "for i in $(seq 2000); do /bin/true; done"),
    Workload("python", "for i in $(seq 50); do python3 -c pass; done"),
    Workload("files", f"python3 - <<'EOF'\n{FILES_PY}\nEOF", setup="rm -rf /work/files"),
    Workload("untar", "tar -xf /opt/work/many-files.tar -C /work/untar",
             setup="rm -rf /work/untar && mkdir -p /work/untar"),
    Workload("node-require", f"node - <<'EOF'\n{NODE_REQUIRE_JS}\nEOF", setup=UNTAR_READY),
    Workload("git", "cd /work/redis && git status >/dev/null && git log -p -n 300 >/dev/null "
                    "&& git grep -c function >/dev/null",
             setup="rm -rf /work/redis && cp -a /opt/work/redis.git /work/redis "
                   "&& git config --global --add safe.directory '*'"),
    Workload("compile", "gcc -O2 -c /opt/work/sqlite/sqlite3.c -o /work/sqlite3.o", setup="rm -f /work/sqlite3.o"),
    Workload("fio-root", "fio --name=rw --filename=/root/fio.dat --size=256M --rw=randwrite --bs=4k "
                         "--ioengine=psync --runtime=5 --time_based --output-format=json",
             setup="rm -f /root/fio.dat", unit="IOPS", parse=fio_iops),
]


def boot_all(backend_ids, listener) -> dict[str, tuple[Backend, Sandbox]]:
    return {bid: boot(bid, SLOTS[bid], listener) for bid in backend_ids}


def stop_all(sandboxes) -> None:
    for b, sb in sandboxes.values():
        b.stop(sb)


def run_workload(b: Backend, sb: Sandbox, w: Workload, trial: int) -> WorkloadRecord:
    rec = WorkloadRecord(b.id, w.name, trial, warmup=trial == 0, unit=w.unit)
    try:
        agent_ok(sb.guest_ip, w.setup, timeout=300)
        t = now_ns()
        rc, out = agent(sb.guest_ip, w.cmd, timeout=w.timeout)
        rec.seconds = round((now_ns() - t) / 1e9, 4)
        rec.raw = out[-1500:]
        if rc != 0:
            raise RuntimeError(f"rc={rc}")
        rec.value = w.parse(out) if w.parse else rec.seconds
        rec.ok = True
    except Exception as e:
        rec.error = short_error(e)
    return rec


def workloads(args, out: Path, listener) -> None:
    selected = [w for w in WORKLOADS if not args.workloads or w.name in args.workloads]
    sandboxes = boot_all(args.backends, listener)
    try:
        for w in selected:
            for trial in range(args.trials + 1):
                for bid in args.backends:
                    rec = run_workload(*sandboxes[bid], w, trial)
                    append(out, rec)
                    print(f"workload {w.name} {bid} trial {trial}: {rec.seconds}s value={rec.value} ok={rec.ok} "
                          f"{rec.error or ''}"[:300], flush=True)
    finally:
        stop_all(sandboxes)


HTTP_RTT_PY = r"""
import sys, time, urllib.request, statistics
url = sys.argv[1]
ts = []
for _ in range(200):
    t = time.perf_counter()
    urllib.request.urlopen(url, timeout=5).read()
    ts.append((time.perf_counter() - t) * 1000)
ts.sort()
print(f"p50_ms={statistics.median(ts):.3f} p95_ms={ts[189]:.3f} min_ms={ts[0]:.3f} max_ms={ts[-1]:.3f}")
"""


def lab_http_rtt(url: str, n: int = 200) -> list[float]:
    import urllib.request
    ts = []
    for _ in range(n):
        t = time.perf_counter()
        urllib.request.urlopen(url, timeout=5).read()
        ts.append((time.perf_counter() - t) * 1000)
    return sorted(ts)


def net_test(b: Backend, sb: Sandbox, name: str, trial: int) -> WorkloadRecord:
    rec = WorkloadRecord(b.id, name, trial)
    host, guest = sb.host_ip, sb.guest_ip
    try:
        if name == "http-rtt-egress-lab":
            out = agent_ok(guest, f"python3 - http://{host}:8000/index.html <<'EOF'\n{HTTP_RTT_PY}\nEOF")
            rec.value, rec.unit, rec.raw = float(re.search(r"p50_ms=([\d.]+)", out).group(1)), "ms p50", out.strip()
        elif name in ("iperf3-up", "iperf3-down"):
            flag = " -R" if name == "iperf3-down" else ""
            out = agent_ok(guest, f"iperf3 -c {host} -t 5 -J{flag}", timeout=60)
            data = json.loads(out[out.index("{"):])
            path = ARTIFACTS / f"{name}-{b.id}-{trial}.json"
            path.write_text(out)
            rec.value = round(data["end"]["sum_received"]["bits_per_second"] / 1e9, 3)
            rec.unit, rec.raw = "Gbit/s", str(path)
        elif name == "egress-https":
            out = agent_ok(guest, "for i in 1 2 3 4 5; do curl -sS -o /dev/null -w '%{time_namelookup} %{time_total}\\n' "
                                  "https://example.com; done", timeout=120)
            totals = [float(l.split()[1]) for l in out.strip().splitlines()]
            rec.value, rec.unit, rec.raw = round(statistics.median(totals) * 1000, 1), "ms median of 5", out.strip()
        elif name == "ingress-http":
            ts = lab_http_rtt(f"http://{guest}:8080/index.html")
            rec.value, rec.unit = round(statistics.median(ts), 3), "ms p50"
            rec.raw = f"p50_ms={rec.value} p95_ms={ts[189]:.3f} min_ms={ts[0]:.3f} max_ms={ts[-1]:.3f}"
        rec.ok = True
    except Exception as e:
        rec.error = short_error(e)
    return rec


NET_TESTS = ["http-rtt-egress-lab", "ingress-http", "iperf3-up", "iperf3-down", "egress-https"]


def network(args, out: Path, listener) -> None:
    www = serve_www()
    iperf = subprocess.Popen(["iperf3", "-s", "-p", "5201"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    sandboxes = boot_all(args.backends, listener)
    try:
        for b, sb in sandboxes.values():
            agent_ok(sb.guest_ip, "nohup python3 -m http.server 8080 --bind 0.0.0.0 -d /opt/work/www "
                                  ">/tmp/http.log 2>&1 &")
        time.sleep(1)
        for name in NET_TESTS:
            for trial in range(args.trials):
                for bid in args.backends:
                    rec = net_test(*sandboxes[bid], name, trial)
                    append(out, rec)
                    print(f"network {name} {bid} trial {trial}: {rec.value} {rec.unit} ok={rec.ok} {rec.error or ''}"[:300],
                          flush=True)
    finally:
        stop_all(sandboxes)
        iperf.kill()
        www.shutdown()
