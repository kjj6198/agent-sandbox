#!/usr/bin/env bash
# Builds the uncompressed guest kernel that Firecracker boots.
#
#   kernel/build.sh /var/lib/sbx/kernel
#
# config-6.18 is the sandbox lab's config: Firecracker's microvm-kernel-ci-x86_64-6.18 plus
# lab/kernel/sbx-changes.config (no modules, virtio-fs, all cgroup controllers, netfilter for
# Docker inside the guest, /proc/config.gz).
set -euo pipefail

VERSION=${KERNEL_VERSION:-6.18.56}
OUT=${1:?usage: build.sh OUT_DIR}
HERE=$(cd "$(dirname "$0")" && pwd)
WORK=${WORK:-/var/tmp/sbx-kernel}
TARBALL=linux-$VERSION.tar.xz

mkdir -p "$WORK" "$OUT"
cd "$WORK"
if [ ! -f "$TARBALL" ]; then
  curl -fsSLO "https://cdn.kernel.org/pub/linux/kernel/v6.x/$TARBALL"
  curl -fsSL https://cdn.kernel.org/pub/linux/kernel/v6.x/sha256sums.asc | grep " $TARBALL\$" | sha256sum -c -
fi
rm -rf "linux-$VERSION"
tar -xf "$TARBALL"
cd "linux-$VERSION"
cp "$HERE/config-6.18" .config
make olddefconfig >/dev/null
make -j"$(nproc)" vmlinux
install -m 0644 vmlinux "$OUT/vmlinux-$VERSION"
ln -sfn "vmlinux-$VERSION" "$OUT/vmlinux"
echo "kernel: $OUT/vmlinux-$VERSION"
