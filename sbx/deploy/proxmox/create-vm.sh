#!/usr/bin/env bash
# Creates the Firecracker host VM on a Proxmox node. Run as root on the Proxmox host.
#
#   SSHKEYS=/root/sbx-keys.pub ./create-vm.sh
#
# The VM copies VM 300's isolation: it sits on the NAT-only bridge vmbr1, and the Proxmox
# firewall drops every connection it starts toward a private range, so a sandbox escape
# cannot reach the LAN. sbx-portfwd.service publishes the API and sandbox ports on the host.
set -euo pipefail

VMID=${VMID:-310}
NAME=${NAME:-sbx-host}
IP=${IP:-10.10.10.40}
GW=${GW:-10.10.10.1}
BRIDGE=${BRIDGE:-vmbr1}
CORES=${CORES:-8}
# P-cores of the i5-13400F, as in VM 300.
AFFINITY=${AFFINITY:-0-11}
MEMORY=${MEMORY:-8192}
STORAGE=${STORAGE:-local-lvm}
OS_DISK=${OS_DISK:-40G}
DATA_DISK_GB=${DATA_DISK_GB:-120}
IMAGE=${IMAGE:-/var/lib/vz/template/cache/noble-server-cloudimg-amd64.img}
CIUSER=${CIUSER:-kalan}
SSHKEYS=${SSHKEYS:?set SSHKEYS to an authorized_keys file}
HERE=$(cd "$(dirname "$0")" && pwd)

if qm status "$VMID" >/dev/null 2>&1; then
  echo "create-vm: VM $VMID already exists" >&2
  exit 1
fi

qm create "$VMID" --name "$NAME" --ostype l26 --cpu host --sockets 1 --cores "$CORES" \
  --affinity "$AFFINITY" --memory "$MEMORY" --balloon 0 --scsihw virtio-scsi-single \
  --serial0 socket --vga serial0 --net0 "virtio,bridge=$BRIDGE,firewall=1" --onboot 1 \
  --description "sbx: Firecracker microVM host for AI agent sandboxes. See sbx/README.md in agent-sandbox-eval."

qm disk import "$VMID" "$IMAGE" "$STORAGE" >/dev/null
qm set "$VMID" --scsi0 "$STORAGE:vm-$VMID-disk-0,discard=on,iothread=1,ssd=1" --boot order=scsi0
qm disk resize "$VMID" scsi0 "$OS_DISK"
# Data disk for /var/lib/sbx. install.sh formats it as XFS so sandbox disks can be reflinked.
qm set "$VMID" --scsi1 "$STORAGE:$DATA_DISK_GB,discard=on,iothread=1,ssd=1"
qm set "$VMID" --ide2 "$STORAGE:cloudinit" --ciuser "$CIUSER" --sshkeys "$SSHKEYS" \
  --ipconfig0 "ip=$IP/24,gw=$GW" --nameserver "1.1.1.1 9.9.9.9"

sed "s/@IP@/$IP/" "$HERE/vm.fw" >"/etc/pve/firewall/$VMID.fw"

qm start "$VMID"
echo "create-vm: VM $VMID ($NAME) started at $IP"
