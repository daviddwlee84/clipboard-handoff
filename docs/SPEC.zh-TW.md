# SPEC — 共用產品契約 (the shared product contract)

!!! note "Terminology rule (zh-TW pages)"
    技術名詞首次出現以「中文 (English original)」格式呈現，例：依賴注入
    (dependency injection)。**不自創翻譯**——若無公認譯名直接保留英文
    （如 `embedding`、`tokenizer`）。代碼、API 名、CLI flag、套件名、檔名一律不翻。

本 repo 中的每一個實作（`mesh-rs`、`room-go`、`experiments/*`）都**必須**呈現同一套介面，好讓使用者
（以及 bake-off 測試腳本 (harness)）能用完全相同的方式操作其中任何一個。唯一不同的只有**執行檔名稱**與
**傳輸層 (transport)**。傳輸格式 (wire format)／IPC 的細節請見 [`PROTOCOL.md`](PROTOCOL.md)。

## 1. 心智模型

- 一台裝置執行一個**常駐程式 (daemon)**，它擁有：網路連線、裝置身分 (identity)、受信任的
  對等節點 (peer) 集合、一個裝著近期收到項目的小型環形緩衝區，以及 **OS 剪貼簿 (clipboard)**。
- `send` / `recv` / `paste` / `tui` 都是**輕量、用完即走的瘦客戶端 (thin client)**，透過本機 IPC socket
  （Unix domain socket，或 Windows 上的具名管道）與本機常駐程式溝通。第一個客戶端指令會**自動啟動
  (auto-spawn)** 常駐程式。
- 「CLI 只需要一條持久連線」講的就是常駐程式。客戶端來來去去；常駐程式則一直保持連線。

為什麼常駐程式是必要的（而非風格上的選擇）：在 X11/Wayland 上，設定剪貼簿的行程**擁有該 selection**，
必須持續存活才能服務之後的貼上動作；而且「收到即自動複製 (auto_copy)」本身就是一個必須永遠在線的行為。

## 2. CLI 介面（所有實作皆相同）

執行檔名稱：`mesh-rs` → `clip`、`room-go` → `room`、`experiments/lan-go` → `lan`、`experiments/libp2p-mesh` →
`libp2p-mesh`。以下用 `BIN` 代表其中任一個。

| 指令 | 行為 |
|---|---|
| `BIN send [PATH] [--text\|--image\|--file\|--auto] [--name NAME]` | 傳送 `PATH`（或讀取 **stdin** 直到 EOF），嗅探型別（預設 `--auto`：PNG/JPEG magic → 圖片；`--file`／二進位 → 檔案；有效 UTF-8 → 文字），廣播給所有已連線的對等節點。檔案／圖片會夾帶它的 `filename`（來自 `PATH`／`--name`）。常駐程式接受之後即以 0 結束。 |
| `BIN recv [--follow] [--latest-image --emit-path] [--out PATH]` | 訂閱進來的項目。文字 → stdout。圖片／檔案 → 寫進暫存檔並印出其路徑（`--emit-path`），或寫到 `--out`。`--follow` 會持續串流直到 Ctrl-C；不加時則等待下一個單一項目（或回傳緩衝區中最新的那一個）。 |
| `BIN paste` | 把**最近收到的項目**寫進本機 OS 剪貼簿（文字用 set_text、圖片用 set_image；`file` 沒有對應的剪貼簿形式——會把它的路徑當成文字複製）。這就是預設 notify-first 模式下的「一鍵貼上」。 |
| `BIN clear [--all] [--yes]` | 清除這個工作階段 (session) 收到的資料（見 §8）。預設只清**暫存 (transient)** 部分（已抓取的 blob 快取、emit-path 暫存檔、記憶體內緩衝區）。`--all` 還會還原本階段對 sink 的寫入（把 `text_file` 截斷回工作階段開始時的長度、刪除本階段寫進 `save_dir` 的檔案）——除非加上 `--yes`，否則會先詢問。 |
| `BIN tui` | 啟動訊息軟體風格的聊天 TUI（見 §4）。 |
| `BIN pair` / `BIN join` | 建立成員資格。**mesh**（`clip`）：`pair --new` 印出一張配對票券 (ticket)（`--json` → `{"ticket":…}`）；對方對等節點執行 `pair <ticket>`。`lan`/`libp2p-mesh` 透過 mDNS 自動探索 (auto-discovery)，因此 `pair` 是一個有明文記載的 no-op。**room**（`room`）：`join <user@host:port>`（SSH 金鑰即身分）。*（規劃中：QR + 短碼驗證 + TOFU 核准——見 §9。）* |
| `BIN peers` | 列出已連線的對等節點（id／指紋 (fingerprint)、位址、直連／經中繼 (relay)）。**`room` 沒有客戶端可見的 peer 名單**（伺服器只負責中繼），因此它的 `peers` 回報的是像 `status` 那樣的連線狀態（§9）。 |
| `BIN status` | 顯示常駐程式狀態：身分、傳輸模式（lan/internet）、`auto_copy` 設定、對等節點數量、緩衝區大小。 |
| `BIN config set KEY VALUE` / `BIN config get KEY` | 持久化設定（見 §5）。 |
| `BIN daemon [--foreground]` · `BIN daemon stop` | 執行常駐程式（通常會自動啟動；`--foreground` 供除錯用）。`daemon stop` 會把它關掉，並套用 `clear_on_exit`（§8）。 |
| `BIN remote <ssh-host> [up\|down] [--room R]` | VSCode Remote 風格：在一台 SSH 主機上 bootstrap 這個工具（安裝到 `~/.local/bin`）並連線——**room** = SSH 通道 + join，**clip** = iroh 票券配對 (pairing)，**lan** = 共用區域網路上的 mDNS。`room` 是原生 Go 實作；`clip`/`lan` 共用 `scripts/remote.sh`。 |

**結束代碼 (exit codes)：** `0` 成功 · `1` 一般錯誤 · `2` 用法錯誤 · `3` 沒有常駐程式／無法連上常駐程式 ·
`4` 保留 · `5` 沒有東西可貼上／可接收。**`send` 在沒有任何已連線對等節點時仍以 `0` 結束**，並在 stderr 印出警告
（項目仍會被接受／緩衝）——因此它在 `set -e` 下是安全的，絕不會中斷 pipeline。

**全域旗標 (global flags)：** `--room NAME`（預設 `default`）、`--config-dir PATH`、`--socket PATH`、`-q/--quiet`、
`-v/--verbose`、`--json`（`peers`/`status` 的機器可讀輸出）。

## 3. Sink 與自動複製 (auto_copy)——收到的項目會流向哪裡（所有實作皆相同）

一個收到的項目可以依型別，散布到任意組合的 **sink**：

- **剪貼簿 (clipboard)**（文字、圖片）——由下面的 `auto_copy` 控制。`file` 沒有圖片的剪貼簿形式。
- **資料夾**（`save_dir`）——若有設定，收到的**圖片／檔案**項目會以它們的 `filename` 寫進該資料夾
  （會去重複：名稱衝突時改用 `name (2).ext`）。這是檔案的主要落地位置。
- **附加檔案**（`text_file`）——若有設定，收到的**文字**項目會被附加進去，每一筆前面加上
  `\n---\n<device> <ISO-ts>\n` 標頭。

Sink 是可疊加的（同一個項目可以同時進入剪貼簿「以及」資料夾／檔案）。所有路由都受下面的允許清單 (allowlist)
與回音抑制 (echo suppression) 規範，而且本工作階段寫出的所有東西都會被記錄下來供 `clear` 使用（§8）。

**剪貼簿——`config set auto_copy notify|on|off`：**

- **`notify`**（預設，最安全）：收到來自**允許清單內**對等節點的項目時，顯示桌面通知與 TUI toast，但
  **不會**寫入 OS 剪貼簿。使用者要在 TUI 按一個鍵，或執行 `BIN paste`，才會真的把它放上去。這就是 AirDrop
  那種「先 hand-off，再接受」的感覺，同時不會劫持剪貼簿。
- **`on`**：靜默地把每一個收到的項目直接寫進 OS 剪貼簿（最完整的 AirDrop 體驗；風險也較高）。
- **`off`**：絕不自動碰剪貼簿；內容只會在 TUI 中／透過 `recv` 看到。

**回音／迴圈抑制（每一種模式都必須實作）：**
1. 以 `msg_id` 去重複——同一個項目絕不處理兩次。
2. 記錄這個常駐程式最後一次**寫入**自己剪貼簿的值的內容雜湊，讓剪貼簿監看器不會把它再廣播出去。
   *（今天已經有記錄；要等 `broadcast_on_copy` 完成之後才會真正派上用場——§9。）*
3. 剛收到的項目絕不再廣播出去。收到的 ≠ 本機產生的。

**信任：** 只有來自**允許清單**上的對等節點的項目會被自動處理。*（Phase 0 信任所有能進到房間 (room) 的
傳送者；把未知對等節點的第一次接觸擋下來、等待明確核准的 TOFU 允許清單仍在規劃中——§9。）*
成員資格本身已經有把關：`clip` 需要票券／房間密鑰，`room` 則以 SSH 金鑰授權。

## 4. TUI（訊息軟體風格）

- 一個由聊天泡泡組成的訊息捲動區 (scrollback)（對等節點名稱 · 相對時間 · 內容），最新的在最下面，
  外加一個文字輸入框 (composer)。
- 文字泡泡直接內嵌顯示。當終端機圖形協定（Kitty/iTerm2/Sixel）可用時，圖片泡泡會顯示固定高度的內嵌
  **縮圖 (thumbnail)**；否則改用 Unicode 半格區塊算繪；再不行就顯示文字佔位符
  （`🖼 screenshot.png 1440×900 · 84 KB`）。圖片的編碼／縮放都在算繪執行緒**之外**進行。
- 每張圖片的按鍵操作：`y` 把全解析度複製到 OS 剪貼簿 · `s` 存成檔案 · `o` 用外部程式開啟。
- 輸入一行文字後按 Enter，會以文字送出（等同 `send`）。在有支援的環境中把圖片貼進輸入框，則會以圖片送出。
- 在 `notify` 模式下，收到項目時會跳出 toast，可用一個鍵**接受 → 剪貼簿**。

*內嵌圖片的一致性（Phase 0）：* `clip`（mesh-rs）能算繪真正的 Kitty/iTerm2/Sixel 縮圖，並以 Unicode 半格區塊
作為退回機制 (fallback)；`room`/`lan` 目前只顯示一行 metadata 佔位符（`🖼 W×H · size`、`📄 name · size`），
但有相同的 `y`/`s`/`o` 操作（§9）。**`clip` 用 `Ctrl+V` 送出 OS 剪貼簿中的圖片**（由常駐程式透過 arboard
讀取後廣播）；`room`/`lan` 則是透過 CLI（`send --image`）送出圖片。

## 5. 設定鍵（持久化於 OS 設定資料夾）

| 鍵 | 值 | 預設 | 意義 |
|---|---|---|---|
| `auto_copy` | `notify` \| `on` \| `off` | `notify` | §3 的剪貼簿 sink |
| `save_dir` | 路徑 \| "" | "" | 資料夾 sink：收到的圖片／檔案項目寫到這裡（§3） |
| `text_file` | 路徑 \| "" | "" | 附加 sink：收到的文字附加到這裡（§3） |
| `clear_on_exit` | `ask` \| `transient` \| `all` \| `never` | `ask` | §8——工作階段結束時要做什麼 |
| `device_name` | 字串 | 主機名稱 | 顯示給對等節點看的名稱 |
| `room` | 字串 | `default` | 預設的房間／主題 |
| `server` | `user@host:port` \| "" | "" | **僅限 `room`：** 客戶端常駐程式要連線的房間伺服器（由 `join` 設定） |
| `internet` | `on` \| `off` | `off` | *（規劃中 §9）* 僅限區域網路 vs 中繼／打洞 (hole punching)／全球探索 (discovery) |
| `broadcast_on_copy` | `on` \| `off` | `off` | *（規劃中 §9）* 監看本機剪貼簿並自動送出變更 |

設定資料夾：macOS `~/Library/Application Support/<dir>/`、Linux `$XDG_CONFIG_HOME/<dir>/`（或 `~/.config/<dir>/`）、
Windows `%APPDATA%\<dir>\`，其中 `<dir>` 是該實作的簡稱——`clip` 用 `mesh-rs`、`room` 用 `room`、`lan` 用
`lan`。IPC socket 放在 OS 的 runtime／暫存資料夾中。（`--config-dir`/`--socket` 可覆寫這兩者。）

## 6. OS 支援矩陣（目標＝全部都是一等公民）

| OS | 剪貼簿文字 | 剪貼簿圖片 | 備註 |
|---|---|---|---|
| macOS | ✅ | ✅（透過 NSPasteboard 的 PNG） | Phase 0 主要開發目標 |
| Linux X11 | ✅ | ✅（`image/png` target） | Phase 0 |
| Linux Wayland | ✅ | ✅ | 需要 `wayland-data-control`；XWayland／純文字退回機制（Phase 3） |
| Windows | ✅ | ✅（CF_DIB） | 具名管道 IPC（Phase 3） |

## 7. Phase 0 驗收（證明魔法成立的 MVP）

在同一個區域網路上、兩台裝置位於同一個房間、測試時設為 `auto_copy on`：
1. 在 A 上執行 `echo hi | BIN send` → B 上的 `BIN recv` 印出 `hi`；B 上的 `BIN paste` 把 `hi` 放進剪貼簿。
2. 在 A 上執行 `BIN send < testdata/small.png` → 在 B 上，`BIN recv --latest-image --emit-path` 寫出的 PNG
   其 **BLAKE3 雜湊值與來源相同**；若 `auto_copy on`，該圖片會出現在 B 的剪貼簿裡。

bake-off 測試腳本（`scripts/`）會為每一個實作自動化執行 (1) 與 (2)。

## 8. 工作階段與清除（隱私）

Hand-off 會留下資料（抓取到的 blob、暫存檔、剪貼簿快取的內容，以及——如果有設定的話——`save_dir` 裡的檔案
與 `text_file` 裡的行）。一個**工作階段**可以在結束時把那些清掉。

**「工作階段結束」是什麼意思（兩者都會觸發一次清除決策）：**
- **TUI 離開**（在 `BIN tui` 中按 `q`/`Ctrl-C`）→ 出現互動式提示：*clear this session? [t]ransient / [a]ll incl sinks / [n]o*。
- **`BIN daemon stop`**（或常駐程式收到 SIGTERM）→ 以非互動方式套用 `clear_on_exit` 政策。

**兩種範圍：**
- **暫存 (transient)**（永遠可以安全清除）：已抓取的 blob 快取、`recv --emit-path` 產生的暫存檔，以及
  記憶體中的已接收項目緩衝區。
- **sink**（以工作階段為範圍，需要確認）：把 `text_file` 截斷回它在**工作階段開始時的位移量**（因此只會移除
  *本階段*附加的文字），並刪除本階段寫進 `save_dir` 的檔案。絕不動到工作階段之前就存在的內容，也絕不刪除
  整個使用者資料夾。

**`clear_on_exit` 政策**（用於 `daemon stop` / SIGTERM）：`never`（全部保留）· `transient`（只清除暫存部分）·
`all`（暫存 + 本階段的 sink 寫入）· `ask`（若有接上 TTY 就詢問，否則退回 `transient`）。

**手動：** 隨時都可以執行 `BIN clear`（暫存）／ `BIN clear --all`（暫存 + 本階段的 sink，除非加 `--yes` 否則會先詢問）。

常駐程式會針對每一個工作階段記錄：暫存儲存區的位置、工作階段開始時 `text_file` 的大小，以及它寫進
`save_dir` 的檔案清單——因此清除是精確的、範圍內可還原的，絕不會破壞工作階段以外的東西。

## 9. 一致性與各實作的偏離（現況）

三個實作在核心 hand-off 上都符合 §2–§8（send/recv/paste/tui/clear/config/sink/工作階段，文字 + 圖片 +
檔案，跨機器，headless（無顯示環境））。以下誠實記錄與上述理想契約之間的已知偏離：

| 面向 | 契約（理想） | 目前現況 |
|---|---|---|
| `pair` 使用體驗 | 票券 **+ QR + 短碼驗證** | `clip`：只有票券（`--json`）。`lan`/`libp2p`：no-op（mDNS）。QR／短碼：**規劃中**。 |
| 信任 / TOFU | 未知對等節點的第一次接觸會被擋下等待核准 | 成員資格有把關（票券 / SSH 金鑰 / 房間密鑰），但房間內的傳送者是**全部信任**；逐一傳送者的 TOFU 核准：**規劃中**。 |
| `peers` | 即時名單（id、位址、直連／經中繼、最後出現時間） | `clip`/`lan`：真正的名單。`room`：**類似 `status`**（伺服器不保留客戶端可見的名單）。 |
| TUI 內嵌圖片 | 縮圖（Kitty/iTerm2/Sixel）→ 半格區塊 → 文字 | `clip`：完整（縮圖 + 半格區塊）。`room`/`lan`：**佔位符行** + `y`/`s`/`o` 操作。 |
| TUI 圖片傳送 | 從 TUI 送出圖片 | `clip`：**`Ctrl+V`** 送出 OS 剪貼簿中的圖片。`room`/`lan`：CLI `send --image`。 |
| `internet` / `broadcast_on_copy` | 設定開關會被實際遵守 | 這些鍵會被接受，但**尚未產生作用**——以區域網路優先；internet = 中繼路徑（延後處理）。 |
| 圖片傳輸 | 先公告，再依 `blob.hash` 拉取 | `clip`：依雜湊拉取。`room`/`lan`：內嵌 `blob_data`（Phase 0 擴充，PROTOCOL §1）。 |
| OS 涵蓋範圍 | macOS + Linux X11/Wayland + Windows | macOS + Linux X11 已驗證；headless Linux 已驗證。Wayland/Windows 剪貼簿：**尚未驗證**。 |
| `remote` | 每個工具一道指令 | `room` 為原生實作（SSH 通道）；`clip`/`lan` 透過 `scripts/remote.sh`（票券 / mDNS）。Bootstrap 需要有這個 repo，或一台架構相符的主機。 |

標示為「規劃中」／「延後處理」的項目，是刻意排除在目前範圍之外的（區域網路優先、Phase 0 plus）。各實作
實際上是怎麼建構的，請見 [`implementation.md`](implementation.md)；實測比較請見 [`BAKEOFF.md`](BAKEOFF.md)。
