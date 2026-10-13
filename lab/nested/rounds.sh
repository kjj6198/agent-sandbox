#!/bin/bash
set -u
cd "$(dirname "$0")/../.."
s() { ssh "$@"; }
s lab 'rm -f /opt/sbx/nested/L1-r*.jsonl'
s home 'rm -f /root/sbx-l0/L0-r*.jsonl'
for round in 1 2; do
  echo "== round $round L1 (host load $(s home 'cut -d" " -f1 /proc/loadavg'))"
  s lab "sudo /opt/sbx/nested/run.sh L1-r$round /opt/sbx/nested 0-7 10 2>&1 | grep -v '^SNAP' | tail -8"
  echo "== round $round L0 (host load $(s home 'cut -d" " -f1 /proc/loadavg'))"
  s home "/root/sbx-l0/run.sh L0-r$round /root/sbx-l0 0-11 10 2>&1 | grep -v '^SNAP' | tail -8"
done
s lab 'cat /opt/sbx/nested/L1-r*.jsonl' > research/raw/nested/L1-rounds.jsonl
s home 'cat /root/sbx-l0/L0-r*.jsonl' > research/raw/nested/L0-rounds.jsonl
