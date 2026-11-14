"""compat: feature checks inside each backend, plus the two snapshot-restore hazards (clock skew, RNG state)."""
import time
from pathlib import Path

from playwright.sync_api import sync_playwright

from .backends import BACKENDS, SLOTS, Backend, Sandbox
from .probe import agent, agent_session, now_ns, poll_connect
from .records import CompatRecord, append
from .spawn import RESTORE_TARGETS, TIMEOUT_S, boot, ensure_snapshots, short_error, snapshot_dir

ARTIFACTS = Path("/opt/sbx/results/artifacts")

INOTIFY_JS = r"""
const fs = require('fs');
fs.mkdirSync('/work/watch', {recursive: true});
const timer = setTimeout(() => { console.log('no event'); process.exit(1); }, 2000);
fs.watch('/work/watch', (ev, name) => { console.log(`event ${ev} ${name}`); clearTimeout(timer); process.exit(0); });
setTimeout(() => fs.writeFileSync('/work/watch/f', 'x'), 100);
"""

SHELL_CHECKS = {
    "unshare-userns-agent": "runuser -u agent -- unshare -Ur true && echo agent-userns-ok",
    "unshare-userns-root": "unshare -Ur true && echo root-userns-ok",
    "inotify-fs-watch": f"node - <<'EOF'\n{INOTIFY_JS}\nEOF",
    "ping-lab": "ping -c 3 -W 2 {host}",
    "ping-internet": "ping -c 3 -W 2 1.1.1.1",
    "mount-tmpfs": "mkdir -p /mnt/t && mount -t tmpfs tmpfs /mnt/t && findmnt /mnt/t && umount /mnt/t",
    "apt-get-install": "apt-get update -qq 2>&1 | tail -3 && apt-get install -y -qq --no-install-recommends jq "
                       ">/tmp/apt.log 2>&1; rc=$?; tail -3 /tmp/apt.log; jq --version; [ $rc = 0 ]",
}


def transcript(b: Backend, check: str, text: str) -> None:
    with (ARTIFACTS / f"compat-{b.id}.log").open("a") as f:
        f.write(f"===== {check}\n{text}\n")


def record(out: Path, b: Backend, check: str, ok: bool, detail: str) -> None:
    transcript(b, check, detail)
    append(out, CompatRecord(b.id, check, ok, detail[-2000:]))
    print(f"compat {b.id} {check}: {'PASS' if ok else 'FAIL'} {detail.strip().splitlines()[-1] if detail.strip() else ''}"[:200],
          flush=True)


def shell_checks(out: Path, b: Backend, sb: Sandbox) -> None:
    for check, cmd in SHELL_CHECKS.items():
        try:
            rc, text = agent(sb.guest_ip, cmd.replace("{host}", sb.host_ip), timeout=300)
            record(out, b, check, rc == 0, f"$ {cmd.splitlines()[0]}\n{text}\nrc={rc}")
        except Exception as e:
            record(out, b, check, False, short_error(e))


def chromium_sandbox_check(out: Path, b: Backend, sb: Sandbox, pw) -> None:
    """Starts Chromium with its own sandbox requested and reads chrome://sandbox."""
    try:
        rc, text = agent(sb.guest_ip, "sbx-browser 2>&1", timeout=60)
        if "SBX_BROWSER ready" not in text:
            record(out, b, "chromium-own-sandbox", False, f"sbx-browser (sandbox requested) failed rc={rc}:\n{text}")
            return
        browser = pw.chromium.connect_over_cdp(f"http://{sb.guest_ip}:9223", timeout=20000)
        page = browser.contexts[0].new_page()
        page.goto("chrome://sandbox")
        status = " | ".join(l.strip() for l in page.inner_text("body").splitlines() if l.strip())
        page.close()
        browser.close()
        record(out, b, "chromium-own-sandbox", "adequately sandboxed" in status, status)
    except Exception as e:
        record(out, b, "chromium-own-sandbox", False, short_error(e))


def live_checks(args, out: Path, listener) -> None:
    with sync_playwright() as pw:
        for bid in args.backends:
            b, sb = boot(bid, SLOTS[bid], listener)
            try:
                chromium_sandbox_check(out, b, sb, pw)
                if bid in ("firecracker", "cloud-hypervisor", "runsc", "runc"):
                    shell_checks(out, b, sb)
            finally:
                b.stop(sb)


def first_command_after_restore(b: Backend, slot: int, src: Path, variant: str, cmd: str) -> tuple[str, int, int]:
    """Restores and runs cmd as the very first agent command. Returns output and lab wall-clock bounds."""
    prep = b.prepare_restore(slot, src, variant)
    sb = b.restore(prep)
    try:
        s = poll_connect(sb.guest_ip, 7000, now_ns() + TIMEOUT_S * 10**9)
        with s:
            before = time.time_ns()
            rc, text = agent_session(s, cmd, timeout=30)
            after = time.time_ns()
        if rc != 0:
            raise RuntimeError(f"rc={rc}: {text}")
        return text.strip(), before, after
    finally:
        b.stop(sb)


def restore_hazards(args, out: Path, listener) -> None:
    targets = [t for t in RESTORE_TARGETS if t[0] in args.backends]
    dirs = {t: snapshot_dir(*t, with_browser=False) for t in targets}
    missing = [t for t, d in dirs.items() if not d.exists()]
    if missing:
        dirs.update(ensure_snapshots(missing, listener, out.with_name("compat-snapshots.jsonl")))
    for (bid, variant), d in dirs.items():
        b, slot = BACKENDS[bid], SLOTS[bid]
        name = f"{bid}/{variant}"
        age_s = time.time() - (d / "slot").stat().st_mtime
        if age_s < 120:
            time.sleep(120 - age_s)
            age_s = time.time() - (d / "slot").stat().st_mtime
        try:
            text, before, after = first_command_after_restore(b, slot, d, variant, "date +%s.%N")
            skew_s = float(text) - (before + after) / 2e9
            record(out, b, f"clock-skew-after-restore[{variant}]", abs(skew_s) < 1.0,
                   f"{name}: snapshot age {age_s:.0f} s, guest minus lab = {skew_s:+.3f} s "
                   f"(lab round trip {(after - before) / 1e6:.1f} ms)")
        except Exception as e:
            record(out, b, f"clock-skew-after-restore[{variant}]", False, f"{name}: {short_error(e)}")
        try:
            cmd = "head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \\n'"
            first = first_command_after_restore(b, slot, d, variant, cmd)[0]
            second = first_command_after_restore(b, slot, d, variant, cmd)[0]
            same = first == second
            record(out, b, f"urandom-after-restore[{variant}]", not same,
                   f"{name}: {'IDENTICAL' if same else 'different'}\n1st {first}\n2nd {second}")
        except Exception as e:
            record(out, b, f"urandom-after-restore[{variant}]", False, f"{name}: {short_error(e)}")


def fc_clock_realtime(args, out: Path, listener) -> None:
    """Documents whether Firecracker accepts clock_realtime on this lab's kvm-clock host."""
    if "firecracker" not in args.backends:
        return
    b = BACKENDS["firecracker"]
    d = snapshot_dir("firecracker", "file", False)
    clocksource = Path("/sys/devices/system/clocksource/clocksource0/current_clocksource").read_text().strip()
    b.clock_realtime = True
    try:
        text, before, after = first_command_after_restore(b, SLOTS["firecracker"], d, "file", "date +%s.%N")
        skew = float(text) - (before + after) / 2e9
        record(out, b, "clock_realtime-restore", True, f"host clocksource {clocksource}: accepted, skew {skew:+.3f} s")
    except Exception as e:
        record(out, b, "clock_realtime-restore", False, f"host clocksource {clocksource}: {short_error(e)}")
    finally:
        b.clock_realtime = False
        b.cleanup(SLOTS["firecracker"])


def compat(args, out: Path, listener) -> None:
    live_checks(args, out, listener)
    restore_hazards(args, out, listener)
    fc_clock_realtime(args, out, listener)
