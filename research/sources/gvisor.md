# gVisor source notes (accessed 2026-10-13)

Legend: [official] = gvisor.dev / google/gvisor / Google Cloud docs. [3p] = third party. UNVERIFIED = could not confirm from a primary source.

## 1. Releases, cadence, notable changes
- [official] Latest release: `release-20261005.0`, published 2026-10-08. Previous: 20260928.0, 20260921.0, 20260914.0, 20260907.0. https://github.com/google/gvisor/releases
- [official] Cadence: roughly weekly, with tags named by date. Derived from the release list above. https://github.com/google/gvisor/releases
- [official] Systrap platform was announced 2023-04-28 and planned to "replace the ptrace platform by September 2023". https://gvisor.dev/blog/2023/04/28/systrap-release/
- [official] "systrap replaced ptrace as the default gVisor platform in mid-2023". ptrace is "no longer supported and is expected to be removed". https://gvisor.dev/docs/architecture_guide/platforms/
- [official] Rootfs overlay became the runsc default in May 2023. https://gvisor.dev/blog/2023/05/08/rootfs-overlay/
- [official] Directfs was announced in June 2023 and is enabled by default (`--directfs=false` turns it off). https://gvisor.dev/blog/2023/06/27/directfs/ , https://gvisor.dev/docs/user_guide/filesystem/
- [official] Checkpoint/restore additions include background restore (`runsc restore --background`), `--exclude-committed-zero-pages`, uncompressed images with parallel restore, and GPU checkpoint via `cuda-checkpoint`. https://gvisor.dev/docs/user_guide/checkpoint_restore/
- [official] 2026-04-15: the MAGI demo ran 3 AI agents, Ollama on 8x L4, a browser (Camofox), Playwright/Firecrawl and Docker-in-gVisor, each component in its own sandbox. It claims sandboxes "start and stop in milliseconds". https://gvisor.dev/blog/2026/04/15/magi-multi-agent-gvisor-isolation/
- [official] 2026-04-23: Tencent runs "millions" of gVisor sandboxes per day for agentic-RL training. It ran 74,379 runc-vs-runsc comparisons, and incompatibilities hit about 1.7% of test cases. It merged more than 10 upstream fixes. No startup numbers are given. https://gvisor.dev/blog/2026/04/23/scaling-agentic-rl-sandboxes-to-the-millions-with-gvisor-at-tencent/
- [official] 2026-09-17: systemd and full Linux desktop apps (GNOME, Google Chrome) run in gVisor. Requires `--in-sandbox-cgroup=v2 --allow-suid`. https://gvisor.dev/blog/2026/09/17/systemd-in-gvisor/
- [official] 2026-10-02: gVisor is being donated to CNCF. Application filed 2026-09-07 and accepted 2026-09-28. It enters at Sandbox level. Non-Google maintainers get merge rights, governance moves to org-based voting, and the repo will leave the `google` GitHub org. https://gvisor.dev/blog/2026/10/02/gvisor-cncf/

## 2. Architecture (for teaching)
- [official] gVisor is an "application kernel": a Linux-compatible kernel that runs as a normal userspace process (the Sentry, written in Go). It implements the syscall API itself instead of passing calls to the host kernel. https://github.com/google/gvisor/blob/master/README.md
- [official] Security design, principle 1: the Sentry intercepts the app's syscalls and implements them. Principle 2: the Sentry's own host syscalls are limited to a minimal, explicitly enumerated set (FD dup/close, synchronization, timers, signals, ...). https://gvisor.dev/docs/architecture_guide/security/
- UNVERIFIED: the exact number of host syscalls allowed by the Sentry's seccomp filter. The security doc gives no number, and older talks cite roughly 50-70. Check `runsc/boot/filter/config` in the repo before quoting a figure. https://gvisor.dev/docs/architecture_guide/security/
- [official] Gofer: a separate process, isolated from the sandbox, that owns the container filesystem and talks to the Sentry over the LISAFS protocol. The sandbox mount namespace is always empty. https://gvisor.dev/docs/user_guide/filesystem/
- [official] Directfs: the gofer donates mount-point FDs to the Sentry, so file ops skip the gofer RPC round trip. Seccomp enforces `O_NOFOLLOW`. With directfs off, seccomp is stricter and the Sentry has fewer capabilities ("more secure but slower"). https://gvisor.dev/docs/user_guide/filesystem/
- [official] Rootfs overlay: a writable tmpfs layer inside the Sentry sits over a read-only rootfs. The default is self-backed (`--overlay2=root:self`): file data goes to a hidden file in the container rootfs and counts toward K8s ephemeral storage. Memory-backed (`--overlay2=all:memory`) can "bloat up container memory usage". `--overlay2=none` sends writes to the host. https://gvisor.dev/docs/user_guide/filesystem/
- [official] Platforms (how syscalls get trapped):
  - systrap (default) uses seccomp `SECCOMP_RET_TRAP`, and it is the better choice inside a VM or without HW virtualization. https://gvisor.dev/docs/architecture_guide/platforms/
  - KVM needs virtualization extensions. It is best on bare metal, and nested virtualization is feasible but often slower than systrap. https://gvisor.dev/docs/architecture_guide/platforms/
  - ptrace uses `PTRACE_SYSEMU` and is deprecated. https://gvisor.dev/docs/architecture_guide/platforms/
- [official] Systrap mechanism: ptrace is used only to start stub threads. A seccomp filter makes each app syscall raise SIGSYS. The signal handler writes the request into memory shared with the Sentry, which services it. On x86_64, the common `mov; syscall` pattern is rewritten into a jump to a trampoline, which skips the seccomp/signal cost. arm64 lacks this optimization. https://gvisor.dev/blog/2023/04/28/systrap-release/
- [official] Netstack: a userspace TCP/IP stack inside the Sentry (default `--network=sandbox`). Loopback stays in the sandbox. `--network=host` passes networking through to the host stack for performance but "decreases the isolation". `--network=none` leaves only loopback. https://gvisor.dev/docs/user_guide/networking/
- [official] Egress control hooks: `--network-proxy-path` (experimental) sends IP packets over a SOCK_SEQPACKET to an external proxy that does egress filtering, and TBF qdisc does egress shaping. Both are useful for per-agent network policy. https://gvisor.dev/docs/user_guide/networking/
- [official] runsc is an OCI runtime used via Docker (`--runtime=runsc`), containerd, and Kubernetes RuntimeClass. GKE Sandbox, Cloud Run and App Engine are built on it. https://gvisor.dev/docs/user_guide/checkpoint_restore/ , https://gvisor.dev/users
- [official] Kubernetes SIG Apps "agent-sandbox" provides a `Sandbox` CRD, `SandboxWarmPool` and `SandboxClaim` for pre-warmed sandboxes. It delegates isolation to gVisor or Kata via RuntimeClass (about 4.2k stars). https://github.com/kubernetes-sigs/agent-sandbox

## 3. Performance
- [official] The performance guide gives numbers only in charts and CSVs, not text. ptrace has "the highest structural costs by far", KVM has low trap overhead but "runs poorly with nested virtualization", and systrap is recommended. https://gvisor.dev/docs/architecture_guide/performance/
- [official] Startup: "most of the time overhead ... is associated [with] Docker itself". Use `runsc do` or call the OCI runtime directly to avoid it. No absolute startup number appears in the text. https://gvisor.dev/docs/architecture_guide/performance/
- [official] File I/O: raw disk I/O has no significant fundamental overhead, tmpfs is bound by memory copies, and small-file serving (Apache 100k files) is "predictably poor" because of VFS costs. Network is "mostly bound by implementation costs". https://gvisor.dev/docs/architecture_guide/performance/
- [official] Systrap is faster than ptrace on the getpid microbenchmark, and the trampoline-optimized version is faster still. Numbers appear only in figures. https://gvisor.dev/blog/2023/04/28/systrap-release/
- [3p, 2019, ptrace/KVM era, pre-systrap/directfs] HotCloud'19 "The True Cost of Containing":
  - simple syscalls ≥2.2x slower than native
  - opening files on external tmpfs 216x slower
  - reading small files 11x slower
  - downloading large files 2.8x slower
  - KVM-mode host calls 9x slower, Gofer opens 72x slower, ptrace latencies 42-232x
  - startup comparable between KVM and ptrace modes

  https://www.usenix.org/conference/hotcloud19/presentation/young
- [3p, 2020] VEE'20 "Blending Containers and VMs":
  - gVisor had the lowest network bandwidth, 0.805 Gbps with netstack (3.03 Gbps with host networking), versus Host/Firecracker highest
  - both gVisor and Firecracker execute substantially more host kernel code than native
  - "neither is best for all workloads"

  https://pages.cs.wisc.edu/~swift/papers/vee20-isolation.pdf
- Caveat: both papers predate systrap (2023), directfs (2023) and the rootfs overlay. The gVisor docs and blogs give no newer independent 2023-2026 numbers in text form. UNVERIFIED: independent post-systrap benchmarks.
- [official] Third-party adopter claim: Cloudflare says "It takes just a few seconds for a new gVisor container to start up" (Pages builds). https://gvisor.dev/users

## 4. Checkpoint/restore
- [official] Usage is `runsc checkpoint --image-path=<p> <id>` (optional `--leave-running`), then `runsc create <id>` and `runsc restore --image-path=<p> <id>`. Restore goes into a new container. https://gvisor.dev/docs/user_guide/checkpoint_restore/
- [official] Docker flow is `docker checkpoint create <c> <ckpt>` then `docker start --checkpoint <ckpt> <c>`. Docker cannot restore into a different container (Moby #37344). https://gvisor.dev/docs/user_guide/checkpoint_restore/
- [official] Speed knobs:
  - `--background` restore starts the app once kernel state loads ("Time to First Instruction"), and requires `--compression=none`
  - `--exclude-committed-zero-pages` shrinks images
  - uncompressed images enable parallel kernel and memory restore

  https://gvisor.dev/docs/user_guide/checkpoint_restore/
- [official] Limitations:
  - with host networking, connected sockets get ECONNRESET after restore and backlog connections are lost
  - the target host needs all source CPU features (mask them with the `dev.gvisor.internal.cpufeatures` annotation)
  - GPU checkpoint uses `cuda-checkpoint` and is not supported on arm64

  https://gvisor.dev/docs/user_guide/checkpoint_restore/
- [official] GKE Pod snapshots save memory and filesystem to Cloud Storage and require GKE Sandbox (gVisor).
  - GKE Sandbox kernel restore "typically takes a few seconds", with app memory lazily loaded in the background, which causes brief latency on first touch
  - the restored Pod gets a new IP and active connections are closed
  - no E2 (whole-pod scope), no MIG, no TPU, no GCS FUSE sidecar
  - GPU state uses cuda-checkpoint and raises memory use during snapshot and restore

  https://docs.cloud.google.com/kubernetes-engine/docs/concepts/pod-snapshots
- [official] Agent Sandbox + Pod snapshots how-to (the `PodSnapshotSandboxClient` freezes and rehydrates a gVisor sandbox). https://docs.cloud.google.com/kubernetes-engine/docs/how-to/agent-sandbox-pod-snapshots , https://agent-sandbox.sigs.k8s.io/docs/sandbox/snapshots/
- [3p] Pod snapshots were announced around 2025-11-11 (KubeCon NA). https://siliconangle.com/2025/11/11/google-debuts-new-open-source-ai-tools-gke-pod-snapshots/
- [3p, citing Google] Pod snapshots went GA in May 2026 (GKE 1.35.3-gke.1234000+). A 70B model restores in 37 s and an 8B in 15 s, with startup latency up to 89% lower. Codeway went from about 1 min to 8 s. https://infoq.com/news/2026/09/gke-pod-snapshots-benchmarks
- UNVERIFIED: how restore time scales with memory size. Lazy background loading suggests time-to-first-instruction is mostly independent of memory size, while full restore scales with image size. No published curve was found.

## 5. Compatibility
- [official] gVisor implements "a large portion" of the Linux syscall surface but not all of it. Most missing calls have supported alternatives. https://gvisor.dev/docs/user_guide/compatibility/
- [official] io_uring is disabled by default and supports only basic ops when enabled. nftables support is limited, and iptables is partial ("enough to run Docker"). https://gvisor.dev/docs/user_guide/compatibility/
- [official] In-sandbox cgroups do accounting only and do not enforce limits between processes in a sandbox. Use host cgroups to enforce limits. https://gvisor.dev/docs/user_guide/compatibility/
- [official] Docker-in-gVisor is used in the MAGI demo for code execution and runs under systemd per the 2026-09 post. https://gvisor.dev/blog/2026/04/15/magi-multi-agent-gvisor-isolation/ , https://gvisor.dev/blog/2026/09/17/systemd-in-gvisor/
- [official] systemd works in gVisor (Ubuntu 26.04, Debian, UBI 10) with `--in-sandbox-cgroup=v2 --allow-suid`. The guest reports kernel `4.19.0-gvisor`. https://gvisor.dev/blog/2026/09/17/systemd-in-gvisor/
- [official] Chrome: Google Chrome browses the web inside a GNOME desktop in gVisor. The 2026-09 post does not say whether Chrome's own sandbox runs or whether `--no-sandbox` was used. MAGI runs the Camofox browser and Playwright (Firecrawl) in gVisor. https://gvisor.dev/blog/2026/09/17/systemd-in-gvisor/ , https://gvisor.dev/blog/2026/04/15/magi-multi-agent-gvisor-isolation/
- UNVERIFIED: whether Chrome's namespace or seccomp-bpf sandbox works under gVisor without `--no-sandbox`. No official guidance or open issue was found in a GitHub search for "chrome", "chromium" and "puppeteer". Test this directly. https://github.com/google/gvisor/issues?q=chromium
- [official] GPU (nvproxy):
  - supports CUDA (PyTorch, LLMs), Vulkan and NVENC/NVDEC
  - officially supports T4, A100, A10G, L4 and H100, with a rolling window of driver versions (strict match)
  - exposes only compute and utility capabilities by default
  - no MIG
  - cannot protect against bugs in the NVIDIA driver ioctls it passes through

  https://gvisor.dev/docs/user_guide/gpu/
- UNVERIFIED (not in compatibility page text): FUSE (a separate guide exists), eBPF, inotify, and user namespaces. https://gvisor.dev/docs/user_guide/compatibility/

## 6. Security model
- [official] The threat model groups vectors as System API, System ABI, side channels and other. gVisor mainly minimizes the System API vector. It relies on the host/platform for hardware attacks, on host cgroups for DoS, and side channels are largely out of scope. https://gvisor.dev/docs/architecture_guide/security/
- [official] Two layers: (1) a guest exploit first hits the Sentry, not the host kernel. (2) A compromised Sentry is still confined by seccomp to a small host syscall set, plus empty mount namespace and gofer-mediated FS. An escape therefore needs a Sentry bug and then a host-kernel bug reachable through the allowed syscalls. https://gvisor.dev/docs/architecture_guide/security/ , https://gvisor.dev/docs/user_guide/filesystem/
- [official] VM comparison: a VM replaces host interaction with a guest OS plus virtual hardware implemented by a VMM. The Sentry implements the System API directly. Per the docs, "neither approach is inherently more secure", and the trade-offs are performance, efficiency and compatibility. https://gvisor.dev/docs/architecture_guide/security/
- [official] Directfs widens the Sentry's host syscall surface (relative to directfs off). nvproxy and `--network=host` also widen attack surface. https://gvisor.dev/docs/user_guide/filesystem/ , https://gvisor.dev/docs/user_guide/gpu/ , https://gvisor.dev/docs/user_guide/networking/
- UNVERIFIED: CVE history highlights. Not researched to primary sources in this pass. Check https://github.com/google/gvisor/security/advisories

## 7. Host requirements
- [official] systrap needs only seccomp `SECCOMP_RET_TRAP`, not KVM. It is the recommended platform inside VMs or without virtualization support, so it runs on ordinary cloud VMs without nested virtualization. https://gvisor.dev/docs/architecture_guide/platforms/
- [official] Tencent picked gVisor partly because it "can run inside regular VMs" while microVMs need bare metal, which is cheaper at scale. https://gvisor.dev/blog/2026/04/23/scaling-agentic-rl-sandboxes-to-the-millions-with-gvisor-at-tencent/
- [official] Northflank uses gVisor for GPU sandboxes "when nested virtualization is unavailable". https://gvisor.dev/users
- [official] Networking note: kernels older than 4.14.77 need `--gso=false`. https://gvisor.dev/docs/user_guide/networking/
- UNVERIFIED: an explicit minimum host kernel version (not stated on the platforms page) and an explicit host cgroup v1/v2 support statement.

## 8. Adopters (official users page unless noted) https://gvisor.dev/users
- Google: designed gVisor, "millions of sandbox instances daily", powers GKE Sandbox, Cloud Run and App Engine. https://gvisor.dev/users
- Anthropic: a regular contributor that uses gVisor "to contain code execution within claude.ai". https://gvisor.dev/users
- OpenAI: uses it for "some higher-risk tasks", e.g. code execution. https://gvisor.dev/users
- Modal: compute jobs are containerized and virtualized with gVisor. https://gvisor.dev/users
- Cloudflare (Pages builds), DigitalOcean (App Platform), Snowflake (Snowpark UDF sandbox), Ant Group, Beam (AI-agent sandboxes with snapshots and GPU), 3B (single-use sandboxes via checkpoint/restore), Deductive AI (agent code sandboxes plus Cilium), Northflank, Blink, Grist, Freedom of the Press (Dangerzone). Docker (Docker Desktop) and Tailscale use the netstack library only. https://gvisor.dev/users
- Tencent: agentic-RL sandboxes at millions per day. https://gvisor.dev/blog/2026/04/23/scaling-agentic-rl-sandboxes-to-the-millions-with-gvisor-at-tencent/
- Kubernetes SIG Apps agent-sandbox: supports gVisor via RuntimeClass. https://github.com/kubernetes-sigs/agent-sandbox
- UNVERIFIED: Cloud Functions. It is not named on the users page.

## 9. Maintenance signals
- Stars: 19,655. License: Apache-2.0. Last push: 2026-10-13. https://github.com/google/gvisor
- About 394 contributors (GitHub API contributors list including anonymous). https://api.github.com/repos/google/gvisor/contributors
- 2,122 commits on the default branch from 2026-01-01 to 2026-10-13 (GitHub API). https://github.com/google/gvisor/commits
- Moving to CNCF (Sandbox), with the repo set to leave the `google` org. https://gvisor.dev/blog/2026/10/02/gvisor-cncf/
