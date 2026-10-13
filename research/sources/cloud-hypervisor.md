# Cloud Hypervisor (CH): source notes (accessed 2026-10-13)

Abbreviations: GH = https://github.com/cloud-hypervisor/cloud-hypervisor ; DOCS = https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/
"Official" = project repo or website. "Third-party" = labelled as such.

## 1. Releases, cadence, governance, contributors
- Latest release: v53.0, published 2026-07-12. Previous releases: v52.0 and v51.2 (2026-05-14), v51.0 (2026-02-20), v50.0 (2025-12-19), v49.0 (2025-11-09), v48.0 (2025-09-12). Official. https://github.com/cloud-hypervisor/cloud-hypervisor/releases
- Cadence (official): "A new major release is issued approximately every 6 weeks"; point releases on demand; each major release gets bug fixes for two cycles (~12 weeks), then reaches EOL. Releases since 2025 are actually closer to 8-10 weeks apart (see dates above). https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/releases.md
- Stability (official): the API and CLI are not broken without at least 2 major releases of notice. Snapshot/restore and live migration are NOT supported across versions. TDX, vfio-user and vDPA are experimental. https://github.com/cloud-hypervisor/cloud-hypervisor#3-status
- Governance (official): "governed openly as part of the Linux Foundation". Supporting orgs listed on the website: Alibaba, AMD, Ampere, ARM, ByteDance, Cyberus Technology, Intel, Microsoft, SAP, Tencent Cloud. https://www.cloudhypervisor.org/
- Commit author email domains, 2026-01-01 to 2026-10-13 (my count from the GitHub API, ~2,640 commits): meta.com 615, linux.microsoft.com 557 + microsoft.com 352, cyberus-technology.de 520, crusoe.ai 118, fb.com 66, google.com 13. So Meta, Microsoft and Cyberus are the main contributors in 2026. Intel, the project's original author, no longer appears among the top committers. https://api.github.com/repos/cloud-hypervisor/cloud-hypervisor/commits?since=2026-01-01T00:00:00Z
- Notable 2026 changes (official release notes):
  - v52.0: fix for CVE-2026-45782 (use-after-free in the virtio-block async I/O completion path, GHSA-f47p-p25q-83rh, also backported as v51.2); AMD SEV-SNP on KVM with measured boot; VFIO via iommufd/vfio-cdev; multi-connection TCP live migration; userfaultfd demand-paged snapshot restore (`memory_restore_mode`); configurable PCI BDFs. https://github.com/cloud-hypervisor/cloud-hypervisor/releases/tag/v52.0
  - v53.0: offloaded snapshot/restore daemon; postcopy live migration via userfaultfd; background prefault after a uffd restore; mTLS for migration; VFIO migration v2 for same-host snapshot/restore; post-migration network announcements, so connectivity recovers within milliseconds instead of up to 20 s; nested Hyper-V in Windows guests (WSL2) on KVM; virtio-rtc; guest clock advanced on restore/migration; buffered socket serial output; `--memory reserve=on`; pre-opened VFIO cdev/iommufd FDs for an unprivileged VMM; `--seccomp=errno`. https://github.com/cloud-hypervisor/cloud-hypervisor/releases/tag/v53.0

## 2. Architecture
- CH is a userspace VMM (Virtual Machine Monitor) written in Rust. It runs on top of KVM or the Microsoft Hypervisor (MSHV). Its stated goals are minimal emulation, low latency, low memory footprint, small attack surface, 64-bit only, CPU/memory/PCI hotplug, and machine-to-machine migration. Official. https://github.com/cloud-hypervisor/cloud-hypervisor#objectives
- Relation to rust-vmm, Firecracker and crosvm (official): CH is built on the rust-vmm crates and "a large part of the Cloud Hypervisor code is based on either the Firecracker or the crosvm project's implementations". Its goal differs from theirs: CH aims to be a "general purpose VMM for Cloud Workloads and not limited to container/serverless or client workloads". https://github.com/cloud-hypervisor/cloud-hypervisor#4-relationship-with-rust-vmm-project
- Thread model (official): CH is multi-threaded, with dedicated threads for each vCPU, for virtio devices and for the HTTP API server, plus the main VMM thread. Each thread type gets its own seccomp filter. https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/seccomp.md
- Hypervisor backends: KVM and MSHV (see docs/mshv.md). Official. https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/mshv.md
- Device model (official device_model.md):
  - Legacy devices: serial, RTC/CMOS, I/O APIC, i8042, PL061 GPIO, ACPI device.
  - Virtio devices: block, console, iommu, net, pmem, rng, vsock.
  - vhost-user devices: blk, fs, net, generic.
  - VFIO passthrough.
  https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/device_model.md
- Other device docs in the repo: balloon.md, memory.md (virtio-mem and hotplug), vdpa.md, vfio-user.md, iommu.md, tpm.md, ivshmem.md, fw_cfg.md, generic-vhost-user.md, intel_tdx.md, amd_sev_snp.md; virtio-rtc was added in v53. https://github.com/cloud-hypervisor/cloud-hypervisor/tree/main/docs
- Watchdog device: UNVERIFIED in this pass (not in the device_model.md headings).
- virtio-fs uses an external virtiofsd daemon over vhost-user, and requires `--memory shared=on`. Official. https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/fs.md
- GPU passthrough uses VFIO. Third-party: Northflank cites GPU passthrough as a CH advantage. https://northflank.com/blog/what-is-a-microvm
- Boot (official README):
  - Direct kernel boot with `--kernel`. On x86-64 the kernel must be a PVH-enabled `vmlinux` ELF or a bzImage.
  - Firmware boot with `--firmware`, using either rust-hypervisor-firmware (a lightweight PVH firmware) or the edk2 UEFI build `CLOUDHV.fd` (x86-64) / `CLOUDHV_EFI.fd` (AArch64).
  - Guests: 64-bit Linux, and Windows 10 / Windows Server 2019.
  https://github.com/cloud-hypervisor/cloud-hypervisor#booting-linux
- Hotplug (official): CPUs, VFIO devices, virtio-{net,block,pmem,fs,vsock}, and memory resizing. https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/hotplug.md
- ACPI is part of the guest-facing stable surface (device list, ACPI, Hyper-V enlightenments). Official. https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/releases.md

## 3. Snapshot/restore and live migration
- Snapshot flow (official): pause with `ch-remote pause`, then `ch-remote snapshot file:///dir`. The snapshot directory holds three files:
  - `config.json`: the VM config, human-readable and editable.
  - `memory-ranges`: the full guest RAM, e.g. a 4 GiB file for a 4G VM.
  - `state.json`: the device and vCPU state.
  https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/snapshot_restore.md
- Restore (official): `--restore source_url=file:///dir[,resume=true]`. The VM comes back paused unless `resume=true` is set. Net FDs can be re-supplied with `net_fds=[...]`. Same doc.
- Memory restore modes (official):
  - `copy` (default): eager copy of all memory before the VM starts.
  - `ondemand` (v52+): userfaultfd faults pages in on first access. Strict: no fallback to `copy` if uffd is unavailable. Needs Linux 6.6+ to poison pages that failed to load. Not supported with `prefault=on`, VFIO, vDPA, vfio-user or vhost-user devices (so not with virtio-fs/virtiofsd). Needs access to `/dev/userfaultfd` or `vm.unprivileged_userfaultfd=1`.
  - `copyonwrite`: maps the snapshot file MAP_PRIVATE, so many VMs restored from one snapshot share the page cache. Restricted to plain private RAM; `shared=on`, hugepages, virtio-mem, passthrough and similar configs fall back to `copy`. The snapshot file must stay unchanged for the VM's whole lifetime. This mode is documented on `main` but NOT mentioned in the v53.0 notes, so it is probably unreleased as of 2026-10-13 (my inference).
  https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/snapshot_restore.md
- v53 adds background prefault after a uffd restore, with thread count scaled to available parallelism. https://github.com/cloud-hypervisor/cloud-hypervisor/releases/tag/v53.0
- VFIO: snapshot/restore works only for devices that implement VFIO migration v2 (e.g. mlx5_vfio_pci). Official. https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/snapshot_restore.md
- Live migration (official):
  - Transports: TCP (cross-host) and UNIX socket (same host; `memory_mode=memfds` enables low-downtime VMM live upgrade).
  - Options: `downtime_ms`, `timeout_s`, mTLS via `tls_dir`, parallel `connections`.
  - Both sides fully trust each other; snapshot files are trusted input.
  https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/live_migration.md
- Restore latency is a tracked perf metric (`restore_latency_time_ms`). No official number was found. https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/performance_metrics.md

## 4. Performance numbers
- Official website: "Boot to userspace in less than 100ms with direct kernel boot". It also claims "Minimal memory overhead for dense deployments", with no figure given. https://www.cloudhypervisor.org/
- performance_metrics.md is a harness, not a dashboard:
  - Metrics: boot_time_ms, boot_time_pmem_ms, 16-vCPU variants, virtio-net latency/throughput, block MiB/s and IOPS, restore_latency_time_ms.
  - Sample output shows boot_time_pmem_ms mean 105.9 ms (v40.0, 2024). The doc explicitly calls this "for illustration purpose only".
  https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/performance_metrics.md
- Public perf CI dashboard: UNVERIFIED (none found).
- Per-VM memory footprint figure (MiB): UNVERIFIED (no official number found).
- Third-party (Northflank) quotes ~1-2 s sandbox startup in one post and "under 1 second" in its docs. That is platform-level time (Kata plus orchestration), not raw CH boot time. https://northflank.com/blog/what-is-a-microvm , https://northflank.com/docs/v1/application/sandboxes/deploy-sandboxes-on-northflank

## 5. Security
- Seccomp (official): enabled by default ("security should not be an option") with per-thread filters; the main thread gets a stricter filter after workers start. Modes: `--seccomp false|log|errno`. https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/seccomp.md
- Landlock (official): opt-in with `--landlock` (or `landlock_enable` in the API). It is applied at vm_create from the set of configured paths. Extra paths needed for hotplug are passed with `--landlock-rules`. https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/landlock.md
- Threat model (official): https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/threat-model.md
  - The guest is untrusted. The CLI, HTTP API and D-Bus API are trusted.
  - CH is NOT hardened against guest denial of service (a guest can cause a Rust panic or excess host CPU/memory use); use cgroups to limit it.
  - No protection against symlink attacks or `/proc` paths, so pass FDs.
  - Disk images are untrusted unless qcow2/vmdk `backing_files` is enabled. Decompression bombs are not handled.
  - vhost-user backends get full control of the guest.
  - x86 virtio-iommu lacks interrupt remapping.
  - CH avoids user namespaces, AF_ALG and splice(), so host bugs in those cannot be reached from the guest.
- Rust memory safety claim (official): "Minimal emulated devices and implemented in Rust to avoid many common security issues". https://www.cloudhypervisor.org/
- Rust does not eliminate memory bugs: CVE-2026-45782 was a use-after-free in virtio-block async I/O (fixed in v52.0/v51.2). https://github.com/cloud-hypervisor/cloud-hypervisor/releases/tag/v52.0
- Attack surface versus Firecracker (my analysis, not a project claim): CH emulates more than Firecracker does: PCI, ACPI, legacy devices, VFIO, vDPA, vhost-user, TPM, Windows/Hyper-V enlightenments. Its v53 bug fixes include several guest-triggerable MMIO/PIO/PCI panics, which shows the extra surface. https://github.com/cloud-hypervisor/cloud-hypervisor/releases/tag/v53.0

## 6. CLI and API ergonomics
- Main flags: `--kernel`, `--firmware`, `--initramfs`, `--cmdline`, `--disk path=...`, `--net`, `--cpus boot=N`, `--memory size=...,shared=on`, `--api-socket`, `--serial tty|socket=...`, `--console off`, `--fs`, `--restore`. Official. https://github.com/cloud-hypervisor/cloud-hypervisor#booting-linux
- Networking (official): `--net "tap=<if_name>,ip=<ip_addr>,mask=<net_mask>,mac=..."`. If `tap=` is empty, CH creates the tap itself and assigns the host-side IP and mask. This needs `cap_net_admin` (`setcap cap_net_admin+ep`). Pre-opened tap FDs can be passed instead. https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/vmm/src/config.rs , https://github.com/cloud-hypervisor/cloud-hypervisor#firmware-booting
- Control interfaces (official): a REST API over a UNIX socket, an optional D-Bus API, and `ch-remote`, a CLI client for the REST API that covers pause, resume, snapshot, restore, add-disk, send/receive-migration and more. https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/api.md
- virtio-fs setup (official):
  1. Run `virtiofsd --socket-path=... --shared-dir=... --cache=never` (`cache=never` is the CH default, to keep host memory low).
  2. Start CH with `--fs tag=myfs,socket=...` and `--memory shared=on`.
  3. In the guest, run `mount -t virtiofs myfs /mnt`. The guest kernel needs 5.10+.
  DAX is not supported. https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/fs.md

## 7. Limitations for agent sandboxes
- Architectures (official): x86-64 and AArch64 (AArch64 needs GICv3); riscv64 is experimental. Host kernel 5.13+ is recommended and CI uses 5.15. https://github.com/cloud-hypervisor/cloud-hypervisor#architectures
- CH requires KVM or MSHV. In the cloud that means bare-metal instances or nested virtualization. The threat model explicitly covers "KVM under nested virtualization". https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/threat-model.md
- Guest kernel (official): a PVH vmlinux or bzImage for direct boot. The project ships `ch_defconfig` in its kernel fork (`cloud-hypervisor/linux`, branch ch-7.2.8); otherwise "provided the required options are enabled any recent kernel will suffice". Exact CONFIG list (virtio-pci, PVH): UNVERIFIED here; see ch_defconfig. https://github.com/cloud-hypervisor/cloud-hypervisor#building-your-kernel
- No virtio-gpu or display device appears in the device model. Headless Chrome over CDP would need only CPU, network and vsock/virtio-net. GPU acceleration requires VFIO passthrough. No browser-specific official doc was found (UNVERIFIED). https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/device_model.md
- Snapshots are tied to the CH version, so a fleet of pre-warmed snapshots must be rebuilt on every CH upgrade. The `ondemand` restore mode excludes vhost-user devices, including virtio-fs. https://github.com/cloud-hypervisor/cloud-hypervisor#3-status
- The guest can DoS the VMM, so pair CH with cgroups. https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/threat-model.md

## 8. Ecosystem and maintenance
- Kata Containers: CH is a supported hypervisor (`configuration-clh.toml`), but the default `configuration.toml` uses QEMU, so CH is NOT the Kata default. Official (Kata). https://github.com/kata-containers/kata-containers/blob/main/docs/hypervisors.md
- Ubicloud: its README says it uses Cloud Hypervisor as its virtual machine monitor; the repo has `prog/download_cloud_hypervisor.rb` and `rhizome/host/bin/download-cloud-hypervisor`. Third-party primary source. https://github.com/ubicloud/ubicloud
- Northflank: says it uses Kata Containers with Cloud Hypervisor as its primary microVM approach for sandboxes, and Firecracker or gVisor where supported. Third-party vendor claim. https://northflank.com/blog/what-is-a-microvm
- Microsoft: a heavy contributor (MSHV backend, ~900 commits in 2026). Production use in Azure: UNVERIFIED (no public source found). https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/mshv.md
- Meta (meta.com + fb.com, ~680 commits in 2026) and Crusoe contribute heavily, but their production use is UNVERIFIED. https://api.github.com/repos/cloud-hypervisor/cloud-hypervisor/commits?since=2026-01-01T00:00:00Z
- Alibaba, ByteDance and Tencent are listed as supporters; their deployment details are UNVERIFIED. https://www.cloudhypervisor.org/
- Maintenance signals (GitHub API, 2026-10-13): 6,323 stars, 793 forks, last push 2026-10-12, 207 contributors (stats endpoint), ~2,640 commits since 2026-01-01. The project is very active. https://github.com/cloud-hypervisor/cloud-hypervisor
