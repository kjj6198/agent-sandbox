"""cold, restore, parallel and memory scenarios."""
import shutil
import statistics
import threading
import time
import traceback
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

from .backends import BACKENDS, SLOTS, Backend, Prep, Sandbox
from .probe import (ReadyListener, agent_ok, first_exec, mem_available_mib, ms, now_ns, pss_rss_kib,
                    steal_ticks, ticks_to_ms, wait_pids_gone)
from .records import ParallelRecord, SnapshotRecord, SpawnRecord, append

SNAPSHOTS = Path("/opt/sbx/snapshots/harness")
TIMEOUT_S = 30
PSS_DELAY_S = 2.0


def short_error(e: BaseException, console: str = "") -> str:
    msg = f"{type(e).__name__}: {e}"
    if console:
        msg += f"\nconsole tail:\n{console[-1500:]}"
    return msg


class Launch:
    """One sandbox launch on a shared T0; fills the timing fields of a SpawnRecord."""

    def __init__(self, backend: Backend, prep: Prep, listener: ReadyListener | None, rec: SpawnRecord):
        self.b, self.prep, self.listener, self.rec = backend, prep, listener, rec
        self.sb: Sandbox | None = None
        self.t_exec_ns: int | None = None

    def run(self, t0: int, wall0: int, restore: bool = False) -> None:
        rec, deadline = self.rec, t0 + TIMEOUT_S * 10**9
        try:
            self.sb = self.b.restore(self.prep) if restore else self.b.start(self.prep)
            rec.t_spawned_ms = ms(now_ns(), t0)
            self.sb.t0_ns, self.sb.wall0_ns = t0, wall0
            if restore:
                self.t_exec_ns = first_exec(self.sb.guest_ip, deadline)
            else:
                t_ready, rec.guest_uptime_s = self.listener.wait(self.prep.slot, t0, deadline)
                rec.t_ready_ms = ms(t_ready, t0)
                self.t_exec_ns = first_exec(self.sb.guest_ip, deadline)
            rec.t_exec_ms = ms(self.t_exec_ns, t0)
            rec.ok = True
        except Exception as e:
            rec.error = short_error(e, self.b.console(self.sb) if self.sb else "")

    def finish(self, t0: int, restore: bool) -> None:
        """Post-exec measurements outside the timed path: init marker, uptime after restore."""
        if not self.sb:
            return
        if not restore:
            self.rec.t_init_ms = ms(self.b.init_ns(self.sb), t0)
        elif self.rec.ok:
            try:
                self.rec.guest_uptime_s = float(agent_ok(self.sb.guest_ip, "cut -d' ' -f1 /proc/uptime").split()[0])
            except Exception as e:
                self.rec.error = short_error(e)

    def measure_memory(self) -> None:
        if self.sb and self.rec.ok:
            self.rec.pss_kib, self.rec.rss_kib = pss_rss_kib(self.b.pids(self.sb))

    def teardown(self) -> None:
        pids = self.b.pids(self.sb) if self.sb else set()
        t = now_ns()
        try:
            if self.sb:
                self.b.stop(self.sb)
                wait_pids_gone(pids)
            else:
                self.b.cleanup(self.prep.slot)
            self.rec.t_teardown_ms = ms(now_ns(), t)
        except Exception as e:
            self.rec.error = (self.rec.error or "") + f"\nteardown: {short_error(e)}"


def spawn_once(backend: Backend, prep: Prep, listener, rec: SpawnRecord, restore: bool = False,
               pss_delay_s: float = PSS_DELAY_S, keep: bool = False) -> Launch:
    launch = Launch(backend, prep, listener, rec)
    s0 = steal_ticks()
    t0, wall0 = now_ns(), time.time_ns()
    launch.run(t0, wall0, restore)
    launch.finish(t0, restore)
    if launch.t_exec_ns and pss_delay_s:
        time.sleep(max(0.0, (launch.t_exec_ns + pss_delay_s * 1e9 - now_ns()) / 1e9))
        launch.measure_memory()
    rec.steal_ms = ticks_to_ms(steal_ticks() - s0)
    if not keep:
        launch.teardown()
    return launch


def boot(backend_id: str, slot: int, listener, mem_mib: int = 1024, **prep_kw) -> tuple[Backend, Sandbox]:
    """Untimed boot for long-lived sandboxes; raises if the sandbox does not come up."""
    b = BACKENDS[backend_id]
    rec = SpawnRecord(backend_id, "cold", None, trial=-1, concurrency=1, slot=slot)
    launch = spawn_once(b, b.prepare(slot, mem_mib, **prep_kw), listener, rec, pss_delay_s=0, keep=True)
    if not rec.ok:
        launch.teardown()
        raise RuntimeError(f"{backend_id} boot failed: {rec.error}")
    return b, launch.sb


def cold(args, out: Path, listener: ReadyListener) -> None:
    for trial in range(-args.warmup, args.trials):
        for bid in args.backends:
            b, slot = BACKENDS[bid], SLOTS[bid]
            rec = SpawnRecord(bid, "cold", None, trial=trial, concurrency=1, slot=slot, warmup=trial < 0)
            try:
                prep = b.prepare(slot)
            except Exception as e:
                rec.error = f"prepare: {short_error(e)}"
                append(out, rec)
                continue
            spawn_once(b, prep, listener, rec)
            append(out, rec)
            print(f"cold {bid} trial {trial}: exec={rec.t_exec_ms} ok={rec.ok} {rec.error or ''}"[:300], flush=True)
            time.sleep(args.gap)


RESTORE_TARGETS = [("firecracker", "file"), ("cloud-hypervisor", "copy"), ("cloud-hypervisor", "ondemand"),
                   ("runsc", "plain"), ("runsc", "background")]


def snapshot_dir(bid: str, variant: str, with_browser: bool | str) -> Path:
    # Cloud Hypervisor's two restore modes read the same snapshot.
    snap_variant = "default" if bid == "cloud-hypervisor" else variant
    tag = with_browser if isinstance(with_browser, str) else ("browser" if with_browser else "")
    return SNAPSHOTS / f"{bid}-{snap_variant}{'-' + tag if tag else ''}"


def snapshot_tag(prepare_guest) -> bool | str:
    return getattr(prepare_guest, "snapshot_tag", "browser") if prepare_guest else False


def make_snapshot(bid: str, variant: str, listener, out: Path, prepare_guest=None, idle_s: float = 3.0) -> Path:
    """Boots a fresh sandbox, optionally prepares it (e.g. starts a browser), idles, snapshots, stops it."""
    with_browser = prepare_guest is not None
    d = snapshot_dir(bid, variant, snapshot_tag(prepare_guest))
    rec = SnapshotRecord(bid, variant, with_browser)
    b = sb = None
    try:
        b, sb = boot(bid, SLOTS[bid], listener, **({"api": True} if bid == "firecracker" else {}))
        if prepare_guest:
            prepare_guest(b, sb)
        time.sleep(idle_s)
        info = b.snapshot(sb, d, variant)
        rec.t_create_ms, rec.mem_file_bytes, rec.state_bytes = info["t_create_ms"], info["mem_file_bytes"], info["state_bytes"]
        rec.files = {"sizes": info["files"], "t_pause_ms": info["t_pause_ms"], "dir": str(d)}
        rec.ok = True
    except Exception as e:
        rec.error = short_error(e, b.console(sb) if b and sb else "")
        traceback.print_exc()
        shutil.rmtree(d, ignore_errors=True)
    finally:
        if b and sb:
            b.stop(sb)
    append(out, rec)
    print(f"snapshot {bid}/{variant} browser={with_browser}: {rec.t_create_ms} ms, mem={rec.mem_file_bytes} ok={rec.ok}", flush=True)
    return d if rec.ok else None


def ensure_snapshots(targets, listener, out: Path, prepare_guest=None) -> dict:
    """Returns {(backend, variant): dir} for the snapshots that were created successfully."""
    dirs, made = {}, {}
    for bid, variant in targets:
        key = snapshot_dir(bid, variant, snapshot_tag(prepare_guest))
        if key not in made:
            made[key] = make_snapshot(bid, variant, listener, out, prepare_guest)
        if made[key]:
            dirs[(bid, variant)] = made[key]
    return dirs


def warm(dirs) -> None:
    from .backends import warm_page_cache
    for d in set(dirs.values()):
        if d.exists():
            warm_page_cache(d)


def restore(args, out: Path, listener: ReadyListener) -> None:
    targets = [t for t in RESTORE_TARGETS if t[0] in args.backends]
    dirs = ensure_snapshots(targets, listener, out)
    warm(dirs)
    for trial in range(-args.warmup, args.trials):
        for bid, variant in dirs:
            b, slot = BACKENDS[bid], SLOTS[bid]
            rec = SpawnRecord(bid, "restore", variant, trial=trial, concurrency=1, slot=slot, warmup=trial < 0)
            try:
                prep = b.prepare_restore(slot, dirs[(bid, variant)], variant)
            except Exception as e:
                rec.error = f"prepare: {short_error(e)}"
                append(out, rec)
                continue
            spawn_once(b, prep, listener, rec, restore=True)
            append(out, rec)
            print(f"restore {bid}/{variant} trial {trial}: exec={rec.t_exec_ms} ok={rec.ok} {rec.error or ''}"[:300], flush=True)
            time.sleep(args.gap)


def percentile(values: list[float], p: float) -> float | None:
    if not values:
        return None
    if len(values) == 1:
        return values[0]
    return statistics.quantiles(values, n=100, method="inclusive")[int(p) - 1]


def launch_group(bid: str, k: int, listener, mem_mib: int) -> tuple[list[Launch], int, float]:
    """Prepares k slots, then launches all of them from one T0 on k threads. Returns launches, t0, steal."""
    b = BACKENDS[bid]
    launches = []
    for slot in range(k):
        rec = SpawnRecord(bid, "cold", None, trial=0, concurrency=k, slot=slot)
        launches.append(Launch(b, b.prepare(slot, mem_mib), listener, rec))
    go = threading.Event()
    t0_box = []
    s0 = steal_ticks()

    def worker(launch: Launch):
        go.wait()
        launch.run(t0_box[0], t0_box[1])

    with ThreadPoolExecutor(max_workers=k) as pool:
        futures = [pool.submit(worker, l) for l in launches]
        time.sleep(0.2)
        t0_box += [now_ns(), time.time_ns()]
        go.set()
        for f in futures:
            f.result()
    for l in launches:
        l.finish(t0_box[0], False)
    return launches, t0_box[0], ticks_to_ms(steal_ticks() - s0)


def teardown_all(launches: list[Launch]) -> None:
    with ThreadPoolExecutor(max_workers=8) as pool:
        list(pool.map(lambda l: l.teardown(), launches))


def parallel(args, out: Path, listener: ReadyListener) -> None:
    ks = args.k or [1, 5, 10, 20]
    for k in ks:
        mem_mib = args.mem_mib or 1024
        for rnd in range(args.rounds):
            for bid in args.backends:
                before = mem_available_mib()
                launches, t0, steal = launch_group(bid, k, listener, mem_mib)
                time.sleep(PSS_DELAY_S)
                for l in launches:
                    l.measure_memory()
                after = mem_available_mib()
                execs = sorted(l.rec.t_exec_ms for l in launches if l.rec.ok)
                rec = ParallelRecord(bid, "cold", k, rnd, mem_mib, t_exec_ms=[l.rec.t_exec_ms for l in launches],
                                     p50=percentile(execs, 50), p95=percentile(execs, 95),
                                     max=execs[-1] if execs else None,
                                     failures=sum(not l.rec.ok for l in launches),
                                     errors=[l.rec.error for l in launches if l.rec.error],
                                     mem_available_delta_mib=round(before - after, 1),
                                     pss_kib_total=sum(l.rec.pss_kib or 0 for l in launches), steal_ms=steal)
                teardown_all(launches)
                for l in launches:
                    append(out.with_name(out.stem + "-spawns.jsonl"), l.rec)
                append(out, rec)
                print(f"parallel {bid} k={k} round {rnd}: p50={rec.p50} p95={rec.p95} max={rec.max} "
                      f"fail={rec.failures} dMemAvail={rec.mem_available_delta_mib} MiB", flush=True)
                time.sleep(max(args.gap, 3))


def memory(args, out: Path, listener: ReadyListener) -> None:
    """MemAvailable delta and total PSS for 10 idle sandboxes per backend, launched one by one."""
    k = 10
    for bid in args.backends:
        b = BACKENDS[bid]
        time.sleep(5)
        before = mem_available_mib()
        launches = []
        s0 = steal_ticks()
        for slot in range(k):
            rec = SpawnRecord(bid, "cold", None, trial=0, concurrency=1, slot=slot)
            launches.append(spawn_once(b, b.prepare(slot), listener, rec, pss_delay_s=0, keep=True))
        time.sleep(10)
        for l in launches:
            l.measure_memory()
        after = mem_available_mib()
        execs = sorted(l.rec.t_exec_ms for l in launches if l.rec.ok)
        rec = ParallelRecord(bid, "idle-sequential", k, 0, 1024, t_exec_ms=[l.rec.t_exec_ms for l in launches],
                             p50=percentile(execs, 50), p95=percentile(execs, 95), max=execs[-1] if execs else None,
                             failures=sum(not l.rec.ok for l in launches),
                             errors=[l.rec.error for l in launches if l.rec.error],
                             mem_available_delta_mib=round(before - after, 1),
                             pss_kib_total=sum(l.rec.pss_kib or 0 for l in launches),
                             steal_ms=ticks_to_ms(steal_ticks() - s0))
        teardown_all(launches)
        for l in launches:
            append(out.with_name(out.stem + "-spawns.jsonl"), l.rec)
        append(out, rec)
        print(f"memory {bid}: dMemAvail={rec.mem_available_delta_mib} MiB pss_total={rec.pss_kib_total} KiB fail={rec.failures}", flush=True)
