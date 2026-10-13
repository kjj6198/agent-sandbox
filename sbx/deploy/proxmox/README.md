# sbx on Proxmox

This runs sbx in its own VM instead of on the Proxmox host, so untrusted code is two walls away from the LAN: Firecracker inside the VM, and the VM's firewall outside it. It is how the reference install runs, as VM 310 (`sbx-host`, 10.10.10.40).

| File | What it does |
|---|---|
| `create-vm.sh` | Creates the VM from the Ubuntu 24.04 cloud image, with a second disk for `/var/lib/sbx` |
| `vm.fw` | Proxmox firewall for the VM: drops every connection it starts toward a private range, and accepts SSH and the sbx ports only from the host and the LAN |
| `sbx-portfwd.service` | Publishes port 7070 and 30000 to 30999 on the Proxmox host's LAN address |

The VM sits on a NAT-only bridge (`vmbr1`, 10.10.10.0/24), so the LAN cannot reach it directly; `sbx-portfwd` forwards the sbx ports from the host. Adjust the variables at the top of `create-vm.sh` and the addresses in `sbx-portfwd.service` and `vm.fw` to your network.

```sh
# On the Proxmox host
SSHKEYS=/root/sbx-keys.pub ./create-vm.sh
cp sbx-portfwd.service /etc/systemd/system/ && systemctl enable --now sbx-portfwd

# From a checkout, once the VM answers ssh. sbx-host is an ssh alias for kalan@10.10.10.40
# with ProxyJump through the Proxmox host, because the VM is only on vmbr1.
make install HOST=sbx-host INSTALL_ENV=SBX_DATA_DISK=/dev/sdb
```

The VM needs `--cpu host` for nested virtualization, which `create-vm.sh` sets.
