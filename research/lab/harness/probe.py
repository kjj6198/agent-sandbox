"""Clocks, the guest agent client, readiness signals, and host-side resource probes."""
import ctypes
import errno
import http.server
import os
import selectors
import socket
import threading
import time
from collections import deque
from functools import partial

WWW_DIR = "/opt/sbx/guest/www"
MAX_IN_FLIGHT = 64


def now_ns() -> int:
    return time.perf_counter_ns()


def ms(t_ns: int | None, t0_ns: int) -> float | None:
    return None if t_ns is None else round((t_ns - t0_ns) / 1e6, 3)


def steal_ticks() -> int:
    with open("/proc/stat") as f:
        return int(f.readline().split()[8])


def ticks_to_ms(ticks: int) -> float:
    return ticks * 1000 / os.sysconf("SC_CLK_TCK")


def poll_connect(ip: str, port: int, deadline_ns: int, interval_s: float = 0.001) -> socket.socket:
    """Opens a new connect attempt every interval while older ones stay in flight.

    A single blocking connect would wait out a lost SYN (1 s retransmit) when the guest is not up yet.
    """
    sel = selectors.DefaultSelector()
    in_flight: deque[socket.socket] = deque()
    next_attempt = 0.0

    def drop(s):
        sel.unregister(s)
        in_flight.remove(s)
        s.close()

    try:
        while now_ns() < deadline_ns:
            if time.monotonic() >= next_attempt:
                if len(in_flight) >= MAX_IN_FLIGHT:
                    drop(in_flight[0])
                s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
                s.setblocking(False)
                rc = s.connect_ex((ip, port))
                if rc in (0, errno.EINPROGRESS):
                    sel.register(s, selectors.EVENT_WRITE)
                    in_flight.append(s)
                else:
                    s.close()
                next_attempt = time.monotonic() + interval_s
            for key, _ in sel.select(timeout=interval_s):
                s = key.fileobj
                if s.getsockopt(socket.SOL_SOCKET, socket.SO_ERROR) == 0:
                    sel.unregister(s)
                    in_flight.remove(s)
                    s.setblocking(True)
                    return s
                drop(s)
        raise TimeoutError(f"connect {ip}:{port} timed out")
    finally:
        for key in list(sel.get_map().values()):
            key.fileobj.close()
        sel.close()


def agent_session(s: socket.socket, cmd: str, timeout: float) -> tuple[int, str]:
    s.settimeout(timeout)
    s.sendall(f"{cmd}\necho __RC=$?__\n".encode())
    buf = b""
    while True:
        chunk = s.recv(65536)
        if not chunk:
            raise ConnectionError(f"agent closed before marker: {buf[-300:]!r}")
        buf += chunk
        tail = buf.rstrip()
        idx = tail.rfind(b"__RC=")
        if idx >= 0 and tail.endswith(b"__") and idx + 5 < len(tail) - 2:
            out = buf[:idx].decode(errors="replace")
            return int(tail[idx + 5:-2]), out


def agent(ip: str, cmd: str, timeout: float = 120) -> tuple[int, str]:
    with socket.create_connection((ip, 7000), timeout=10) as s:
        return agent_session(s, cmd, timeout)


def agent_ok(ip: str, cmd: str, timeout: float = 120) -> str:
    rc, out = agent(ip, cmd, timeout)
    if rc != 0:
        raise RuntimeError(f"rc={rc}: {cmd[:80]}: {out[-500:]}")
    return out


def first_exec(ip: str, deadline_ns: int, interval_s: float = 0.001) -> int:
    """Poll-connects to the agent and runs `echo ok`; returns the timestamp of the completed round trip."""
    while True:
        s = poll_connect(ip, 7000, deadline_ns, interval_s)
        try:
            rc, out = agent_session(s, "echo ok", timeout=max(0.5, (deadline_ns - now_ns()) / 1e9))
            t = now_ns()
            if rc == 0 and out.strip() == "ok":
                return t
            raise RuntimeError(f"echo ok returned rc={rc} out={out!r}")
        except ConnectionError:
            # socat's listener can accept before bash is ready during very early boot.
            if now_ns() >= deadline_ns:
                raise
            time.sleep(interval_s)
        finally:
            s.close()


def read_http_response(s: socket.socket) -> bytes:
    buf = b""
    while chunk := s.recv(65536):
        buf += chunk
        head, sep, body = buf.partition(b"\r\n\r\n")
        if sep:
            for line in head.split(b"\r\n"):
                if line.lower().startswith(b"content-length:") and len(body) >= int(line.split(b":")[1]):
                    return buf
    return buf


def http_get(ip: str, port: int, path: str, deadline_ns: int, interval_s: float) -> bytes:
    """Polls until GET returns 200; returns the body."""
    last = None
    while now_ns() < deadline_ns:
        try:
            s = poll_connect(ip, port, deadline_ns, interval_s)
            with s:
                s.settimeout(2)
                # Chromium's DevTools server drops HTTP/1.0 requests without answering.
                s.sendall(f"GET {path} HTTP/1.1\r\nHost: {ip}:{port}\r\nConnection: close\r\n\r\n".encode())
                buf = read_http_response(s)
            if buf.startswith(b"HTTP/1.1 200"):
                return buf.split(b"\r\n\r\n", 1)[1]
            last = buf[:100]
        except OSError as e:
            last = e
        time.sleep(interval_s)
    raise TimeoutError(f"GET http://{ip}:{port}{path} never returned 200 (last: {last!r})")


class ReadyListener:
    """Receives `SBX_READY <slot> <uptime>` hellos on :7001 and timestamps them."""

    def __init__(self):
        self.events: dict[int, list[tuple[int, str]]] = {}
        self.cond = threading.Condition()
        self.srv = socket.socket()
        self.srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self.srv.bind(("0.0.0.0", 7001))
        self.srv.listen(128)
        threading.Thread(target=self._loop, daemon=True).start()

    def _loop(self):
        while True:
            conn, _ = self.srv.accept()
            threading.Thread(target=self._read, args=(conn,), daemon=True).start()

    def _read(self, conn):
        with conn:
            conn.settimeout(5)
            data = b""
            try:
                while not data.endswith(b"\n") and (chunk := conn.recv(4096)):
                    data += chunk
            except OSError:
                pass
        t = now_ns()
        parts = data.decode(errors="replace").split()
        if len(parts) >= 3 and parts[0] == "SBX_READY":
            with self.cond:
                self.events.setdefault(int(parts[1]), []).append((t, parts[2]))
                self.cond.notify_all()

    def wait(self, slot: int, since_ns: int, deadline_ns: int) -> tuple[int, float]:
        with self.cond:
            while True:
                for t, uptime in self.events.get(slot, []):
                    if t >= since_ns:
                        return t, float(uptime)
                remaining = (deadline_ns - now_ns()) / 1e9
                if remaining <= 0:
                    raise TimeoutError(f"no SBX_READY from slot {slot}")
                self.cond.wait(remaining)


class LineWatcher:
    """Reads a process pipe in a thread, timestamps the first line containing each marker, keeps a tail."""

    def __init__(self, fd, marker: str = "SBX_INIT"):
        self.marker = marker.encode()
        self.t_marker: int | None = None
        self.tail: list[str] = []
        self.event = threading.Event()
        threading.Thread(target=self._loop, args=(fd,), daemon=True).start()

    def _loop(self, fd):
        for line in iter(fd.readline, b""):
            if self.t_marker is None and self.marker in line:
                self.t_marker = now_ns()
                self.event.set()
            self.tail = (self.tail + [line.decode(errors="replace").rstrip()])[-40:]
        fd.close()

    def text(self) -> str:
        return "\n".join(self.tail)


class QuietHandler(http.server.SimpleHTTPRequestHandler):
    def log_message(self, *args):
        pass


def serve_www(port: int = 8000) -> http.server.ThreadingHTTPServer:
    handler = partial(QuietHandler, directory=WWW_DIR)
    http.server.ThreadingHTTPServer.allow_reuse_address = True
    srv = http.server.ThreadingHTTPServer(("0.0.0.0", port), handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


def descendants(roots: set[int]) -> set[int]:
    children: dict[int, list[int]] = {}
    for d in os.listdir("/proc"):
        if d.isdigit():
            try:
                with open(f"/proc/{d}/stat") as f:
                    ppid = int(f.read().rsplit(")", 1)[1].split()[1])
                children.setdefault(ppid, []).append(int(d))
            except (OSError, IndexError, ValueError):
                pass
    out, stack = set(), list(roots)
    while stack:
        p = stack.pop()
        if p not in out:
            out.add(p)
            stack.extend(children.get(p, []))
    return out


def pids_matching(needle: str) -> set[int]:
    found = set()
    for d in os.listdir("/proc"):
        if d.isdigit() and int(d) != os.getpid():
            try:
                with open(f"/proc/{d}/cmdline", "rb") as f:
                    if needle.encode() in f.read():
                        found.add(int(d))
            except OSError:
                pass
    return found


_libc = ctypes.CDLL(None, use_errno=True)
SYS_KCMP, KCMP_VM = 312, 1


def distinct_mm(pids: set[int]) -> list[int]:
    """Drops processes that share an address space with one already kept.

    gVisor's systrap stubs come in groups sharing one mm; smaps_rollup would count that mm once per process.
    """
    kept: list[int] = []
    for pid in sorted(pids):
        if not any(_libc.syscall(SYS_KCMP, pid, k, KCMP_VM, 0, 0) == 0 for k in kept):
            kept.append(pid)
    return kept


def pss_rss_kib(pids: set[int]) -> tuple[int, int]:
    pss = rss = 0
    for pid in distinct_mm(pids):
        try:
            with open(f"/proc/{pid}/smaps_rollup") as f:
                for line in f:
                    if line.startswith("Pss:"):
                        pss += int(line.split()[1])
                    elif line.startswith("Rss:"):
                        rss += int(line.split()[1])
        except OSError:
            pass
    return pss, rss


def mem_available_mib() -> float:
    with open("/proc/meminfo") as f:
        for line in f:
            if line.startswith("MemAvailable:"):
                return int(line.split()[1]) / 1024
    raise RuntimeError("MemAvailable missing")


def wait_pids_gone(pids: set[int], timeout_s: float = 10) -> None:
    deadline = time.monotonic() + timeout_s
    for pid in pids:
        while os.path.exists(f"/proc/{pid}"):
            if time.monotonic() > deadline:
                raise TimeoutError(f"pid {pid} still alive")
            time.sleep(0.001)


def wait_quiet(max_load: float = 1.0, timeout_s: float = 900) -> float:
    deadline = time.monotonic() + timeout_s
    while True:
        load = os.getloadavg()[0]
        if load <= max_load or time.monotonic() > deadline:
            return load
        print(f"load {load:.2f} > {max_load}, waiting", flush=True)
        time.sleep(15)
