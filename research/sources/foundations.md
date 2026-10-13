# Foundations: teaching notes (virtualization, KVM, virtio, microVMs, containers, gVisor)

All URLs accessed 2026-10-13. "VERIFIED" = fetched and checked during this session. Items without that tag come from well-known primary sources but were not re-fetched this session; anything with a specific number not checked is marked UNVERIFIED.

---

## 1. Why virtualization needs hardware help

- Popek and Goldberg (1974): a machine is efficiently virtualizable if every *sensitive* instruction (one that reads or changes privileged machine state) is also *privileged* (traps when run outside kernel mode). Then the hypervisor can run the guest directly and only handle the traps ("trap-and-emulate"). https://dl.acm.org/doi/10.1145/361011.361073
- Classic x86 broke this rule: about 17 instructions (e.g. `POPF`, `SGDT`, `SMSW`) behave differently in ring 3 vs ring 0 without trapping. So early VMware used binary translation, and Xen used paravirtualization (guest modified to call the hypervisor). Robin and Irvine, USENIX Security 2000: https://www.usenix.org/legacy/events/sec2000/full_papers/robin/robin.pdf ; Adams and Agesen, ASPLOS 2006: https://dl.acm.org/doi/10.1145/1168857.1168860
- Intel VT-x (VMX) adds two modes: **VMX root** (hypervisor) and **VMX non-root** (guest). Each has its own rings 0-3, so the guest kernel really runs in ring 0, but in non-root mode. Intel SDM Vol. 3C, ch. "Introduction to Virtual Machine Extensions": https://www.intel.com/content/www/us/en/developer/articles/technical/intel-sdm.html
- **VMCS** (Virtual Machine Control Structure): an in-memory structure per vCPU holding guest state, host state, and "execution controls" (which events must cause an exit). Same source (Intel SDM 3C).
- **VM entry** (`VMLAUNCH`/`VMRESUME`) loads guest state and runs the guest. **VM exit** saves guest state, loads host state, and records an **exit reason** (e.g. I/O instruction, EPT violation, CPUID, HLT, external interrupt). Some instructions (e.g. `CPUID`) exit unconditionally. Same source.
- AMD-V (SVM) is the equivalent: `VMRUN` instruction, a **VMCB** (Virtual Machine Control Block) instead of VMCS, `#VMEXIT` with exit codes. AMD64 Architecture Programmer's Manual Vol. 2, ch. 15 "Secure Virtual Machine": https://www.amd.com/en/search/documentation/hub.html (doc #24593)
- **EPT (Intel) / NPT a.k.a. RVI (AMD)**: two-level address translation done by hardware. Guest page tables map guest-virtual -> guest-physical; EPT maps guest-physical -> host-physical. Before this, hypervisors kept "shadow page tables" and trapped every guest page-table write. Bhargava et al., "Accelerating two-dimensional page walks for virtualized systems", ASPLOS 2008: https://dl.acm.org/doi/10.1145/1346281.1346286 . Cost: a TLB miss can need up to 24 memory references for 4-level tables (vs 4 native). Same paper.
- **Cost of a VM exit**: the hardware round-trip (exit + entry) is on the order of hundreds to ~1-2 thousand CPU cycles on modern CPUs, i.e. roughly 0.1-1 microsecond, *before* the hypervisor does any work; exits handled in userspace (VMM process) add a full context switch and cost several microseconds. Agesen, Mattson, Rugina, Sheldon, "Software Techniques for Avoiding Hardware Virtualization Exits", USENIX ATC 2012 (shows exit latency trend across CPU generations): https://www.usenix.org/conference/atc12/technical-sessions/presentation/agesen . Exact cycle counts per CPU generation UNVERIFIED this session; teach it as "order of 1 microsecond, so minimize exits".

## 2. KVM

- KVM is a Linux kernel module (`kvm.ko` + `kvm-intel.ko`/`kvm-amd.ko`) that exposes VT-x/AMD-V through `/dev/kvm`, turning Linux into a hypervisor. Kivity et al., "kvm: the Linux Virtual Machine Monitor", Ottawa Linux Symposium 2007: https://www.kernel.org/doc/ols/2007/ols2007v1-pages-225-230.pdf
- API flow (kernel docs, "The Definitive KVM API Documentation"): https://docs.kernel.org/virt/kvm/api.html
  1. `open("/dev/kvm")`, check `KVM_GET_API_VERSION` (== 12).
  2. `KVM_CREATE_VM` -> VM file descriptor.
  3. `mmap` an anonymous host buffer, then `KVM_SET_USER_MEMORY_REGION` tells KVM "guest physical addresses X..Y are backed by this host virtual memory". Guest RAM is just ordinary process memory of the VMM.
  4. `KVM_CREATE_VCPU` -> vCPU fd; `mmap` the vCPU fd to get the shared `struct kvm_run`.
  5. Set registers (`KVM_SET_REGS`, `KVM_SET_SREGS`).
  6. Loop: `ioctl(vcpu, KVM_RUN)`. It returns to userspace when the guest does something KVM cannot handle in-kernel; `kvm_run->exit_reason` says why, e.g. `KVM_EXIT_IO` (port I/O), `KVM_EXIT_MMIO` (access to an unbacked guest-physical address, i.e. an emulated device register), `KVM_EXIT_HLT`, `KVM_EXIT_SHUTDOWN`. Userspace emulates the device access and calls `KVM_RUN` again.
- Josh Triplett's walk-through with a complete ~200-line C example: "Using the KVM API", LWN, 2015: https://lwn.net/Articles/658511/
- The split: KVM virtualizes CPU and memory (and a few hot devices in-kernel: local APIC, IOAPIC/PIC, PIT, optionally via `KVM_CREATE_IRQCHIP`). The userspace **VMM** (QEMU, Firecracker, Cloud Hypervisor, crosvm) owns guest memory layout, emulates devices, loads the kernel, and drives the `KVM_RUN` loop. Firecracker design doc: https://github.com/firecracker-microvm/firecracker/blob/main/docs/design.md ; Cloud Hypervisor README: https://github.com/cloud-hypervisor/cloud-hypervisor
- Each vCPU is a normal host thread; the host scheduler schedules vCPUs like any thread (LWN, above).
- "Type 1 vs Type 2": Type 1 runs on bare metal (Xen, ESXi, Hyper-V); Type 2 runs on a host OS (VirtualBox, VMware Workstation). KVM blurs it: it lives inside a general-purpose OS kernel but that kernel *is* running on bare metal, so it is commonly called Type 1; others call it Type 2 because a full OS hosts it. Teach: the label matters less than "what runs in ring 0 of VMX root and how big is it". Red Hat explainer: https://www.redhat.com/en/topics/virtualization/what-is-KVM (classification is opinion, not a spec).

## 3. Devices: emulation, paravirtualization, virtio, vhost

- **Full emulation**: VMM pretends to be real hardware (e.g. Intel e1000 NIC, IDE disk). Unmodified guest drivers work, but every register poke is a VM exit; slow and a large code surface. QEMU docs: https://www.qemu.org/docs/master/system/device-emulation.html
- **Paravirtualization**: guest knows it is virtualized and uses a driver designed for VMs, batching work through shared memory to minimize exits. Russell, "virtio: towards a de-facto standard for virtual I/O devices", ACM SIGOPS OSR 2008: https://dl.acm.org/doi/10.1145/1400097.1400108
- **virtio** (OASIS standard, v1.3): a device = feature bits + config space + one or more **virtqueues**. A virtqueue (split ring) = descriptor table + available ring (driver -> device) + used ring (device -> driver), in guest memory. **Notifications**: driver "kicks" the device (one MMIO/PIO write -> one exit), device signals via interrupt; both can be suppressed to batch. https://docs.oasis-open.org/virtio/virtio/v1.3/virtio-v1.3.html
- **Transports**: virtio over PCI (discoverable via PCI enumeration, used by QEMU and Cloud Hypervisor) vs **virtio-MMIO** (fixed memory-mapped address, described to guest via kernel command line or device tree, no PCI bus; used by Firecracker). Same OASIS spec, sections "Virtio Over PCI Bus" and "Virtio Over MMIO". Firecracker design doc confirms MMIO devices: https://github.com/firecracker-microvm/firecracker/blob/main/docs/design.md
- **vhost**: moves the virtqueue data path into the host kernel (`vhost-net`, `vhost-vsock`), avoiding a trip through the userspace VMM per packet. https://docs.kernel.org/driver-api/vhost.html (UNVERIFIED that this exact path exists; alternative: LWN "vhost" coverage https://lwn.net/Articles/346267/)
- **vhost-user**: same protocol but the backend is a separate userspace process connected by a Unix socket; guest memory is shared via fd passing. Example: **virtiofsd** (virtio-fs daemon) and DPDK/SPDK backends. QEMU vhost-user spec: https://www.qemu.org/docs/master/interop/vhost-user.html ; virtio-fs: https://virtio-fs.gitlab.io/
- **Why minimal device model matters**: every emulated device is attacker-reachable code from inside the guest (VENOM, CVE-2015-3456, was a QEMU floppy-controller bug: https://nvd.nist.gov/vuln/detail/CVE-2015-3456). Firecracker exposes only virtio-net, virtio-block, virtio-vsock, virtio-balloon(, virtio-rng, virtio-pmem in later versions), a serial console, and a minimal keyboard controller used only to reset the VM. Firecracker README/design: https://github.com/firecracker-microvm/firecracker . Fewer devices also = fewer drivers to probe at boot = faster boot (NSDI 2020 paper, below).

## 4. Boot path

- Classic VM: firmware (SeaBIOS legacy BIOS, or OVMF UEFI) -> POST, PCI enumeration, option ROMs -> bootloader (GRUB) reads disk -> kernel decompresses -> initramfs -> mounts root -> systemd starts units. Each stage costs from hundreds of ms to seconds. SeaBIOS: https://www.seabios.org/ ; OVMF: https://github.com/tianocore/tianocore.github.io/wiki/OVMF
- **Direct kernel boot**: VMM loads an uncompressed `vmlinux` ELF (or bzImage) straight into guest RAM, fills in the `boot_params` "zero page" and command line, sets registers, and jumps to the kernel entry. No firmware, no bootloader. Protocol: "The Linux/x86 Boot Protocol": https://docs.kernel.org/arch/x86/boot.html (64-bit entry: the bootloader must set up `boot_params` and page tables; see "64-bit Boot Protocol" section).
- **PVH entry**: a Xen-originated boot ABI (ELF note `XEN_ELFNOTE_PHYS32_ENTRY`) that starts the kernel in 32-bit protected mode with paging off and a `hvm_start_info` struct; also used by KVM VMMs (Firecracker, Cloud Hypervisor, QEMU `-kernel`) to boot vmlinux directly. Xen docs: https://xenbits.xen.org/docs/unstable/misc/pvh.html ; Cloud Hypervisor boots via PVH or firmware: https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/uefi.md (UNVERIFIED exact doc path)
- Firecracker: no PCI enumeration (virtio-MMIO devices announced on the kernel command line), no BIOS, minimal guest kernel config; README claims it can "launch ... in as little as 125 ms" with "less than 5 MiB" memory overhead per microVM: https://github.com/firecracker-microvm/firecracker (125 ms / 5 MiB figures UNVERIFIED this session against the live README; they also appear in the NSDI paper).
- Agache et al., "Firecracker: Lightweight Virtualization for Serverless Applications", NSDI 2020. VERIFIED: Firecracker is a VMM "specialized for serverless workloads", deployed in AWS Lambda and Fargate, "millions of production workloads, and trillions of requests per month". https://www.usenix.org/conference/nsdi20/presentation/agache . The paper's evaluation compares boot times of Firecracker vs QEMU and Cloud Hypervisor and discusses guest kernel config impact (details UNVERIFIED this session).
- Where the milliseconds go (teaching model): VMM process start + API config (few ms) -> load kernel into memory (~ms) -> kernel early init, decompression avoided with vmlinux -> device probing (dominant in classic VMs; tiny with a handful of MMIO devices) -> mount rootfs directly (no initrd) -> run `/sbin/init` or the agent directly. Firecracker gives a boot-time measurement method in its docs: https://github.com/firecracker-microvm/firecracker/blob/main/docs/api_requests/actions.md (UNVERIFIED exact path).

## 5. Nested virtualization

- Levels: **L0** = hypervisor on bare metal (e.g. AWS Nitro, GCE's KVM, Proxmox host KVM); **L1** = guest that itself runs a hypervisor (your Firecracker/Cloud Hypervisor host inside a cloud VM); **L2** = the nested guest (the microVM).
- x86 has a single level of hardware VMX. L0 runs L1's hypervisor in non-root mode, so L1's `VMLAUNCH`, `VMREAD`, `VMWRITE` all trap to L0, which emulates them, maintaining a "VMCS01/VMCS12 -> VMCS02" merge so L2 actually runs directly on hardware under L0's control. Every L2 exit goes first to L0, which decides whether to forward it to L1; L1's handling itself causes many more L0 exits ("exit multiplication"). Ben-Yehuda et al., "The Turtles Project: Design and Implementation of Nested Virtualization", OSDI 2010: https://www.usenix.org/conference/osdi10/turtles-project-design-and-implementation-nested-virtualization
- VERIFIED quote (Turtles abstract): it "can achieve performance that is within 6-8% of single-level (non-nested) virtualization for common workloads, through multi-dimensional paging for MMU virtualization and multi-level device assignment for I/O virtualization." Same URL. Caveat for teaching: that is with device assignment; exit-heavy workloads (lots of I/O via emulated/virtio devices) degrade far more.
- **VMCS shadowing** (Intel, Haswell+): hardware lets L1's `VMREAD`/`VMWRITE` hit a shadow VMCS without exiting to L0, removing a big class of exits. Intel white paper "4th Gen Intel Core vPro: VMCS Shadowing": https://www.intel.com/content/dam/www/public/us/en/documents/white-papers/intel-vmcs-shadowing-paper.pdf (UNVERIFIED link stability). KVM nested docs: https://docs.kernel.org/virt/kvm/x86/running-nested-guests.html
- Practical relevance: Firecracker requires `/dev/kvm`; on AWS that means bare-metal (`.metal`) instances or newer instance families with nested virtualization; GCP and Azure offer nested virtualization on selected VM types; Proxmox enables it via `cpu: host` + `kvm_intel nested=1`. Firecracker getting-started: https://github.com/firecracker-microvm/firecracker/blob/main/docs/getting-started.md ; GCP nested virt: https://cloud.google.com/compute/docs/instances/nested-virtualization/overview . (AWS nested-virtualization availability on non-metal instances: UNVERIFIED as of 2026-10-13; check current AWS docs.)
- Newer measurement: UNVERIFIED; candidate sources to check: "DVH: Direct Virtual Hardware" (Lim and Nieh, ASPLOS 2020) https://dl.acm.org/doi/10.1145/3373376.3378540 , which reports nested I/O overheads and proposes reducing them.

## 6. Containers

- A container is a normal Linux process with restricted views and limits; there is no second kernel.
  - **Namespaces** (mount, PID, net, UTS, IPC, user, cgroup, time) give each container its own view of a resource. `man 7 namespaces`: https://man7.org/linux/man-pages/man7/namespaces.7.html
  - **cgroups v2** limit/account CPU, memory, I/O, PIDs. https://docs.kernel.org/admin-guide/cgroup-v2.html
  - **Capabilities** split root privilege into ~40 bits (e.g. `CAP_SYS_ADMIN`, `CAP_NET_ADMIN`). https://man7.org/linux/man-pages/man7/capabilities.7.html
  - **seccomp-bpf** filters syscalls by number/args. https://docs.kernel.org/userspace-api/seccomp_filter.html ; Docker's default profile blocks ~44 syscalls: https://docs.docker.com/engine/security/seccomp/
- Security boundary = the shared host kernel's syscall interface (~350+ syscalls on x86-64). One kernel bug reachable from inside the container can mean host compromise.
- Examples:
  - **Dirty Pipe, CVE-2022-0847**: uninitialized `pipe_buffer.flags` let unprivileged users overwrite page-cache of read-only files (Linux 5.8+). Max Kellermann's write-up: https://dirtypipe.cm4all.com/ ; NVD: https://nvd.nist.gov/vuln/detail/CVE-2022-0847 . Container-escape demos existed (e.g. overwriting runc binary) - details per write-up.
  - **CVE-2024-1086**: use-after-free / double free in netfilter `nf_tables` (`nft_verdict_init`), local privilege escalation; added to CISA KEV (exploited in the wild, ransomware) in 2024. NVD: https://nvd.nist.gov/vuln/detail/CVE-2024-1086 ; CISA KEV: https://www.cisa.gov/known-exploited-vulnerabilities-catalog . Reachable from containers when unprivileged user namespaces are allowed.
  - **runc CVE-2024-21626 "Leaky Vessels"** (Snyk, Jan 2024): leaked internal file descriptor to host `/sys/fs/cgroup` let a container process set its working directory in the host filesystem, enabling escape; fixed in runc 1.1.12. Advisory: https://github.com/opencontainers/runc/security/advisories/GHSA-xr7r-f8xq-vfvv ; Snyk: https://snyk.io/blog/leaky-vessels-docker-runc-container-breakout-vulnerabilities/ . Note: this is a runtime bug, not a kernel bug; VM-based sandboxes still run a runtime but the escape lands inside a VM.

## 7. gVisor: an application kernel

- gVisor implements the Linux syscall interface in a userspace Go program called the **Sentry**. The app's syscalls are intercepted (via a "platform": **systrap** (seccomp `SECCOMP_RET_TRAP`, default since 2023) or **KVM**, earlier **ptrace**) and handled by the Sentry, which itself uses only a small, seccomp-filtered set of host syscalls. File access goes through a separate **Gofer** process (or directfs) speaking a protocol (LISAFS). Architecture guide: https://gvisor.dev/docs/architecture_guide/intro/ ; platforms: https://gvisor.dev/docs/architecture_guide/platforms/ ; systrap default: https://gvisor.dev/blog/2023/04/28/systrap-release/
- Differs from containers: the app never talks to the host kernel directly; host kernel attack surface is the Sentry's filtered syscall set, not the full ABI. Differs from VMs: no virtual hardware, no guest kernel image, no boot; it's a process with a re-implemented kernel, so memory overhead and start-up resemble containers but syscall-heavy and I/O-heavy workloads pay interception cost. Security model: https://gvisor.dev/docs/architecture_guide/security/
- Isolation boundary (security model page): defense in depth; to escape, an attacker must first compromise the Sentry (memory-safe Go, reduced surface) and then exploit the host via the restricted syscall set in a seccomp/namespace sandbox. It does not claim protection against hardware side channels. https://gvisor.dev/docs/architecture_guide/security/
- Runs as an OCI runtime (`runsc`), so Docker/Kubernetes can use it directly. https://gvisor.dev/docs/user_guide/quick_start/docker/

## 8. Snapshot/restore

- State to capture: guest RAM (largest, MBs-GBs), vCPU registers (general, segment, MSRs, FPU/xsave, LAPIC), device model state (virtio queue indices, serial, clock). Firecracker snapshot doc: https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md
- Firecracker writes a "microVM state" file + a memory file; on restore, memory can be `mmap`ed from file (pages loaded lazily by host page faults) or served by a **UFFD** handler process. Same doc + https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/handling-page-faults-on-snapshot-resume.md
- **userfaultfd**: lets a userspace thread receive notifications of page faults on a registered range and resolve them with `UFFDIO_COPY`/`UFFDIO_ZEROPAGE`; used for post-copy live migration and lazy restore. https://docs.kernel.org/admin-guide/mm/userfaultfd.html
- Why restore beats boot: skip kernel init, device probing, and app warm-up (language runtime, imports); only touch pages actually needed. Papers:
  - Du et al., "Catalyzer: Sub-millisecond Startup for Serverless Computing with Initialization-less Booting", ASPLOS 2020 (built on gVisor): https://dl.acm.org/doi/10.1145/3373376.3378512
  - Ustiugov et al., "Benchmarking, Analysis, and Optimization of Serverless Function Snapshots" (REAP), ASPLOS 2021, Firecracker-based; records the working set and prefetches it, reporting large cold-start reductions (paper reports ~3.7x average; UNVERIFIED this session): https://dl.acm.org/doi/10.1145/3445814.3446714
  - Ao et al., "FaaSnap: FaaS Made Fast Using Snapshot-based VMs", EuroSys 2022: https://dl.acm.org/doi/10.1145/3492321.3524270
- Clone hazards (VERIFIED, Firecracker "random-for-clones" doc, https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/random-for-clones.md):
  - Clones resume with identical RNG state; "The conservative approach is to presume the stale state has a significant influence on RNG output."
  - VMGenID (Linux >= 5.18 ACPI, >= 6.10 DeviceTree) lets Firecracker write a new identifier and notify the guest to reseed, but there's still a "race window between resuming vCPUs and Linux CSPRNG getting successfully re-seeded."
  - `/proc/sys/kernel/random/boot_id` and `/var/lib/systemd/random-seed` are identical across clones; userspace libraries with own entropy pools have no generic fix.
  - Mitigations: virtio-rng, `RNDADDENTROPY` + `RNDRESEEDCRNG` ioctls after resume.
- Other hazards: duplicate MAC/IP and TCP connection state (network identity) -> restore each clone into its own netns/tap with same guest IP + NAT, or reconfigure after resume. Clocks: guest wall clock jumps; Firecracker docs on snapshot network and clock handling: https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/network-for-clones.md (UNVERIFIED path). Snapshots also carry secrets present in memory at snapshot time.

## 9. Side channels and multi-tenancy

- Spectre (2018): speculative execution leaks data via cache timing across boundaries. https://spectreattack.com/ ; kernel docs index of hardware vulnerabilities: https://docs.kernel.org/admin-guide/hw-vuln/index.html
- L1TF/Foreshadow (2018): leaks L1 data cache contents, including across VMs on sibling hyperthreads. https://docs.kernel.org/admin-guide/hw-vuln/l1tf.html
- MDS (2019): leaks from CPU internal buffers; sibling hyperthreads share them. https://docs.kernel.org/admin-guide/hw-vuln/mds.html
- Hence SMT off or **core scheduling** (only mutually trusting tasks share a physical core). https://docs.kernel.org/admin-guide/hw-vuln/core-scheduling.html
- VERIFIED, Firecracker prod-host-setup (https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md): SMT is "frequently a precondition for speculation issues utilized in side channel attacks such as Spectre variants, MDS, and others"; "our recommendation is to disable SMT in production scenarios that require tenant separation"; "Firecracker itself has no control over many lower-level software and hardware behaviors"; disable KSM (dedup can reveal "what memory pages are accessed by another process"); disable swap to mitigate "data remanence issues".
- Teaching point: the VM boundary stops software bugs crossing kernels but not microarchitectural leaks; containers and gVisor share the same exposure plus a shared kernel.

## 10. Glossary

- **Hypervisor**: software that runs VMs by controlling CPU virtualization (VMX root). In Linux-land, KVM.
- **VMM**: Virtual Machine Monitor; here, the userspace process that configures a VM, emulates devices, runs the KVM_RUN loop (QEMU, Firecracker, Cloud Hypervisor, crosvm).
- **KVM**: Linux kernel module exposing hardware virtualization via `/dev/kvm`.
- **microVM**: a VM with a minimal VMM and device model, direct kernel boot, small memory overhead; term popularized by Firecracker.
- **virtio**: OASIS standard for paravirtual devices using shared-memory virtqueues.
- **vCPU**: a virtual CPU; in KVM, one host thread running `KVM_RUN`.
- **VM exit**: hardware switch from guest to hypervisor on a configured event; costly.
- **EPT / NPT**: hardware second-level page tables translating guest-physical to host-physical.
- **PVH**: boot ABI that starts a kernel directly in 32-bit mode without firmware.
- **MMIO**: memory-mapped I/O; device registers appear at physical addresses; guest accesses to unbacked ones cause `KVM_EXIT_MMIO`.
- **vsock**: `AF_VSOCK` socket family for host<->guest communication without networking. https://man7.org/linux/man-pages/man7/vsock.7.html
- **tap device**: virtual L2 network interface on the host whose other end is a file descriptor (used by the VMM for virtio-net). https://docs.kernel.org/networking/tuntap.html
- **netns**: network namespace; isolated network stack on the host.
- **seccomp**: kernel syscall filter; Firecracker applies per-thread seccomp filters to itself.
- **rootfs**: root filesystem image the guest or container mounts as `/`.
- **OCI runtime**: program implementing the OCI runtime spec (runc, runsc, kata-runtime). https://github.com/opencontainers/runtime-spec
- **snapshot**: saved VM/sandbox state (memory + vCPU + devices) to resume later or clone.
- **UFFD**: userfaultfd; userspace page-fault handling.
- **Nested virtualization**: running a hypervisor inside a VM (L0/L1/L2).

## Proposed ASCII diagrams

### A. Layer stack

```
 Container (runc)        gVisor (runsc)             microVM (Firecracker/CH)
 +-------------+         +-------------+            +-------------------+
 |  agent app  |         |  agent app  |            |  agent app        |
 +-------------+         +-------------+            +-------------------+
        | syscalls              | syscalls (trapped)| guest Linux kernel|
        |                +-------------+            +-------------------+
        |                | Sentry (Go, |            | virtio drivers    |
        |                | user-space  |            +---------+---------+
        |                | "kernel")   |                      | VM exits / virtqueues
        |                +------+------+            +---------v---------+
        |                       | ~small filtered   | VMM process       |
        |                       | host syscalls     | (seccomp, jailer) |
 =======v=======================v===================+---------+---------+
 |            HOST LINUX KERNEL (namespaces, cgroups, seccomp, KVM)      |
 ========================================================================
                               hardware (VT-x / AMD-V, EPT)
 Boundary:  full syscall ABI | Sentry + small syscall set | VM exit + device model
```

### B. KVM_RUN loop

```
 VMM (userspace)                    KVM (host kernel)          Guest
 open /dev/kvm
 KVM_CREATE_VM
 mmap RAM; KVM_SET_USER_MEMORY_REGION
 KVM_CREATE_VCPU; set regs
 loop:
   ioctl(KVM_RUN) --------------->  VM entry ----------------> runs natively
                                    <------- VM exit (e.g. MMIO write to virtio kick)
                                    handled in kernel? -> re-enter
                                    else return to userspace
   switch(run->exit_reason) <------
     KVM_EXIT_MMIO -> emulate device, process virtqueue
     KVM_EXIT_IO   -> serial port etc.
     KVM_EXIT_HLT/SHUTDOWN -> stop
```

### C. Boot timeline

```
Classic VM:  [firmware POST+PCI scan][GRUB][kernel decompress+init][probe many devices][initramfs][systemd units...] -> app
             |---- hundreds of ms to seconds ----|-------------- seconds ---------------------------------|
microVM:     [VMM start+config][load vmlinux][kernel init][probe 2-4 virtio-mmio][mount rootfs][init/agent] -> app
             |-------------------- ~100-200 ms total (Firecracker claim: as little as 125 ms) ------------|
Snapshot:    [VMM start][load state + mmap/UFFD memory][resume vCPUs] -> app already warm   (ms to tens of ms)
```

### D. Nested virtualization

```
 +--------------------------------------------------+
 | L2: microVM guest (agent)                        |
 +--------------------------------------------------+
 | L1: cloud VM / Proxmox VM running KVM + Firecracker |
 +--------------------------------------------------+
 | L0: cloud hypervisor (Nitro/KVM) or Proxmox host  |
 +--------------------------------------------------+
 | hardware: one real VMX level                      |

 L2 exit --> L0 (always first) --> reflect to L1? --> L1 handler runs
          L1's VMREAD/VMWRITE/VMRESUME each trap to L0 (unless VMCS shadowing)
          => one L2 exit can cost many L0 exits ("exit multiplication")
```
