# 使用指南 (Usage) — `clip` (mesh-rs, Rust/iroh P2P)

!!! note "Terminology rule (zh-TW pages)"
    技術名詞首次出現以「中文 (English original)」格式呈現，例：依賴注入
    (dependency injection)。**不自創翻譯**——若無公認譯名直接保留英文
    （如 `embedding`、`tokenizer`）。代碼、API 名、CLI flag、套件名、檔名一律不翻。

**免伺服器、預設私密 (no-server, private-by-default)** 的實作。位於同一個*房間 (room)* 的兩台裝置會直接
找到彼此（區域網路 (LAN) 上用 mDNS，跨網路則用複製貼上的*配對票券 (ticket)*），透過 iroh QUIC 連線互通——
不需中繼 (relay)、不需帳號、不需任何基礎設施。在一台裝置上複製的文字與圖片，可在另一台上接收/貼上，
圖片抵達時**BLAKE3 雜湊完全相同 (hash-equal)**。

- 執行檔 (binary)：**`clip`** · 傳輸層 (transport)：**iroh**（Ed25519 NodeId 身分 (identity)） · 實作：[`../mesh-rs/`](https://github.com/daviddwlee84/clipboard-handoff/tree/main/mesh-rs/)
- 共用契約：[SPEC](SPEC.md)（CLI §2、sink §3、設定 §5、工作階段 (session) §8） · wire/IPC：[PROTOCOL](PROTOCOL.md)
- 更深入的傳輸層說明請見 [`../mesh-rs/README.md`](https://github.com/daviddwlee84/clipboard-handoff/blob/main/mesh-rs/README.md)。

## 安裝 / 建置

```sh
cargo build --release --manifest-path mesh-rs/Cargo.toml
# binary -> mesh-rs/target/release/clip   (debug: drop --release -> target/debug/clip)
```

第一次建置會拉進整棵 iroh 相依樹（約 390 個 crate）；release 編譯需要數分鐘。
請把執行檔放到你的 `PATH` 上（例如 `install mesh-rs/target/release/clip ~/.local/bin/`），
這樣下面的範例才能直接以 `clip` 呼叫。

## 快速上手（兩台裝置，同一區域網路）

不需要配對 (pairing) 步驟——在同一個區域網路上使用相同的 `--room`，就會透過 mDNS 自動探索 (auto-discovery)。

```sh
# Device B: bring up a daemon (or just run any client command; it auto-spawns one)
clip --room demo daemon --foreground        # leave running

# Device A: send
echo "hello from A" | clip --room demo send

# Device B: receive
clip --room demo recv                        # -> hello from A
clip --room demo paste                       # place it on B's OS clipboard
```

`send`/`recv`/`paste`/`tui` 都是瘦客戶端 (thin client)，透過 Unix socket 與本機常駐的**常駐程式 (daemon)**
溝通；第一個被執行的指令若發現常駐程式尚未啟動，會**自動生成 (auto-spawn)** 一個。常駐程式擁有 iroh 連線、
身分、已接收項目的緩衝區，以及作業系統剪貼簿 (clipboard)。

## 連接裝置

### 1. mDNS 自動探索（預設，零設定）

在每台主機上以**同一區域網路內相同的 `--room`**啟動常駐程式；它們會互相探索並開啟直連的 QUIC 連線，
**完全不需要配對票券**。隔離會被強制執行兩次：mDNS 服務名稱以房間為範圍，而房間密鑰會被折入 QUIC ALPN
（跨房間的撥號會在交握階段被拒絕）。為了避免重複撥號，只有 NodeId 較大的那個對等節點 (peer) 會發起連線。

```sh
clip --room demo daemon --foreground     # host 1
clip --room demo daemon --foreground     # host 2  — then send/recv, no pair step
```

### 2. 配對票券退回機制 (fallback)（不同區域網路 / 無 multicast）

mDNS 僅限於區域網路內，因此跨越不同網路——或在 multicast（`224.0.0.251` / `ff02::fb`）被過濾的環境——
請使用明確的**配對票券**：

```sh
# Host 1: mint a ticket
clip --room demo pair --new              # prints a bare base32 ticket
clip --room demo pair --new --json       # -> {"ticket":"<base32>"}

# Host 2: join with it
clip --room demo pair "<ticket>"
```

配對票券是端點 id 加上可達直連位址所組成的 CBOR，以 base32 編碼。兩種路徑可以並存；在多重網路介面
(multi-homed) 的主機上，配對票券這條路徑才是可靠的（見疑難排解）。

## 指令參考

全域 flag（可放在子指令**之前或之後**）：`--config-dir PATH`、`--socket PATH`、
`--room NAME`（預設 `default`）、`--json`、`-q/--quiet`、`-v/--verbose`。

| 指令 | 功能 |
|---|---|
| `clip send [PATH] [--text\|--image\|--file\|--auto] [--name NAME]` | 讀取 `PATH`（或**標準輸入 (stdin)** 直到 EOF），嗅探型別（預設 `--auto`），廣播給對等節點。即使沒有任何對等節點也會以 `0` 結束（項目仍會被緩衝）——在 `set -e` 下是安全的。 |
| `clip recv [--follow] [--latest-image --emit-path] [--out PATH]` | 訂閱傳入的項目。文字 → 標準輸出 (stdout)；圖片/檔案 → blob 快取檔案，並印出路徑。`--follow` 會持續串流直到 Ctrl-C；否則回傳最新一筆已緩衝的項目。`--latest-image` 只過濾出圖片；`--emit-path`/`--out` 控制路徑輸出。 |
| `clip paste` | 把最新接收到的項目寫入作業系統剪貼簿（text→set_text、image→PNG→set_image、**file → 以文字形式寫入它的路徑**）。 |
| `clip tui` | 啟動通訊軟體風格的聊天 TUI（見下文）。 |
| `clip pair --new [--json]` · `clip pair <ticket>` | 產生一張配對票券 / 用票券加入（見上文）。 |
| `clip peers` | 列出目前已連線的對等節點。 |
| `clip status` | 顯示常駐程式狀態：身分、房間、對等節點數量、`auto_copy`、緩衝區大小、`clipboard: available\|unavailable`。加上 `--json` 可得到機器可讀的輸出。 |
| `clip config set KEY VALUE` · `clip config get KEY` | 保存 / 讀取設定（見設定鍵）。 |
| `clip clear [--all] [--yes]` | 清除這個工作階段的資料（見工作階段與清除）。 |
| `clip daemon [--foreground]` · `clip daemon stop` | 執行 / 停止常駐程式。`stop` 會套用 `clear_on_exit`。 |

**傳送任意檔案**——`image` 是唯一可貼到剪貼簿的二進位型別；`file` 則是任意位元組，附帶盡力而為
(best-effort) 的 MIME 與一個 `filename`：

```sh
clip send report.pdf                     # --auto: binary → file, filename=report.pdf
clip send notes.txt --file               # force a file even though it is UTF-8
tar cz dir | clip send --file --name dir.tgz   # stdin + explicit name
clip send shot.png                       # PNG magic → image (clipboard-pasteable)
```

圖片**與**檔案的位元組都透過以 `blob.hash` 為鍵的直連串流取得，並在接收時經過 BLAKE3 驗證——所以
`recv --emit-path` 與 `paste` 永遠都有一個本機路徑可用。型別嗅探遵循 [PROTOCOL §2](PROTOCOL.md)。

## Sink — 收到的項目會去哪裡 (SPEC §3)

Sink 是**可疊加的 (additive)**：同一個項目可以同時進入剪貼簿*以及*資料夾/附加檔案。

| Sink | 設定鍵 | 接收內容 |
|---|---|---|
| clipboard | `auto_copy` | 文字、圖片（`file` 永遠不會自動落到剪貼簿上） |
| folder | `save_dir` | 圖片 → `<filename\|hash>.png`，檔案 → `<filename\|hash>`——以 `name (2).ext` 去除重複 |
| append-file | `text_file` | 文字，附加在 `\n---\n<device> <ISO8601 ts>\n` 標頭之後 |

```sh
clip config set save_dir  ~/Downloads/clip
clip config set text_file ~/notes/clip.md
```

**`auto_copy` 的三種模式**（`clip config set auto_copy notify|on|off`）：
- **`notify`**（預設）：緩衝該項目並發出通知 (toast)；**不**碰觸剪貼簿。在 TUI 中按 `y`，
  或執行 `clip paste`，才會把它放上剪貼簿。
- **`on`**：把每一筆接收到的項目直接寫入作業系統剪貼簿。
- **`off`**：永不碰觸剪貼簿；項目只在 TUI 中 / 透過 `recv` 才看得到。

## 設定鍵 (SPEC §5)

`auto_copy`（`notify`|`on`|`off`，預設 `notify`） · `save_dir`（路徑|""） · `text_file`（路徑|""） ·
`clear_on_exit`（`ask`|`transient`|`all`|`never`，預設 `ask`） · `device_name`（預設為主機名稱） ·
`room`（預設 `default`） · `internet`（`on`|`off`，Phase 3） · `broadcast_on_copy`（`on`|`off`，Phase 3）。

設定資料夾：macOS `~/Library/Application Support/mesh-rs/`、Linux `$XDG_CONFIG_HOME/mesh-rs/`、
Windows `%APPDATA%\mesh-rs\`。可用 `--config-dir` 覆寫。

## 工作階段與清除 (SPEC §8)

一個工作階段就是常駐程式的一次執行。常駐程式會針對每個工作階段追蹤：**暫存 (transient)** 儲存區
（`<config-dir>/blobs`——已取得的 blob 快取加上 `recv --emit-path` 產生的檔案——以及記憶體中的
緩衝區）、**工作階段開始時的 `text_file` 大小**，以及這個工作階段中**它寫入 `save_dir` 的檔案**。
因此清除動作是精確的，永遠不會碰到工作階段開始前既有的內容。

```sh
clip clear                 # transient only (buffer + blob cache + emit-path temp files)
clip clear --all           # + revert this session's sink writes (prompts unless --yes)
clip clear --all --yes     # non-interactive
clip daemon stop           # applies clear_on_exit; SIGTERM does the same
```

`clear_on_exit` 決定 `daemon stop`/SIGTERM 的行為：`never` · `transient` · `all` · `ask`（在 TTY 上
會出現提示，否則退回 `transient`）。離開 TUI 時會出現同樣的行內選擇。

## TUI (`clip tui`)

一個綁定本機常駐程式的通訊軟體風格聊天介面。它純粹是前端——每一次複製都會經由常駐程式（剪貼簿的
擁有者）路由。它會自動生成/接上常駐程式，然後即時串流項目。

```sh
clip --room demo tui
```

標頭：房間 · 端點 id 縮寫 · 對等節點數量 · `auto_copy` 模式 · `clipboard: available|unavailable`。
中間：可捲動的泡泡訊息歷史（你自己的泡泡靠右對齊，標示為 `you`）。底部：輸入框 (composer)。

| 按鍵 | 動作 |
|---|---|
| 輸入文字 + `Enter` | 把這一行當成文字項目送出（會立刻以你自己的泡泡出現）。 |
| `Esc` | 在**輸入框**與**瀏覽 (browse)** 模式之間切換焦點。 |
| `↑` / `↓`（瀏覽模式下也可用 `k` / `j`） | 在訊息之間移動反白選取。 |
| `y` | 把反白選取（或最新）的項目**經由常駐程式**複製到作業系統剪貼簿（`file` 會以文字形式複製它的路徑）。 |
| `s` | 把反白選取的圖片/檔案儲存到 `~/Downloads/`（否則存到目前工作目錄）。 |
| `o` | 用作業系統預設應用程式（`open`/`xdg-open`/`start`）開啟反白選取的圖片/檔案。 |
| `q`（瀏覽模式） · `Ctrl-C`（任何模式） | 離開。若這個工作階段收到過任何東西，會先提示 `⚑ clear this session? [t]ransient / [a]ll incl sinks / [n]o`。 |

**行內圖片：**每個圖片泡泡都會顯示中繼資料（`🖼 W×H · size · filename`），以及透過 ratatui-image 呈現的
真實行內縮圖 (thumbnail)——啟動時若偵測到終端機圖形協定（**Kitty / iTerm2 / Sixel**）就使用它，否則使用
**Unicode 半格區塊 (half-blocks)**，再不然就以中繼資料那一行作為佔位。解碼/縮放都在 UI 執行緒之外進行。
在不回應圖形能力查詢的終端機上第一次啟動時，會因為探測逾時而暫停約 1–2 秒；現代終端機則會立即啟動。
把圖片貼*進*輸入框的功能還沒接上——請用 `clip send --image` 傳送圖片。

## 跨機器（mac ↔ headless Linux）

把執行檔部署到兩台主機上，使用相同的 `--room`，並依賴 mDNS 自動探索——若區域網路阻擋 multicast，
則改用配對票券退回機制。已在 mac → headless Ubuntu 上以雜湊完全相同的 PNG 驗證過。

## Headless / 無顯示環境的主機

常駐程式會**在啟動時偵測一次**作業系統剪貼簿是否存在，並優雅降級（不會崩潰），記錄
`WARN clipboard unavailable (headless) — send/recv/paste-to-file still work`。`send`、`recv`
（含 `--latest-image --emit-path`）、探索 (discovery) 與傳輸全都繼續運作；只有寫入剪貼簿的動作變成
空操作 (no-op)：`clip paste` 會回傳 `clipboard unavailable (headless)` 並以結束碼 `1` 離開，而
`auto_copy on` 會繼續緩衝但不碰觸剪貼簿。`clip status` 會回報
`clipboard: available|unavailable`。若要在*確實有*顯示環境的機器上測試這條路徑，請用
`CLIP_FORCE_HEADLESS=1` 啟動常駐程式。

## 疑難排解

- **對等節點在區域網路上一直連不上** → mDNS multicast 可能被過濾，或者你的主機是多重網路介面
  (multi-homed)（Tailscale/VPN + LAN）。mDNS 只能到達承載 multicast 的網路介面；VPN 介面通常不會。
  請退回**配對票券**流程（`pair --new` / `pair <ticket>`）——透過區域網路位址的直連 QUIC 仍然可用。
  在多網路介面的主機上，這才是可靠的路徑。
- **不同網路** → mDNS 僅限區域網路；請使用配對票券。
- **`clip paste` 顯示 "clipboard unavailable (headless)"** → 在沒有顯示環境的主機上這是預期行為；
  請改用 `recv --emit-path` / `save_dir`。
- **沒有東西可以 paste/recv** → 結束碼 `5`；你還沒有收到任何項目。
- **信任說明（Phase 0）：**任何你配對/自動連上的對等節點都會被自動加入允許清單 (allowlist)。TOFU
  的等待核准 (hold-for-approval)，以及 SPEC 中的短碼/QR 確認，都屬於 Phase 1（不在這個建置中）。
