# gVisor vs Firecracker vs Cloud Hypervisor for agent sandboxes on your Proxmox box

**Decision.** Use Firecracker, and create each sandbox by restoring a snapshot of a pre-booted template instead of booting one. A restored Firecracker sandbox answered its first command in 29 ms in the lab VM and 20 ms on bare metal, and 20 clones restored at once cost 8.8 MiB of memory each, which is the start speed and density this platform needs.

Cloud Hypervisor tied on the weighted score, 3.90 against 3.95. It loses on the property the platform depends on most. Each restored clone took 45 to 60 ms and pulled its whole guest RAM into memory. Choose it if agents need GPUs or a host folder shared live. gVisor scored lowest, 3.40, despite the fastest cold start, because agent-style work ran 2.1 times slower inside it on every command. Choose it only for hosts without KVM.

What the design needs, based on what broke in the lab:

1. **Snapshot as create.** Boot a template once with the agent, and Chromium if needed, already running. Snapshot it, then restore it per task. E2B's Apache-2.0 runtime ([e2b-dev/runtime](https://github.com/e2b-dev/runtime)) implements this design on Firecracker and is worth reading before writing your own.
2. **One network namespace per clone, identical inside.** Use the same tap name, the same IP, and the same tap MAC address in every clone's namespace, plus a static neighbor entry for the guest. With a different MAC, clones lost their network for up to 30 s, and without the static entry one restore waited 1 s for ARP.
3. **Reset the guest clock after restore.** A restored Firecracker guest in a VM keeps the snapshot's time. Send the current time with the first command, or run on bare metal where `clock_realtime` is available.
4. **Huge pages if the host is itself a VM.** They cut Firecracker's nested cold boot from 261 to 160 ms and its cold browser start from 1.8 to 1.3 s.
5. **Production launcher.** Start Firecracker through its jailer. It added 11 ms here.

If hypervisors are new to you, read [How these sandboxes work](#how-these-sandboxes-work) first. It explains every term the rest of the report uses.

## Question

Which isolation technology should run one sandbox per agent task, created on demand? The sandbox has to start as fast as possible and still be a usable Linux machine. It must run programs, reach the internet, accept incoming connections, and run a browser that the agent drives over CDP, the Chrome DevTools Protocol.

Constraints:

- The code inside is untrusted. An agent wrote it seconds ago, so isolation must be clearly stronger than a plain Docker container.
- The hardware is your Proxmox box, an Intel i5-13400F with 16 threads and 31 GiB of RAM. Sandboxes can run on the host itself or inside a VM on it.
- A small team runs it, so operating effort counts.

## Criteria

I fixed the criteria, thresholds, weights, and scoring bands before measuring anything ([criteria.md](criteria.md)). [score.py](score.py) turns the measurements into the scores below.

| # | Criterion | Kind | Threshold | Weight | Score bands for 5 / 4 / 3 / 2 |
|---|---|---|---|---|---|
| 1 | Cold start to first command, p50 | Quantitative | ≤ 500 ms | 20% | ≤ 150 / 250 / 500 / 1000 ms |
| 2 | Snapshot restore to first command, p50 | Quantitative | ≤ 100 ms | 15% | ≤ 25 / 50 / 100 / 500 ms |
| 3 | Browser, start to first CDP screenshot | Quantitative | cold ≤ 3 s, restored ≤ 500 ms | 15% | cold ≤ 1 / 1.5 / 3 / 6 s and restored ≤ 150 / 300 / 500 / 1000 ms, averaged |
| 4 | Slowdown vs runc on agent-style work, geometric mean | Quantitative | ≤ 1.5x | 15% | ≤ 1.10 / 1.25 / 1.5 / 2.0x |
| 5 | Network: egress and ingress work, at least 1 Gbit/s | Gate | pass | 5% | pass scores 5 |
| 6 | Memory per idle sandbox, and 10 sandboxes started at once | Quantitative | ≤ 50 MiB; p95 at 10 ≤ 2x single | 5% | both pass 5, one passes 3, none 1 |
| 7 | Compatibility: OCI images, Chromium's own sandbox, Docker inside, mounts, host folders, GPU | Qualitative | Chromium over CDP must work | 10% | judged below |
| 8 | Operating effort: images, networking, snapshots, guest kernel, host requirements | Qualitative | | 10% | judged below |
| 9 | Isolation strength | Qualitative | stronger than runc | 5% | judged below |

## Candidates

Measured in the lab VM unless noted. "Best config" means huge pages for Firecracker and a reused browser tab where that was faster, as described in [Evidence](#evidence).

| | Firecracker 1.17 | Cloud Hypervisor 53 | gVisor 20261005 |
|---|---|---|---|
| Cold start, p50 | 253 ms default, 160 ms with huge pages, 135 to 165 ms on bare metal | 174 ms | 63 ms raw, 186 ms through Docker |
| Restore, p50 | 29 ms, 20 ms on bare metal | 45 ms `ondemand`, varied up to 70 ms | 74 ms |
| 10 restores at once, p50 / p95 | 124 / 167 ms | 261 / 437 ms | 171 / 210 ms |
| Memory per restored idle clone | 8.8 MiB, shared snapshot | about 520 MiB, full guest RAM | 13 to 23 MiB |
| Browser to first screenshot, cold / restored | 1.26 s / 0.76 s best config | 1.11 s / 0.95 s | 1.85 s / 1.05 s |
| Agent-style work vs runc | 0.98x | 0.97x | 2.09x, process start 6x |
| Needs KVM | yes | yes | no |
| Weighted score | **3.95**, 3.67 at defaults | 3.90 | 3.40 |
| Verdict | **Pick** | Runner-up, pick for GPUs or shared folders | Pick only without KVM |

Scores by criterion, from [score.py](score.py):

| Candidate | Cold 20% | Restore 15% | Browser 15% | Workloads 15% | Network 5% | Memory 5% | Compat 10% | Ops 10% | Isolation 5% | Weighted |
|---|---|---|---|---|---|---|---|---|---|---|
| Firecracker | 4 | 4 | 3 | 5 | 5 | 3 | 4 | 3 | 5 | 3.95 |
| Cloud Hypervisor | 4 | 4 | 3 | 5 | 5 | 1 | 5 | 3 | 4 | 3.90 |
| gVisor | 5 | 3 | 2 | 1 | 5 | 3 | 4 | 5 | 4 | 3.40 |
| Firecracker, defaults | 3 | 4 | 2.5 | 5 | 5 | 3 | 4 | 3 | 5 | 3.67 |

No candidate passed the parallel half of criterion 6. Ten sandboxes at once on the lab's 8 vCPUs always took more than twice as long as one, so that criterion separated the candidates only by memory.

The three judged criteria, and why:

- **Compatibility.** Cloud Hypervisor gets 5. It runs a full Linux kernel where everything tested worked, and it adds shared host folders, which I verified with virtio-fs, plus GPU passthrough and hotplug from its docs. Firecracker gets 4. Everything inside works, including Docker inside the VM, but it cannot share a host folder or pass through a GPU. gVisor gets 4. It runs OCI images unchanged and passed the Chromium sandbox, user namespace, and inotify checks. It denies root operations such as `mount` by default, and Docker inside it and systemd need extra flags.
- **Operating effort.** gVisor gets 5. It is a Docker or Kubernetes runtime with no kernel to build, no disk images, and no KVM requirement. Firecracker and Cloud Hypervisor get 3 each. Both need KVM, a guest kernel you maintain, a pipeline that turns container images into disk images, tap devices and namespaces per sandbox, and snapshots rebuilt on every VMM upgrade. Cloud Hypervisor creates its own taps and fixes the clock on restore. Firecracker has the jailer and E2B's open-source runtime as a reference.
- **Isolation.** Firecracker gets 5, for hardware virtualization with the smallest device model, per-thread seccomp, and the jailer, the setup AWS runs multi-tenant Lambda on. Cloud Hypervisor gets 4. It has the same hardware boundary but more code a guest can reach, such as PCI, ACPI, VFIO, and vhost-user, and it fixed a virtio-block use-after-free in 2026. gVisor gets 4. Its two layers are strong, but the host kernel stays reachable through the Sentry's allowed system calls, and its own docs say neither gVisor nor VMs is "inherently more secure".

## Evidence

### Setup

All three tools ran the same guest on the same hardware, and a harness timed them the same way.

- **Hardware.** The Proxmox VE 8.2 host has an i5-13400F with 6 performance cores, 4 efficiency cores, and 16 threads, plus 31 GiB of RAM. The main runs happened inside VM 300, an Ubuntu 24.04 VM with 8 vCPUs pinned to the performance cores and 8 GiB of memory. Firecracker and Cloud Hypervisor guests were therefore nested VMs. A second set of runs repeated boot and restore on the Proxmox host itself.
- **Versions.** Firecracker 1.17.0, Cloud Hypervisor 53.0, gVisor `release-20261005.0` on the default systrap platform, runc 1.5.2, Docker 29.9.0.
- **Guest.** One Linux 6.18.56 kernel built from Firecracker's 6.18 config, used by both VMMs. One Debian 13 image of 2.3 GB with Chromium 155, Node 20, Python 3.13, git, and gcc, used by every backend: as an ext4 disk for the VMs, as an OCI bundle for raw runsc and runc, and as a Docker image. Every sandbox had 2 vCPUs and 1 GiB of memory.
- **Timing.** The clock starts right before the harness launches the runtime. It stops when `echo ok` comes back through a small command agent inside the sandbox. Network devices, namespaces, and config files are created before the clock starts, as a sandbox pool would do. Trials run round-robin across backends, 30 per backend after 2 discarded warm-ups unless stated otherwise.
- **Raw data.** Every record is in [raw/](raw/). [raw/summary.md](raw/summary.md) has the full tables.
- **Code.** The harness, launch scripts, guest image, and kernel config are in [lab/](../lab/).

### gVisor cold-starts fastest, and nesting hurts Firecracker most

Time from launch to the first command answered, in milliseconds, inside the lab VM:

| Backend | p50 | p95 | Notes |
|---|---|---|---|
| runc, no isolation | 33 | 33 | baseline |
| gVisor, raw `runsc run` | 63 | 76 | |
| runc via `docker run` | 150 | 157 | Docker itself costs about 120 ms |
| Cloud Hypervisor | 174 | 183 | |
| gVisor via `docker run` | 186 | 197 | |
| gVisor via Docker, KVM platform | 246 | 262 | slower than systrap when nested, as gVisor's docs predict |
| Firecracker | 253 | 269 | |
| Firecracker through the jailer | +11 | | separate run: 323 ms with the jailer vs 311 ms without |
| Normal Proxmox VM, `qm start` to SSH | 9,700 | 10,000 | 3 trials, Ubuntu cloud image, for scale |

Firecracker's own specification promises 125 ms to the guest's init on bare metal, so 253 ms needed an explanation. I traced it in four steps:

1. Turning the serial console off saved only about 15 ms on either VMM.
2. Each guest's kernel log puts the start of init at 173 ms on Firecracker and 122 ms on Cloud Hypervisor. Before the guest clock even starts, Firecracker spends about 72 ms and Cloud Hypervisor about 38 ms, judged by the guest uptime each reports in its first message.
3. Cloud Hypervisor backs guest RAM with transparent huge pages by default, and Firecracker uses 4 KiB pages unless you configure huge pages. Under nesting, every first touch of a guest page is an expensive VM exit, and huge pages need 512 times fewer of them.
4. An A/B test confirmed it. These runs alternated between the lab VM and the bare-metal host while your VM 100 kept the host at a load of about 8.8, so compare within the table:

| Configuration, p50 to first command | Lab VM, nested | Proxmox host, bare metal |
|---|---|---|
| Firecracker, 4 KiB pages, default | 303 to 307 ms | 164 to 168 ms |
| Firecracker, 2 MiB huge pages | 171 to 178 ms | 135 to 140 ms |
| Cloud Hypervisor, THP on, default | 219 to 220 ms | 189 to 199 ms |
| Cloud Hypervisor, THP off | 343 to 348 ms | 202 to 219 ms |

On bare metal, Firecracker is the fastest VM to boot even with default settings. Nested, it is the slowest unless you give it huge pages. Huge pages have a cost. Firecracker needs them reserved up front with `vm.nr_hugepages`, so that memory can no longer be overcommitted.

Starting many at once shows the CPU cost of a boot. Ten 2-vCPU VMs ask for 20 vCPUs on the lab's 8, so every VM boot slows down:

| p50 / p95 ms | 1 at a time | 5 at once | 10 at once | 20 at once |
|---|---|---|---|---|
| Firecracker | 253 / 269 | 348 / 379 | 835 / 885 | 1610 / 1761 |
| Cloud Hypervisor | 174 / 183 | 252 / 281 | 642 / 693 | 1455 / 1685 |
| gVisor, raw | 63 / 76 | 116 / 128 | 207 / 235 | 378 / 431 |
| runc | 33 / 33 | 36 / 37 | 42 / 49 | 78 / 93 |

### Firecracker restores fastest and shares memory between clones

A snapshot of an idle sandbox was restored 30 times into the same slot. Times are to the first command answered.

| Backend and mode | p50 ms | p95 ms | Memory after restore | Snapshot size |
|---|---|---|---|---|
| Firecracker, memory file mapped lazily | 29 | 33 | 32 MiB | 1 GiB file, written in 0.9 s |
| Cloud Hypervisor, `ondemand` | 45 | 50 | 1030 MiB | 1 GiB file, written in 0.9 s |
| Cloud Hypervisor, `copy`, the default | 257 | 319 | 1030 MiB | same file |
| gVisor, `restore --background` | 74 | 74 | 65 MiB | 1.1 MiB idle, 149 MiB with Chromium |
| gVisor, plain restore | 74 | 75 | 65 MiB | same |

Three details matter more than the medians:

- Firecracker maps the memory file into the new VM, so a restored VM only reads the pages it touches, 32 MiB here. Clones of one snapshot share those pages through the host's page cache. Restoring 20 clones of one 512 MiB snapshot at the same time cost 8.8 MiB of extra memory per clone.
- Cloud Hypervisor's `ondemand` mode answers early, but then keeps loading the whole guest RAM in the background, so each restored VM ends up holding its full 1 GiB. Its `copyonwrite` mode, which would share pages the way Firecracker does, is documented on the main branch but not in the v53 release notes.
- Cloud Hypervisor's numbers moved between sessions. `ondemand` gave 45, 56, and 70 ms, and `copy` gave 257 to 675 ms. `copy` reads the entire 1 GiB file, so it depends on whether the file is still in the page cache.

On the Proxmox host, restore took 20 ms for Firecracker and 61 to 71 ms for Cloud Hypervisor `ondemand`, against 37 and 70 ms nested.

Restoring many clones at once is what a burst of agents does. Each clone got its own network namespace with identical addresses, the guests had 512 MiB, and each cell is 3 rounds:

| p50 / p95, extra memory per clone | 1 | 5 at once | 10 at once | 20 at once |
|---|---|---|---|---|
| Firecracker | 30 / 51 ms, under 1 MiB | 50 / 71 ms, 2.4 MiB | 124 / 167 ms, 5.7 MiB | 270 / 349 ms, 8.8 MiB |
| Cloud Hypervisor, `ondemand` | 58 / 62 ms, 526 MiB | 123 / 149 ms, 525 MiB | 261 / 437 ms, 520 MiB | not run, would need 10 GiB |
| gVisor, `--background` | 73 / 124 ms, 10 MiB | 131 / 133 ms, 17 MiB | 171 / 210 ms, 13 MiB | 375 / 603 ms, 23 MiB, 2 of 60 failed |

Extra memory is the drop in the lab's available memory divided by the clone count. The kernel counts the shared snapshot pages as reclaimable cache, so the numbers are each clone's private memory. The two gVisor failures were sandboxes that refused connections for the full 20 s window after restoring.

### Chromium works everywhere, and a snapshot of a running browser is the fast path

Chromium 155 ran headless in every backend, and Playwright on the lab VM drove it over CDP, the way an agent would. Screenshots of a local test page with Japanese text, and of example.com, are in [raw/artifacts/](raw/artifacts/). Chromium's own sandbox worked in every backend except runc under Docker's default seccomp profile. Raw runc needed the lab VM's AppArmor restriction on unprivileged user namespaces lifted first.

Main run, opening a new tab, p50 in ms from launch for cold starts and from the restore call for restores:

| Backend | CDP answering | First screenshot | example.com loaded | Memory, Chromium idle |
|---|---|---|---|---|
| runc | 231 | 865 | 969 | 439 MiB |
| Cloud Hypervisor | 555 | 1,140 | 1,264 | 765 MiB |
| Firecracker | 979 | 1,748 | 1,869 | 580 MiB |
| gVisor | 935 | 1,977 | 2,120 | 577 MiB |
| Firecracker, restored | 38 | 897 | 1,070 | |
| Cloud Hypervisor `ondemand`, restored | 178 | 953 | 1,076 | |
| gVisor `--background`, restored | 316 | 1,302 | 1,456 | |

Chromium's own work dominates. Even on runc it took about 630 ms from CDP answering to the first screenshot. Playwright needed 130 ms to attach, opening a tab and loading the page took 230 ms, and the first screenshot with software rendering took 280 ms. A restored Firecracker browser answers CDP in 38 ms. What remains is that same Chromium work, a little slower because the browser's memory loads from the snapshot as it is touched.

Two cheap changes helped. Both were measured in later sessions while your VM 100 loaded the host, so compare within each table, not with the one above.

| Reusing the open tab, p50 to first screenshot | New tab | Reused tab |
|---|---|---|
| runc, cold | 981 ms | 797 ms |
| Cloud Hypervisor, cold | 1,336 ms | 1,105 ms |
| Firecracker, cold | 2,113 ms | 1,683 ms |
| gVisor, cold | 2,299 ms | 1,852 ms |
| Firecracker, restored | 934 ms | 762 ms |
| gVisor `--background`, restored | 1,388 ms | 1,066 ms |
| Cloud Hypervisor `ondemand`, restored | 1,004 ms | 2,109 ms |

Cloud Hypervisor's restore got slower with the reused-tab snapshot. My best guess is that its 1 GiB snapshot file fell out of the lab's page cache, but I did not confirm it.

| Huge pages, p50 to first screenshot | Cold browser |
|---|---|
| Firecracker, 4 KiB pages | 1,791 ms |
| Firecracker, 2 MiB huge pages | 1,258 ms |
| Cloud Hypervisor | 1,188 ms |

No backend met the 500 ms target for a restored browser's first screenshot. The best was Firecracker with a reused tab at 762 ms. Most of that time is Chromium rendering in software, so no sandbox will get much below 600 ms here without a GPU. A browser ready to take commands, CDP up and Playwright attached, took about 150 ms after a Firecracker restore.

### Programs run at native speed in the VMs, and gVisor pays per system call

Each backend ran 9 workloads 5 times inside one long-lived sandbox, in a tmpfs, so the numbers measure the isolation layer and not the disk. Ratios are time relative to runc, so lower is better. For CPU the table shows runc's throughput divided by the backend's.

| Workload | Firecracker | Cloud Hypervisor | gVisor | runc absolute |
|---|---|---|---|---|
| CPU, 1 thread, `sysbench cpu` | 1.01x | 1.01x | 1.04x | 3793 events/s |
| Start `/bin/true` 2000 times | 1.14x | 1.12x | 5.97x | 0.77 s |
| Start Python 50 times | 0.91x | 0.91x | 1.65x | 0.40 s |
| Create, stat, and delete 20,000 files | 0.87x | 0.90x | 3.89x | 0.18 s |
| Untar a node_modules of 23,405 files | 0.85x | 0.80x | 3.65x | 0.18 s |
| Node `require` of 622 modules | 1.03x | 1.00x | 1.42x | 0.43 s |
| git status, log -p, and grep | 1.02x | 0.99x | 1.66x | 0.55 s |
| Compile sqlite3.c with gcc -O2 | 1.02x | 1.02x | 1.07x | 17.4 s |
| **Geometric mean of the 8 above** | **0.98x** | **0.97x** | **2.09x** | |

The VMs matched runc and sometimes beat it. A likely reason is that their minimal guest kernel does less work per file operation than the stock Ubuntu kernel that runc shares with the lab VM, but I did not verify that. gVisor was about 2x slower overall and 6x slower at starting processes, the things agents do constantly with shells, package managers, and test runners. This cost repeats on every command, while a start-time advantage is paid once.

### All three pass the network checks with room to spare

| Measurement | Firecracker | Cloud Hypervisor | gVisor | runc |
|---|---|---|---|---|
| HTTP round trip, sandbox to lab, p50 | 0.61 ms | 0.51 ms | 0.65 ms | 0.38 ms |
| HTTP round trip, lab into sandbox, p50 | 0.58 ms | 0.41 ms | 0.82 ms | 0.38 ms |
| iperf3 to the lab, up / down | 39.8 / 37.6 Gbit/s | 35.1 / 43.8 Gbit/s | 24.5 / 32.6 Gbit/s | 58.3 / 58.6 Gbit/s |
| HTTPS to example.com, median of 5 | 69 ms | 70 ms | 72 ms | 69 ms |

Egress, DNS, and an HTTP server reached from outside worked everywhere. Two setup problems are worth knowing. In every backend, DNS lookups stalled for 5 s behind the double NAT until the guest's `resolv.conf` got `options single-request-reopen`. And gVisor's network stack cannot see Docker's internal DNS resolver at 127.0.0.11, so gVisor containers under Docker need a real resolver.

### gVisor uses the least memory per idle sandbox, and Firecracker clones come close

Average drop in the lab VM's available memory per sandbox, with 10 idle sandboxes running:

| Backend | MiB per idle sandbox |
|---|---|
| runc | 2.7 |
| gVisor, raw | 16.9 |
| gVisor via Docker | 25.6 |
| Firecracker, booted | 77.7 |
| Cloud Hypervisor, booted | 91.2 |
| Firecracker, restored | 32, process PSS |
| Cloud Hypervisor, restored | 1030, process PSS |

With Chromium open on one page, a sandbox used about 440 MiB on runc, 580 MiB on gVisor and Firecracker, and 765 MiB on Cloud Hypervisor.

### Compatibility checks

| Check | Firecracker | Cloud Hypervisor | gVisor |
|---|---|---|---|
| Chromium's own sandbox, as a non-root user | pass | pass | pass |
| User namespaces, `unshare -Ur` | pass | pass | pass |
| inotify, as used by dev servers and watchers | pass | pass | pass |
| `apt-get install`, ping, egress | pass | pass | pass |
| `mount -t tmpfs` as root inside | pass | pass | fail, missing `CAP_SYS_ADMIN` |
| Docker inside the sandbox | pass, with a tmpfs for `/var/lib/docker` | not tested | needs extra flags per gVisor's docs, not tested |
| Share a host folder | no, block devices only, from docs | pass, virtio-fs | yes, OCI bind mounts, from docs |
| GPU | none, from docs | VFIO passthrough, from docs | nvproxy for CUDA, from docs |
| Guest clock right after restore | 2952 s behind | correct | correct |
| `/dev/urandom` across two restores of one snapshot | differs | differs | differs |
| `/dev/urandom` across two restores of a 1-second-old snapshot | differs | differs | differs |

The clock result is a real trap. A restored Firecracker guest believes it is still the moment of the snapshot until something resets its clock, which breaks TLS certificate checks and token expiry. Firecracker's fix, `clock_realtime`, is rejected inside the lab VM because the lab's clock source is kvm-clock instead of tsc. On bare metal with tsc it should work, but I did not test that. Cloud Hypervisor 53 moves the clock forward itself, and gVisor reads the host clock directly.

## Landscape

The choice is technical, but the market shows which designs hold up in production. Latency figures from vendors are their own claims, not measurements.

- **Agent-sandbox vendors mostly converged on Firecracker plus snapshot restore.** E2B's open-source runtime says "creating" a sandbox means restoring a pre-booted snapshot, with memory loaded lazily through userfaultfd and a copy-on-write root filesystem ([e2b-dev/runtime](https://github.com/e2b-dev/runtime)). Vercel Sandbox ([docs](https://vercel.com/docs/vercel-sandbox/concepts)), Fly.io ([Fly](https://fly.io/learn/firecracker-vm.md)), and CodeSandbox ([blog](https://codesandbox.io/blog/joining-together-ai-introducing-codesandbox-sdk)) run Firecracker as well. CodeSandbox says it picked Firecracker because it can pause a VM, write its memory to disk, and resume it later.
- **Model labs and hyperscalers lean on gVisor.** gVisor's users page lists Anthropic for code execution in claude.ai, OpenAI for "some higher-risk tasks", Modal, and Google's GKE Sandbox, Cloud Run, and App Engine ([gvisor.dev/users](https://gvisor.dev/users/)). Tencent runs millions of agent sandboxes a day on gVisor, and chose it because it "can run inside regular VMs" ([gVisor blog](https://gvisor.dev/blog/2026/04/23/scaling-agentic-rl-sandboxes-to-the-millions-with-gvisor-at-tencent/)). Modal offers a VM runtime next to gVisor for the cases gVisor handles poorly, Docker inside the sandbox and FUSE ([Modal docs](https://modal.com/docs/guide/sandboxes)).
- **Cloud Hypervisor shows up mostly underneath Kata Containers.** Northflank says it moved from gVisor to Kata with Cloud Hypervisor and keeps gVisor where nested virtualization is missing ([Northflank](https://northflank.com/blog/kata-containers-vs-gvisor)). I found no vendor that publishes Cloud Hypervisor restore times.
- **The fastest browser product uses the technique this report recommends.** Kernel boots Chrome, snapshots the whole VM, and claims restarts under 20 ms ([kernel-images](https://github.com/kernel/kernel-images)).
- **Warm pools are the other approach.** Azure Container Apps dynamic sessions keep pre-started Hyper-V sandboxes ([Microsoft Learn](https://learn.microsoft.com/azure/container-apps/sessions)), and the Kubernetes agent-sandbox project has a `SandboxWarmPool` ([agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox)). A pool hides start time but pays for machines that sit idle.
- I found no credible independent estimate of the market's size. The funded specialists are still small. E2B raised a $21M Series A in 2025 ([E2B](https://e2b.dev/blog/series-a)).

## Risks and open questions

What could make this decision wrong:

- **Hosts without KVM.** Firecracker and Cloud Hypervisor need `/dev/kvm`, so bare metal or a VM with nested virtualization. If the sandboxes must run on ordinary cloud VMs without nested virtualization, gVisor is the only option of the three. This flips the decision.
- **GPUs or shared host folders.** Firecracker has neither, and its maintainers see no path to GPUs. If agents need a GPU or a host directory mounted live, use Cloud Hypervisor with VFIO or virtio-fs, or gVisor with nvproxy. A passed-through GPU also blocks Cloud Hypervisor snapshots unless its driver supports VFIO migration v2.
- **Snapshot clones need care.** Each clone keeps the snapshot's IP and MAC, so every clone needs its own network namespace. A restored Firecracker guest in a nested VM keeps the snapshot's wall clock until you reset it, 2952 s behind in this lab. Random-number state cached inside programs such as Chromium survives every restore, even when the kernel reseeds. Secrets in memory at snapshot time end up in every clone, so build templates before any credentials go in.
- **Snapshots are tied to the VMM version.** Cloud Hypervisor does not restore snapshots across versions at all, and Firecracker needs a binary that supports the snapshot's format. Every upgrade means rebuilding template snapshots.
- **Cloud Hypervisor could close the gap.** If its `copyonwrite` restore mode ships and shares memory between clones the way Firecracker's file-backed restore does, its density problem goes away. Its extra features would then weigh more.
- **Light workloads favor gVisor.** If your agents mostly run short tool calls with little process spawning and file churn, gVisor's 2x overhead matters less, and its simpler operation weighs more.

Limits of these measurements:

- The main runs were nested inside a VM on a desktop CPU. Bare-metal runs covered boot and restore only.
- From the reuse-tab browser runs on, your VM 100 kept the Proxmox host at a load average near 8.8. Tables from those later sessions compare backends within a session, not with the earlier main run.
- Cloud Hypervisor restore times varied by up to 2.6x between sessions, depending on whether the 1 GiB snapshot file was in the page cache. Firecracker's cold boot drifted between 253 and 311 ms across sessions.
- The 10-at-once cold boot test was limited by the lab's 8 vCPUs.
- iperf3 used a single stream to the lab VM, with no physical network in the path.
- Chromium used software rendering everywhere, because the lab has no GPU.
- Docker inside the sandbox was tested only on Firecracker. Docker inside gVisor and gVisor's syscall gaps were not exercised beyond the compatibility table. Tencent reports that about 1.7% of its test cases hit gVisor incompatibilities.

## How these sandboxes work

This section is for a reader who uses Docker and Proxmox every day but has never looked inside a hypervisor. It explains what each tool does, why a microVM boots in a fraction of a second, and why a snapshot is faster still. Numbers marked "measured" come from the lab described in [Evidence](#evidence).

### A container shares the host kernel, and that is the problem

Every program asks the kernel to do its work through system calls. Open a file, create a socket, start a process. Linux 6.18 on x86-64 has 383 of them, counted in the kernel's `arch/x86/entry/syscalls/syscall_64.tbl`.

A container started by runc, the runtime Docker uses by default, is an ordinary Linux process with a restricted view. Namespaces give it its own process IDs, mounts, and network stack. Cgroups cap its CPU and memory. Capabilities and a seccomp filter take away some privileges and some system calls. Docker's default seccomp profile blocks about 44 calls ([Docker docs](https://docs.docker.com/engine/security/seccomp/)). For everything else, the process calls the host kernel directly.

So one exploitable kernel bug that the container can reach is enough to take over the host. It happens. Dirty Pipe, [CVE-2022-0847](https://nvd.nist.gov/vuln/detail/CVE-2022-0847), let an unprivileged process overwrite read-only files through the page cache. The nf_tables bug [CVE-2024-1086](https://nvd.nist.gov/vuln/detail/CVE-2024-1086) gives local root and is on CISA's list of vulnerabilities exploited in the wild. The runtime can fail as well. runc's [CVE-2024-21626](https://github.com/opencontainers/runc/security/advisories/GHSA-xr7r-f8xq-vfvv) let a container start with its working directory inside the host filesystem.

Code that an agent wrote a few seconds ago deserves a second wall. There are two ways to build one. gVisor puts a kernel written in Go between the program and Linux. A virtual machine gives the program a real Linux kernel of its own, on virtual hardware that the CPU keeps separate. Firecracker and Cloud Hypervisor are two programs that build such virtual machines.

```
 runc container          gVisor (runsc)               microVM (Firecracker, Cloud Hypervisor)
 +-------------+         +-------------+              +---------------------+
 |  agent code |         |  agent code |              |  agent code         |
 +------+------+         +------+------+              +---------------------+
        |                       | system calls,       |  guest Linux kernel |
        |                       | trapped             +----------+----------+
        |                +------v------+                         | VM exits, virtqueues
        |                | Sentry: a   |              +----------v----------+
        |                | kernel in Go|              |  VMM process        |
        |                +------+------+              |  (seccomp, jailer)  |
        |                       | short list of       +----------+----------+
        | most of 383           | host system calls              | KVM ioctls
 +------v-----------------------v--------------------------------v----------+
 |                      host Linux kernel (with KVM)                         |
 +---------------------------------------------------------------------------+
 What the agent can reach:
   the whole syscall API | the Sentry, then a short list | VM exits and a few virtual devices
```

### gVisor answers the system calls itself

gVisor's core is the Sentry, a Linux-compatible kernel that runs as a normal user-space process. When the sandboxed program makes a system call, the call goes to the Sentry, not to the host kernel. The Sentry implements it in Go, with its own file descriptor tables, process model, memory management, and TCP/IP stack, called netstack ([architecture guide](https://gvisor.dev/docs/architecture_guide/intro/)).

The part that catches system calls is called the platform. The default since mid-2023 is systrap ([platforms](https://gvisor.dev/docs/architecture_guide/platforms/)). It installs a seccomp filter on the sandboxed threads that turns every system call into a `SIGSYS` signal. A signal handler copies the request into memory shared with the Sentry, and the Sentry answers it. On x86-64, systrap also rewrites the common `mov; syscall` instruction pair into a jump to a trampoline, which skips the signal on hot paths ([systrap announcement](https://gvisor.dev/blog/2023/04/28/systrap-release/)). A second platform uses KVM. It is fast on bare metal, and the gVisor docs say it "runs poorly with nested virtualization".

The Sentry is locked down too. A seccomp filter limits it to a short list of host system calls, and it cannot open files by itself. A second process, the Gofer, owns the container's filesystem and hands the Sentry file descriptors. With directfs, the default since 2023, the Sentry uses those descriptors directly, and seccomp forces `O_NOFOLLOW` so a symlink cannot lead it out ([filesystem guide](https://gvisor.dev/docs/user_guide/filesystem/)). An attacker needs two bugs. The first takes over the Sentry. The second is a host-kernel bug reachable through the Sentry's short list ([security model](https://gvisor.dev/docs/architecture_guide/security/)).

To Docker, gVisor is one more OCI runtime called `runsc`. You keep your container images and add `--runtime=runsc`. There is no kernel to build and no disk image to make. Systrap does not need KVM, so gVisor runs on any Linux machine, including a cloud VM without nested virtualization. Tencent gives exactly this as its reason for choosing gVisor over microVMs for millions of agent sandboxes a day ([Tencent post](https://gvisor.dev/blog/2026/04/23/scaling-agentic-rl-sandboxes-to-the-millions-with-gvisor-at-tencent/)).

The price is that every system call runs through Go code instead of the host kernel. Workloads that make many calls pay for it, such as starting processes, touching many small files, and moving packets. In the lab, gVisor was 2.1 times slower than runc across agent-style workloads and 6 times slower at starting processes, while pure CPU work ran at native speed.

### A VM gives the program its own kernel, and the CPU enforces the wall

A virtual machine runs a complete guest kernel that believes it owns a computer. Making that safe and fast needs help from the CPU.

Intel VT-x adds two modes to the processor ([Intel SDM, vol. 3C](https://www.intel.com/content/www/us/en/developer/articles/technical/intel-sdm.html)). The hypervisor runs in VMX root mode. The guest runs in VMX non-root mode, with its own rings 0 to 3, so the guest kernel really runs in ring 0. A per-vCPU structure called the VMCS lists the events that must stop the guest. When one happens, for example the guest touches a device register, the CPU performs a VM exit. It saves the guest's state and hands control back to the hypervisor. A second set of page tables, EPT, translates guest memory addresses to host memory addresses in hardware, so the guest cannot even name memory that belongs to someone else. AMD's equivalents are AMD-V and NPT.

In Linux, the KVM kernel module turns these features into an API on `/dev/kvm` ([KVM API docs](https://docs.kernel.org/virt/kvm/api.html), [LWN walkthrough](https://lwn.net/Articles/658511/)). A program that wants to run a VM does roughly this:

1. Open `/dev/kvm` and call `KVM_CREATE_VM`.
2. `mmap` some ordinary memory and register it as guest RAM with `KVM_SET_USER_MEMORY_REGION`. Guest RAM is plain memory of this process.
3. Create a vCPU with `KVM_CREATE_VCPU`. Each vCPU is a normal host thread.
4. Load a kernel into guest RAM, set the registers, and call `KVM_RUN` in a loop.

`KVM_RUN` returns whenever the guest does something KVM cannot finish inside the kernel. Most often the guest wrote to a device register, and the exit reason is `KVM_EXIT_MMIO` or `KVM_EXIT_IO`. The program emulates the device and calls `KVM_RUN` again.

```
 VMM process (user space)              KVM (host kernel)            guest
 open /dev/kvm, KVM_CREATE_VM
 mmap RAM, KVM_SET_USER_MEMORY_REGION
 KVM_CREATE_VCPU, set registers
 loop:
   ioctl(vcpu, KVM_RUN) ------------>  VM entry ------------------>  runs at full speed
                                       <-------------------------  VM exit, e.g. write to a
                                       handled in kernel?             virtio "kick" register
                                       yes: re-enter. no: return
   switch (exit_reason) <-------------
     KVM_EXIT_MMIO: emulate the device, process its virtqueue
     KVM_EXIT_IO:   serial port and similar
     KVM_EXIT_SHUTDOWN: stop
```

That program is the VMM, the virtual machine monitor. So the "hypervisor" is two pieces. KVM, inside the host kernel, handles the CPU and memory. The VMM, a user-space process, handles everything else, including the memory layout, loading the kernel, and every virtual device. QEMU, Firecracker, and Cloud Hypervisor are all VMMs on top of the same KVM. Your Proxmox box already works this way. Each VM in `qm list` is one `/usr/bin/kvm` process, and that binary is QEMU. I checked this with `ps -C kvm` on the host.

The guest reaches the host only through VM exits. The code that handles them, KVM plus the VMM's device emulation, is therefore the attack surface. QEMU emulates a whole PC in more than 1.4 million lines of code ([Firecracker paper](https://www.usenix.org/system/files/nsdi20-paper-agache.pdf), section 2.1). A bug in its virtual floppy controller, [VENOM](https://nvd.nist.gov/vuln/detail/CVE-2015-3456), let a guest break out. Firecracker and Cloud Hypervisor exist largely to shrink that code.

### virtio is how a VM gets a disk and a network card cheaply

Emulating a real network card means a VM exit on every register write. That is slow, and every emulated register is more code an attacker can poke. VMs use paravirtual devices from the [virtio standard](https://docs.oasis-open.org/virtio/virtio/v1.3/virtio-v1.3.html) instead. The guest driver knows it runs in a VM. It puts requests into rings in shared memory, called virtqueues, and notifies the device once per batch. One VM exit can then carry many packets or disk blocks.

The guest finds virtio devices in one of two ways. With virtio-MMIO, the VMM places each device at a fixed memory address and lists the addresses on the kernel command line. Firecracker does this by default, so its guests never scan a PCI bus. With virtio-PCI, the guest enumerates a virtual PCI bus, as on a real PC. Cloud Hypervisor and QEMU use PCI, and Firecracker can since v1.13.

A virtio-net device needs somewhere to send packets on the host. That is a tap device, a virtual network interface on the host whose other end is a file descriptor held by the VMM. Packets the guest sends come out of the tap, and the host routes or NATs them like traffic from any other interface. In the lab, VM number N uses `sbxtapN`, and the host side of the link is 172.16.N.1.

### A microVM boots in milliseconds because it skips the PC

A normal VM boots like a PC. Firmware, SeaBIOS or OVMF, initializes virtual hardware and scans PCI. GRUB loads a compressed kernel from disk. The kernel decompresses itself, probes dozens of device types, mounts an initramfs, and then systemd starts services. On this Proxmox box, the Ubuntu lab VM took 9.6 to 10.0 s from `qm start` until SSH answered. `systemd-analyze` puts 2.5 s in the kernel and 3.6 s in user space. The rest is `qm start` itself, about 1.1 s, plus firmware and GRUB.

A microVM removes almost all of it:

- The VMM loads an uncompressed kernel straight into guest memory and jumps to its entry point, with no firmware and no bootloader. This is direct kernel boot, through the [Linux x86 boot protocol](https://docs.kernel.org/arch/x86/boot.html) or the PVH entry point.
- The kernel contains only the drivers this VM can have, mostly virtio and a serial port. There are no modules and no initramfs. The lab's kernel is 26 MiB uncompressed, built from Firecracker's 6.18 config.
- PID 1 is not systemd. In the lab it is a short shell script that mounts `/proc`, makes the root filesystem writable with an overlay, and starts the command agent.

```
Normal VM, measured:   [qm start 1.1 s][firmware + GRUB][kernel 2.5 s][systemd 3.6 s] -> SSH at 9.6-10.0 s
microVM, measured:     [VMM start + kernel load 40-70 ms][kernel 120-170 ms][init + agent] -> command at 160-250 ms
Snapshot, measured:    [VMM start + map memory][resume] -> command at 20-37 ms, nothing boots
```

Firecracker's specification promises at most 125 ms from the `InstanceStart` call to the start of guest user space, measured with the serial console disabled and a minimal kernel and root filesystem on bare-metal EC2 instances ([SPECIFICATION.md](https://github.com/firecracker-microvm/firecracker/blob/main/SPECIFICATION.md)). On your Proxmox host, Firecracker answered its first command 164 ms after launch with default settings and 135 ms with huge pages. That includes the kernel's network autoconfiguration and a round trip through the command agent, so it agrees with the specification.

### Firecracker is the smallest VMM that runs Lambda

AWS built Firecracker for Lambda and Fargate ([NSDI 2020 paper](https://www.usenix.org/conference/nsdi20/presentation/agache)). One Firecracker process runs exactly one microVM, with three kinds of threads. An API thread serves a REST API on a Unix socket. A VMM thread runs one event loop for all devices. One thread per vCPU runs the `KVM_RUN` loop ([design doc](https://github.com/firecracker-microvm/firecracker/blob/main/docs/design.md)).

The device list is short on purpose. It has virtio-net, virtio-block, virtio-vsock, virtio-balloon, virtio-rng, virtio-pmem, and virtio-mem, plus a serial port and a partial i8042 keyboard controller whose only job is to let the guest reset. Devices use MMIO unless you pass `--enable-pci`. In production you start Firecracker through the jailer, which puts it in a chroot with its own namespaces and cgroup and drops it to an unprivileged user. Each thread also gets its own seccomp filter, compiled into the binary ([jailer](https://github.com/firecracker-microvm/firecracker/blob/main/docs/jailer.md), [seccomp](https://github.com/firecracker-microvm/firecracker/blob/main/docs/seccomp.md)).

What it leaves out matters as much. There is no GPU or other device passthrough, and the maintainers say there is "no known path" to it ([issue 849](https://github.com/firecracker-microvm/firecracker/issues/849)). There is no virtio-fs or 9p, so you cannot share a host directory. You put files in through a block device or over the network. There is no live migration, no nested virtualization inside the guest, and the guest must be Linux.

This is the whole configuration the lab used for one Firecracker VM:

```json
{
  "boot-source": {
    "kernel_image_path": "/opt/sbx/kernel/vmlinux",
    "boot_args": "console=ttyS0 reboot=k panic=1 pci=off quiet random.trust_cpu=on ip=172.16.1.2::172.16.1.1:255.255.255.0::eth0:off init=/sbin/sbx-init ro root=/dev/vda"
  },
  "drives": [{"drive_id": "rootfs", "path_on_host": "/opt/sbx/images/rootfs.ext4",
              "is_root_device": true, "is_read_only": true}],
  "network-interfaces": [{"iface_id": "eth0", "host_dev_name": "sbxtap1", "guest_mac": "06:00:ac:10:01:02"}],
  "machine-config": {"vcpu_count": 2, "mem_size_mib": 1024},
  "entropy": {}
}
```

`firecracker --no-api --config-file config.json` boots it. `boot-source` is the kernel and its command line. `ip=` makes the kernel configure `eth0` before init runs, `init=` names the script that replaces systemd, and `pci=off` skips a bus that does not exist. `drives` attaches the root filesystem image read-only, so every VM shares one file. `network-interfaces` names the host tap device. `entropy` adds a virtio-rng device.

A restore is a separate Firecracker process that receives one API call over its Unix socket:

```sh
firecracker --api-sock /run/fc.sock &
curl --unix-socket /run/fc.sock -X PUT http://localhost/snapshot/load -d '{
  "snapshot_path": "/snap/vmstate",
  "mem_backend": {"backend_type": "File", "backend_path": "/snap/mem"},
  "resume_vm": true}'
```

### Cloud Hypervisor keeps the same core and adds what a cloud needs

Cloud Hypervisor is also a Rust VMM on KVM, built from the same rust-vmm crates, and its README says "a large part" of its code comes from Firecracker or crosvm ([README](https://github.com/cloud-hypervisor/cloud-hypervisor#4-relationship-with-rust-vmm-project)). Its goal is different. It wants to run general cloud workloads, not only serverless functions. The Linux Foundation hosts it, and in 2026 most commits come from Meta, Microsoft, and Cyberus Technology.

So it adds what Firecracker leaves out. Every device sits on PCI, and the VM has ACPI. You can hot-add CPUs, memory, and devices to a running VM. VFIO passes real PCI devices, including GPUs, into the guest. vhost-user devices include virtio-fs, which shares a host directory through a separate `virtiofsd` process. It can boot UEFI firmware and Windows guests, migrate a running VM to another host, and run on Microsoft's hypervisor as well as KVM ([device model](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/device_model.md), [hotplug](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/hotplug.md)).

Each of those features is code that a guest can reach. Cloud Hypervisor v52 fixed a use-after-free in its virtio-block code, CVE-2026-45782, and v53 fixed several crashes that a guest could trigger ([v52](https://github.com/cloud-hypervisor/cloud-hypervisor/releases/tag/v52.0), [v53](https://github.com/cloud-hypervisor/cloud-hypervisor/releases/tag/v53.0)). Rust removes whole classes of bugs, not all of them. The project's threat model also says it is not hardened against a guest that tries to exhaust host CPU or memory, so you should put it in a cgroup ([threat model](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/threat-model.md)).

The same VM on Cloud Hypervisor is one command line, with the same kernel and image:

```sh
cloud-hypervisor --api-socket path=/run/ch.sock \
  --kernel /opt/sbx/kernel/vmlinux \
  --cmdline "console=ttyS0 reboot=k panic=1 quiet random.trust_cpu=on ip=... init=/sbin/sbx-init ro root=/dev/vda" \
  --disk path=/opt/sbx/images/rootfs.ext4,readonly=on \
  --net tap=sbxtap2,mac=06:00:ac:10:02:02 \
  --cpus boot=2 --memory size=1024M --rng src=/dev/urandom --serial tty --console off
```

A restore is `cloud-hypervisor --api-socket path=/run/ch.sock --restore source_url=file:///snap,resume=true,memory_restore_mode=ondemand`. For gVisor, the equivalent of all of this is `docker run --runtime=runsc <image>`, or `runsc run --bundle <dir> <id>` without Docker.

### A snapshot skips the boot and the warm-up

A snapshot of a VM is all of its state, frozen. Guest RAM is by far the largest part. The rest is vCPU registers and the state of each virtual device. Restoring one does not boot anything. The guest continues from the instruction where it stopped, with its kernel initialized and its programs already running. For an agent sandbox, that means Chromium can already be open when the sandbox "starts".

Restore is fast because the VMM does not read the memory file up front. Firecracker maps the file into the new VM's memory with `mmap`, so a page is read from the file only when the guest first touches it ([snapshot docs](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md)). A userfaultfd handler can serve pages from anywhere instead, which is how E2B loads memory lazily ([E2B runtime](https://github.com/e2b-dev/runtime)). Cloud Hypervisor reads all memory before resuming by default, and since v52 it can load on demand through userfaultfd ([snapshot_restore.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/snapshot_restore.md)). gVisor's `runsc checkpoint` saves the whole Sentry, including the application's memory, and `runsc restore --background` lets the application run before all memory is loaded ([checkpoint/restore](https://gvisor.dev/docs/user_guide/checkpoint_restore/)).

A restore is a perfect copy, which is also the problem. Every clone of one snapshot starts with the same state:

- **Network identity.** Every clone has the snapshot's IP and MAC address, so each clone needs its own network namespace or a new address ([network for clones](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/network-for-clones.md)).
- **Random numbers.** Every clone starts with the same random number generator state ([random for clones](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/random-for-clones.md)). Firecracker's VMGenID device tells Linux 5.18 and later to reseed its kernel generator on restore. Random state cached inside programs stays duplicated.
- **Time.** The wall clock shows the snapshot's time until something resets it.
- **Connections.** Open TCP connections are dead, because their peer is gone.
- **Secrets.** Everything that was in memory at snapshot time is in every clone.

The lab hit three of these:

- **Network identity.** Restored Firecracker clones kept sending replies to the MAC address of the snapshot's host tap device. Their clock does not move on restore, so that stale ARP entry stayed valid for up to about 30 s. Every clone timed out until each clone's tap got the same MAC as the original. Cloud Hypervisor 53 moves the guest clock forward on restore, which expires the entry right away.
- **Clock.** A restored Firecracker guest was 2952 s behind, the snapshot's age. Cloud Hypervisor and gVisor were within 20 ms.
- **ARP.** One restore on the bare-metal host waited 1.07 s for ARP. A permanent neighbor entry for the guest removed the wait.

Random numbers did not repeat. Two restores of one snapshot read different bytes from `/dev/urandom` on all three, even 1 second after the snapshot. Firecracker reseeds through VMGenID. Cloud Hypervisor has no VMGenID device yet ([PR #8907](https://github.com/cloud-hypervisor/cloud-hypervisor/pull/8907)), so its divergence comes from the guest kernel's own reseeding, which I would not rely on. Randomness cached inside programs, such as a browser's TLS state, is still copied into every clone.

### Nested virtualization taxes every VM exit

The lab in this report is itself a VM. The Proxmox host is level 0, written L0. The lab VM is L1, and each Firecracker or Cloud Hypervisor guest is L2. The same happens when you run microVMs on most cloud VMs.

x86 CPUs have only one real level of VMX. KVM on L0 therefore emulates VMX for the L1 hypervisor and runs L2 directly on the hardware under its own control. Every L2 VM exit goes to L0 first. If the exit belongs to L1, L0 forwards it, and L1's handling of it causes more exits to L0. Intel's VMCS shadowing removes some of them. The Turtles paper measured nested guests "within 6-8%" of single-level virtualization for common workloads, with device assignment ([OSDI 2010](https://www.usenix.org/conference/osdi10/turtles-project-design-and-implementation-nested-virtualization)). Workloads with many exits do worse.

Measured here, nesting cost Firecracker about 140 ms of cold boot, 165 ms on the host against 305 ms in the lab VM. It cost Cloud Hypervisor about 20 ms, 190 against 210 ms. Page size explains the difference. Cloud Hypervisor backs guest RAM with transparent huge pages, so the guest takes far fewer first-touch exits. With 2 MiB huge pages, Firecracker's nested boot dropped to 171 ms. Restores touch little memory, and nesting added about 17 ms to Firecracker's, 20 against 37 ms.

gVisor with systrap uses no virtualization, so running it in L1 costs nothing extra beyond what any program in a VM pays.

### Glossary

- **Hypervisor.** The software that runs VMs. On Linux, KVM plus a VMM.
- **KVM.** The Linux kernel module that exposes VT-x and AMD-V through `/dev/kvm`.
- **VMM.** The user-space process that owns one VM's memory, devices, and vCPU threads. QEMU, Firecracker, and Cloud Hypervisor are VMMs.
- **microVM.** A VM with a minimal VMM and device model, direct kernel boot, and small memory overhead.
- **vCPU.** A virtual CPU. In KVM, one host thread that calls `KVM_RUN`.
- **VM exit.** The CPU leaving the guest and returning to the hypervisor because of an event the hypervisor asked to see.
- **EPT, NPT.** Hardware page tables that translate guest-physical to host-physical addresses.
- **virtio.** The standard for paravirtual devices that exchange requests through virtqueues in shared memory.
- **MMIO.** Memory-mapped I/O. Device registers appear at memory addresses, and touching them causes a VM exit.
- **PVH.** A boot entry point that lets a VMM start a kernel directly, without firmware.
- **tap device.** A virtual network interface on the host whose other end is a file descriptor held by the VMM.
- **netns.** A network namespace. A separate copy of the network stack on one host.
- **vsock.** A socket family for host-to-guest communication that needs no network setup.
- **seccomp.** A kernel filter that restricts which system calls a process can make.
- **OCI runtime.** A program that starts containers from an OCI bundle. runc and runsc are OCI runtimes.
- **Sentry.** gVisor's kernel, written in Go, that answers the sandboxed program's system calls.
- **Gofer.** The gVisor process that owns the container's filesystem.
- **netstack.** gVisor's TCP/IP stack, written in Go.
- **Snapshot.** Saved VM or sandbox state, used to resume or clone.
- **userfaultfd, UFFD.** A Linux feature that lets a user-space process handle page faults for a memory range.
- **Nested virtualization.** Running a hypervisor inside a VM. L0 is the bare-metal host, L1 the VM, L2 the VM inside it.

## Sources

All links were opened on 2026-10-13. The research notes in [sources/](sources/) hold every page the research agents opened, with one link per claim.

**Verified by experiment in this lab.** Every timing, throughput, and memory number in [Evidence](#evidence), including the bare-metal runs. The huge-page effect and its absence on bare metal. The compatibility table rows not marked "docs". Firecracker rejecting `clock_realtime` on a kvm-clock host. Cloud Hypervisor 53 having no VMGenID device, from its binary and the open PR [#8907](https://github.com/cloud-hypervisor/cloud-hypervisor/pull/8907). The count of 383 x86-64 system calls in Linux 6.18. Proxmox VMs being QEMU processes, from `ps -C kvm` on the host.

**From sources only.**

Firecracker
- [SPECIFICATION.md](https://github.com/firecracker-microvm/firecracker/blob/main/SPECIFICATION.md): 125 ms to user space, 5 MiB overhead, 8 CPU ms startup.
- [design.md](https://github.com/firecracker-microvm/firecracker/blob/main/docs/design.md), [jailer.md](https://github.com/firecracker-microvm/firecracker/blob/main/docs/jailer.md), [seccomp.md](https://github.com/firecracker-microvm/firecracker/blob/main/docs/seccomp.md), [prod-host-setup.md](https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md): threads, devices, jailer, host hardening.
- [snapshot-support.md](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md), [network-for-clones.md](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/network-for-clones.md), [random-for-clones.md](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/random-for-clones.md): snapshot format, lazy loading, clone hazards.
- [CHANGELOG.md](https://github.com/firecracker-microvm/firecracker/blob/main/CHANGELOG.md), [issue 849](https://github.com/firecracker-microvm/firecracker/issues/849): PCI since 1.13, no GPU path.
- Agache et al., [Firecracker: Lightweight Virtualization for Serverless Applications](https://www.usenix.org/conference/nsdi20/presentation/agache), NSDI 2020.

Cloud Hypervisor
- [README](https://github.com/cloud-hypervisor/cloud-hypervisor), [device_model.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/device_model.md), [hotplug.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/hotplug.md), [threat-model.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/threat-model.md).
- [snapshot_restore.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/snapshot_restore.md): `copy`, `ondemand`, and `copyonwrite` restore modes.
- Release notes for [v52.0](https://github.com/cloud-hypervisor/cloud-hypervisor/releases/tag/v52.0) and [v53.0](https://github.com/cloud-hypervisor/cloud-hypervisor/releases/tag/v53.0): CVE-2026-45782, clock advance on restore, background prefault.

gVisor
- [Architecture guide](https://gvisor.dev/docs/architecture_guide/intro/), [platforms](https://gvisor.dev/docs/architecture_guide/platforms/), [security model](https://gvisor.dev/docs/architecture_guide/security/), [filesystem](https://gvisor.dev/docs/user_guide/filesystem/), [checkpoint/restore](https://gvisor.dev/docs/user_guide/checkpoint_restore/), [GPU](https://gvisor.dev/docs/user_guide/gpu/).
- [Systrap announcement](https://gvisor.dev/blog/2023/04/28/systrap-release/), [users](https://gvisor.dev/users/), [Tencent case study](https://gvisor.dev/blog/2026/04/23/scaling-agentic-rl-sandboxes-to-the-millions-with-gvisor-at-tencent/).
- Young et al., [The True Cost of Containing: A gVisor Case Study](https://www.usenix.org/conference/hotcloud19/presentation/young), HotCloud 2019.

Foundations
- [The Definitive KVM API Documentation](https://docs.kernel.org/virt/kvm/api.html) and Josh Triplett's [Using the KVM API](https://lwn.net/Articles/658511/), LWN 2015.
- [Intel SDM volume 3C](https://www.intel.com/content/www/us/en/developer/articles/technical/intel-sdm.html), [virtio 1.3](https://docs.oasis-open.org/virtio/virtio/v1.3/virtio-v1.3.html), [Linux x86 boot protocol](https://docs.kernel.org/arch/x86/boot.html).
- Ben-Yehuda et al., [The Turtles Project](https://www.usenix.org/conference/osdi10/turtles-project-design-and-implementation-nested-virtualization), OSDI 2010.
- [Docker seccomp profile](https://docs.docker.com/engine/security/seccomp/), [CVE-2022-0847](https://nvd.nist.gov/vuln/detail/CVE-2022-0847), [CVE-2024-1086](https://nvd.nist.gov/vuln/detail/CVE-2024-1086), [runc CVE-2024-21626](https://github.com/opencontainers/runc/security/advisories/GHSA-xr7r-f8xq-vfvv), [VENOM CVE-2015-3456](https://nvd.nist.gov/vuln/detail/CVE-2015-3456).

Landscape
- [e2b-dev/runtime](https://github.com/e2b-dev/runtime), [Vercel Sandbox](https://vercel.com/docs/vercel-sandbox/concepts), [Fly.io](https://fly.io/learn/firecracker-vm.md), [CodeSandbox SDK](https://codesandbox.io/blog/joining-together-ai-introducing-codesandbox-sdk), [Modal sandboxes](https://modal.com/docs/guide/sandboxes), [Northflank](https://northflank.com/blog/kata-containers-vs-gvisor), [kernel-images](https://github.com/kernel/kernel-images), [Azure dynamic sessions](https://learn.microsoft.com/azure/container-apps/sessions), [agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox), [E2B Series A](https://e2b.dev/blog/series-a).
