# agent-sandbox-eval

An evaluation of gVisor, Firecracker, and Cloud Hypervisor for giving an AI agent a fresh, isolated Linux machine as fast as possible. The machine still has to run programs, use the network, and drive a browser. Everything ran on a home Proxmox server with an Intel i5-13400F and 31 GiB of RAM, mostly inside an Ubuntu VM and partly on the host itself.

**Result.** Use Firecracker, and create each sandbox by restoring a snapshot of a template that has already booted. A restored sandbox answered its first command in 29 ms inside the VM and 20 ms on bare metal. Twenty clones restored at once cost about 9 MiB of memory each, and programs inside ran as fast as in a plain container. Cloud Hypervisor nearly tied, but every restored clone loads its whole memory. gVisor starts fastest from nothing, but agent-style work runs about twice as slowly inside it.

Start with the [report](research/agent-sandbox-isolation.md). Its last chapter, "How these sandboxes work", explains the technology from the ground up.

## Contents

| Path | Contents |
|---|---|
| `research/agent-sandbox-isolation.md` | The report, with the decision, scores, evidence, risks, and a primer |
| `research/summary.html` | Charts of the main results. Open it in a browser |
| `research/criteria.md`, `research/score.py` | Criteria and weights fixed before measuring, and the script that computes the scores |
| `research/harness-spec.md` | How the harness measures |
| `research/sources/` | Research notes with a link for every claim |
| `research/raw/` | Every measurement record, plus screenshots, iperf3 output, and boot logs |
| `lab/` | The lab as deployed at `/opt/sbx` on the lab VM, with launch scripts, guest image, kernel config, and the measurement harness. `lab/README.md` explains it |
| `lab/nested/` | Bare metal vs nested comparisons and the parallel restore test |

## Reproduce

1. On a Linux machine with `/dev/kvm`, install the versions pinned in `lab/VERSIONS` and copy `lab/` to `/opt/sbx`. Then follow `lab/README.md` to set up networking and build the guest image and kernel.
2. From `/opt/sbx`, `sudo ./harness-run-all.sh` runs the main scenarios, and `sudo venv/bin/python -m harness <scenario>` runs one. Results go to `/opt/sbx/results`.
3. `python3 research/score.py` prints the score table from the numbers used in the report.
