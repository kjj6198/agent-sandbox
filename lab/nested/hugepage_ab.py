#!/usr/bin/env python3
"""A/B of guest-memory backing on cold boot: Firecracker 4K vs hugetlbfs 2M, Cloud Hypervisor THP on vs off.

Needs `sysctl vm.nr_hugepages>=600` for the Firecracker 2M case. Runs inside the run.sh network namespace.
"""
import argparse
import asyncio
import json
import os

import nested_compare as nc


def fc_hugepage_config(lab, pages):
    path = lab.fc_config("noconsole")
    cfg = json.load(open(path))
    if pages:
        cfg["machine-config"]["huge_pages"] = pages
    out = os.path.join(lab.run, f"fc-hp-{pages or '4K'}.json")
    json.dump(cfg, open(out, "w"))
    return [lab.fc, "--id", "ab", "--no-api", "--config-file", out]


def ch_thp_argv(lab, api, thp):
    argv = lab.ch_argv("noconsole", api)
    i = argv.index("--memory")
    argv[i + 1] = f"size=1024M,thp={thp}"
    return argv


async def cold_argv(lab, argv):
    lab.drain_hello()
    t0 = nc.time.perf_counter_ns()
    proc = lab.spawn(argv)
    try:
        t_hello, line = await asyncio.wait_for(lab.hello.get(), 10)
        await nc.exec_once(poll=True)
        return {"t_ready_ms": (t_hello - t0) / 1e6, "t_exec_ms": nc.now_ms(t0), "hello": line}
    finally:
        nc.kill(proc)


async def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--level", required=True)
    ap.add_argument("--dir", required=True)
    ap.add_argument("--trials", type=int, default=10)
    ap.add_argument("--out", required=True)
    a = ap.parse_args()
    d = a.dir
    lab = nc.Lab(d, f"{d}/firecracker", f"{d}/cloud-hypervisor", f"{d}/ch-remote")
    server = await lab.serve_hello()
    api = os.path.join(lab.run, "ch-ab.sock")
    cases = [
        ("firecracker/4K", lambda: cold_argv(lab, fc_hugepage_config(lab, None))),
        ("firecracker/hugetlb-2M", lambda: cold_argv(lab, fc_hugepage_config(lab, "2M"))),
        ("cloud-hypervisor/thp-on", lambda: cold_argv(lab, ch_thp_argv(lab, api, "on"))),
        ("cloud-hypervisor/thp-off", lambda: cold_argv(lab, ch_thp_argv(lab, api, "off"))),
    ]
    rows = []
    for trial in range(-2, a.trials):
        for case, fn in cases:
            if os.path.exists(api):
                os.unlink(api)
            try:
                row = {"level": a.level, "case": case, "trial": trial, "ok": True, "error": None, **await fn()}
            except Exception as e:
                row = {"level": a.level, "case": case, "trial": trial, "ok": False, "error": f"{type(e).__name__}: {e}"}
            if trial >= 0:
                rows.append(row)
                with open(a.out, "a") as f:
                    f.write(json.dumps(row) + "\n")
            await asyncio.sleep(0.2)
    server.close()


if __name__ == "__main__":
    asyncio.run(main())
