"""Usage: python -m harness <scenario> [--backends a,b] [--trials N] [--warmup N] [--pilot]
Run as root from /opt/sbx: sudo /opt/sbx/venv/bin/python -m harness cold
"""
import argparse
import json
import os
import platform
import resource
import subprocess
import sys
import time
from pathlib import Path

from .backends import BACKENDS
from .probe import ReadyListener, serve_www, wait_quiet

RESULTS = Path("/opt/sbx/results")

ALL = [b for b in BACKENDS if b not in ("firecracker-jailer", "firecracker-hp")]
CORE = ["firecracker", "cloud-hypervisor", "runsc", "runc"]
SCENARIOS = {
    # name: (module, function, default backends, default trials, default warm-up)
    "cold": ("spawn", "cold", ALL, 30, 2),
    "restore": ("spawn", "restore", ["firecracker", "cloud-hypervisor", "runsc"], 30, 2),
    "browser-cold": ("browser", "browser_cold",
                     ["firecracker", "cloud-hypervisor", "runsc", "runc", "docker-runsc", "docker-runc"], 10, 1),
    "browser-restore": ("browser", "browser_restore", ["firecracker", "cloud-hypervisor", "runsc"], 10, 1),
    "parallel": ("spawn", "parallel", CORE, 3, 0),
    "memory": ("spawn", "memory", ALL, 1, 0),
    "workloads": ("workloads", "workloads", CORE, 5, 1),
    "network": ("workloads", "network", CORE, 3, 0),
    "compat": ("compat", "compat", CORE + ["docker-runsc", "docker-runc"], 1, 0),
}


def parse(argv):
    p = argparse.ArgumentParser(prog="harness")
    p.add_argument("scenario", choices=[*SCENARIOS, "summarize"])
    p.add_argument("--backends", type=lambda s: s.split(","))
    p.add_argument("--trials", type=int)
    p.add_argument("--warmup", type=int)
    p.add_argument("--rounds", type=int, default=3)
    p.add_argument("--k", type=lambda s: [int(x) for x in s.split(",")])
    p.add_argument("--mem-mib", type=int)
    p.add_argument("--workloads", type=lambda s: s.split(","))
    p.add_argument("--gap", type=float, default=0.5, help="seconds between trials, outside timing")
    p.add_argument("--pilot", action="store_true", help="2 trials, no warm-up, written to results/pilot/")
    p.add_argument("--tag", help="suffix for the output file, e.g. cold-<tag>.jsonl")
    p.add_argument("--max-load", type=float, default=1.0)
    return p.parse_args(argv)


def environment() -> dict:
    def sh(cmd):
        return subprocess.run(cmd, shell=True, capture_output=True, text=True).stdout.strip()
    return {"time": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "host": platform.node(),
            "kernel": platform.release(), "loadavg": os.getloadavg(),
            "clocksource": sh("cat /sys/devices/system/clocksource/clocksource0/current_clocksource"),
            "mem_available_kib": sh("grep MemAvailable /proc/meminfo"),
            "userns_sysctl": sh("sysctl -n kernel.apparmor_restrict_unprivileged_userns")}


def main(argv=None):
    resource.setrlimit(resource.RLIMIT_NOFILE, (65536, 65536))
    args = parse(argv if argv is not None else sys.argv[1:])
    if args.scenario == "summarize":
        from .summarize import summarize
        summarize(RESULTS)
        return
    module, func, backends, trials, warmup = SCENARIOS[args.scenario]
    args.backends = args.backends or backends
    args.trials = args.trials if args.trials is not None else (2 if args.pilot else trials)
    args.warmup = args.warmup if args.warmup is not None else (0 if args.pilot else warmup)
    if args.pilot:
        args.rounds = 1
    out_dir = RESULTS / "pilot" if args.pilot else RESULTS
    out = out_dir / f"{args.scenario}{'-' + args.tag if args.tag else ''}.jsonl"
    (RESULTS / "artifacts").mkdir(parents=True, exist_ok=True)
    out_dir.mkdir(parents=True, exist_ok=True)
    load = wait_quiet(args.max_load)
    env = {**environment(), "scenario": args.scenario, "args": vars(args), "load_at_start": load}
    with (out_dir / "runs.jsonl").open("a") as f:
        f.write(json.dumps(env) + "\n")
    print(json.dumps(env), flush=True)
    listener = ReadyListener()
    www = serve_www() if args.scenario.startswith("browser") else None
    fn = getattr(__import__(f"harness.{module}", fromlist=[func]), func)
    try:
        fn(args, out, listener)
    finally:
        if www:
            www.shutdown()


if __name__ == "__main__":
    main()
