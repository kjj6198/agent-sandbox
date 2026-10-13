import json
from dataclasses import asdict, dataclass, field
from pathlib import Path


@dataclass
class SpawnRecord:
    backend: str
    mode: str
    restore_variant: str | None
    trial: int
    concurrency: int
    slot: int
    host: str = "L1"
    warmup: bool = False
    t_spawned_ms: float | None = None
    t_init_ms: float | None = None
    t_ready_ms: float | None = None
    t_exec_ms: float | None = None
    t_teardown_ms: float | None = None
    guest_uptime_s: float | None = None
    pss_kib: int | None = None
    rss_kib: int | None = None
    steal_ms: float | None = None
    ok: bool = False
    error: str | None = None


@dataclass
class BrowserRecord:
    backend: str
    mode: str
    trial: int
    restore_variant: str | None = None
    warmup: bool = False
    t_exec_ms: float | None = None
    t_cdp_ms: float | None = None
    t_connected_ms: float | None = None
    t_local_page_ms: float | None = None
    t_screenshot_ms: float | None = None
    t_public_page_ms: float | None = None
    chrome_sandbox: bool | None = None
    pss_kib_browser_idle: int | None = None
    screenshot_path: str | None = None
    steal_ms: float | None = None
    ok: bool = False
    error: str | None = None


@dataclass
class WorkloadRecord:
    backend: str
    workload: str
    trial: int
    warmup: bool = False
    seconds: float | None = None
    value: float | None = None
    unit: str | None = None
    raw: str | None = None
    ok: bool = False
    error: str | None = None


@dataclass
class ParallelRecord:
    backend: str
    mode: str
    k: int
    round: int
    mem_mib: int
    t_exec_ms: list = field(default_factory=list)
    p50: float | None = None
    p95: float | None = None
    max: float | None = None
    failures: int = 0
    errors: list = field(default_factory=list)
    mem_available_delta_mib: float | None = None
    pss_kib_total: int | None = None
    steal_ms: float | None = None


@dataclass
class SnapshotRecord:
    backend: str
    variant: str
    with_browser: bool
    t_create_ms: float | None = None
    mem_file_bytes: int | None = None
    state_bytes: int | None = None
    files: dict = field(default_factory=dict)
    ok: bool = False
    error: str | None = None


@dataclass
class CompatRecord:
    backend: str
    check: str
    ok: bool
    detail: str


def append(path: Path, record) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a") as f:
        f.write(json.dumps({"type": type(record).__name__, **asdict(record)}, ensure_ascii=False) + "\n")
