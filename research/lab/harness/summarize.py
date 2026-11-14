"""Builds results/summary.md from the JSONL files. Warm-up records are excluded everywhere."""
import json
import statistics
from collections import defaultdict
from pathlib import Path

SHORT = {"firecracker": "FC", "cloud-hypervisor": "CH", "runsc": "runsc", "runc": "runc"}
BACKEND_ORDER = ["firecracker", "cloud-hypervisor", "runsc", "runc", "docker-runsc", "docker-runc", "docker-runsc-kvm"]


def load(path: Path) -> list[dict]:
    if not path.exists():
        return []
    return [r for r in map(json.loads, path.read_text().splitlines()) if not r.get("warmup")]


def pct(values: list[float], p: int) -> float | None:
    values = sorted(v for v in values if v is not None)
    if not values:
        return None
    if len(values) == 1:
        return values[0]
    return statistics.quantiles(values, n=100, method="inclusive")[p - 1]


def fmt(v, digits: int = 1) -> str:
    return "" if v is None else f"{v:.{digits}f}"


def table(header: list[str], rows: list[list]) -> str:
    lines = ["| " + " | ".join(header) + " |", "|" + "---|" * len(header)]
    lines += ["| " + " | ".join(str(c) for c in row) + " |" for row in rows]
    return "\n".join(lines)


def order(key: tuple) -> tuple:
    b = key[0]
    return (BACKEND_ORDER.index(b) if b in BACKEND_ORDER else 99, *[str(k) for k in key[1:]])


def stats_cells(values: list[float], digits: int = 1) -> list[str]:
    return [fmt(pct(values, 50), digits), fmt(pct(values, 95), digits),
            fmt(min(values) if values else None, digits), fmt(max(values) if values else None, digits)]


def spawn_section(records: list[dict], title: str, key_fn) -> str:
    groups = defaultdict(list)
    for r in records:
        if r["type"] == "SpawnRecord":
            groups[key_fn(r)].append(r)
    rows = []
    for key in sorted(groups, key=order):
        rs = groups[key]
        ok = [r for r in rs if r["ok"]]
        col = lambda f: [r[f] for r in ok if r[f] is not None]
        pss = col("pss_kib")
        rows.append([" / ".join(k for k in key if k), f"{len(ok)}/{len(rs)}",
                     fmt(pct(col("t_spawned_ms"), 50)), fmt(pct(col("t_init_ms"), 50)), fmt(pct(col("t_ready_ms"), 50)),
                     *stats_cells(col("t_exec_ms")), fmt(pct(col("t_teardown_ms"), 50)),
                     fmt(pct(pss, 50) / 1024 if pss else None), fmt(sum(r["steal_ms"] or 0 for r in rs), 0)])
    header = ["backend", "ok", "spawned p50", "init p50", "ready p50", "exec p50", "exec p95", "exec min", "exec max",
              "teardown p50", "PSS MiB p50", "steal ms total"]
    return f"## {title}\n\nAll times in ms from T0.\n\n" + table(header, rows)


def snapshot_section(records: list[dict], title: str) -> str:
    rows = [[r["backend"], r["variant"], r["with_browser"], fmt(r["t_create_ms"]),
             fmt((r["mem_file_bytes"] or 0) / 2**20), fmt((r["state_bytes"] or 0) / 2**10),
             r["ok"], (r["error"] or "").splitlines()[0][:80] if r["error"] else ""]
            for r in records if r["type"] == "SnapshotRecord"]
    header = ["backend", "variant", "browser", "create ms", "memory file MiB", "state KiB", "ok", "error"]
    return f"## {title}\n\n" + table(header, rows)


BROWSER_FIELDS = ["t_exec_ms", "t_cdp_ms", "t_connected_ms", "t_local_page_ms", "t_screenshot_ms", "t_public_page_ms"]


def browser_section(records: list[dict], title: str) -> str:
    groups = defaultdict(list)
    for r in records:
        if r["type"] == "BrowserRecord":
            groups[(r["backend"], r.get("restore_variant") or "")].append(r)
    rows = []
    for key in sorted(groups, key=order):
        rs = groups[key]
        ok = [r for r in rs if r["ok"]]
        cells = []
        for f in BROWSER_FIELDS:
            vals = [r[f] for r in ok if r[f] is not None]
            cells.append(f"{fmt(pct(vals, 50), 0)} / {fmt(pct(vals, 95), 0)}" if vals else "")
        pss = [r["pss_kib_browser_idle"] for r in ok if r["pss_kib_browser_idle"]]
        rows.append([" / ".join(k for k in key if k), f"{len(ok)}/{len(rs)}", *cells,
                     fmt(pct(pss, 50) / 1024 if pss else None, 0), rs[0]["chrome_sandbox"]])
    header = ["backend", "ok", "exec", "CDP up", "connected", "local page", "screenshot", "example.com",
              "idle PSS MiB", "chrome sandbox"]
    return f"## {title}\n\nEach cell is p50 / p95 in ms from T0.\n\n" + table(header, rows)


def parallel_section(records: list[dict], title: str) -> str:
    groups = defaultdict(list)
    for r in records:
        if r["type"] == "ParallelRecord":
            groups[(r["backend"], r["mode"], r["k"])].append(r)
    rows = []
    for key in sorted(groups, key=lambda k: (k[2], order(k))):
        rs = groups[key]
        execs = [v for r in rs for v in r["t_exec_ms"] if v is not None]
        mem = [r["mem_available_delta_mib"] for r in rs]
        pss = [r["pss_kib_total"] / 1024 for r in rs if r["pss_kib_total"]]
        rows.append([key[0], key[2], len(rs), rs[0]["mem_mib"], *stats_cells(execs),
                     sum(r["failures"] for r in rs), fmt(statistics.mean(mem)), fmt(statistics.mean(pss) if pss else None)])
    header = ["backend", "k", "rounds", "guest MiB", "exec p50", "exec p95", "exec min", "exec max", "failures",
              "MemAvailable drop MiB (mean)", "PSS total MiB (mean)"]
    return f"## {title}\n\nPooled per-sandbox t_exec across rounds, ms from the common T0.\n\n" + table(header, rows)


def memory_section(records: list[dict]) -> str:
    rows = []
    for r in sorted((r for r in records if r["type"] == "ParallelRecord"), key=lambda r: order((r["backend"],))):
        rows.append([r["backend"], r["k"], r["failures"], fmt(r["mem_available_delta_mib"]),
                     fmt(r["mem_available_delta_mib"] / r["k"]), fmt((r["pss_kib_total"] or 0) / 1024),
                     fmt((r["pss_kib_total"] or 0) / 1024 / r["k"])])
    header = ["backend", "sandboxes", "failures", "MemAvailable drop MiB", "per sandbox", "PSS total MiB", "PSS per sandbox"]
    return "## memory (10 idle sandboxes, 10 s after the last one answered)\n\n" + table(header, rows)


def workload_section(records: list[dict], title: str, lower_is_better_units=("s", "ms p50", "ms median of 5")) -> str:
    groups = defaultdict(list)
    names = []
    for r in records:
        if r["type"] == "WorkloadRecord":
            groups[(r["workload"], r["backend"])].append(r)
            if r["workload"] not in names:
                names.append(r["workload"])
    backends = sorted({b for _, b in groups}, key=lambda b: order((b,)))
    rows = []
    for w in names:
        unit = next((r["unit"] for (wn, _), rs in groups.items() if wn == w for r in rs if r["unit"]), "")
        cells = []
        base = None
        for b in backends:
            vals = [r["value"] for r in groups.get((w, b), []) if r["ok"] and r["value"] is not None]
            n_fail = sum(not r["ok"] for r in groups.get((w, b), []))
            med = pct(vals, 50)
            if b == "runc":
                base = med
            spread = f" ({fmt(min(vals), 2)}-{fmt(max(vals), 2)})" if len(vals) > 1 else ""
            cells.append((b, med, f"{fmt(med, 3 if med is not None and med < 10 else 1)}{spread}"
                          + (f" [{n_fail} failed]" if n_fail else "")))
        rel = []
        for b, med, _ in cells:
            rel.append(f"{med / base:.2f}x" if base and med is not None else "")
        rows.append([w, unit, *[c for _, _, c in cells], " ".join(f"{SHORT.get(b, b)}={x}" for (b, _, _), x in zip(cells, rel) if b != "runc")])
    header = ["workload", "unit", *backends, "ratio to runc (median)"]
    return f"## {title}\n\nMedian (min-max) of measured runs.\n\n" + table(header, rows)


def compat_section(records: list[dict]) -> str:
    checks, backends, cells = [], [], {}
    for r in records:
        if r["type"] != "CompatRecord":
            continue
        if r["check"] not in checks:
            checks.append(r["check"])
        if r["backend"] not in backends:
            backends.append(r["backend"])
        cells[(r["check"], r["backend"])] = "PASS" if r["ok"] else "FAIL"
    rows = [[c, *[cells.get((c, b), "-") for b in backends]] for c in checks]
    return "## compat\n\nDetails and transcripts: compat.jsonl and artifacts/compat-*.log.\n\n" + table(["check", *backends], rows)


def summarize(results: Path) -> None:
    parts = ["# Sandbox benchmark summary\n\nGenerated by `python -m harness summarize` from results/*.jsonl. "
             "Lab: 8 vCPU / 7.8 GiB nested-KVM VM; guests 2 vCPU / 1 GiB unless noted."]
    cold = load(results / "cold.jsonl")
    if cold:
        parts.append(spawn_section(cold, "cold spawn", lambda r: (r["backend"],)))
    jailer = load(results / "cold-jailer.jsonl")
    if jailer:
        parts.append(spawn_section(jailer, "cold spawn, Firecracker via jailer (separate interleaved run)",
                                   lambda r: (r["backend"],)))
    restore = load(results / "restore.jsonl")
    if restore:
        parts.append(spawn_section(restore, "restore", lambda r: (r["backend"], r["restore_variant"])))
        parts.append(snapshot_section(restore, "snapshots (restore scenario)"))
    recheck = load(results / "restore-recheck.jsonl")
    if recheck:
        parts.append(spawn_section(recheck, "restore recheck (separate later run, 10 trials)",
                                   lambda r: (r["backend"], r["restore_variant"])))
    for name in ("browser-cold", "browser-restore"):
        recs = load(results / f"{name}.jsonl")
        if recs:
            parts.append(browser_section(recs, name))
    snaps = load(results / "browser-restore-snapshots.jsonl")
    if snaps:
        parts.append(snapshot_section(snaps, "snapshots (browser-restore scenario)"))
    par = load(results / "parallel.jsonl")
    if par:
        parts.append(parallel_section(par, "parallel cold spawn"))
    mem = load(results / "memory.jsonl")
    if mem:
        parts.append(memory_section(mem))
    for name in ("workloads", "network"):
        recs = load(results / f"{name}.jsonl")
        if recs:
            parts.append(workload_section(recs, name))
    comp = load(results / "compat.jsonl")
    if comp:
        parts.append(compat_section(comp))
    (results / "summary.md").write_text("\n\n".join(parts) + "\n")
    print((results / "summary.md").read_text())
