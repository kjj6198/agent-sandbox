import json, statistics as st, sys
rows = [json.loads(l) for f in sys.argv[1:] for l in open(f)]
groups = {}
for r in rows:
    if r.get("t_exec_ms") is not None and r["ok"]:
        groups.setdefault((r["case"], r["level"]), []).append(r["t_exec_ms"])
for r in rows:
    if not r["ok"]:
        print("FAIL", r["level"], r["case"], r["trial"], (r["error"] or "")[:150])
    elif "t_create_ms" in r:
        print("SNAP", r["level"], r["case"], f'{r["t_create_ms"]:.0f} ms', f'{r["bytes"]/2**20:.0f} MiB')
for (case, level), xs in sorted(groups.items()):
    xs.sort()
    p95 = xs[min(len(xs) - 1, round(0.95 * (len(xs) - 1)))]
    print(f"{level} {case:34s} n={len(xs):3d} p50={st.median(xs):7.1f} p95={p95:7.1f} min={xs[0]:7.1f} max={xs[-1]:7.1f}")
