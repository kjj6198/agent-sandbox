#!/bin/bash
# Usage: run.sh LEVEL DIR CPUS [trials]   (e.g. run.sh L0 /root/sbx-l0 0-11 20)
set -euo pipefail
level=$1 dir=$2 cpus=$3 trials=${4:-20}
ns=sbxl0
ip netns del "$ns" 2>/dev/null || true
ip netns add "$ns"
trap 'ip netns del "$ns"' EXIT
ip -n "$ns" link set lo up
ip -n "$ns" tuntap add dev sbxtap0 mode tap vnet_hdr
ip -n "$ns" addr add 172.16.0.1/24 dev sbxtap0
ip -n "$ns" link set sbxtap0 up
rm -f "$dir/$level.jsonl"
taskset -c "$cpus" ip netns exec "$ns" python3 "$dir/nested_compare.py" --level "$level" --dir "$dir" \
  --fc "$dir/firecracker" --ch "$dir/cloud-hypervisor" --ch-remote "$dir/ch-remote" \
  --trials "$trials" --out "$dir/$level.jsonl"
