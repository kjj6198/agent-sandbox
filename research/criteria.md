# Decision and criteria (fixed before looking at candidates)

## Decision
Which isolation technology, gVisor, Firecracker, or Cloud Hypervisor, should back an agent
platform that spawns one isolated sandbox per agent task on demand, on self-hosted Linux x86_64
(today a Proxmox box, so sandboxes run nested inside a VM), minimizing time to a usable sandbox
while keeping it usable for running programs, networking, and browser automation?

Constraints:
- Untrusted, agent-generated code. Isolation must be meaningfully stronger than runc.
- Browser automation is a hard requirement (Chromium driven over CDP from outside the sandbox).
- Small team, so operational burden counts.
- Hardware: i5-13400F, 31 GiB RAM, Proxmox VE 8.2. Experiments run in an L1 VM (nested KVM).

## Criteria
| # | Criterion | Kind | Threshold | Weight |
|---|---|---|---|---|
| 1 | Cold start to first command executed, p50 / p95 | Quant | p50 <= 500 ms, p95 <= 1 s | 20% |
| 2 | Snapshot or warm restore to first command executed, p50 / p95 | Quant | p50 <= 100 ms | 15% |
| 3 | Browser ready: start to first CDP screenshot, cold and from snapshot | Quant | cold <= 3 s, snapshot <= 500 ms | 15% |
| 4 | Runtime overhead on agent-like workloads vs runc (CPU, fork/exec, file metadata, tar extract, git, compile) | Quant | composite slowdown <= 1.5x | 15% |
| 5 | Network: egress DNS+HTTPS works, ingress port reachable, throughput and RTT to host | Quant + gate | gate: egress and ingress both work; throughput >= 1 Gbit/s | 5% |
| 6 | Memory overhead per idle sandbox (host RSS/PSS) and parallel spawn of 10 | Quant | overhead <= 50 MiB; p95 at 10 concurrent <= 2x single | 5% |
| 7 | Compatibility and usability: OCI images as-is, Chrome sandbox, Docker-in-sandbox, inotify, user namespaces, file sharing, GPU path | Qual | gate: Chromium headless + CDP works | 10% |
| 8 | Operational complexity: image pipeline, networking, snapshot handling, guest kernel ownership, host requirements (KVM, nesting) | Qual | | 10% |
| 9 | Isolation strength and attack surface | Qual | gate: stronger than runc | 5% |

Maintenance activity, adopters, and license are recorded but unweighted unless one is disqualifying.
