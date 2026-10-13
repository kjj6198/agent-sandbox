# sbx: disposable Firecracker sandboxes for AI agents

sbx lets an agent create a Linux machine, use it, and throw it away. Each sandbox is a Firecracker microVM restored from a snapshot of a booted template. It has its own kernel, a writable disk, and internet access, and it cannot reach the host or the LAN. The design follows the decision in [the sandbox evaluation](../research/agent-sandbox-isolation.md).

```sh
id=$(sbx create)                       # a fresh machine, ready in tens of milliseconds
sbx cp ./app $id:app
sbx exec $id 'cd app && pnpm install && pnpm test'
sbx exec -d $id 'cd app && pnpm dev --port 5173'
sbx expose $id 5173                    # http://192.168.1.33:30000
sbx rm $id
```

`sbx --help` is written for agents: every command has examples, `create` prints only the ID, `exec` exits with the command's own exit code, and read commands take `--json`.

## Measured on the sbx host

VM 310 has 8 vCPUs and 8 GiB of memory on the i5-13400F. Sandboxes use the default template, 2 vCPUs and 2 GiB.

| What | Result |
|---|---|
| `sbx create`, request to guest agent answering | 32 ms |
| 10 `sbx create` at once | 54 to 106 ms each |
| Memory held by a new sandbox | 3 MiB, plus what it writes |
| Memory held by an idle `browser` sandbox | about 126 MiB |
| `sbx snapshot` | 1.0 s |
| `sbx fork` | 2.2 s |
| Cold boot during a template build, launch to agent answering | 270 ms |
| Template build when podman has the image cached | 13 s |
| Playwright `connectOverCDP` from the LAN, new page, screenshot, navigate to example.com | 0.5 s |

## Parts

| Part | Where it runs | What it does |
|---|---|---|
| `sbx` | anywhere | CLI. Talks HTTP to sbxd with a bearer token |
| `sbxd` | sbx host | Daemon. Restores sandboxes, builds templates, publishes ports, removes idle sandboxes |
| `sbx-agent` | inside every sandbox | PID 1. Builds the root filesystem, then serves exec, files, and TCP over vsock |
| `templates/default` | sbx host | Dockerfile of the default image |
| `kernel/` | sbx host | Guest kernel config and build script |
| `deploy/` | sbx host, Proxmox | Installer, systemd unit, Proxmox VM and firewall |
| `skill/sbx/SKILL.md` | your agent | Claude Code skill. Copy it to `~/.claude/skills/sbx/` |

## How a sandbox is made

A template is a directory under `/var/lib/sbx/templates/NAME`:

| File | Content |
|---|---|
| `rootfs.ext4` | The image, attached read-only to every sandbox |
| `upper.ext4` | A sparse writable disk, mounted as the overlay's upper layer |
| `vmstate`, `mem` | The Firecracker snapshot of the booted guest |

`sbx template build` turns a Dockerfile into a template. podman builds the image, the image is exported to `rootfs.ext4` with `sbx-agent` as init, a microVM boots it, `--warmup` runs, and the VM is snapshotted.

`sbx create` then:

1. Takes a network namespace that sbxd prepared ahead of time.
2. Creates a jail directory with hard links to the template's `rootfs.ext4`, `vmstate`, and `mem`, plus a reflink copy of `upper.ext4`. XFS makes the copy instant, and it costs only the blocks the sandbox writes.
3. Starts Firecracker through the jailer, as its own unprivileged user, chrooted into the jail, in the namespace.
4. Loads the snapshot. Memory is mapped privately from `mem`, so clones share the template's pages through the page cache and pay only for pages they write.
5. Sends the agent the current time and the sandbox's hostname, because a restored guest keeps the snapshot's clock.

The jail matters for more than isolation: a snapshot records file paths, and the chroot gives every clone the same paths.

### Network

Every clone has the same guest IP, MAC, and tap name, because they are copies of one snapshot. Each lives in its own network namespace with an identical tap and a permanent neighbor entry. The namespace NATs the guest to a unique veth address, and the host NATs that to the internet. The lab measured 30 s outages with a changed MAC and a 1 s ARP wait without the neighbor entry.

The host's nftables table `inet sbx` drops anything a sandbox sends to the host or to private, link-local, or multicast ranges, and all IPv6. The Proxmox firewall on the sbx VM drops the same ranges again, so a VM escape still cannot reach the LAN.

Nothing can connect into a sandbox's network. `sbx expose` and `sbx forward` tunnel connections through the agent over vsock, so a server inside may listen on localhost.

## Install

The sbx host is VM 310 (`sbx-host`, 10.10.10.40) on the Proxmox node `home`. It sits on the NAT bridge `vmbr1`, like VM 300. `sbx-portfwd.service` on the Proxmox host publishes port 7070 (API) and 30000-30999 (exposed ports) on 192.168.1.33.

From scratch:

```sh
# On the Proxmox host: create the VM, then install the port forward
SSHKEYS=/root/sbx-keys.pub deploy/proxmox/create-vm.sh
cp deploy/proxmox/sbx-portfwd.service /etc/systemd/system/ && systemctl enable --now sbx-portfwd

# On your machine: build, copy, install
make dist
COPYFILE_DISABLE=1 tar -C dist -czf - . | ssh sbx-host 'mkdir -p sbx-dist && tar -xzf - -C sbx-dist'
ssh sbx-host 'sudo SBX_DATA_DISK=/dev/sdb sbx-dist/install.sh'
```

`install.sh` installs packages and Firecracker, formats the data disk as XFS, builds the guest kernel (about 3 minutes), starts sbxd, and builds the `default` and `browser` templates. Each step can run alone, for example `install.sh sbx` to update the binaries. Sandboxes keep running across an sbxd restart.

Any Linux host with `/dev/kvm` works, including bare metal. Run `install.sh` there without the Proxmox parts.

Then on your machine:

```sh
make cli && cp bin/sbx ~/.local/bin/
ssh sbx-host sudo cat /etc/sbx/token | sbx login http://192.168.1.33:7070
sbx status
```

`SBX_URL` and `SBX_TOKEN` override the saved login, which is useful for agents in containers.

## Templates

| Template | Content |
|---|---|
| `default` | Debian 13, Node.js 24 with pnpm, Go 1.27, Python 3.13 with uv, build-essential, git, ripgrep, jq, ffmpeg, ImageMagick, Chromium, Noto CJK fonts (Japanese, Traditional and Simplified Chinese, Korean) and emoji. 2 vCPUs, 2 GiB memory, 16 GiB disk |
| `browser` | The same image with headless Chromium already running, its DevTools protocol on port 9222 |

Commands run as `user` (uid 1000, passwordless sudo) in `/home/user`. Versions are pinned at the top of `templates/default/Dockerfile`.

To drive the browser from outside:

```sh
id=$(sbx create browser)
sbx forward $id 9222 &
# Playwright: chromium.connectOverCDP('http://127.0.0.1:9222')
```

Make your own template from any Debian or Ubuntu based Dockerfile, or save a prepared sandbox:

```sh
sbx template build myapp ./my-template-dir --memory 4096 --warmup 'cd /srv && ./start-db'
sbx snapshot $id repo-ready          # memory, processes, and files of a running sandbox
sbx create repo-ready
sbx fork $id                         # the same, as a one-off clone
```

## API

sbxd serves HTTP on port 7070. Every request needs `Authorization: Bearer <token from /etc/sbx/token>`.

| Method and path | Body | Answer |
|---|---|---|
| `GET /v1/status` | | host capacity |
| `GET /v1/sandboxes` | | list of sandboxes |
| `POST /v1/sandboxes` | `{"template", "ttl", "env"}` | the sandbox |
| `GET`, `PATCH`, `DELETE /v1/sandboxes/{id}` | `PATCH`: `{"ttl"}` | |
| `POST /v1/sandboxes/{id}/exec` | `{"script", "env", "cwd", "user", "timeout", "detach", "stdin", "tty"}` | frame stream, see `internal/proto/exec.go` |
| `GET`, `PUT /v1/sandboxes/{id}/files?path=&user=` | tar | tar |
| `GET /v1/sandboxes/{id}/tcp?port=` | | `101 Switching Protocols`, then raw TCP |
| `POST /v1/sandboxes/{id}/ports` | `{"port"}` | `{"port", "host_port", "url"}` |
| `DELETE /v1/sandboxes/{id}/ports/{port}` | | |
| `POST /v1/sandboxes/{id}/snapshot` | `{"name"}` | the template |
| `POST /v1/sandboxes/{id}/fork` | | the new sandbox |
| `GET /v1/templates` | | list of templates |
| `POST /v1/templates/{name}?cpus=&memory_mib=&disk_gib=&warmup=` | tar of the build context | frame stream of the build log |
| `DELETE /v1/templates/{name}` | | |

Durations are strings such as `"30m"` or `"2d"`. `"0"` disables the idle timeout.

## Configuration

`/etc/sbx/sbxd.json` overrides the defaults in `internal/daemon/config.go`:

| Key | Default | Meaning |
|---|---|---|
| `listen` | `:7070` | API address |
| `public_host` | empty | Host name in exposed-port URLs. Empty uses the one the client used for the API |
| `default_ttl` | `1h` | Idle timeout of new sandboxes |
| `max_sandboxes` | `64` | Limit on running sandboxes |
| `min_free_mib` | `512` | Refuse new sandboxes below this much available host memory |
| `pool_size` | `8` | Network namespaces kept ready |
| `port_min`, `port_max` | `30000`, `30999` | Host ports for `sbx expose` |

## Why not Packer

Packer builds machine images for hypervisors and clouds. A Firecracker template is a snapshot of a running VM, which no Packer builder produces. sbxd builds it from a Dockerfile, which is also the format agents and people already write. The one machine image here is the sbx host VM, and `deploy/proxmox/create-vm.sh` creates it from the Ubuntu cloud image in about 40 lines.

## Limits

- x86_64 only. The guest kernel, the agent, and the Firecracker download are built for it.
- No GPUs and no live shared folders. Firecracker has neither. Copy files with `sbx cp`.
- The snapshot memory file is the template's full memory size on disk, 2 GiB for the default template.
- No huge pages. The lab measured faster cold boots with them on a nested host, but snapshot restore from a file needs normal pages.
- An exposed port is open to the LAN without authentication. Use `sbx forward` for anything sensitive, such as the browser's DevTools port.
