# sbx: disposable Firecracker sandboxes for AI agents

sbx lets an agent create a Linux machine, use it, and throw it away. Each sandbox is a Firecracker microVM restored from a snapshot of a booted template. It has its own kernel, a writable disk, a desktop you can watch, and internet access, and it cannot reach the host or the LAN. The design follows the decision in [the sandbox evaluation](../research/agent-sandbox-isolation.md).

```sh
id=$(sbx create --view)                # a fresh machine in tens of milliseconds, its screen in a small window
sbx cp ./app $id:app
sbx exec $id 'cd app && pnpm install && pnpm test'
sbx exec -d $id 'cd app && pnpm dev --port 5173'
sbx expose $id 5173                    # http://sbx-host:30000
sbx rm $id
```

[ARCH.md](ARCH.md) explains the design, with diagrams of every flow.

`sbx --help` is written for agents: every command has examples, `create` prints only the ID, `exec` exits with the command's own exit code, and read commands take `--json`. To give an agent sbx, install the CLI where it runs and copy `skill/sbx/` to `~/.claude/skills/`, which Claude Code and Grok both read.

## Requirements

The sbx host is any x86_64 Linux machine with `/dev/kvm`: a desktop or server on bare metal, or a VM with nested virtualization, such as a cloud instance that offers it or a VM on Proxmox. The installer handles Debian and Ubuntu. On other distributions it lists the commands to install first.

The CLI runs on macOS and Linux and only needs to reach the host over HTTP. The host can be the same machine.

## Install

From a checkout with Go installed:

```sh
make install                                   # on the sbx host itself
make install HOST=me@sbx-host                  # or from another machine, over ssh
make install HOST=me@sbx-host INSTALL_ENV=SBX_DATA_DISK=/dev/sdb
```

`install.sh` installs packages and Firecracker, builds the guest kernel (about 3 minutes), starts `sbxd`, and builds the `default` and `browser` templates (about 5 minutes). Every step is idempotent and can run alone, for example `make install HOST=me@sbx-host STEPS=sbx` to update the binaries. Sandboxes keep running across an sbxd restart.

| Setting | Meaning |
|---|---|
| `SBX_DATA_DISK` | Block device to format as XFS for `/var/lib/sbx`. Without it sbx uses the root filesystem |
| `SBX_PUBLIC_HOST` | Host name in URLs of exposed ports. By default it is the name the client used to reach the API |
| `SBX_REBUILD=1` | Rebuild templates that already exist |

On XFS or Btrfs, each sandbox's disk is a reflink of the template's. On other filesystems it is a sparse copy, which takes 3 ms for a fresh template and longer for snapshot templates with many written files.

Then, on each machine that runs agents:

```sh
make cli && cp bin/sbx ~/.local/bin/
ssh sbx-host sudo cat /etc/sbx/token | sbx login http://sbx-host:7070
sbx status
```

`SBX_URL` and `SBX_TOKEN` override the saved login, which is useful for agents in containers.

Clients need TCP 7070 for the API and 30000 to 30999 for `sbx expose`. If the host runs a firewall such as ufw, or sits behind a cloud security group, open those ports to your clients only.

[deploy/proxmox](deploy/proxmox/README.md) runs sbx in an isolated VM on Proxmox, with its own firewall and a port forward from the Proxmox host.

## Watching a sandbox

Every sandbox has a 1280x800 desktop: an X server that serves VNC inside the guest, with openbox. Commands get `DISPLAY=:0`, so any GUI program an agent starts appears there. Right-click the desktop for a terminal or a browser.

`sbx view ID`, or `sbx create --view`, opens a small window at the top right of your screen with a live view. It starts in watch mode, so a stray click cannot disturb the agent; click "Watching" to take over the mouse and keyboard. The viewer is a page served by the CLI on 127.0.0.1, embedding [noVNC](https://github.com/novnc/noVNC), and it reaches the screen through the same API tunnel as `sbx forward`. It opens as an app window of Chrome, Edge, Brave, or Chromium, and falls back to a tab in your default browser.

The viewer runs in the background, so an agent's `sbx create --view` returns at once. It exits a minute after its window closes or its sandbox is removed, and watching does not keep an idle sandbox alive.

Drop files on the viewer window to copy them into the sandbox's `~/Downloads`, where a browser's file picker finds them. Folders need zipping first.

## Agents over MCP

`sbx mcp` is an MCP server on stdin and stdout, for agents that work through tools rather than a shell:

```sh
claude mcp add sbx -- sbx mcp
```

| Tools | What they do |
|---|---|
| `sandbox_create`, `sandbox_list`, `sandbox_remove`, `exec` | Lifecycle and commands, as in the CLI |
| `click`, `move_mouse`, `drag`, `scroll`, `type_text`, `press_key` | Operate the desktop and return the screen once it settles |
| `screenshot` | Look again after waiting |
| `request_secret` | Ask the user for a password or code in the viewer window |
| `view` | Open the viewer window |

Every screen action returns the screen after the action. The server keeps a VNC connection open and its own copy of the screen current, so it knows when the screen last changed. After an action it waits up to a second for a change, then until the screen has been still for 300 ms, at most 5 s in total. If nothing changed it says so and sends no image, which saves the agent about 1,400 tokens per call. A screen action takes 300 to 700 ms against a LAN host.

`type_text` presses keys for ASCII and pastes everything else through the VNC clipboard, then empties the clipboard. Keystrokes for characters outside the keyboard map are unreliable: Xvnc binds them to spare keycodes and reuses those, and Chromium typed 日 for 京 after the keycode had moved on.

### Signing in for the user

An agent that needs the user's password, for example to sign in to Google in the sandbox's browser, calls `request_secret`. The viewer window opens if it is not already open and shows a masked field with the agent's reason. The value stays in the memory of the `sbx mcp` process, under a name the agent chose, until the process exits. The agent then calls `type_text {"secret": "google.password"}`, and the value reaches the sandbox only as input. It is never in a tool result, so it stays out of the agent's context, the transcript, and the provider's logs.

The screen is not filtered. A secret typed into a field that shows its text appears in the next screenshot, so agents are told to type secrets only into password fields. A full Google sign-in with a real account has not been tried. Google's sign-in page accepts the typed address, but Google may refuse a browser it considers automated, and the `browser` template's Chromium runs with its DevTools port open.

The viewer page can drive the sandbox and answer secret prompts, so its URL carries a random token. Other web pages the user opens cannot guess the address.

## Templates

| Template | Content |
|---|---|
| `default` | Debian 13, Node.js 24 with pnpm, Go 1.27, Python 3.13 with uv, build-essential, git, ripgrep, jq, ffmpeg, ImageMagick, Chromium, Noto CJK fonts (Japanese, Traditional and Simplified Chinese, Korean) and emoji, and the desktop. 2 vCPUs, 2 GiB memory, 16 GiB disk |
| `browser` | The same image with Chromium already open on the desktop, its DevTools protocol on port 9222 |

Commands run as `user` (uid 1000, passwordless sudo) in `/home/user`. Versions are pinned at the top of `templates/default/Dockerfile`.

To drive the browser from outside, and watch it:

```sh
id=$(sbx create browser --view)
sbx forward $id 9222 &
# Playwright: chromium.connectOverCDP('http://127.0.0.1:9222')
```

Make your own template from any Debian or Ubuntu based Dockerfile, or save a prepared sandbox:

```sh
sbx template build myapp ./my-template-dir --memory 4096 --warmup 'sbx-desktop && cd /srv && ./start-db'
sbx snapshot $id repo-ready          # memory, processes, and files of a running sandbox
sbx create repo-ready
sbx fork $id                         # the same, as a one-off clone
```

`--warmup` runs in the booted VM before the snapshot, so whatever it starts is already running in every sandbox. Images based on `templates/default` get a desktop by running `sbx-desktop` there.

## Parts

| Part | Where it runs | What it does |
|---|---|---|
| `sbx` | where agents run | CLI. Talks HTTP to sbxd with a bearer token, serves the screen viewer, and serves MCP with `sbx mcp` |
| `sbxd` | sbx host | Daemon. Restores sandboxes, builds templates, publishes ports, removes idle sandboxes |
| `sbx-agent` | inside every sandbox | PID 1. Builds the root filesystem, then serves exec, files, and TCP over vsock |
| `templates/default` | sbx host | Dockerfile of the default image |
| `kernel/` | sbx host | Guest kernel config and build script |
| `deploy/` | sbx host | Installer and systemd unit. `deploy/proxmox` holds the optional Proxmox setup |
| `skill/sbx/SKILL.md` | where agents run | Skill for Claude Code, Grok, and other agents that read `~/.claude/skills` |

## How a sandbox is made

A template is a directory under `/var/lib/sbx/templates/NAME`:

| File | Content |
|---|---|
| `rootfs.ext4` | The image, attached read-only to every sandbox |
| `upper.ext4` | A sparse writable disk, mounted as the overlay's upper layer |
| `vmstate`, `mem` | The Firecracker snapshot of the booted guest |

`sbx template build` turns a Dockerfile into a template. podman builds the image, the image is exported to `rootfs.ext4` with `sbx-agent` as init, a microVM boots it, `--warmup` runs, and the VM is snapshotted once its free memory has stopped changing for 2 seconds. Whatever the warmup started keeps loading after it returns, and every page touched after the snapshot is copied into each clone: snapshotting the desktop mid-startup cost every sandbox 70 MiB.

`sbx create` then:

1. Takes a network namespace that sbxd prepared ahead of time.
2. Creates a jail directory with hard links to the template's `rootfs.ext4`, `vmstate`, and `mem`, plus a copy of `upper.ext4` that costs only the blocks the sandbox writes.
3. Starts Firecracker through the jailer, as its own unprivileged user, chrooted into the jail, in the namespace.
4. Loads the snapshot. Memory is mapped privately from `mem`, so clones share the template's pages through the page cache and pay only for pages they write.
5. Sends the agent the current time and the sandbox's hostname, because a restored guest keeps the snapshot's clock.

The jail matters for more than isolation: a snapshot records file paths, and the chroot gives every clone the same paths.

### Network and isolation

Every clone has the same guest IP, MAC, and tap name, because they are copies of one snapshot. Each lives in its own network namespace with an identical tap and a permanent neighbor entry. The namespace NATs the guest to a unique veth address, and the host NATs that to the internet. The lab measured 30 s outages with a changed MAC and a 1 s ARP wait without the neighbor entry.

sbxd's nftables table `inet sbx` drops anything a sandbox sends to the host or to private, link-local (including cloud metadata at 169.254.169.254), or multicast ranges, and all IPv6. Docker and ufw set the iptables FORWARD policy to DROP, so sbxd also adds accept rules for sandbox traffic there; the private ranges stay blocked by its own table. Other firewalls, such as firewalld, must allow forwarding from the `sbxv*` interfaces.

Nothing can connect into a sandbox's network. `sbx expose`, `sbx forward`, and `sbx view` tunnel connections through the agent over vsock, so a server inside may listen on localhost.

A guest that escapes Firecracker and its jail is a process on the host. For a second wall, run sbx in a VM whose own firewall blocks the LAN, as the Proxmox setup does.

## Measured

On a Proxmox VM with 8 vCPUs and 8 GiB of memory on an i5-13400F, so with nested virtualization. Sandboxes use the default template, 2 vCPUs and 2 GiB.

| What | Result |
|---|---|
| `sbx create`, request to guest agent answering | 32 to 42 ms |
| 10 `sbx create` at once | 54 to 106 ms each |
| Memory held by an idle sandbox, desktop included | 5 MiB, plus what it writes |
| Memory held by an idle `browser` sandbox | about 48 MiB |
| `sbx snapshot` | 1.0 s |
| `sbx fork` | 2.2 s |
| Cold boot during a template build, launch to agent answering | 270 ms |
| Playwright `connectOverCDP` from the LAN, new page, screenshot, navigate to example.com | 0.5 s |

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
| `GET /v1/sandboxes/{id}/tcp?port=` | | `101 Switching Protocols`, then raw TCP. Port 5900 is the screen's VNC |
| `POST /v1/sandboxes/{id}/ports` | `{"port"}` | `{"port", "host_port", "url"}` |
| `DELETE /v1/sandboxes/{id}/ports/{port}` | | |
| `POST /v1/sandboxes/{id}/snapshot` | `{"name"}` | the template |
| `POST /v1/sandboxes/{id}/fork` | | the new sandbox |
| `GET /v1/templates` | | list of templates |
| `POST /v1/templates/{name}?cpus=&memory_mib=&disk_gib=&warmup=` | tar of the build context | frame stream of the build log |
| `DELETE /v1/templates/{name}` | | |

Durations are strings such as `"30m"` or `"2d"`. `"0"` disables the idle timeout. Any sandbox request counts as activity unless it has `passive=1`.

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

Packer builds machine images for hypervisors and clouds. A Firecracker template is a snapshot of a running VM, which no Packer builder produces. sbxd builds it from a Dockerfile, which is also the format agents and people already write.

## Limits

- x86_64 only. The guest kernel, the agent, and the Firecracker download are built for it.
- No GPUs and no live shared folders. Firecracker has neither. Copy files with `sbx cp`.
- The snapshot memory file is the template's full memory size on disk, 2 GiB for the default template.
- No huge pages. The lab measured faster cold boots with them on a nested host, but snapshot restore from a file needs normal pages.
- An exposed port is open to your network without authentication. Use `sbx forward` for anything sensitive, such as the browser's DevTools port.
