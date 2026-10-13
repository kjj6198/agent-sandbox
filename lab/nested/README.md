# nested: bare metal vs the lab VM

These scripts produced the bare metal vs nested comparison, the huge page A/B test, and the parallel restore test in the report. They were added after `lab/README.md` was written.

| Script | What it does |
|---|---|
| `nested_compare.py` | Cold boot, with and without a serial console, and snapshot restore for Firecracker and Cloud Hypervisor |
| `hugepage_ab.py` | Firecracker with 4 KiB pages vs 2 MiB huge pages, and Cloud Hypervisor with THP on vs off |
| `parallel_restore.py` | Restores k clones of one snapshot at the same moment, measures memory per clone, and checks whether clones share RNG output |
| `run.sh` | Creates the `sbxl0` network namespace and tap, then runs `nested_compare.py` pinned to the given CPUs |
| `rounds.sh`, `ab-rounds.sh` | Run from your own machine. They alternate rounds between the lab VM and the Proxmox host so both see the same host load, then copy the results into `research/raw/nested/` |
| `summ.py` | Prints p50 and p95 per case from the JSONL files |

Each level needs a directory holding these scripts plus `vmlinux`, `rootfs.ext4`, and the `firecracker`, `cloud-hypervisor`, and `ch-remote` binaries. That is `/opt/sbx/nested` on the lab VM and `/root/sbx-l0` on the Proxmox host. `rounds.sh` and `ab-rounds.sh` reach the two machines as `ssh lab` and `ssh home`, so define both hosts in `~/.ssh/config`.

`parallel_restore.py` runs as root on the lab VM and uses the kernel, disk image, and OCI bundle under `/opt/sbx`.
