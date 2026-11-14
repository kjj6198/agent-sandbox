English | [繁體中文](README.zh-TW.md)

# sbx

An open-source, blazing fast sandbox for agents.

`sbx` gives an AI agent a fresh, isolated Linux machine in tens of milliseconds, lets it run code and drive a desktop or browser, and throws the machine away when done. Each sandbox is a Firecracker microVM restored from a snapshot of a booted template, with its own kernel, a writable disk, a `1280x800` desktop you can watch, egress allowlists, and an MCP server with computer-use tools.

```sh
id=$(sbx create --view)                # a fresh machine in tens of milliseconds, its screen in a small window
sbx cp ./app $id:app
sbx exec $id 'cd app && pnpm install && pnpm test'
sbx exec -d $id 'cd app && pnpm dev --port 5173'
sbx expose $id 5173                    # http://sbx-host:30000
sbx rm $id
```

## Why sbx

- **32 to 42 ms create time.** `sbx create` restores a running Firecracker microVM from a snapshot instead of booting. Ten sandboxes created at once finish in 54 to 106 ms each.
- **5 MiB per idle sandbox.** Clones map the template's memory privately through the host page cache and share a read-only root image, paying only for memory pages and disk blocks they write. An idle `browser` sandbox with Chromium open uses about 48 MiB.
- **Hardware-virtualized isolation.** Every sandbox runs its own guest kernel inside Firecracker's jailer as a dedicated unprivileged user, chrooted inside its own network namespace. Host, LAN, cloud metadata at `169.254.169.254`, multicast, and IPv6 traffic are dropped outside the VM.
- **Designed for agent shells and tool calls.** Commands mirror Docker (`exec`, `cp`, `rm`, `ls`), `create` prints only the sandbox ID, `exec` exits with the command's exit status, and read commands accept `--json`. Copy `skill/sbx/` to `~/.claude/skills/` for Claude Code and Grok, or connect over MCP with `sbx mcp`.
- **Live desktop and browser included.** Every sandbox starts with a `1280x800` X desktop on `DISPLAY=:0`, a live noVNC viewer window, still-image screenshot endpoints, and optional pre-launched Chromium with CDP on port 9222.
- **Snapshot and fork in place.** Save a running sandbox's memory, processes, and filesystem as a reusable template in 1.0 s with `sbx snapshot`, or clone it in 2.2 s with `sbx fork`.

## Feature tour

### Desktop and live viewer

Every sandbox runs a `1280x800` X desktop serving VNC inside the guest, with openbox, a bottom dock, an open terminal window, Noto CJK and emoji fonts, and fcitx5 input methods for Japanese (Mozc), Traditional Chinese (Chewing), and Simplified Chinese (Pinyin). Commands run with `DISPLAY=:0`, so any GUI app an agent starts appears on screen.

`sbx view ID` or `sbx create --view` opens a live noVNC window at the top right of your screen. It starts in watch mode so stray clicks cannot disturb the agent; click "Watching" to take over the mouse and keyboard, or drop files onto the window to copy them into `~/Downloads`. Watching is passive and does not reset the idle timeout. You can also grab screenshots directly or embed the viewer in another web page:

```sh
sbx screenshot $id                     # ./sbx-$id.png
sbx view --no-open --listen 127.0.0.1:7681 $id
```

### MCP and computer use

`sbx mcp` runs an MCP server over stdio for agents that work through tool calls rather than a shell:

```sh
claude mcp add sbx -- sbx mcp
```

It provides lifecycle and execution tools (`sandbox_create`, `sandbox_list`, `sandbox_remove`, `exec`, `view`) alongside desktop actions (`click`, `move_mouse`, `drag`, `scroll`, `type_text`, `press_key`, `screenshot`, `request_secret`). After each screen action, `sbx mcp` waits up to 1 s for a change and then until the screen has been still for 300 ms, at most 5 s total, taking 300 to 700 ms against a LAN host. If nothing changed, it sends no image, saving about 1,400 tokens per call.

The `default` template also ships [cua-driver](https://github.com/trycua/cua/tree/main/libs/cua-driver), an in-sandbox MCP server that reads the desktop's accessibility tree so agents can interact with apps by menu and control name instead of pixel coordinates:

```sh
claude mcp add cua -- sbx exec -i $id cua-driver mcp
```

### Egress allowlist

By default a sandbox can reach the public internet. `--allow` restricts outbound traffic to specific hosts or wildcard subdomains, and `--offline` cuts off internet access completely:

```sh
id=$(sbx create --allow github.com,codeload.github.com --allow '*.npmjs.org')
sbx allow $id example.com              # takes effect for new connections at once
sbx run --offline -- python3 untrusted.py
```

The rules live in the sandbox's network namespace on the host, outside the VM, so nothing in the guest can bypass them, not even `root`. `sbxd` answers DNS only for allowed names and relays HTTP and HTTPS on ports 80 and 443 after checking the HTTP `Host` header or TLS SNI. TLS remains end to end. Blocked hosts are reported at the end of a failed `exec` and listed in `sbx inspect`.

### Environments and secrets

An environment is a named set of variables and files stored on the `sbx` host under `/etc/sbx/environments` (`0600` in a `0700` directory). Clients can start sandboxes already signed in to Claude Code, Codex, or GitHub without holding the credentials themselves. New sandboxes load the `base` environment automatically when it exists:

```sh
sbx env set base CLAUDE_CODE_OAUTH_TOKEN GH_TOKEN
sbx env file base ~/.codex/auth.json --to /home/user/.codex/auth.json
sbx env ls                             # lists variable names and file paths, never values
id=$(sbx create)                       # starts with base
id=$(sbx create --env-name none)       # starts without any
```

`sbx snapshot` refuses sandboxes that started with an environment so secrets cached in process memory or on disk never leak into a shared template. For interactive browser sign-ins over MCP, `request_secret` asks the user in a masked field in the viewer window and holds the secret in `sbx mcp` memory; the agent types it with `type_text {"secret": "google.password"}` so the value stays out of the agent context, transcript, and provider logs.

### Templates, snapshots, and forks

Templates are built from Dockerfiles, booted once in a microVM, warmed up with `--warmup`, and snapshotted once free memory settles for 2 seconds:

- `default`: Debian 13, Node.js 24 with pnpm, Go 1.27, Python 3.13 with uv, build-essential, git, ripgrep, jq, ffmpeg, ImageMagick, Chromium, `cua-driver`, Noto CJK and emoji fonts, Japanese and Chinese input methods, Claude Code, Codex, `gh`, and the desktop. 2 vCPUs, 2 GiB memory, 16 GiB disk.
- `browser`: the `default` image with Chromium already open on the desktop and its DevTools protocol on port 9222.
- `ubuntu`: Ubuntu 24.04 with the tools of a boat.dev machine, including everything in `default`, Google Chrome as `chromium`, Rust, Ruby 4, PHP 8.4 with Composer, Java 11 with Maven and Gradle, Kotlin, Scala 3 with sbt, .NET 8, Elixir, R, Bun, Deno, Poetry, clang, opencode, pi, and the HinaCode server.

Build custom templates from a Dockerfile, drive the `browser` template over CDP, or snapshot and fork running sandboxes:

```sh
sbx template build myapp ./my-template-dir --memory 4096 --warmup 'sbx-desktop && cd /srv && ./start-db'
sbx snapshot $id repo-ready            # memory, processes, and files of a running sandbox
sbx create repo-ready
sbx fork $id                           # one-off clone of a running sandbox
```

### HinaCode integration

A sandbox created from the `ubuntu` template includes the [HinaCode](https://github.com/hinadev-ai/hinacode) server and can run as a remote HinaCode machine. Because nothing routes directly into a sandbox, the server binds to loopback and connects out through HinaCode's relay. Clients can also attach labels such as `{"app": "hinacode", "thread": "t1"}` at create time and query `GET /v1/sandboxes?label=KEY=VALUE` to find their sandboxes again after a restart:

```sh
sbx template build ubuntu /usr/local/share/sbx/templates/ubuntu --warmup sbx-desktop
id=$(sbx create ubuntu --ttl 0)
sbx exec -d $id 'hinacode serve --host 127.0.0.1 --relay >~/hinacode.log 2>&1'
sbx exec $id hinacode pair
```

## Measured performance and research

Measured on a Proxmox VM with 8 vCPUs and 8 GiB of RAM on an Intel i5-13400F with nested virtualization, using the `default` template (2 vCPUs, 2 GiB RAM):

| Operation | Result |
|---|---|
| `sbx create`, request to guest agent answering | 32 to 42 ms |
| 10 `sbx create` at once | 54 to 106 ms each |
| Memory held by an idle sandbox, desktop included | 5 MiB, plus what it writes |
| Memory held by an idle `browser` sandbox | about 48 MiB |
| `sbx snapshot` | 1.0 s |
| `sbx fork` | 2.2 s |
| Cold boot during a template build, launch to agent answering | 270 ms |
| Playwright `connectOverCDP` from the LAN, new page, screenshot, navigate to example.com | 0.5 s |

Why Firecracker snapshots? Before building `sbx`, we evaluated Firecracker, gVisor, and Cloud Hypervisor on the same Proxmox server ([research/README.md](research/README.md), [full report](research/agent-sandbox-isolation.md)). A restored Firecracker snapshot answered its first command in 29 ms inside the VM and 20 ms on bare metal, 20 clones restored at once cost about 9 MiB of memory each, and workloads inside ran as fast as in a plain container. Cloud Hypervisor nearly tied on latency, but every restored clone loaded its full memory allocation. gVisor started fastest from scratch, but agent workloads ran about twice as slowly inside it.

## Requirements and install

The `sbx` host requires an `x86_64` Linux machine with `/dev/kvm`: bare metal, or a VM with nested virtualization such as a supported cloud instance or Proxmox VM. The installer handles Debian and Ubuntu and lists the prerequisite commands on other distributions. The `sbx` CLI runs on macOS and Linux and only needs HTTP access to the host.

From a checkout with Go installed, install the daemon, Firecracker, guest kernel, and `default` and `browser` templates on the host:

```sh
make install                                   # on the sbx host itself
make install HOST=me@sbx-host                  # or from another machine, over ssh
make install HOST=me@sbx-host INSTALL_ENV=SBX_DATA_DISK=/dev/sdb
```

Then, on each machine that runs agents:

```sh
make cli && cp bin/sbx ~/.local/bin/
ssh sbx-host sudo cat /etc/sbx/token | sbx login http://sbx-host:7070
sbx status
```

## Documentation

| Document | Contents |
|---|---|
| [docs/guide.md](docs/guide.md) | Full usage guide, installation options, viewer, MCP, templates, egress allowlist, environments, HTTP API, configuration, and limits |
| [docs/architecture.md](docs/architecture.md) | Architecture of `sbx`, `sbxd`, and `sbx-agent`, CLI design principles, vsock framing, snapshot lifecycle, networking, and security boundaries |
| [deploy/proxmox/README.md](deploy/proxmox/README.md) | Running `sbx` inside an isolated VM on Proxmox with its own firewall and port forwarding |
| [skill/sbx/SKILL.md](skill/sbx/SKILL.md) | Agent skill for Claude Code, Grok, and other agents that load `~/.claude/skills` |
| [research/README.md](research/README.md) | Evaluation comparing Firecracker, gVisor, and Cloud Hypervisor, with benchmarks, raw data, and reproduction steps |

## Status

> [!WARNING]
> HIGHLY EXPERIMENTAL, USE AT YOUR OWN RISK!
