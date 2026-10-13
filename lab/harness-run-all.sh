#!/bin/bash
cd /opt/sbx
for s in cold restore browser-cold browser-restore parallel memory workloads network compat; do
  echo "=== $s $(date -u +%FT%TZ)"
  venv/bin/python -m harness $s
  echo "=== $s exit $? $(date -u +%FT%TZ)"
done
