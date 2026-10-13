"""Scores each candidate against the criteria fixed in criteria.md, using the rules written there.

Run: python3 score.py   (prints the table used in the report)
"""
WEIGHTS = {"cold": 20, "restore": 15, "browser": 15, "workloads": 15, "network": 5,
           "memory": 5, "compat": 10, "ops": 10, "isolation": 5}


def band(value, limits):
    """5 if value <= limits[0], 4 if <= limits[1], ... 1 otherwise."""
    for score, limit in zip((5, 4, 3, 2), limits):
        if value <= limit:
            return score
    return 1


def memory_band(idle_mib, parallel_ratio):
    passed = (idle_mib <= 50) + (parallel_ratio <= 2.0)
    return {2: 5, 1: 3, 0: 1}[passed]


def score(m):
    s = {
        "cold": band(m["cold_ms"], (150, 250, 500, 1000)),
        "restore": band(m["restore_ms"], (25, 50, 100, 500)),
        "browser": (band(m["browser_cold_ms"], (1000, 1500, 3000, 6000))
                    + band(m["browser_restore_ms"], (150, 300, 500, 1000))) / 2,
        "workloads": band(m["slowdown"], (1.10, 1.25, 1.50, 2.00)),
        "network": 5 if m["net_gate"] and m["gbps"] >= 1 else 1,
        "memory": memory_band(m["idle_mib"], m["parallel_ratio"]),
        "compat": m["compat_score"],
        "ops": m["ops_score"],
        "isolation": m["isolation_score"],
    }
    total = sum(s[k] * w for k, w in WEIGHTS.items()) / 100
    return s, total


# Best measured configuration per candidate. Sources (all under raw/):
#   cold: cold.jsonl, except Firecracker with 2 MiB huge pages from cold-s3.jsonl
#   restore: restore.jsonl; browser: browser-cold*.jsonl, browser-restore*.jsonl (reused tab where faster)
#   idle_mib: best of the cold path (memory.jsonl) and the restore path (nested/parallel-restore.jsonl, k=20)
#   parallel_ratio: best of cold (parallel.jsonl) and restore (nested/parallel-restore.jsonl) p95 at k=10 / single p95
CANDIDATES = {
    "gVisor": dict(cold_ms=62.7, restore_ms=73.7, browser_cold_ms=1852, browser_restore_ms=1052,
                   slowdown=2.09, net_gate=True, gbps=24.5, idle_mib=16.9, parallel_ratio=210.0 / 74.9,
                   compat_score=4, ops_score=5, isolation_score=4),
    "Firecracker": dict(cold_ms=160.0, restore_ms=29.4, browser_cold_ms=1258, browser_restore_ms=762,
                        slowdown=0.98, net_gate=True, gbps=37.6, idle_mib=8.8, parallel_ratio=885 / 268.5,
                        compat_score=4, ops_score=3, isolation_score=5),
    "Cloud Hypervisor": dict(cold_ms=174.2, restore_ms=44.7, browser_cold_ms=1105, browser_restore_ms=953,
                             slowdown=0.97, net_gate=True, gbps=35.1, idle_mib=91.2, parallel_ratio=693 / 183.4,
                             compat_score=5, ops_score=3, isolation_score=4),
}
DEFAULTS = {
    "Firecracker, defaults": {**CANDIDATES["Firecracker"], "cold_ms": 252.8, "browser_cold_ms": 1748,
                              "browser_restore_ms": 897},
}

if __name__ == "__main__":
    print(f"{'candidate':22s} " + " ".join(f"{k:>9s}" for k in WEIGHTS) + "  weighted")
    for name, m in {**CANDIDATES, **DEFAULTS}.items():
        s, total = score(m)
        print(f"{name:22s} " + " ".join(f"{s[k]:9.1f}" for k in WEIGHTS) + f"  {total:8.2f}")
