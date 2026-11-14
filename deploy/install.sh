#!/usr/bin/env bash
# Installs sbx on an x86_64 Linux host with /dev/kvm: bare metal, or a VM with nested
# virtualization. Debian and Ubuntu get their packages installed; on other distributions,
# install the commands listed in step_packages first.
#
#   curl -fsSL https://github.com/kjj6198/agent-sandbox/releases/latest/download/install.sh | sudo bash
#   sudo ./install.sh [step...]      # from the directory `make dist` produces, or `make install`
#
# Run with no steps and no bundle beside it, it downloads the release bundle and runs the
# copy inside.
# Steps run in order and are idempotent: preflight packages firecracker disk sysctl kernel sbx
# templates. Settings come from the environment:
#   SBX_VERSION      release tag to download (default: the latest release)
#   SBX_BUNDLE_URL   download the host bundle from here instead, such as a mirror
#   SBX_DATA_DISK    block device to format as XFS for /var/lib/sbx (empty: use the root fs).
#                    XFS or Btrfs lets sandbox disks be reflinked; any filesystem works.
#   SBX_PUBLIC_HOST  host name put in URLs of exposed ports (default: the one clients use
#                    to reach the API, which is right behind a port forward)
#   SBX_REBUILD=1    rebuild templates that already exist
set -euo pipefail

REPO=kjj6198/agent-sandbox
FC_VERSION=v1.17.0
DATA=/var/lib/sbx
HERE=$(cd "$(dirname "${BASH_SOURCE[0]:-.}")" && pwd)
ARCH=$(uname -m)

log() { echo "install: $*"; }
die() { echo "install: $*" >&2; exit 1; }

step_preflight() {
  [ "$ARCH" = x86_64 ] || die "sbx supports x86_64 only, this is $ARCH"
  [ -e /dev/kvm ] || die "/dev/kvm missing; enable (nested) virtualization"
}

step_packages() {
  if command -v apt-get >/dev/null; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -q
    # podman builds template images; the rest is networking and filesystems.
    apt-get install -y -q --no-install-recommends \
      podman xfsprogs e2fsprogs nftables iproute2 procps jq curl ca-certificates
  fi
  require podman mkfs.ext4 nft ip sysctl jq curl sha256sum
}

require() {
  local missing=()
  for c in "$@"; do
    command -v "$c" >/dev/null || missing+=("$c")
  done
  [ ${#missing[@]} -eq 0 ] || die "missing commands: ${missing[*]}"
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
    local fs
    fs=$(findmnt -no FSTYPE --target "$DATA")
    case $fs in
      xfs | btrfs) log "$DATA is on $fs: sandbox disks are reflinked" ;;
      *) log "$DATA is on $fs: sandbox disks are sparse copies, slower for big snapshot templates" ;;
    esac
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

# Release bundles carry a prebuilt kernel. A checkout builds one, which takes about 3 minutes.
step_kernel() {
  if [ -e "$DATA/kernel/vmlinux" ] && [ ! -e "$HERE/kernel/vmlinux" ]; then
    log "guest kernel present: $(readlink "$DATA/kernel/vmlinux")"
    return
  fi
  if [ -e "$HERE/kernel/vmlinux" ]; then
    local version
    version=$(readlink "$HERE/kernel/vmlinux")
    install -D -m 0644 "$HERE/kernel/$version" "$DATA/kernel/$version"
    ln -sfn "$version" "$DATA/kernel/vmlinux"
    log "guest kernel $version installed"
    return
  fi
  if command -v apt-get >/dev/null; then
    apt-get update -q
    apt-get install -y -q --no-install-recommends \
      build-essential flex bison bc libelf-dev libssl-dev curl ca-certificates xz-utils
  fi
  require make gcc flex bison bc xz curl
  "$HERE/kernel/build.sh" "$DATA/kernel"
}

step_sbx() {
  install -m 0755 "$HERE/bin/sbxd" "$HERE/bin/sbx" /usr/local/bin/
  install -D -m 0755 "$HERE/bin/sbx-agent" /usr/local/lib/sbx/sbx-agent
  install -d -m 0700 /etc/sbx
  if [ ! -e /etc/sbx/sbxd.json ]; then
    jq -n --arg host "${SBX_PUBLIC_HOST:-}" '{public_host: $host}' >/etc/sbx/sbxd.json
  fi
  rm -rf /usr/local/share/sbx/templates
  mkdir -p /usr/local/share/sbx
  cp -R "$HERE/templates" /usr/local/share/sbx/
  # A container image runs sbxd itself, without systemd.
  [ -d /run/systemd/system ] || return 0
  install -m 0644 "$HERE/sbxd.service" /etc/systemd/system/sbxd.service
  systemctl daemon-reload
  systemctl enable sbxd.service >/dev/null 2>&1
  systemctl restart sbxd.service
  wait_sbxd
  log "sbxd $(sbxd --version) running. API token: /etc/sbx/token, created on its first start"
  if command -v ufw >/dev/null && ufw status | grep -q 'Status: active'; then
    log "ufw is active: allow 7070/tcp (API) and 30000:30999/tcp (sbx expose) for your clients"
  fi
}

wait_sbxd() {
  for _ in $(seq 50); do sbx status >/dev/null 2>&1 && return; sleep 0.2; done
  die "sbxd is not answering on 127.0.0.1:7070"
}

# Builds the default template, then the browser template: the same image with Chromium
# already open when the snapshot is taken. Both start the desktop first, so every sandbox
# has a screen. Set SBX_REBUILD=1 to rebuild existing ones.
step_templates() {
  wait_sbxd
  local have
  have=$(sbx template ls --json | jq -r '.[].name')
  if [ -n "${SBX_REBUILD:-}" ] || ! grep -qx default <<<"$have"; then
    sbx template build default /usr/local/share/sbx/templates/default --warmup sbx-desktop
  fi
  if [ -n "${SBX_REBUILD:-}" ] || ! grep -qx browser <<<"$have"; then
    sbx template build browser /usr/local/share/sbx/templates/default --warmup sbx-browser
  fi
  sbx template ls
}

# Piped from curl, there is no bundle beside this script: fetch the release and run its copy.
bootstrap() {
  local url=${SBX_BUNDLE_URL:-} tmp
  if [ -z "$url" ] && [ -n "${SBX_VERSION:-}" ]; then
    url=https://github.com/$REPO/releases/download/$SBX_VERSION/sbx-linux-amd64.tar.gz
  fi
  url=${url:-https://github.com/$REPO/releases/latest/download/sbx-linux-amd64.tar.gz}
  tmp=$(mktemp -d)
  log "downloading $url"
  curl -fsSL "$url" | tar -xz -C "$tmp"
  local status=0
  "$tmp/install.sh" || status=$?
  rm -rf "$tmp"
  exit $status
}

[ "$(id -u)" = 0 ] || die "run as root"
if [ ! -x "$HERE/bin/sbxd" ] && [ $# -eq 0 ]; then
  step_preflight
  bootstrap
fi

steps=("$@")
[ ${#steps[@]} -gt 0 ] || steps=(preflight packages firecracker disk sysctl kernel sbx templates)
for s in "${steps[@]}"; do
  log "== $s"
  "step_$s"
done
