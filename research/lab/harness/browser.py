"""browser-cold and browser-restore: drive Chromium in the sandbox over CDP from the lab, like an agent would."""
import os
import time
from pathlib import Path

from playwright.sync_api import sync_playwright

from .backends import BACKENDS, SLOTS, Backend, Sandbox
from .probe import agent_ok, http_get, ms, now_ns, pss_rss_kib, steal_ticks, ticks_to_ms
from .records import BrowserRecord, SpawnRecord, append
from .spawn import RESTORE_TARGETS, TIMEOUT_S, ensure_snapshots, short_error, spawn_once, warm

ARTIFACTS = Path("/opt/sbx/results/artifacts")
PUBLIC_URL = "https://example.com"
IDLE_S = 2.0
# Drive the tab that already exists (Chromium's first tab, or the warm tab kept in the snapshot)
# instead of opening a new one, which costs a renderer process start.
REUSE_TAB = os.environ.get("SBX_REUSE_TAB") == "1"


def local_url(sb: Sandbox) -> str:
    return f"http://{sb.host_ip}:8000/index.html"


def launch_browser(b: Backend, sb: Sandbox) -> None:
    flag = "" if b.chrome_sandbox else " --no-sandbox"
    agent_ok(sb.guest_ip, f"nohup sbx-browser{flag} >/tmp/sbx-browser.out 2>&1 &")


def drive(pw, b: Backend, sb: Sandbox, rec: BrowserRecord, t0: int, cdp_interval_s: float, shot: Path | None) -> None:
    deadline = t0 + TIMEOUT_S * 10**9
    http_get(sb.guest_ip, 9223, "/json/version", deadline, cdp_interval_s)
    rec.t_cdp_ms = ms(now_ns(), t0)
    browser = pw.chromium.connect_over_cdp(f"http://{sb.guest_ip}:9223", timeout=TIMEOUT_S * 1000)
    try:
        rec.t_connected_ms = ms(now_ns(), t0)
        ctx = browser.contexts[0] if browser.contexts else browser.new_context()
        page = ctx.pages[0] if REUSE_TAB and ctx.pages else ctx.new_page()
        page.set_viewport_size({"width": 1280, "height": 800})
        page.goto(local_url(sb), wait_until="load", timeout=TIMEOUT_S * 1000)
        rec.t_local_page_ms = ms(now_ns(), t0)
        if shot:
            page.screenshot(path=str(shot))
            rec.screenshot_path = str(shot)
        else:
            page.screenshot()
        rec.t_screenshot_ms = ms(now_ns(), t0)
        page.goto(PUBLIC_URL, wait_until="load", timeout=TIMEOUT_S * 1000)
        rec.t_public_page_ms = ms(now_ns(), t0)
    finally:
        browser.close()


def idle_pss(b: Backend, sb: Sandbox) -> int:
    time.sleep(IDLE_S)
    return pss_rss_kib(b.pids(sb))[0]


def screenshot_path(bid: str, mode: str, variant: str | None, trial: int, saved: set) -> Path | None:
    key = (bid, mode, variant)
    if trial < 0 or key in saved:
        return None
    saved.add(key)
    prefix = "browser-tab" if REUSE_TAB else "browser"
    return ARTIFACTS / f"{prefix}-{mode}-{bid}{'-' + variant if variant else ''}.png"


def browser_cold(args, out: Path, listener) -> None:
    saved: set = set()
    for trial in range(-args.warmup, args.trials):
        for bid in args.backends:
            # A fresh driver per trial: Node's keep-alive pool would otherwise reuse a socket to a dead guest.
            with sync_playwright() as pw:
                rec = browser_cold_trial(pw, bid, trial, listener, saved)
            append(out, rec)
            print(f"browser-cold {bid} trial {trial}: cdp={rec.t_cdp_ms} shot={rec.t_screenshot_ms} "
                  f"public={rec.t_public_page_ms} ok={rec.ok} {rec.error or ''}"[:300], flush=True)
            time.sleep(args.gap)


def browser_cold_trial(pw, bid: str, trial: int, listener, saved: set) -> BrowserRecord:
    b, slot = BACKENDS[bid], SLOTS[bid]
    rec = BrowserRecord(bid, "cold", trial, warmup=trial < 0, chrome_sandbox=b.chrome_sandbox)
    spawn = SpawnRecord(bid, "cold", None, trial, 1, slot)
    s0 = steal_ticks()
    launch = spawn_once(b, b.prepare(slot), listener, spawn, pss_delay_s=0, keep=True)
    try:
        if not spawn.ok:
            raise RuntimeError(f"spawn failed: {spawn.error}")
        rec.t_exec_ms = spawn.t_exec_ms
        launch_browser(b, launch.sb)
        drive(pw, b, launch.sb, rec, launch.sb.t0_ns, 0.010, screenshot_path(bid, "cold", None, trial, saved))
        rec.steal_ms = ticks_to_ms(steal_ticks() - s0)
        rec.pss_kib_browser_idle = idle_pss(b, launch.sb)
        rec.ok = True
    except Exception as e:
        rec.error = short_error(e, b.console(launch.sb) if launch.sb else "")
        rec.steal_ms = ticks_to_ms(steal_ticks() - s0)
    launch.teardown()
    return rec


def drain_cdp_connections(sb: Sandbox, timeout_s: float = 120) -> None:
    """Waits until the guest holds no CDP-port sockets, so no stale TCP state lands in the snapshot."""
    deadline = time.monotonic() + timeout_s
    # /proc/net/tcp rather than ss: gVisor has no sock_diag netlink. 0x2406/0x2407 = 9222/9223, 0A = LISTEN.
    cmd = ("cat /proc/net/tcp /proc/net/tcp6 2>/dev/null "
           "| awk '$4 != \"0A\" && ($2 ~ /:240[67]$/ || $3 ~ /:2406$/)' | wc -l")
    while int(agent_ok(sb.guest_ip, cmd).strip()) > 0:
        if time.monotonic() > deadline:
            raise TimeoutError("CDP sockets still open in guest: " + agent_ok(sb.guest_ip, "cat /proc/net/tcp"))
        time.sleep(2)


def warm_up_browser(b: Backend, sb: Sandbox) -> None:
    """Snapshot preparation: Chromium up, one navigation to the local page, every CDP connection closed.

    Runs its own Playwright driver and shuts it down, because the driver keeps HTTP connections alive.
    """
    launch_browser(b, sb)
    http_get(sb.guest_ip, 9223, "/json/version", now_ns() + TIMEOUT_S * 10**9, 0.010)
    with sync_playwright() as pw:
        browser = pw.chromium.connect_over_cdp(f"http://{sb.guest_ip}:9223", timeout=TIMEOUT_S * 1000)
        page = browser.contexts[0].new_page()
        page.goto(local_url(sb), wait_until="load")
        page.close()
        browser.close()
    drain_cdp_connections(sb)


def warm_up_browser_keep_tab(b: Backend, sb: Sandbox) -> None:
    """Like warm_up_browser, but the first tab stays open on the local page and goes into the snapshot."""
    launch_browser(b, sb)
    http_get(sb.guest_ip, 9223, "/json/version", now_ns() + TIMEOUT_S * 10**9, 0.010)
    with sync_playwright() as pw:
        browser = pw.chromium.connect_over_cdp(f"http://{sb.guest_ip}:9223", timeout=TIMEOUT_S * 1000)
        page = browser.contexts[0].pages[0]
        page.set_viewport_size({"width": 1280, "height": 800})
        page.goto(local_url(sb), wait_until="load")
        page.screenshot()
        browser.close()
    drain_cdp_connections(sb)


warm_up_browser_keep_tab.snapshot_tag = "browser-tab"


def browser_restore(args, out: Path, listener) -> None:
    targets = [t for t in RESTORE_TARGETS if t[0] in args.backends]
    saved: set = set()
    prepare = warm_up_browser_keep_tab if REUSE_TAB else warm_up_browser
    dirs = ensure_snapshots(targets, listener, out.with_name(out.stem + "-snapshots.jsonl"), prepare)
    warm(dirs)
    for trial in range(-args.warmup, args.trials):
        for bid, variant in dirs:
            with sync_playwright() as pw:
                rec = browser_restore_trial(pw, bid, variant, dirs[(bid, variant)], trial, saved)
            append(out, rec)
            print(f"browser-restore {bid}/{variant} trial {trial}: cdp={rec.t_cdp_ms} shot={rec.t_screenshot_ms} "
                  f"public={rec.t_public_page_ms} ok={rec.ok} {rec.error or ''}"[:300], flush=True)
            time.sleep(args.gap)


def browser_restore_trial(pw, bid: str, variant: str, src: Path, trial: int, saved: set) -> BrowserRecord:
    b, slot = BACKENDS[bid], SLOTS[bid]
    rec = BrowserRecord(bid, "restore", trial, restore_variant=variant, warmup=trial < 0,
                        chrome_sandbox=b.chrome_sandbox)
    sb = None
    s0 = steal_ticks()
    try:
        prep = b.prepare_restore(slot, src, variant)
        t0 = now_ns()
        sb = b.restore(prep)
        sb.t0_ns = t0
        drive(pw, b, sb, rec, t0, 0.001, screenshot_path(bid, "restore", variant, trial, saved))
        rec.steal_ms = ticks_to_ms(steal_ticks() - s0)
        rec.pss_kib_browser_idle = idle_pss(b, sb)
        rec.ok = True
    except Exception as e:
        rec.error = short_error(e, b.console(sb) if sb else "")
        rec.steal_ms = ticks_to_ms(steal_ticks() - s0)
    if sb:
        b.stop(sb)
    else:
        b.cleanup(slot)
    return rec
