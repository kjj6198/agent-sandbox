# Harness spec (methodology, fixed before measuring)

## Backends
| id | what is timed |
|---|---|
| firecracker | firecracker process exec (no jailer), `--config-file --no-api` cold boot; API `PUT /snapshot/load` for restore |
| cloud-hypervisor | cloud-hypervisor process exec, direct kernel boot; `--restore` (+ resume) for restore, both default and `ondemand` memory restore modes |
| runsc | raw `runsc run --detach` of an OCI bundle in a pre-created netns (systrap, netstack); `runsc restore` for restore |
| runc | raw runc of the same bundle in the same kind of netns (no-isolation baseline) |
| docker-runsc / docker-runc | `docker run -d` of the same image (what most people would do first) |
| docker-runsc-kvm | gVisor KVM platform, cold spawn only |

Pool preparation is outside the timed window: tap devices, netns, config files, per-slot dirs, snapshot files on disk with warm page cache. Everything the runtime does to create the sandbox is inside it.

## Clock and events
All timestamps are `time.perf_counter_ns()` on the lab VM, relative to T0 taken immediately before the launch call.
- t_spawned: launcher call returned (process exec'd, API accepted, or `docker run -d` returned)
- t_init: `SBX_INIT` seen on the console/stdout (cold only)
- t_ready: `SBX_READY` hello received on :7001 (cold only)
- t_exec: first command round trip (`echo ok`) through the guest agent on :7000 completed. This is the headline "usable sandbox" number for both cold and restore. For restore, poll-connect to guest:7000 every 1 ms.
- t_teardown: stop call to all processes gone and resources released (measured separately, after the trial)

## Scenarios
| id | backends | N | notes |
|---|---|---|---|
| cold | all | 30 (+2 warm-up discarded) | round-robin across backends per trial to spread drift |
| restore | firecracker (File backend, `clock_realtime: true`), cloud-hypervisor (`memory_restore_mode` copy and ondemand, `resume=true`), runsc (plain, and `--background` from a `--compression=none --exclude-committed-zero-pages` checkpoint) | 30 (+2) | snapshot of an idle guest with the agent listening and no open harness connections; record snapshot create time and file sizes |
| browser-cold | firecracker, cloud-hypervisor, runsc, runc, docker-runsc | 10 | T0 spawn -> exec -> `sbx-browser` -> CDP /json/version (10 ms poll) -> Playwright connect_over_cdp -> goto local test page (load) -> screenshot -> goto https://example.com |
| browser-restore | firecracker, cloud-hypervisor, runsc | 10 | snapshot after Chromium is up and one warm-up navigation; T0 restore -> CDP -> connect -> local page -> screenshot -> example.com |
| parallel | firecracker, cloud-hypervisor, runsc, runc | k = 1, 5, 10, 20; 3 rounds each | common T0, per-sandbox t_exec; report p50, p95, max |
| memory | all | from cold and browser-cold trials | PSS of all sandbox processes from /proc/PID/smaps_rollup 2 s after t_exec, and with the browser idle on one page; plus MemAvailable delta for 10 idle sandboxes |
| workloads | firecracker, cloud-hypervisor, runsc, runc | 5 per workload (+1 warm-up) inside one long-lived sandbox per backend, interleaved | in a tmpfs /work so the result measures the isolation layer, not the disk |
| network | firecracker, cloud-hypervisor, runsc, runc | as noted | HTTP RTT to lab-side server x200 (p50), iperf3 5 s both directions, egress https://example.com x5, ingress to an in-sandbox http server |
| compat | firecracker, cloud-hypervisor, runsc, runc | 1 | pass/fail with output |

## Workloads (2 vCPU, 1 GiB in every backend)
- cpu: `sysbench cpu --threads=1 --time=10 run` and `--threads=2` (events/s)
- spawn: 2000 x `/bin/true` from bash (s)
- python: 50 x `python3 -c pass` (s)
- files: create, stat, and unlink 20k 1 KiB files from Python (s)
- untar: `tar -xf /opt/work/many-files.tar` into /work (s)
- node-require: `node -e "require('<untarred>/typescript')"` (s)
- git: status + `log -p -n 300` + `grep -c function` on a copy of the fixture repo (s)
- compile: `gcc -O2 -c sqlite3.c` (or the fixture C project) (s)
- fio-root: optional 4k randwrite on the root filesystem, reported with the caveat of where each backend's writable layer lives

## Compat checks
Chromium own sandbox on/off; `unshare -Ur true`; inotify via node `fs.watch`; ping; `mount -t tmpfs` as root; apt-get install of a small package; guest clock skew right after restore; /dev/urandom bytes from two restores of the same snapshot (identical or not); optional Docker-in-sandbox (`dockerd` + `docker run hello-world`).

## Output
JSONL per scenario in /opt/sbx/results/, one record per trial or check, with backend, mode, trial, concurrency, every t_* in ms, PSS/RSS KiB, ok, error, and L1 CPU steal-time delta for the trial. A summarize script produces p50/p95/min/max tables.
