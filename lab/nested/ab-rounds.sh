#!/bin/bash
set -u
cd "$(dirname "$0")/../.."
s() { ssh "$@"; }
ab() {
  local level=$1 dir=$2 cpus=$3
  cat <<SH
set -u
orig=\$(cat /proc/sys/vm/nr_hugepages)
sysctl -qw vm.nr_hugepages=600
echo "hugepages free: \$(grep HugePages_Free /proc/meminfo | awk '{print \$2}')"
ns=sbxl0
ip netns del \$ns 2>/dev/null; ip netns add \$ns
ip -n \$ns link set lo up; ip -n \$ns tuntap add dev sbxtap0 mode tap vnet_hdr
ip -n \$ns addr add 172.16.0.1/24 dev sbxtap0; ip -n \$ns link set sbxtap0 up
cd $dir && taskset -c $cpus ip netns exec \$ns python3 hugepage_ab.py --level $level --dir $dir --trials 10 --out $dir/$level-ab.jsonl 2>&1 | tail -2
ip netns del \$ns
sysctl -qw vm.nr_hugepages=\$orig
echo "nr_hugepages restored to \$(cat /proc/sys/vm/nr_hugepages)"
SH
}
s lab 'rm -f /opt/sbx/nested/L1-ab-r*.jsonl'; s home 'rm -f /root/sbx-l0/L0-ab-r*.jsonl'
for round in 1 2; do
  echo "== round $round L1 (host load $(s home 'cut -d" " -f1 /proc/loadavg'))"
  ab "L1-ab-r$round" /opt/sbx/nested 0-7 | s lab 'sudo bash -s'
  echo "== round $round L0 (host load $(s home 'cut -d" " -f1 /proc/loadavg'))"
  ab "L0-ab-r$round" /root/sbx-l0 0-11 | s home 'bash -s'
done
s lab 'cat /opt/sbx/nested/L1-ab-r*-ab.jsonl' > research/raw/nested/L1-ab-rounds.jsonl
s home 'cat /root/sbx-l0/L0-ab-r*-ab.jsonl' > research/raw/nested/L0-ab-rounds.jsonl
