# 使用指南 (Usage) — `room` (room-go, Go/wish central server)

!!! note "Terminology rule (zh-TW pages)"
    技術名詞首次出現以「中文 (English original)」格式呈現，例：依賴注入
    (dependency injection)。**不自創翻譯**——若無公認譯名直接保留英文
    （如 `embedding`、`tokenizer`）。代碼、API 名、CLI flag、套件名、檔名一律不翻。

**只需一台小伺服器 (one-small-server)** 的實作。裝置連線到一台中央 **SSH 房間 (room) 伺服器**
（charmbracelet/wish，公鑰認證），伺服器負責在同一個房間裡的所有人之間中繼 (relay) 剪貼簿 (clipboard)
項目。運行一台伺服器換來的好處：一條通往保留歷史記錄的自然路徑，以及一層免安裝的 SSH 客戶端。
伺服器看得到明文（Phase 0 沒有客戶端側的端到端加密 E2E）。

- 執行檔：**`room`** · 傳輸方式：**SSH/wish 中繼 (relay)**（SSH 公鑰指紋 (fingerprint) = 身分 (identity)） ·
  實作：[`../room-go/`](https://github.com/daviddwlee84/clipboard-handoff/tree/main/room-go/)
- 共用契約：[SPEC](SPEC.md)（CLI §2、sink §3、設定 §5、工作階段 (session) §8） · 傳輸格式/IPC：[PROTOCOL](PROTOCOL.md)
- 更深入的伺服器／測試腳本 (harness) 說明請見 [`../room-go/README.md`](https://github.com/daviddwlee84/clipboard-handoff/blob/main/room-go/README.md)。

## 安裝／建置

```sh
cd room-go
go build -o bin/room ./cmd/room          # binary -> room-go/bin/room
```

Go 1.26；剪貼簿後端（golang.design/x/clipboard）需要 cgo。請把 `bin/room` 放進你的 `PATH`，
這樣範例才能直接以 `room` 呼叫它。

## 身分 (identity) 模型（SSH 金鑰）

這裡沒有配對票券 (ticket)。裝置是以它的 **SSH 公鑰指紋 (fingerprint)** 來識別（PROTOCOL §3）：
金鑰*就是*身分。第一次執行 `join` 時，客戶端會產生自己的金鑰、印出指紋加上一行 `authorized_keys`、
自動啟動常駐程式 (daemon)，然後連線。伺服器依金鑰授權——Phase 0 是全部信任，或用
`room server --authorized-keys FILE` 加以限制。房間名稱就是 SSH 的 exec 指令；只有同一個房間的成員
才看得到彼此的項目。

## 運行伺服器

```sh
room server --addr :2299 --host-key /tmp/host_key
# --addr defaults to :2222; --host-key auto-generates if the path is missing/empty
# --authorized-keys FILE  -> restrict to listed keys (default: trust any key, Phase 0)
```

伺服器**不需要顯示環境**（它從不碰剪貼簿），而且它是唯一必須讓每台裝置都連得到的元件。
在兩台裝置都撥得到的機器上跑一份就好。

## 快速上手（伺服器 ＋ 兩台裝置）

```sh
BIN=./bin/room
PORT=2299
A=(--config-dir /tmp/a --socket /tmp/a.sock --room demo)
B=(--config-dir /tmp/b --socket /tmp/b.sock --room demo)

# 1) server
$BIN server --addr :$PORT --host-key /tmp/host_key &

# 2) connect each device — `join` prints its key fingerprint, auto-spawns the daemon, connects
$BIN "${A[@]}" join room@localhost:$PORT
$BIN "${B[@]}" join room@localhost:$PORT

# 3) text hand-off A -> B
printf 'hi' | $BIN "${A[@]}" send --text
$BIN "${B[@]}" recv                       # -> hi

# 4) image hand-off A -> B (hash-equal)
$BIN "${A[@]}" send --image < ../testdata/small.png
$BIN "${B[@]}" recv --latest-image --emit-path   # prints a PNG path; hash == source
$BIN "${B[@]}" paste                      # (auto_copy notify) place it on B's clipboard
```

`join` 會自動啟動常駐程式，所以明確執行 `daemon` 這個步驟是選用的。跨實體機器時，請把 `join` 指向
伺服器的區域網路 (LAN) IP，而不是 `localhost`（例如 `room join room@192.168.1.20:2299`）。

> **一行指令的 SSH 通道 —— `room remote <host>`。** 一個平行的變更加入了 `room remote <host>`
> 便利指令，可以一步透過 SSH 通道 (tunnel) 連線。這裡不記載它的細節以免內容過時——目前的 flag
> 與行為請見 [`../room-go/README.md`](https://github.com/daviddwlee84/clipboard-handoff/blob/main/room-go/README.md)。

## 指令參考

全域 flag（放在子指令之前）：`--config-dir PATH`、`--socket PATH`、`--room NAME`
（預設 `default`）、`--json`、`-q/--quiet`、`-v/--verbose`。

| 指令 | 作用 |
|---|---|
| `room server [--addr :2222] [--host-key PATH] [--authorized-keys FILE]` | 運行 SSH 房間伺服器。 |
| `room join <user@host:port>` | 印出客戶端金鑰指紋，並把常駐程式連上伺服器與房間。 |
| `room send [PATH] [--text\|--image\|--file\|--auto] [--name NAME]` | 讀取 `PATH`（或讀 **stdin** 直到 EOF）、嗅探型別、廣播出去。即使沒有任何對等節點 (peer) 也會以 `0` 結束（stderr 會有警告）。 |
| `room recv [--follow] [--latest-image --emit-path] [--out PATH]` | 文字 → stdout；圖片／檔案 → 具體化成檔案並印出路徑。`--follow` 會持續串流；`--latest-image` 只過濾出圖片。 |
| `room paste` | 把最近收到的項目寫入作業系統剪貼簿（文字→設定文字、圖片→PNG、**檔案 → 把它的路徑當成文字**）。 |
| `room tui` | 附掛在本機常駐程式上的通訊軟體風格聊天介面（見下文）。 |
| `room status [--json]` | 身分／裝置／房間／伺服器／連線狀態／`auto_copy`／`clipboard`／緩衝區。 |
| `room config set KEY VALUE` · `room config get KEY` | 保存／讀取設定（見「設定鍵」）。 |
| `room clear [--all] [--yes]` | 清除這個工作階段的資料（見「工作階段」）。 |
| `room daemon [--foreground] [--server user@host:port]` | 運行客戶端常駐程式（通常會自動啟動）。`--server` 一次完成設定並連線。 |
| `room daemon stop` | 關閉常駐程式，並套用 `clear_on_exit`。 |

> **關於 `room peers` 的說明：** 在 Phase 0，`peers` 是 `status` 的別名——它印出的是狀態區塊
> （其中包含對等節點數量），而不是一張獨立的對等節點表格。

**傳送檔案** —— `image` 是唯一可以貼到剪貼簿的二進位型別；`file` 則是任意位元組，附帶盡力而為的
MIME 與一個 `filename`：

```sh
room "${A[@]}" send report.pdf --file                 # force a file item, filename=report.pdf
cat archive.tgz | room "${A[@]}" send --file --name archive.tgz
```

型別嗅探遵循 [PROTOCOL §2](PROTOCOL.md)：`--auto`（預設）→ PNG/JPEG magic number = 圖片、合法 UTF-8 =
文字、其餘為檔案。在 Phase 0，圖片／檔案的位元組是**內嵌 (inline)** 在 envelope（訊息封裝格式）裡，
並透過伺服器中繼（announce+pull 是 Phase 1）；PNG 維持為標準格式，且 `blob.hash`（BLAKE3）會在
接收時驗證。

## Sink —— 收到的項目會去哪裡（SPEC §3）

可疊加；依型別決定，與 `auto_copy` 無關：

| Sink | 設定鍵 | 接收 |
|---|---|---|
| clipboard | `auto_copy` | 文字、圖片（`file` 永遠不會自動落到剪貼簿上） |
| folder | `save_dir` | 圖片 → `<filename\|hash>.png`、檔案 → `<filename\|hash>` —— 會去重複命名為 `name (2).ext` |
| append-file | `text_file` | 文字，附加在一段 `\n---\n<device> <ISO8601 ts>\n` 標頭之後 |

```sh
room "${B[@]}" config set save_dir  ~/Drop      # folder sink (image/file)
room "${B[@]}" config set text_file ~/room.log  # append sink (text)
```

**`auto_copy` 模式**（`room config set auto_copy notify|on|off`）：`notify`（預設）會緩衝並跳通知，
但不碰剪貼簿（請用 `room paste` 或 TUI 裡的 `y`）；`on` 會把每個項目直接寫入剪貼簿；`off` 則完全不碰。

## 設定鍵（SPEC §5，外加 `server`）

`auto_copy`（`notify`|`on`|`off`，預設 `notify`） · `save_dir`（路徑|""） · `text_file`（路徑|""） ·
`clear_on_exit`（`ask`|`transient`|`all`|`never`，預設 `ask`） · `device_name`（預設為主機名稱） ·
`room`（預設 `default`） · `internet`（Phase 3） · `broadcast_on_copy`（Phase 3） ·
**`server`**（`user@host:port`，由 `join` 設定）—— room-go 額外加的一個鍵，讓常駐程式能重新連回
正確的伺服器。

設定資料夾：macOS `~/Library/Application Support/room/`、Linux `$XDG_CONFIG_HOME/room/`、
Windows `%APPDATA%\room\`。可用 `--config-dir` 覆寫。

## 工作階段 (session) 與清除（SPEC §8）

一個工作階段就是常駐程式的一次執行。它會追蹤暫存 (transient) 儲存區（已取得的 blob 快取 ＋
`recv --emit-path` 產生的暫存檔 ＋ 記憶體內緩衝區）、工作階段開始時的 `text_file` 大小，以及它寫進
`save_dir` 的檔案——因此清除是精準的，永遠不會動到工作階段開始前就存在的內容。

```sh
room "${B[@]}" clear              # transient only
room "${B[@]}" clear --all        # + revert this session's sink writes (prompts unless --yes)
room "${B[@]}" clear --all --yes  # non-interactive
room "${B[@]}" daemon stop        # applies clear_on_exit; SIGTERM does the same
```

`clear_on_exit`：`never` · `transient` · `all` · `ask`（在 TTY 上會詢問，否則採用 `transient`）。
TUI 在離開時也會提出同樣的選擇。

## TUI (`room tui`)

一個附掛在本機常駐程式上的全螢幕聊天介面（charmbracelet bubbletea + lipgloss + bubbles）。它會自動
啟動／連線常駐程式、訂閱它的事件串流，並即時算繪收到的項目。它只是前端——`y` 是請常駐程式執行複製，
並遵循 `auto_copy`（notify-first）。

```sh
room "${A[@]}" tui        # after `join`, or with a server configured
```

標頭列：房間 · 🔑指紋 · 連線狀態／伺服器 · `auto_copy` · `clipboard: available|unavailable`。
共有兩種模式；`Esc` 在兩者之間切換。

| 模式 | 按鍵 | 動作 |
|---|---|---|
| compose | 輸入文字 + `Enter` | 把該行當成文字項目送出（會顯示成你自己的訊息泡泡）。 |
| compose | `Esc` | 切換到 browse 模式。 |
| browse | `↑`/`k`, `↓`/`j` | 移動訊息選取位置。 |
| browse | `g` / `G` | 跳到最舊／最新。 |
| browse | `y` | **透過常駐程式**把選取的（或最新的）項目複製到作業系統剪貼簿。 |
| browse | `s` | 把選取的圖片存到 `~/Downloads`（否則存到目前工作目錄）。 |
| browse | `o` | 用外部程式開啟選取的圖片（`open`/`xdg-open`）。 |
| browse | `i` / `Enter` | 回到輸入框 (composer)。 |
| browse | `q` | 離開（如果收過任何東西，會詢問是否清除這個工作階段）。 |
| 任一模式 | `Ctrl-C` | 離開（同樣會出現清除提示）。 |

**圖片：**每個圖片泡泡會顯示中繼資料（`🖼 name  W×H · size`），並可對常駐程式具體化出來的全解析度
PNG 使用 `y`/`s`/`o`。終端機內嵌圖形算繪（Kitty/iTerm2/Sixel）在 Phase 0 是一個**有明文記載的 stub**
——只有一行佔位文字，而不是真的像素（`clip` 則不同，它會算繪內嵌縮圖 (thumbnail)）。

## 跨機器注意事項

伺服器在設計上就是不需要顯示環境的，只要連得到就好。把 `bin/room` 部署到每個客戶端（常駐程式用
`CGO_ENABLED=0` 交叉編譯 ＋ scp 就可以了），然後讓每台都 `join` 到伺服器的位址。在沒有顯示環境的
客戶端上，剪貼簿功能會優雅降級（`status` 會顯示 `clipboard: unavailable`）；`send`／`recv`／`save_dir`
仍然可以運作。

## 疑難排解

- **`join` 失敗** → 確認伺服器連得到（`--addr` host:port）；如果你有設定 `--authorized-keys`，
  請確認這台裝置的指紋（由 `join` 印出）有在那個檔案裡。
- **沒有東西可以 paste/recv** → 結束碼 `5`；表示還沒收到任何東西。
- **沒有常駐程式** → 結束碼 `3`；表示客戶端連不到／無法啟動常駐程式。
- **Phase 0 的信任但書：** 伺服器信任任何金鑰，而且主機金鑰在連線時直接被信任
  （`InsecureIgnoreHostKey`）；寄送端允許清單 (allowlist)／TOFU 要到 Phase 1 才有。用在 loopback／
  區域網路上沒問題。
