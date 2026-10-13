#!/usr/bin/env bash
# Installs sbx on a Debian or Ubuntu host that has /dev/kvm (bare metal, or a VM with nested
# virtualization). Run as root from the directory `make dist` produces:
#
#   sudo ./install.sh [step...]
#
# Steps run in order and are idempotent: packages firecracker disk sysctl kernel sbx templates.
# Settings come from the environment:
#   SBX_DATA_DISK    block device to format as XFS for /var/lib/sbx (empty: use the root fs)
#   SBX_PUBLIC_HOST  host name put in URLs of exposed ports (default: the one clients use
#                    to reach the API, which is right behind a port forward)
set -euo pipefail

FC_VERSION=v1.17.0
DATA=/var/lib/sbx
HERE=$(cd "$(dirname "$0")" && pwd)
ARCH=$(uname -m)

log() { echo "install: $*"; }

step_packages() {
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -q
  # podman builds template images; the rest is networking, filesystems, and the kernel toolchain.
  apt-get install -y -q --no-install-recommends \
    podman xfsprogs e2fsprogs nftables iproute2 jq curl ca-certificates xz-utils \
    build-essential flex bison bc libelf-dev libssl-dev
}

step_firecracker() {
  if [ "$(firecracker --version 2>/dev/null | head -1)" = "Firecracker $FC_VERSION" ]; then
    log "firecracker $FC_VERSION already installed"
    return
  fi
  local tmp tgz=firecracker-$FC_VERSION-$ARCH.tgz
  tmp=$(mktemp -d)
  curl -fsSL -o "$tmp/$tgz" "https://github.com/firecracker-microvm/firecracker/releases/download/$FC_VERSION/$tgz"
  curl -fsSL -o "$tmp/$tgz.sha256.txt" "https://github.com/firecracker-microvm/firecracker/releases/download/$FC_VERSION/$tgz.sha256.txt"
  (cd "$tmp" && sha256sum -c "$tgz.sha256.txt")
  tar -xzf "$tmp/$tgz" -C "$tmp"
  install -m 0755 "$tmp/release-$FC_VERSION-$ARCH/firecracker-$FC_VERSION-$ARCH" /usr/local/bin/firecracker
  install -m 0755 "$tmp/release-$FC_VERSION-$ARCH/jailer-$FC_VERSION-$ARCH" /usr/local/bin/jailer
  rm -rf "$tmp"
  log "$(firecracker --version | head -1)"
}

step_disk() {
  mkdir -p "$DATA"
  if mountpoint -q "$DATA" || [ -z "${SBX_DATA_DISK:-}" ]; then
    return
  fi
  # XFS gives reflinks, so each sandbox's writable disk is an instant copy of its template's.
  blkid "$SBX_DATA_DISK" >/dev/null 2>&1 || mkfs.xfs -q -m reflink=1 -L sbx-data "$SBX_DATA_DISK"
  local uuid
  uuid=$(blkid -s UUID -o value "$SBX_DATA_DISK")
  grep -q "$uuid" /etc/fstab || echo "UUID=$uuid $DATA xfs defaults,noatime 0 2" >>/etc/fstab
  systemctl daemon-reload
  mount "$DATA"
  log "$SBX_DATA_DISK mounted on $DATA"
}

step_sysctl() {
  cat >/etc/sysctl.d/90-sbx.conf <<'EOF'
net.ipv4.ip_forward = 1
EOF
  sysctl -q --system
}

step_kernel() {
  if [ -e "$DATA/kernel/vmlinux" ]; then
    log "guest kernel present: $(readlink "$DATA/kernel/vmlinux")"
    return
  fi
  "$HERE/kernel/build.sh" "$DATA/kernel"
}

step_sbx() {
  install -m 0755 "$HERE/bin/sbxd" "$HERE/bin/sbx" /usr/local/bin/
  install -D -m 0755 "$HERE/bin/sbx-agent" /usr/local/lib/sbx/sbx-agent
  install -d -m 0700 /etc/sbx
  if [ ! -s /etc/sbx/token ]; then
    head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' >/etc/sbx/token
    chmod 0600 /etc/sbx/token
  fi
  if [ ! -e /etc/sbx/sbxd.json ]; then
    jq -n --arg host "${SBX_PUBLIC_HOST:-}" '{public_host: $host}' >/etc/sbx/sbxd.json
  fi
  rm -rf /usr/local/share/sbx/templates
  mkdir -p /usr/local/share/sbx
  cp -R "$HERE/templates" /usr/local/share/sbx/
  install -m 0644 "$HERE/sbxd.service" /etc/systemd/system/sbxd.service
  systemctl daemon-reload
  systemctl enable sbxd.service >/dev/null 2>&1
  systemctl restart sbxd.service
  for _ in $(seq 50); do sbx status >/dev/null 2>&1 && break; sleep 0.2; done
  log "sbxd $(sbxd --version) running. API token: /etc/sbx/token"
}

# Builds the default template, then the browser template: the same image with Chromium
# already running when the snapshot is taken. Set SBX_REBUILD=1 to rebuild existing ones.
step_templates() {
  local have
  have=$(sbx template ls --json | jq -r '.[].name')
  if [ -n "${SBX_REBUILD:-}" ] || ! grep -qx default <<<"$have"; then
    sbx template build default /usr/local/share/sbx/templates/default
  fi
  if [ -n "${SBX_REBUILD:-}" ] || ! grep -qx browser <<<"$have"; then
    sbx template build browser /usr/local/share/sbx/templates/default --warmup sbx-browser
  fi
  sbx template ls
}

[ "$(id -u)" = 0 ] || { echo "install: run as root" >&2; exit 1; }
[ -e /dev/kvm ] || { echo "install: /dev/kvm missing; enable (nested) virtualization" >&2; exit 1; }

steps=("$@")
[ ${#steps[@]} -gt 0 ] || steps=(packages firecracker disk sysctl kernel sbx templates)
for s in "${steps[@]}"; do
  log "== $s"
  "step_$s"
done
