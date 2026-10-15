---
name: sbx
description: Run code, builds, tests, dev servers, or a browser in a disposable, isolated Linux microVM with the sbx CLI. Use when a task needs to execute untrusted or generated code, install packages, try something destructive, run a web server and look at it, or drive Chromium, without touching the local machine.
---

# sbx

`sbx` creates Firecracker microVMs in about 30 ms. Each one is a Debian machine with Node.js 24 and pnpm, Go, Python 3.13 and uv, build tools, ffmpeg, ImageMagick, Chromium, CJK fonts, and a desktop. It reaches the internet but not the LAN. Commands run as `user` with passwordless sudo, in `/home/user`.

Run `sbx --help` and `sbx <command> --help` for every option.

## Workflow

```sh
id=$(sbx create)                         # prints only the ID
sbx cp ./project $id:project             # relative paths are under /home/user
sbx exec $id 'cd project && pnpm install && pnpm test'
sbx cp $id:project/out ./out
sbx rm $id
```

One-off command: `sbx run -- python3 -c 'print(1)'` creates, runs, and removes a sandbox.

## Rules

- `sbx exec` exits with the command's exit code and streams its stdout and stderr. Exit code 125 means sbx itself failed, 124 means `--timeout` fired.
- One argument after the ID is a bash script (`sbx exec $id 'a && b'`). Several arguments are an argv (`sbx exec $id -- ls -la`).
- Long-running servers: `sbx exec -d $id 'cd app && pnpm dev'` returns the PID and a log path. Read the log with `sbx exec $id 'tail -50 /tmp/sbx-PID.log'`.
- To open a server from outside: `sbx expose $id PORT` prints a LAN URL. `sbx forward $id PORT` tunnels it to 127.0.0.1 instead.
- Use `--timeout` on commands that might hang.
- Sandboxes are removed after 1 hour without activity. Change it with `sbx ttl $id 4h`, or `0` to keep. Remove sandboxes you no longer need.
- Save a prepared machine with `sbx snapshot $id NAME`, then `sbx create NAME`. Try alternatives from one state with `sbx fork $id`.

## Screen

Every sandbox has a desktop with a terminal already open, and GUI programs you start appear on it. When the user wants to watch, create with `sbx create --view`, or run `sbx view $id` later. It opens a small window on the user's screen and returns at once. Files the user drops on that window land in `~/Downloads` in the sandbox.

To see the screen yourself, or to show it somewhere else, `sbx screenshot $id [FILE]` saves the desktop as a PNG (`-` writes the bytes to stdout; `--width 640` scales it, `--format jpeg` shrinks it). Read the file back as an image to check what a GUI program is showing. A running viewer also serves the image as `screenshot.png` and its size as `screen.json` under the URL `sbx view` prints, for pages that render it.

To click and type on the desktop, or to sign in with the user's password, use the `sbx mcp` server's tools if they are available. Each action returns the screen, and `request_secret` asks the user for a password without you ever seeing it. Never ask the user to paste a password into the chat.

## Browser

```sh
id=$(sbx create browser --view)          # Chromium is already open; the user watches it
sbx forward $id 9222 &                   # DevTools protocol on 127.0.0.1:9222
```

Connect with Playwright `chromium.connectOverCDP('http://127.0.0.1:9222')` or Puppeteer `puppeteer.connect({ browserURL: 'http://127.0.0.1:9222' })`. Stop the forward and `sbx rm $id` when done.
