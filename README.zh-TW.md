[English](README.md) | 繁體中文

# sbx

專為 AI agent 打造的開源、極速沙箱。

`sbx` 能在數十毫秒內為 AI agent 建立一台全新、隔離的 Linux 機器，讓它執行程式、操作桌面或瀏覽器，並在工作完成後直接丟棄。每個沙箱都是從已開機範本快照還原的 Firecracker microVM，擁有獨立核心、可寫入磁碟、可即時觀看的 `1280x800` 桌面、對外連線白名單，以及內建電腦操作工具的 MCP 伺服器。

```sh
id=$(sbx create --view)                # a fresh machine in tens of milliseconds, its screen in a small window
sbx cp ./app $id:app
sbx exec $id 'cd app && pnpm install && pnpm test'
sbx exec -d $id 'cd app && pnpm dev --port 5173'
sbx expose $id 5173                    # http://sbx-host:30000
sbx rm $id
```

## 為什麼選擇 sbx

- **32 到 42 ms 建立時間。** `sbx create` 直接從快照還原執行中的 Firecracker microVM，無須重新開機。同時建立 10 個沙箱，每個僅需 54 到 106 ms。
- **閒置沙箱僅佔 5 MiB 記憶體。** 沙箱透過主機頁面快取以私有方式映射範本記憶體，並共用唯讀根磁碟映像檔，只為實際寫入的記憶體頁面與磁碟區塊付出成本。已開啟 Chromium 的閒置 `browser` 沙箱約佔用 48 MiB。
- **硬體虛擬化隔離。** 每個沙箱在 Firecracker jailer 中以專屬的非特權使用者身分執行獨立客體機核心，並透過 chroot 隔離於獨立的網路命名空間。通往主機、區網、`169.254.169.254` 雲端中繼資料、群播與 IPv6 的流量皆在虛擬機外直接丟棄。
- **專為 agent shell 與工具呼叫設計。** 指令格式比照 Docker（`exec`、`cp`、`rm`、`ls`），`create` 只輸出沙箱 ID，`exec` 直接回傳指令本身的結束狀態碼，查詢類指令皆支援 `--json`。可將 `skill/sbx/` 複製到 `~/.claude/skills/` 供 Claude Code 與 Grok 讀取，或透過 `sbx mcp` 以 MCP 連接。
- **內建即時桌面與瀏覽器。** 每個沙箱啟動時皆具備 `DISPLAY=:0` 的 `1280x800` X 桌面、即時 noVNC 檢視視窗、靜態截圖介面，以及在連接埠 9222 開啟 CDP 的預啟動 Chromium 範本。
- **原地建立快照與分支複製。** 透過 `sbx snapshot` 可在 1.0 秒內將執行中沙箱的記憶體、行程與檔案系統存為可重複使用的範本，或透過 `sbx fork` 在 2.2 秒內複製出獨立沙箱。

## 功能導覽

### 桌面環境與即時檢視器

每個沙箱在客體機內執行提供 VNC 的 `1280x800` X 桌面，內含 openbox、底部工作列、預設開啟的終端機視窗、Noto CJK 與 emoji 字型，以及支援日文（Mozc）、繁體中文（新酷音 Chewing）與簡體中文（Pinyin）的 fcitx5 輸入法。指令預設帶有 `DISPLAY=:0`，因此 agent 啟動的任何圖形介面程式都會直接顯示在畫面上。

執行 `sbx view ID` 或 `sbx create --view` 會在螢幕右上角開啟即時 noVNC 小視窗。視窗預設為觀看模式，避免誤觸干擾 agent；點擊「Watching」即可接管滑鼠與鍵盤，將檔案拖放至視窗內即可上傳至沙箱的 `~/Downloads`。觀看模式屬於被動連線，不會延長閒置沙箱的存活時間。你也可以直接擷取畫面截圖，或將檢視器嵌入其他網頁：

```sh
sbx screenshot $id                     # ./sbx-$id.png
sbx view --no-open --listen 127.0.0.1:7681 $id
```

### MCP 與電腦操作

`sbx mcp` 在標準輸入與輸出上提供 MCP 伺服器，供透過工具呼叫而非 shell 操作的 agent 使用：

```sh
claude mcp add sbx -- sbx mcp
```

它提供沙箱生命週期與指令執行工具（`sandbox_create`、`sandbox_list`、`sandbox_remove`、`exec`、`view`），以及桌面操作工具（`click`、`move_mouse`、`drag`、`scroll`、`type_text`、`press_key`、`screenshot`、`request_secret`）。每次執行畫面操作後，`sbx mcp` 會等待最多 1 秒讓畫面開始變化，接著等待畫面靜止達 300 ms（總計最多 5 秒），在區網主機上每次操作約需 300 到 700 ms。若畫面沒有變化則不回傳圖片，每次呼叫可為 agent 節省約 1,400 個 token。

`default` 範本同時內建 [cua-driver](https://github.com/trycua/cua/tree/main/libs/cua-driver)，這是在沙箱內部執行的 MCP 伺服器，能讀取桌面應用程式的無障礙樹，讓 agent 直接依選單與元件名稱操作程式，而不必依賴像素座標：

```sh
claude mcp add cua -- sbx exec -i $id cua-driver mcp
```

### 對外連線白名單

沙箱預設可連線至公開網際網路。使用 `--allow` 可將對外連線限制於指定主機名稱或萬用字元子網域，使用 `--offline` 則完全切斷網際網路連線：

```sh
id=$(sbx create --allow github.com,codeload.github.com --allow '*.npmjs.org')
sbx allow $id example.com              # takes effect for new connections at once
sbx run --offline -- python3 untrusted.py
```

規則設於主機端的沙箱網路命名空間內，位於虛擬機外部，因此客體機內的任何程式（包含 `root`）都無法繞過限制。`sbxd` 只會回應白名單內網域的 DNS 查詢，並在檢查 HTTP `Host` 標頭或 TLS SNI 後才轉發連接埠 80 與 443 的連線，同時保持點對點 TLS 加密。被阻擋的主機名稱會顯示在失敗的 `exec` 輸出末尾，並記錄於 `sbx inspect` 中。

### 環境設定與機密資訊

環境設定（environment）是儲存在 `sbx` 主機 `/etc/sbx/environments` 下的一組具名環境變數與檔案（目錄權限 `0700`，檔案權限 `0600`）。用戶端不需自行保存憑證，即可啟動已登入 Claude Code、Codex 或 GitHub 的沙箱。若 `base` 環境存在，新建立的沙箱會自動載入：

```sh
sbx env set base CLAUDE_CODE_OAUTH_TOKEN GH_TOKEN
sbx env file base ~/.codex/auth.json --to /home/user/.codex/auth.json
sbx env ls                             # lists variable names and file paths, never values
id=$(sbx create)                       # starts with base
id=$(sbx create --env-name none)       # starts without any
```

`sbx snapshot` 會拒絕為帶有環境設定的沙箱建立快照，避免行程記憶體或磁碟中快取的憑證外洩至共用範本。若需要在 MCP 下進行互動式登入，`request_secret` 會在檢視視窗中顯示遮罩輸入欄向使用者索取密碼，並僅保存在 `sbx mcp` 記憶體中；隨後 agent 透過 `type_text {"secret": "google.password"}` 輸入，讓機密內容不會進入 agent 上下文、對話紀錄或模型供應商日誌。

### 範本、快照與分支複製

範本由 Dockerfile 建置而成，會在 microVM 中開機一次、執行 `--warmup` 預熱指令，並在可用記憶體穩定 2 秒後拍下快照：

- `default`：Debian 13、Node.js 24 與 pnpm、Go 1.27、Python 3.13 與 uv、build-essential、git、ripgrep、jq、ffmpeg、ImageMagick、Chromium、`cua-driver`、Noto CJK 與 emoji 字型、日文與中文輸入法、Claude Code、Codex、`gh` 以及桌面環境。配置 2 vCPU、2 GiB 記憶體、16 GiB 磁碟。
- `browser`：與 `default` 相同，但已在桌面上預先開啟 Chromium，並於連接埠 9222 啟用 DevTools 協定。
- `ubuntu`：Ubuntu 24.04，具備 boat.dev 機器的完整工具鏈，包含 `default` 的所有工具、作為 `chromium` 的 Google Chrome，以及 Rust、Ruby 4、PHP 8.4 與 Composer、Java 11 與 Maven 及 Gradle、Kotlin、Scala 3 與 sbt、.NET 8、Elixir、R、Bun、Deno、Poetry、clang、opencode、pi 與 HinaCode 伺服器。

你可以從 Dockerfile 建立自訂範本、透過 CDP 操作 `browser` 範本，或對執行中的沙箱建立快照與分支複製：

```sh
sbx template build myapp ./my-template-dir --memory 4096 --warmup 'sbx-desktop && cd /srv && ./start-db'
sbx snapshot $id repo-ready            # memory, processes, and files of a running sandbox
sbx create repo-ready
sbx fork $id                           # one-off clone of a running sandbox
```

### HinaCode 整合

由 `ubuntu` 範本建立的沙箱內建 [HinaCode](https://github.com/hinadev-ai/hinacode) 伺服器，可直接作為遠端 HinaCode 機器執行。由於外部無法直接連入沙箱網路，伺服器會綁定 loopback 並透過 HinaCode relay 主動向外連線。用戶端也可以在建立沙箱時加上如 `{"app": "hinacode", "thread": "t1"}` 的標籤，重啟後透過 `GET /v1/sandboxes?label=KEY=VALUE` 找回原本的沙箱：

```sh
sbx template build ubuntu /usr/local/share/sbx/templates/ubuntu --warmup sbx-desktop
id=$(sbx create ubuntu --ttl 0)
sbx exec -d $id 'hinacode serve --host 127.0.0.1 --relay >~/hinacode.log 2>&1'
sbx exec $id hinacode pair
```

## 實測效能與研究報告

以下數據實測於配備 Intel i5-13400F 的 Proxmox 虛擬機（8 vCPU、8 GiB 記憶體，啟用巢狀虛擬化），沙箱使用 `default` 範本（2 vCPU、2 GiB 記憶體）：

| 項目 | 實測結果 |
|---|---|
| `sbx create`，從發出請求到客體機 agent 回應 | 32 到 42 ms |
| 同時執行 10 個 `sbx create` | 每個 54 到 106 ms |
| 閒置沙箱佔用記憶體（含桌面） | 5 MiB，加上實際寫入量 |
| 閒置 `browser` 沙箱佔用記憶體 | 約 48 MiB |
| `sbx snapshot` | 1.0 s |
| `sbx fork` | 2.2 s |
| 建置範本時的冷開機（啟動至 agent 回應） | 270 ms |
| 從區網透過 Playwright `connectOverCDP` 開新分頁、截圖並導向 example.com | 0.5 s |

為什麼選擇 Firecracker 快照？在打造 `sbx` 之前，我們於同一台 Proxmox 伺服器上評估了 Firecracker、gVisor 與 Cloud Hypervisor（見 [research/README.md](research/README.md) 與[完整評估報告](research/agent-sandbox-isolation.md)）。從快照還原的 Firecracker 沙箱在虛擬機內僅需 29 ms 即可回應第一個指令，在實體機上僅需 20 ms；同時還原 20 個沙箱時，每個僅佔用約 9 MiB 記憶體，且內部程式執行速度與一般容器相當。Cloud Hypervisor 的延遲表現幾乎持平，但每個還原的沙箱都會載入完整配置的記憶體。gVisor 從零啟動最快，但執行 agent 類型的工作負載時速度約慢上一倍。

## 系統需求與安裝

`sbx` 主機需要具備 `/dev/kvm` 的 `x86_64` Linux 機器，可為實體機，或支援巢狀虛擬化的雲端執行個體與 Proxmox 虛擬機。安裝腳本原生支援 Debian 與 Ubuntu，在其他發行版上則會列出需先安裝的指令。`sbx` CLI 可在 macOS 與 Linux 上執行，只需能透過 HTTP 連線至主機即可。

在主機上執行一行指令，即可安裝 daemon、Firecracker、預先建置的客體機核心，以及 `default` 與 `browser` 範本：

```sh
curl -fsSL https://github.com/kjj6198/agent-sandbox/releases/latest/download/install.sh | sudo bash
```

也可以用 Docker 執行 daemon，不會動到主機的套件與防火牆。在專案目錄中執行 `docker compose up -d` 即可建置並啟動；`docker run` 的寫法請見[使用指南](docs/guide.md#with-docker)。

接著，在每台執行 agent 的機器上安裝 CLI：

```sh
curl -fsSL -o ~/.local/bin/sbx "https://github.com/kjj6198/agent-sandbox/releases/latest/download/sbx-$(uname -s)-$(uname -m)"
chmod +x ~/.local/bin/sbx
ssh sbx-host sudo cat /etc/sbx/token | sbx login http://sbx-host:7070
sbx status
```

從原始碼安裝與安裝腳本的設定，請見 [docs/guide.md](docs/guide.md#install)。

## 文件索引

| 文件 | 內容說明 |
|---|---|
| [docs/guide.md](docs/guide.md) | 完整使用指南、安裝選項、桌面檢視器、MCP、範本、對外連線白名單、環境設定、HTTP API、設定檔與系統限制 |
| [docs/architecture.md](docs/architecture.md) | `sbx`、`sbxd` 與 `sbx-agent` 架構設計、CLI 設計原則、vsock 封包格式、快照生命週期、網路架構與安全邊界 |
| [deploy/proxmox/README.md](deploy/proxmox/README.md) | 在 Proxmox 隔離虛擬機中部署 `sbx`，搭配獨立防火牆與連接埠轉發 |
| [skill/sbx/SKILL.md](skill/sbx/SKILL.md) | 供 Claude Code、Grok 及其他讀取 `~/.claude/skills` 的 agent 使用的技能檔 |
| [research/README.md](research/README.md) | Firecracker、gVisor 與 Cloud Hypervisor 的隔離技術評估報告、效能數據與重現步驟 |

## 專案狀態

> [!WARNING]
> 本專案仍處於高度實驗階段，請自行承擔使用風險！
