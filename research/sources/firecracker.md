# Firecracker: source notes (accessed 2026-10-13)

Abbreviations: GH = https://github.com/firecracker-microvm/firecracker/blob/main ; PAPER = https://www.usenix.org/system/files/nsdi20-paper-agache.pdf (NSDI 2020, Agache et al.). All official unless marked [3rd-party].

## 1. Releases, cadence, 2024-2026 changes

- Latest release: **v1.17.0, 2026-09-10** (v1.16.2 patch released the same day). https://github.com/firecracker-microvm/firecracker/releases/tag/v1.17.0
- Minor release dates: v1.6.0 2023-12-20, v1.7.0 2024-03-18, v1.8.0 2024-07-10, v1.9.0 2024-09-03, v1.10.0 2024-11-07, v1.11.0 2025-03-18, v1.12.0 2025-05-07, v1.13.0 2025-08-28, v1.14.0 2025-12-17, v1.15.0 2026-03-09, v1.16.0 2026-06-04, v1.17.0 2026-09-10. A minor release lands about every 2-4 months, with patch releases backported to the previous minor. https://github.com/firecracker-microvm/firecracker/releases
- v1.6.0: vhost-user block frontend as a **developer preview**. GH/CHANGELOG.md
- v1.8.0 (2024-07): **ACPI** for x86_64 guests and the **VMGenID** device on x86_64. GH/CHANGELOG.md
- v1.9.0: VMGenID on ARM via DeviceTree (needs mainline Linux 6.10+). GH/CHANGELOG.md
- v1.10.0: GDB debugging support. GH/CHANGELOG.md
- v1.12.0: ARM Graviton4 supported. GH/CHANGELOG.md
- v1.13.0 (2025-08): **optional PCI support** via `--enable-pci`. With the flag, all virtio devices use a PCI transport; without it, MMIO. Intel Skylake dropped from official testing. GH/CHANGELOG.md
- v1.14.0 (2025-12): **virtio-pmem**, **memory hotplug via virtio-mem**, and balloon free-page reporting/hinting (reporting is a developer preview). GH/CHANGELOG.md
- v1.15.0 (2026-03): **VMClock** device, which exposes `vm_generation_counter` to guest userspace on snapshot restore. GH/CHANGELOG.md
- v1.16.0 (2026-06): **developer-preview PCI device hotplug/unplug** (block, pmem, net); the guest must rescan the PCI bus by hand. Also host kernel 6.18 support and a pmem rate limiter. GH/CHANGELOG.md ; GH/docs/device-hotplug.md
- v1.16.1: guest kernel 6.18 support. GH/docs/kernel-policy.md
- v1.17.0 (2026-09): **bzImage boot on x86_64** in addition to ELF vmlinux, a `huge_pages` option on snapshot load, `sync_snapshot_files` (async snapshot writeback), and several virtio-mem fixes. GH/CHANGELOG.md
- Kernel policy: each kernel version is supported for at least 2 years, with at least 2 host and 2 guest versions at any time. Host kernels: 5.10, 6.1, 6.18 (6.18 until at least 2028-05-28). Guest kernels: 5.10, 6.1 (minimum end 2026-09-02), 6.18 (until at least 2028-06-01). Tested guest kernels are Amazon Linux `microvm-kernel-*` builds. GH/docs/kernel-policy.md
- Doc inconsistency: kernel-policy.md still says "Firecracker does not support PCI devices", while the CHANGELOG says PCI transport exists since v1.13. The CHANGELOG is the more recent source. GH/docs/kernel-policy.md

## 2. Architecture

- One Firecracker process runs exactly one microVM. Threads: an **API** thread (HTTP server, control plane, "never in the fast path"), a **VMM** thread (machine model, legacy devices, MMDS, virtio net/block/vsock emulation, rate limiting, all on one epoll event loop), and **one vCPU thread per guest core** that runs the `KVM_RUN` loop and handles synchronous PIO/MMIO exits. GH/docs/design.md
- Pausing stops the VMM event loop and the vCPUs; it only serves API calls. GH/docs/design.md
- KVM provides the in-kernel PIC, IOAPIC and PIT. Firecracker adds virtio devices, a serial console, and a partial i8042 keyboard controller that is used only for guest reset (Ctrl-Alt-Del). GH/docs/design.md
- FAQ: "only 6 emulated devices": virtio-net, virtio-balloon, virtio-block, virtio-vsock, serial, keyboard controller. The FAQ is older; the current device API also lists an `EntropyDevice` (virtio-rng), pmem, memory hotplug (virtio-mem), VMGenID and VMClock. GH/FAQ.md ; GH/docs/device-api.md ; GH/docs/entropy.md
- virtio-rng: at most one device, which takes its bytes from aws-lc-rs and can be rate-limited. GH/docs/entropy.md ; GH/docs/snapshotting/random-for-clones.md
- Transport: virtio-mmio by default, PCI optional since v1.13 (`--enable-pci`). GH/CHANGELOG.md
- Boot on x86_64: direct kernel load with no BIOS or bootloader. It loads an uncompressed ELF `vmlinux` (recommended) or, since v1.17, a `bzImage`, which costs extra boot time and memory to decompress. aarch64 uses a PE `Image`. PVH direct boot is used if the kernel has the XEN_ELFNOTE_PHYS32_ENTRY note. initrd is optional. GH/docs/rootfs-and-kernel-setup.md ; GH/docs/pvh.md ; GH/docs/initrd.md
- Network: virtio-net backed by a **host TAP** device. Firecracker does **not** filter traffic; all guest egress is untrusted and must be filtered on the host. GH/docs/design.md
- Block: virtio-block backed by a pre-formatted host file. GH/docs/design.md
- Rate limiters: per drive and per NIC, as two token buckets (ops/s and bandwidth) with size, refill time and one-time burst. GH/docs/design.md
- MMDS: a guest-reachable metadata store whose content the user configures through the API. The default IPv4 address is 169.254.169.254 and is configurable (examples use 169.254.170.2). There are V1 and V2 (token-based) versions. GH/docs/design.md ; GH/docs/mmds/mmds-user-guide.md
- CPU templates: static and custom templates mask the CPUID, MSRs and ARM registers exposed to the guest, so a fleet with mixed CPUs looks uniform (this also matters for snapshot portability). GH/docs/design.md ; GH/docs/cpu_templates/cpu-templates.md
- Clocksources: x86_64 offers kvm-clock and tsc; aarch64 offers arch_sys_counter. GH/docs/design.md
- Host and guest communicate over vsock: guest AF_VSOCK is mapped to host Unix domain sockets. GH/docs/vsock.md

## 3. Security

- Threat model: all vCPU threads are assumed to run malicious code as soon as they start. Isolation comes from nested trust zones, from the guest out to the host. GH/docs/design.md
- The **jailer** is the required launcher in production. It validates paths, creates a chroot, joins a netns (`--netns`) and optionally a new PID namespace (`--new-pid-ns`), sets cgroups v1/v2 (`--cgroup`, `--parent-cgroup`) and resource limits, then drops to an unprivileged `--uid`/`--gid` and execs Firecracker. The jailer trusts all of its inputs. GH/docs/jailer.md ; GH/docs/prod-host-setup.md
- **seccomp** filters are on by default and installed per thread (API thread before its HTTP server starts, VMM and vCPU threads before guest code runs). The filters are compiled into the binary from `resources/seccomp`. Using `--no-seccomp` or custom filters in production is discouraged. GH/docs/seccomp.md ; GH/docs/prod-host-setup.md
- Production host recommendations: **disable SMT**, **disable KSM**, **disable swap** (or use secure swap), use RAM with Rowhammer mitigation, keep microcode and kernels patched, check with spectre-meltdown-checker, block guest access to the host IMDS with iptables, and handle the iTLB multihit huge-page issue. GH/docs/prod-host-setup.md
- Balloon caveat: the guest driver is untrusted, so balloon limits can be broken by a compromised guest. GH/docs/ballooning.md
- The serial console is not exposed in production builds. GH/docs/design.md

## 4. Official performance numbers

SPECIFICATION.md conditions: m5d.metal (HT disabled) and m6g.metal, enough free host resources, 1 vCPU and 128 MiB guest, Firecracker-tuned kernel. GH/SPECIFICATION.md
- VMM start up to API socket ready: **8 CPU ms**. Wall clock ranges from 6 to 60 ms, typically about 12 ms. GH/SPECIFICATION.md
- VMM thread memory overhead: **<= 5 MiB**. This excludes the MMDS store, and workloads with many vsock connections use more. GH/SPECIFICATION.md
- **<= 125 ms** from the InstanceStart API call to guest `/sbin/init`, with the serial console disabled and a minimal kernel and rootfs (enforced in CI). GH/SPECIFICATION.md
- Guest CPU at more than 95% of bare metal; network up to 14.5 Gbps at <= 80% of one host core, 25 Gbps at 100%; +0.06 ms latency; storage up to 1 GiB/s at <= 70% of one core. All of these are marked "integration test pending". GH/SPECIFICATION.md
- Creation rate: "5 microVMs per host core per second", for example 180/s on a 36-core host (1 vCPU, 128 MiB, minimal kernel). GH/docs/design.md
- Snapshot restore: the docs give **no official latency number**, only "optimized for speed" and that cost depends on memory size, vCPU count and device count. Restore is much slower under cgroups v1, so v2 is recommended. Any "~X ms restore" figure is third-party or UNVERIFIED. GH/docs/snapshotting/snapshot-support.md
- Paper numbers are in section 10.

## 5. Snapshot/restore

- A snapshot is a **vmstate file** (device, vCPU and KVM state, versioned MAJOR.MINOR.PATCH and CRC-checked) plus a **guest memory file**. Disk files are not included; the user must handle them. GH/docs/snapshotting/snapshot-support.md
- **Full** snapshots write all guest memory, which faults in every page. **Diff** snapshots write only dirty pages, using either `track_dirty_pages` (KVM dirty log) or mincore. Diff snapshots must be merged onto a base, except diffs of freshly booted VMs. Diff is still a **developer preview**. GH/docs/snapshotting/snapshot-support.md
- Memory backends on load: **File**, where the memory file is mapped `MAP_PRIVATE` and pages load on demand with copy-on-write into anonymous memory (lazy loading is why restore is fast), or **Uffd**, where a user-supplied page-fault handler process gets a userfaultfd and serves pages, for example from a network or a prefetch list. Uffd needs `/dev/userfaultfd` on host 6.1+. GH/docs/snapshotting/snapshot-support.md ; GH/docs/snapshotting/handling-page-faults-on-snapshot-resume.md
- Constraints: the Firecracker binary must support the snapshot's format version. Restoring across GIC versions on ARM is not possible. Use CPU templates for cross-CPU portability. Taking a snapshot during early boot can crash the VM on restore. GH/docs/snapshotting/snapshot-support.md ; GH/docs/snapshotting/versioning.md
- Network for clones: clones keep the original TAP name and guest IP. The documented workaround is one **network namespace per clone** plus iptables NAT; the docs say this is "just an example", not a performant or secure setup. Network and vsock packet loss is expected across restore, vsock connections are reset, and listening sockets survive. GH/docs/snapshotting/network-for-clones.md ; GH/docs/snapshotting/snapshot-support.md
- Uniqueness: restoring one snapshot more than once is considered **insecure by default**, because IDs, RNG state and tokens get duplicated. **VMGenID** is always enabled: on restore Firecracker changes a 16-byte ID and notifies the guest, and Linux 5.18+ (ACPI) or 6.10+ (DeviceTree) reseeds its kernel PRNG. Userspace state such as caches, cached randomness and tokens is still duplicated. GH/docs/snapshotting/snapshot-support.md ; GH/docs/snapshotting/random-for-clones.md
- **VMClock** (since v1.15): userspace can mmap or poll `vm_generation_counter` to detect a restore. It needs the Linux v7.0 patches, which are backported to the AL microvm 5.10 and 6.1 kernels. GH/docs/snapshotting/snapshot-support.md
- Clocks: the guest wall clock resumes at snapshot time and must be resynced in the guest. On x86 kvm-clock, `clock_realtime: true` on LoadSnapshot jumps the clock forward (host 5.16+). GH/docs/snapshotting/snapshot-support.md
- `track_dirty_pages` is not persisted and must be set again on load. Snapshots can be large, so integrators must enforce disk quotas. GH/docs/snapshotting/snapshot-support.md

## 6. Limitations relevant to agent sandboxes

- **No GPU or VFIO passthrough.** Maintainers said passthrough pins memory, which breaks oversubscription, and that one tenant per GPU is impractical: "no known path to supporting GPUs" (issue closed). The CHANGELOG has no VFIO entry through v1.17. https://github.com/firecracker-microvm/firecracker/issues/849 ; GH/CHANGELOG.md
- **No virtio-fs or 9p.** Host data goes in through block devices, pmem (DAX possible), vhost-user-block (preview) or vsock. No virtio-fs device appears in the device API docs. GH/docs/device-api.md ; GH/docs/pmem.md
- Up to **32 vCPUs**. Memory is any size (no documented cap found; UNVERIFIED). CPU and memory oversubscription is allowed. GH/docs/design.md
- Memory return to the host goes through the balloon (cooperative, untrusted guest, `deflate_on_oom`) or virtio-mem hotplug (x86 guest kernel 5.16+; slots protected from the guest). GH/docs/ballooning.md ; GH/docs/memory-hotplug.md
- No live migration feature; only snapshot/restore. No live-migration API in the docs (UNVERIFIED as an explicit statement).
- No nested virtualization inside the guest; CPU templates disable SVM/VMX (for example, the T2A fix). GH/CHANGELOG.md
- Guests are Linux only (plus OSv). The host is Linux with KVM. Tested on bare-metal EC2 `.metal` instances (Intel Cascade Lake through Granite Rapids, AMD Milan/Genoa, Graviton 2-5). Running on nested-virt cloud VMs is not in the tested matrix. GH/FAQ.md ; GH/README.md
- Needs a guest kernel with the recommended configs (virtio-mmio or PCI, ACPI on x86); the stock distro kernel is not the tested path. GH/docs/kernel-policy.md
- No host filesystem sharing and no GPU means a headless Chromium over CDP must live entirely in the rootfs and reach the controller over TAP or vsock (inference, not a documented statement).

## 7. Artifacts and setup

- You need: a kernel image (vmlinux/bzImage/Image), an ext4 (or other) rootfs image with an init, and then either API calls over a Unix socket (`/boot-source`, `/drives`, `/network-interfaces`, `/machine-config`, `/actions InstanceStart`) or a `--config-file` JSON with `--no-api`. GH/docs/getting-started.md ; GH/FAQ.md
- Kernel: start from `resources/guest_configs/`, `make vmlinux`, or run `./tools/devtool build_ci_artifacts kernels 6.1`. Rootfs: any filesystem image with an init, for example ext4 built from a container image. GH/docs/rootfs-and-kernel-setup.md
- Default boot args in the getting-started guide: `console=ttyS0 reboot=k panic=1` (add `keep_bootcon` in some setups). Removing the serial console is the fast-boot condition used in SPECIFICATION. GH/docs/getting-started.md ; GH/SPECIFICATION.md
- The paper recommends a minimal kernel config (only virtio and serial drivers) and no serial logging. Decompression adds about 40 ms; a stock distro kernel took about 900 ms. PAPER §5.1

## 8. Ecosystem and maintenance

- firecracker: 37.3k stars, 2.67k forks, last push 2026-10-09, about 884 commits on main since 2026-01-01, about 1001 commits in the last 52 weeks, 100+ contributors. https://github.com/firecracker-microvm/firecracker (via GitHub API)
- firecracker-containerd: 2.96k stars, active (push 2026-09-25). https://github.com/firecracker-microvm/firecracker-containerd
- firecracker-go-sdk: 675 stars, last push 2026-02-10. https://github.com/firecracker-microvm/firecracker-go-sdk
- Flintlock (liquidmetal-dev): 1.5k stars, active (2026-10-09). https://github.com/liquidmetal-dev/flintlock
- Weave Ignite: **archived** (last push 2023-12). https://github.com/weaveworks/ignite
- Kata Containers: 8.9k stars, active, supports Firecracker as one VMM backend. https://github.com/kata-containers/kata-containers
- E2B open-source backend (the repo is now `e2b-dev/runtime`; `e2b-dev/infra` redirects there): "Firecracker microVMs that resume from a snapshot", with a per-sandbox netns, cgroup and nftables egress firewall; E2B Embed runs on one KVM host. 1.7k stars. https://github.com/e2b-dev/runtime

## 9. Adopters

- AWS Lambda and Fargate (official): "millions of production workloads, and trillions of requests per month". PAPER ; GH/FAQ.md
- AWS Bedrock AgentCore Runtime: AWS docs say each session gets a dedicated microVM, terminated and memory-sanitized at the end, but do not name Firecracker. https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/runtime-sessions.html. Marc Brooker (AWS) names Firecracker in "Seven Years of Firecracker": "Each session with the agent is given its own MicroVM", and credits in-place CPU and memory grow/shrink. https://brooker.co.za/blog/2025/09/18/firecracker
- Aurora DSQL query processors run in Firecracker (Brooker blog, AWS engineer). https://brooker.co.za/blog/2025/09/18/firecracker
- Fly.io: its own explainer covers Firecracker microVMs. https://fly.io/learn/firecracker-vm.md
- E2B: "every agent session runs in its own Firecracker microVM", with pause, snapshot and fork. https://e2b.dev/enterprise.md ; https://github.com/e2b-dev/runtime
- Vercel Sandbox: each sandbox is a Firecracker microVM booting Amazon Linux 2023, with snapshots. https://vercel.com/docs/vercel-sandbox/concepts
- [3rd-party] Koyeb is also mentioned as a user; UNVERIFIED from a primary source.

## 10. NSDI 2020 paper teaching points

- Requirements from Lambda: Isolation, Overhead and Density (thousands per host, initial target 10% overhead), Performance (near bare metal, consistent), Compatibility (unmodified Linux binaries), Fast Switching, Soft Allocation (overcommit). PAPER §2
- Why not containers: a shared kernel forces a choice between isolation and compatibility (seccomp profiles and similar). Why not QEMU: more than 1.4M LOC, up to 270 syscalls, plus about 120k LOC of KVM. Firecracker is about 50k LOC of Rust (96% fewer). It keeps KVM and replaces only the VMM. It was forked from crosvm (USB, GPU and other drivers removed), and Cloud Hypervisor shares code with it. PAPER §2.1, §3
- Claims: less than 5 MB overhead per VM, less than 125 ms to app code, up to 150 microVMs/s per host; in Lambda production since 2018. PAPER abstract/§1
- Evaluation setup: m5d.metal, 1 vCPU and 256 MB, Linux 4.14.94, minimal rootfs. Pre-configured Firecracker and Cloud Hypervisor boot about 2x faster than QEMU. In the 50-parallel test, p99 was 146 ms for Firecracker vs 158 ms for Cloud Hypervisor. Uncompressed kernel boot saves about 40 ms. PAPER §5.1
- Memory overhead: Firecracker about 3 MB, Cloud Hypervisor about 13 MB, QEMU about 131 MB per VM. PAPER §5.2
- Weak spot in 2020: block IO was serial, about 13k IOPS (52 MB/s at 4k) on hardware capable of 340k IOPS. Network was about 15 Gb/s vs 44 Gb/s on the host. PAPER §5.3
- Lambda keeps a small pool of pre-booted microVMs to hide boot latency; creation rate drives the economics. PAPER §5.1
