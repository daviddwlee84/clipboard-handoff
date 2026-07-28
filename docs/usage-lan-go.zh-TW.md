# 使用指南 (Usage) — `lan` (lan-go, Go/quic+mDNS experiment)

!!! note "Terminology rule (zh-TW pages)"
    技術名詞首次出現以「中文 (English original)」格式呈現，例：依賴注入
    (dependency injection)。**不自創翻譯**——若無公認譯名直接保留英文
    （如 `embedding`、`tokenizer`）。代碼、API 名、CLI flag、套件名、檔名一律不翻。

**最精簡的 LAN 探測實作 (minimal LAN probe)**：quic-go 直連 + mDNS 自動探索 (auto-discovery)。在單一區域網路上零設定——位於同一個 *房間 (room)* 的兩台裝置會自動找到彼此並 hand-off 文字／圖片，不需要伺服器、不需要配對票券 (ticket)、也沒有配對 (pairing) 步驟。它存在的目的是回答 bake-off 的問題：*「iroh 的份量值得嗎？還是手工打造的 LAN mesh 就夠好了？」*——參見 [BAKEOFF](BAKEOFF.md)。

> **這是實驗，不是主打項目。** `lan-go` 是 **LAN 優先、且沒有網際網路路徑**：mDNS 只在本地有效，所以
> 一旦離開區域網路，你就得重新打造 iroh（`clip`）免費提供的那一套。要選可出貨的工具，請挑 `clip`
> (mesh-rs) 或 `room` (room-go)；`lan` 是快速路徑的驗證品。CLI 的底層管線放在 `shared-go`，因此
> `lan` 與 `libp2p-mesh` 的行為完全一致。

- 執行檔：**`lan`** · 傳輸層：**quic-go + mDNS**（`grandcat/zeroconf`）；身分 (identity) = 一份持久化
  自簽 TLS 憑證的 SHA-256（Syncthing 風格）· 實作：[`../experiments/lan-go/`](https://github.com/daviddwlee84/clipboard-handoff/tree/main/experiments/lan-go/)
- 共用契約：[SPEC](SPEC.md)（CLI §2、sink §3、設定 §5、工作階段 (session) §8）· wire/IPC：[PROTOCOL](PROTOCOL.md)
- 更多細節：[`../experiments/README.md`](https://github.com/daviddwlee84/clipboard-handoff/blob/main/experiments/README.md)、[`../experiments/lan-go/README.md`](https://github.com/daviddwlee84/clipboard-handoff/blob/main/experiments/lan-go/README.md)。

## 安裝／建置

```sh
cd experiments/lan-go
go build -o bin/lan ./cmd/lan            # binary -> experiments/lan-go/bin/lan
```

`go.work` 加上 `replace` 指示詞會讓這個探測實作指向 `../shared-go`，所以不論有沒有啟用 workspace
（`GOWORK=off`）都能建置。把 `bin/lan` 放到你的 `PATH` 上，就能直接以 `lan` 呼叫。

## mDNS 自動探索（不需配對）

每個常駐程式 (daemon) 都會廣告一個 `_clip-lan._udp` 服務，其 TXT record 帶有 `room=`、`fp=`、`port=`
與 `name=`；它同時瀏覽同一個服務，並且只連線到 `room` 相符的對等節點 (peer)。所以唯一的「配對」就是：
**在同一個區域網路上、以同一個 `--room` 執行一個常駐程式。** `lan pair` / `lan join` 存在，但文件明訂
它們是 **no-op**（只印出一則說明並以 0 結束）。同一台主機上跑兩個實例也可以（各自綁定不同的臨時 UDP
埠；由指紋 (fingerprint) 較小的一方發起撥號，以避免重複連線）。

## 快速上手（兩台裝置，同一個區域網路）

```sh
# Device B
lan --room demo daemon --foreground      # leave running (auto-spawned otherwise)

# Device A
echo "hello from A" | lan --room demo send

# Device B
lan --room demo recv                      # -> hello from A
lan --room demo paste                     # place it on B's OS clipboard
```

## 指令參考

全域 flag（可放在子指令**之前或之後**）：`--config-dir PATH`、`--socket PATH`、
`--room NAME`（預設 `default`）、`--json`、`-q/--quiet`、`-v/--verbose`。

| 指令 | 作用 |
|---|---|
| `lan send [PATH] [--text\|--image\|--file\|--auto] [--name NAME]` | 讀取 `PATH`（或從 **stdin** 讀到 EOF），嗅探型別後廣播。即使沒有任何對等節點也會以 `0` 結束（警告寫到 stderr）。 |
| `lan recv [--follow] [--latest-image --emit-path] [--out PATH]` | 文字 → stdout；圖片／檔案 → 落地成檔案並印出路徑。`--follow` 會持續串流；`--latest-image` 只篩出圖片。 |
| `lan paste` | 把最近收到的項目寫入作業系統剪貼簿 (clipboard)（文字→設定、圖片→PNG、**檔案 → 以文字形式放入其路徑**）。 |
| `lan tui` | 附掛在本機常駐程式上的通訊軟體風格聊天介面（見下文）。 |
| `lan status [--json]` | 身分／裝置／房間／傳輸層／`auto_copy`／`clipboard`／對等節點／緩衝區。 |
| `lan peers [--json]` | 列出目前已連線的對等節點（name · id · addr）。 |
| `lan config set KEY VALUE` · `lan config get KEY` | 寫入／讀取設定（見〈設定鍵值〉）。 |
| `lan clear [--all] [--yes]` | 清除本次工作階段的資料（見〈工作階段與清除〉）。 |
| `lan daemon [--foreground]` · `lan daemon stop` | 執行／停止常駐程式。`stop` 會套用 `clear_on_exit`。 |
| `lan pair` / `lan join` | **No-op** — 探索 (discovery) 是自動的（印出一則說明並以 0 結束）。 |

**傳送檔案** — `image` 是唯一能貼到剪貼簿的二進位型別；`file` 則是任意位元組，附帶盡力而為的 MIME 與
一個 `filename`：

```sh
lan send report.pdf                 # --auto: binary → file, filename=report.pdf
lan send --file blob.bin --name x   # force file, override the filename
cat notes.txt | lan send            # stdin → text (valid UTF-8)
lan send shot.png                   # → image (PNG/JPEG only; JPEG transcoded to PNG)
```

型別嗅探依循 [PROTOCOL §2](PROTOCOL.md)。blob 以 **inline** 方式夾帶（`blob_data`，Phase 0），
收到時會驗證其 BLAKE3 `blob.hash`。

## Sink — 收到的項目會落到哪裡 (SPEC §3)

各 sink 是疊加的；依型別決定，與 `auto_copy` 無關：

| Sink | 設定鍵 | 接收 |
|---|---|---|
| clipboard | `auto_copy` | 文字、圖片（`file` 永遠不會自動落到剪貼簿） |
| folder | `save_dir` | 圖片 → `<filename\|hash>.png`，檔案 → `<filename\|hash>` — 去重為 `name (2).ext` |
| append-file | `text_file` | 文字，接在 `\n---\n<device> <ISO8601 ts>\n` 標頭之後附加 |

```sh
lan config set save_dir  ~/Drop     # received image/file items land here
lan config set text_file ~/clip.md  # received text is appended here
```

**`auto_copy` 模式**（`lan config set auto_copy notify|on|off`）：`notify`（預設）會緩衝並發出
通知，但不碰剪貼簿（改用 `lan paste`，或在 TUI 中按 `p`／`y`）；`on` 會把每個項目都寫入剪貼簿；
`off` 則完全不碰。

## 設定鍵值 (SPEC §5)

`auto_copy`（`notify`|`on`|`off`，預設 `notify`）· `save_dir`（路徑|""）· `text_file`（路徑|""）·
`clear_on_exit`（`ask`|`transient`|`all`|`never`，預設 `ask`）· `device_name`（預設為主機名稱）·
`room`（預設 `default`）· `internet`（Phase 3）· `broadcast_on_copy`（Phase 3）。

設定資料夾：macOS `~/Library/Application Support/lan/`、Linux `$XDG_CONFIG_HOME/lan/`、
Windows `%APPDATA%\lan\`。可用 `--config-dir` 覆寫。

## 工作階段與清除 (SPEC §8)

一個工作階段 (session) 就是常駐程式的一次執行；它會追蹤暫存 (transient) 儲存區（blob 快取 +
`recv --emit-path` 的暫存檔 + 記憶體內緩衝區）、工作階段開始時的 `text_file` 大小，以及它寫入
`save_dir` 的那些檔案。

```sh
lan clear                # transient only
lan clear --all          # + revert this session's sink writes (prompts unless --yes)
lan clear --all --yes    # non-interactive
lan daemon stop          # applies clear_on_exit; SIGTERM does the same
```

`clear_on_exit`：`never` · `transient` · `all` · `ask`（在 TTY 上詢問，否則視同 `transient`）。TUI
在離開時會提出同樣的選擇。

## TUI (`lan tui`)

一個精簡的通訊軟體風格聊天介面（bubbletea + lipgloss + bubbles），附掛在本機常駐程式上；它重用常駐
程式的事件串流與 IPC 操作，從不直接碰觸傳輸層或剪貼簿。

```sh
lan --room default tui              # auto-spawns the daemon if it is down
```

標頭列：房間 · id · 對等節點 · `auto_copy` · `clipboard: available|unavailable`。`Esc` 可在輸入框
(composer) ↔ 瀏覽模式之間切換。

| 按鍵 | 動作 |
|---|---|
| 輸入文字 + `Enter` | 以文字項目送出該行（自己的泡泡會立即出現）。 |
| `Esc` | 在輸入框與瀏覽模式之間切換焦點。 |
| `↑`/`k`, `↓`/`j` | 在瀏覽模式中移動選取（`g` / `G` = 最上 / 最下）。 |
| `y` | **透過常駐程式**把選取的泡泡複製到作業系統剪貼簿（檔案會以文字形式複製其路徑）。 |
| `s` | 把選取的圖片存到 `~/Downloads`（否則存到暫存資料夾）。 |
| `o` | 以作業系統預設應用程式開啟選取的圖片／檔案。 |
| `p` | 把常駐程式最近收到的項目貼到剪貼簿。 |
| `q` / `Ctrl-C` | 離開——若本次工作階段收過任何東西，會先詢問 `clear this session? [t]ransient / [a]ll incl sinks / [n]o`。 |

**未完成的部分 (Stubs, Phase 0)：** 行內圖片預覽只是佔位符（`🖼 <hash>.png W×H · size`），而不是
Kitty/iTerm2/Sixel 縮圖 (thumbnail)；檔案泡泡顯示 `📎 name · size`；輸入框只支援文字（無法貼上
圖片）；歷史紀錄從空白開始（只顯示 TUI 開啟期間收到的項目）。

## 疑難排解

- **對等節點始終連不上** → 區域網路上的 mDNS multicast 可能被封鎖，或兩台主機不在同一個 L2 網段。
  `lan` 沒有配對票券／中繼 (relay) 的退回機制 (fallback)（這正是這個實驗的重點）——若區域網路有過濾，
  或需要跨網路 hand-off，請改用 `clip`（配對票券）或 `room`（伺服器）。
- **沒有東西可貼上／接收** → 離開碼 `5`。**沒有常駐程式** → 離開碼 `3`。
- headless（無顯示環境）主機：剪貼簿會優雅降級（`status` 會顯示 `clipboard: unavailable`）；
  `send`／`recv`／`save_dir` 仍然可用。
