# /opt/sbx: sandbox benchmark lab

Bring-up for comparing Firecracker, Cloud Hypervisor, gVisor (runsc) and runc as AI-agent sandboxes.
Every backend runs the same image (`sbx-guest:latest`), the same `/sbin/sbx-init`, and the same agent contract.
Versions are in `VERSIONS`. Smoke transcripts and screenshots are in `smoke/`.

## Layout

| Path | What |
|---|---|
| `bin/` | launch/stop/snapshot scripts, `net-setup`, `net-teardown`, `build-rootfs`, `smoke` |
| `kernel/vmlinux` | guest kernel 6.18.56, shared by Firecracker and Cloud Hypervisor |
| `kernel/base-microvm-kernel-ci-x86_64-6.18.config` | Firecracker's config it started from (commit in `base-config.fc-commit`) |
| `kernel/sbx-changes.config` | the fragment merged on top; `changed-options.txt` is the resulting diffconfig |
| `guest/` | Dockerfile, `sbx-init`, `sbx-browser`, `resolv.conf`, `www/index.html` |
| `images/rootfs.ext4` | ext4 export of the image, no journal, attached read-only to every VM |
| `oci/rootfs`, `oci/config.json` | same export as a directory plus the OCI template for raw runsc/runc |
| `snapshots/` | snapshot/checkpoint output |
| `venv/` | host Python with Playwright (CDP client only, no host browser) |

All scripts re-exec themselves with `sudo`, so they can be called as `kalan`.

## One-time setup

```sh
/opt/sbx/bin/net-setup                         # taps, netns, NAT, docker network; idempotent
docker build -t sbx-guest:latest /opt/sbx/guest
/opt/sbx/bin/build-rootfs                      # -> oci/rootfs, oci/config.json, images/rootfs.ext4
```

Rebuild the kernel: `cp kernel/base-*.config src/linux-6.18.56/.config && scripts/kconfig/merge_config.sh -m .config /opt/sbx/kernel/sbx-changes.config && make olddefconfig && make -j8 vmlinux`.

## Slots and addresses

Slot N is 0..31. Each backend family has its own subnet, and the orchestrator listens on the host side of it.

| Backend | Guest IP | SBX_HOST (orchestrator) | Plumbing |
|---|---|---|---|
| Firecracker, Cloud Hypervisor | 172.16.N.2 | 172.16.N.1 | tap `sbxtapN`, MAC `06:00:ac:10:<N hex>:02` |
| raw runsc, raw runc | 172.18.N.2 | 172.18.N.1 | netns `sbxnsN`, veth `sbxvN` <-> `eth0` |
| Docker (all runtimes) | 172.30.N.2 | 172.30.0.1 | bridge network `sbxnet` 172.30.0.0/16 |

Two backends cannot share a slot at the same time within the same family (same tap or netns).

## Launch commands

```sh
bin/fc-start N [vcpus=2] [mem_mib=1024]        # --no-api --config-file (fastest cold boot)
bin/fc-start --api N [vcpus] [mem]             # also serves /run/sbx/fc-N/api.sock; needed for fc-snapshot
bin/ch-start N [vcpus=2] [mem_mib=1024]        # SBX_CH_VIRTIOFS=/dir adds virtio-fs tag "work"
bin/runsc-start N                              # SBX_RUNSC_PLATFORM=kvm for the KVM platform (default systrap)
bin/runc-start N
bin/docker-start N --runtime runc|runsc|runsc-kvm
bin/{fc,ch,runsc,runc,docker}-stop N
```

Snapshots (restore into the same slot; the guest keeps its IP):

```sh
bin/fc-snapshot N DIR          bin/fc-restore N DIR
bin/ch-snapshot N DIR          bin/ch-restore N DIR [copy|ondemand]
bin/runsc-checkpoint N DIR [plain|fast]   bin/runsc-restore N DIR [plain|fast]
```

`fc-snapshot` and `ch-snapshot` pause, snapshot, and resume, so the source keeps running. `runsc checkpoint` stops the sandbox.
Every start/restore script first runs the matching stop, and stop kills by exact command line, waits for `/proc/PID` to go away, and deletes `/run/sbx/<backend>-N`. Re-running converges, including after a `kill -9` of the VMM or sandbox (tested, see `smoke/extras.log`).

Console/stdout: `/run/sbx/<fc|ch|runsc|runc>-N/console.log`, or `docker logs sbx-docker-N`.

### What the scripts run

Firecracker (`fc-start` writes `/run/sbx/fc-N/config.json`):
```
firecracker --id sbx-fc-N --no-api --config-file config.json          # or --api-sock /run/sbx/fc-N/api.sock
boot_args: console=ttyS0 reboot=k panic=1 pci=off quiet random.trust_cpu=on
           ip=172.16.N.2::172.16.N.1:255.255.255.0::eth0:off init=/sbin/sbx-init ro root=/dev/vda
           sbx.host=172.16.N.1 sbx.slot=N
drive rootfs.ext4 is_read_only=true; net sbxtapN; vcpu_count, mem_size_mib
```

Cloud Hypervisor:
```
cloud-hypervisor --api-socket path=/run/sbx/ch-N/api.sock --kernel vmlinux --cmdline "<same, minus pci=off>"
  --disk path=rootfs.ext4,readonly=on --net tap=sbxtapN,mac=... --cpus boot=2 --memory size=1024M
  --rng src=/dev/urandom --serial tty --console off
```

raw runsc: `runsc --root /run/sbx/runsc-root --platform=systrap --network=sandbox --overlay2=root:memory run --detach --bundle /run/sbx/runsc-N/bundle sbx-runsc-N`

raw runc: tmpfs-backed overlayfs over `oci/rootfs` at `/run/sbx/runc-N/ovl/merged`, then `runc --root /run/sbx/runc-root run --detach --bundle /run/sbx/runc-N/bundle sbx-runc-N`

Docker: `docker run -d --name sbx-docker-N --runtime R --network sbxnet --ip 172.30.N.2 --cpus 2 --memory 1g --memory-swap 1g -v /opt/sbx/guest/resolv.conf:/etc/resolv.conf:ro -e SBX_HOST=172.30.0.1 -e SBX_SLOT=N sbx-guest:latest`

## Why the non-obvious flags are there

- `pci=off` (Firecracker only): Firecracker has no PCI bus unless started with `--enable-pci`; skipping the probe saves boot time. Cloud Hypervisor needs PCI, so its cmdline omits it. Otherwise the cmdlines are identical.
- `ro root=/dev/vda` + read-only drive: one ext4 file serves every VM. `sbx-init` puts a tmpfs-backed overlay on top and `pivot_root`s into it, so guest writes cost guest RAM and vanish on stop.
- `random.trust_cpu=on`: seeds the CRNG from RDRAND at boot, so nothing blocks on entropy early in boot.
- `ip=` kernel autoconfig: eth0 is up before init runs; no DHCP or `ip` calls on the boot path.
- `sbx.host=`/`sbx.slot=`: dotted params are not passed to init as env; `sbx-init` reads them from `/proc/cmdline`.
- Firecracker `--id sbx-fc-N` and CH `--api-socket path=/run/sbx/ch-N/...`: give each VMM a unique command line so stop scripts can find it with an anchored `pgrep -f '^...'`.
- runsc `--overlay2=root:memory`: rootfs writes stay in sandbox memory. The default `root:self` stores the upper layer as a file inside the rootfs directory, which is shared here.
- runsc `--network=sandbox`: gVisor netstack (the default, spelled out). It takes eth0's IP and routes from the netns.
- OCI template capabilities: Docker's default 14 capabilities, so raw runc/runsc match Docker. Raw runc/runsc have no seccomp profile (Docker applies its default one).
- OCI `linux.resources`: 2 CPUs (quota 200000/100000) and 1 GiB, matching the VMs and `docker --cpus 2 --memory 1g`.
- Docker `--memory-swap 1g`: no swap on top of the 1 GiB limit, like the VMs.

## Init and agent contract (`/sbin/sbx-init`)

1. VM only (detected by `/proc/self` missing): mount proc, sysfs, devtmpfs if not already mounted, tmpfs on `/overlay`, overlayfs `lower=/`, `pivot_root`, then devpts, `/dev/shm`, `/run`, `/tmp` (tmpfs), cgroup2, `lo` up, hostname `sbx-N`.
2. Prints `SBX_INIT <contents of /proc/uptime>` to stdout (serial console on VMs).
3. Starts `socat TCP-LISTEN:7000,reuseaddr,fork EXEC:/bin/bash,stderr` in the background.
4. Sends `SBX_READY <slot> <uptime seconds>\n` to `$SBX_HOST:7001` with socat (100 tries, 0.1 s apart, 1 s connect timeout).
5. `exec tini -s -- sleep infinity`: same PID, tini reaps every orphan, including the socat children.

Agent protocol on port 7000: each TCP connection gets a fresh non-interactive `bash` running as root, with stdout and stderr merged. Send `<cmd>\necho __RC=$?__\n` and read until `__RC=<n>__`. **Keep the socket open until the marker arrives**: `printf ... | socat - TCP:ip:7000` closes 0.5 s after stdin EOF and loses output from slower commands (use `socat -t 60` or a real client). `bin/smoke` contains a 10-line Python client (`agent()`).

Uptime caveat: runc and docker-runc report the host's uptime in `SBX_INIT`/`SBX_READY`, because runc has no time namespace. gVisor and the VMs report sandbox uptime.

## Browser (`/usr/local/bin/sbx-browser [--no-sandbox]`, or `SBX_CHROME_SANDBOX=0`)

Starts Chromium 155 as `agent` with a fresh `--user-data-dir` under /tmp, CDP on 127.0.0.1:9222, plus `socat` 0.0.0.0:9223 -> 127.0.0.1:9222. It prints `SBX_BROWSER ready sandbox=1|0` once `/json/version` answers.
Flags: `--headless=new --remote-debugging-port=9222 --user-data-dir=<fresh> --remote-allow-origins=* --disable-gpu --disable-dev-shm-usage --no-first-run --no-default-browser-check --disable-background-networking --disable-breakpad --mute-audio --hide-scrollbars --window-size=1280,800`. No GPU or Xvfb needed for headless.

- Chrome 136+ refuses remote debugging on the default profile dir, so a fresh `--user-data-dir` is mandatory. Stale `Singleton*` lock files are removed.
- New headless binds CDP to loopback, so the socat forwarder is what makes it reachable. `/json/version` builds `webSocketDebuggerUrl` from the request's Host header, so it comes back as `ws://<guest-ip>:9223/...` and `connect_over_cdp("http://<guest-ip>:9223")` works without rewriting.
- `--disable-dev-shm-usage`: Docker gives /dev/shm 64 MiB; this makes Chromium use /tmp on every backend.

Chromium's own sandbox (from `chrome://sandbox`, logged in each smoke log):

| Backend | Sandbox | Notes |
|---|---|---|
| Firecracker, Cloud Hypervisor | on | namespace + seccomp-BPF, "adequately sandboxed" |
| runsc raw, docker-runsc | on | gVisor implements unprivileged user namespaces |
| runc raw | **off** (`--no-sandbox`) | host has `kernel.apparmor_restrict_unprivileged_userns=1` (Ubuntu 24.04 default). With it set to 0, the sandbox works under raw runc (tested, then reverted). |
| docker-runc | **off** (`--no-sandbox`) | Docker's default seccomp profile blocks user-namespace creation, independent of the sysctl. Needs a custom seccomp profile to enable. |

The Debian package has no setuid `chrome-sandbox` fallback (`chromium-sandbox` is not installed).

## Smoke tests

`bin/smoke <backend>` (backends: firecracker, cloud-hypervisor, runsc, runc, docker-runsc, docker-runc, docker-runsc-kvm). It starts a listener on :7001, starts the sandbox, waits for SBX_READY, runs `uname -a; id; nproc; free -m` and the example.com curl through port 7000, starts `sbx-browser`, logs `chrome://sandbox`, and takes CDP screenshots of the test page and https://example.com. For firecracker, cloud-hypervisor, and runsc it then snapshots with the browser running, stops, restores, and takes another screenshot.

The test page is served **from the lab VM**: `bin/smoke` runs a Python HTTP server on the sandbox's host-side IP port 8000 (`http://172.16.N.1:8000/index.html` etc.) from `/opt/sbx/guest/www`, the same file baked into `/opt/work/www`.

`smoke/extras.log`: Firecracker `--no-api` boot, runsc fast vs plain checkpoint, CH `ondemand` vs `copy` restore, CH virtio-fs, raw runsc on the KVM platform, Docker inside a Firecracker guest, and the idempotency/crash test.

## Guest image

`debian:trixie-slim` plus the requested packages, `tini`, and user `agent` (uid 1000). Fixtures in `/opt/work` (read-only; copy before writing):

- `many-files.tar`: 23,405 files, 200 MB, a `node_modules` from `typescript react react-dom eslint webpack webpack-cli @babel/core @babel/preset-env jest @mui/material @emotion/react @emotion/styled rxjs lodash-es`.
- `sqlite/`: sqlite 3.54.0 amalgamation. `gcc -O2 -c sqlite3.c` takes 16.5 s single-threaded under runc on this host.
- `redis.git`: `git clone --depth 500` of redis/redis (507 commits, 34 MB).
- `www/index.html`: text, CSS gradient, inline SVG.

`/etc/resolv.conf` in the rootfs is `nameserver 1.1.1.1` + `options single-request-reopen` (see quirks).

## Guest kernel

Linux 6.18.56. Firecracker's guest configs now cover 5.10, 6.1, and 6.18 (6.12 is not among them), and 6.18 is a kernel.org LTS, so 6.18 was used instead of 6.12. Base: `microvm-kernel-ci-x86_64-6.18.config`. Changes (`sbx-changes.config`, effect in `changed-options.txt`):

- `MODULES=n`, `BLK_DEV_INITRD=n`: everything built in, no initrd.
- `VIRTIO_MMIO_CMDLINE_DEVICES=y`: requested. Firecracker 1.17 also describes its MMIO devices over ACPI, so each device is registered twice and the cmdline copy logs a harmless `virtio-mmio virtio-mmio.0: error -EBUSY`.
- `VIRTIO_FS=y`, `FUSE_DAX=y`: virtio-fs for Cloud Hypervisor.
- `CGROUP_RDMA`, `CGROUP_MISC`, `CGROUP_NET_PRIO`, `CGROUP_NET_CLASSID`: every cgroup controller on.
- `NFT_MASQ`, `NFT_REDIR`, `IP_NF_FILTER/MANGLE/RAW`, `IP_NF_TARGET_MASQUERADE/REJECT`, `IP6_NF_*`, `NETFILTER_XT_MATCH_MULTIPORT/STATE`: Docker inside the guest.
- `IKCONFIG(_PROC)`: `/proc/config.gz` for inspection.

Already in the base config: PCI, virtio-pci, MSI, ACPI, PVH, virtio blk/net/vsock/balloon/mem, HW_RANDOM_VIRTIO, 8250 console, ext4, overlayfs, tmpfs, devtmpfs(+mount), IP_PNP, all namespaces incl. USER_NS, SECCOMP_FILTER, inotify/fanotify/epoll/eventfd/memfd, SYSVIPC, POSIX_MQUEUE, KVM_GUEST, PARAVIRT, kvmclock, bridge, veth, nftables. **`VMGENID=y`** (CRNG reseed on snapshot restore) and **`PTP_1588_CLOCK_VMCLOCK=y`** are both on (from the base config).

Docker inside a guest works (Firecracker, 2 GiB): `apt-get install docker.io docker-cli iptables`, `mount -t tmpfs tmpfs /var/lib/docker`, `dockerd &`; `hello-world` and container egress work. Without the tmpfs, dockerd falls back to the slow `vfs` driver because overlayfs cannot use the root overlay as an upper dir.

## Snapshot notes

- Firecracker: Full snapshot, File memory backend (`MAP_PRIVATE` mmap, pages fault in lazily from the page cache), `resume_vm: true`, `network_overrides` re-binds `sbxtapN`. UFFD (`backend_type: "Uffd"`) is the alternative when a custom page server is wanted. 1 GiB guest -> 1.1 GB snapshot. The API socket must exist, so start with `fc-start --api`.
- **`clock_realtime`**: Firecracker rejects it with `clock_realtime requested but not present in the snapshot state` unless the snapshot was taken while the *lab VM's* clocksource was `tsc`. This lab runs on `kvm-clock` (nested KVM), so `fc-snapshot` records the clocksource and `fc-restore` only sets `clock_realtime` when it was `tsc`. With `echo tsc > /sys/devices/system/clocksource/clocksource0/current_clocksource` it worked (tested, then reverted to keep the host's timekeeping unchanged for benchmarks).
- Cloud Hypervisor: `ch-remote pause/snapshot file://DIR/resume`, restore with `--restore source_url=file://DIR,resume=true`. `ondemand` adds `memory_restore_mode=ondemand` (userfaultfd; scripts run as root, so no sysctl needed; incompatible with virtio-fs). Agent answered 0.08 s after `ondemand` restore vs 1.05 s after `copy` restore of a 1 GiB guest.
- runsc: `plain` = default compressed checkpoint, `fast` = `--compression=none --exclude-committed-zero-pages` with `runsc restore --background`. With a running browser: 155 MB plain (smoke run), 107 MB fast; agent answered about 0.26-0.28 s after either restore. Netstack state and the in-memory rootfs overlay are part of the checkpoint.
- Restore into a different slot is not supported: the guest's IP is baked into its memory.

## Quirks hit during bring-up

1. **DNS 5 s stall** (VMs, raw netns): glibc sends A and AAAA queries in parallel from one socket. Behind the lab's MASQUERADE plus the Proxmox NAT, one reply goes missing and curl waits out the 5 s resolver timeout (`time_namelookup=5.03`, `curl -4` 0.012 s). Fix: `options single-request-reopen` in the guest resolv.conf (25 ms lookups).
2. **Docker + gVisor DNS**: on user-defined networks Docker's resolver is 127.0.0.11, backed by nftables DNAT inside the container netns. gVisor netstack never sees it, so lookups fail instantly. runsc `--reproduce-nat` and `--reproduce-nftables` (runtimeArgs) did not help. Fix: `docker-start` bind-mounts the image's resolv.conf for every runtime, which also makes DNS identical across backends.
3. **runsc strips the netns**: with `--network=sandbox`, runsc moves eth0's address and routes into netstack and removes them from the netns, and they are not restored when the sandbox exits. The next start on that slot had no route. `runsc-start`/`runsc-restore` call `net-setup N` first.
4. **Docker vs 172.18/16**: Docker's default address pool would hand the next user network 172.18.0.0/16, which collides with the netns range; `sbxnet` is pinned to 172.30.0.0/16.
5. **Docker FORWARD policy DROP**: `net-setup` inserts ACCEPT rules for `sbxtap+`/`sbxv+` ahead of Docker's chains.
6. **Tap busy on restore**: `pgrep -f` stops matching an exiting VMM (its cmdline disappears with its memory) before it closes the tap fd, so an immediate restore failed with `Open tap device failed: Resource busy`. `kill_wait` (in `bin/kill-wait`) waits for `/proc/PID` to disappear.
7. **Self-matching pkill**: an unanchored `pkill -f "cloud-hypervisor --api-socket ..."` killed the calling ssh shell, whose command line contained the same text. All patterns are anchored with `^`.
8. **sudo drops env**: scripts re-exec with `sudo`, which drops `SBX_CH_VIRTIOFS`/`SBX_RUNSC_PLATFORM`, so they use `sudo --preserve-env=...`.
9. **socat half-close**: see the agent protocol note above.
10. runsc leaves a `nsfs` mount at `/run/sbx/runsc-root/null-netns`. It belongs to runsc, so leave it.
11. In runc containers `nproc`/`free` show the host (8 CPUs, 7.9 GiB) because limits are cgroup quotas. gVisor reports the cgroup-limited view (2 CPUs, 1 GiB) and the VMs report their real size.
12. example.com's CJK line renders as tofu in screenshots: the image ships DejaVu and Liberation only. Add `fonts-noto-cjk` if it matters.
13. Cloud Hypervisor logs `No image_type specified - detected as raw` and `sparse=on requested but backend does not support sparse operations` warnings. They are harmless.

## Teardown

```sh
for n in $(seq 0 31); do bin/fc-stop $n; bin/ch-stop $n; bin/runsc-stop $n; bin/runc-stop $n; bin/docker-stop $n; done
bin/net-teardown
```

## Measurement-phase changes (step 0, 2026-10-13)

- Guest image: `fonts-noto-cjk` added (Japanese text renders instead of tofu), `/work` directory created, and a Japanese paragraph on `www/index.html`. New digest in `VERSIONS`.
- `/work` is a tmpfs in every backend: `sbx-init` mounts it in VMs, the OCI template has a `tmpfs` mount (raw runsc/runc), `docker-start` passes `--tmpfs /work:mode=1777`.
- Firecracker config now has `"entropy": {}` (virtio-rng), so Firecracker and Cloud Hypervisor (`--rng`) expose the same device set.
- Lab sysctl `kernel.apparmor_restrict_unprivileged_userns=0` (not persisted), so Chromium runs with its own sandbox under raw runc. docker-runc still needs `--no-sandbox` (Docker's default seccomp profile blocks user namespaces).
- The measurement harness lives in `harness/` (`sudo venv/bin/python -m harness <scenario>`, run from /opt/sbx); results in `results/`. It launches the runtimes directly from Python rather than through `bin/*-start`.
