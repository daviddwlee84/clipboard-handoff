# IMPLEMENTATION — 實作說明 (how it's built, and how the approaches compare)

!!! note "Terminology rule (zh-TW pages)"
    技術名詞首次出現以「中文 (English original)」格式呈現，例：依賴注入
    (dependency injection)。**不自創翻譯**——若無公認譯名直接保留英文
    （如 `embedding`、`tokenizer`）。代碼、API 名、CLI flag、套件名、檔名一律不翻。

這是各工具[使用指南](README.md)的*架構 (architecture)* 姊妹篇。它說明三個實作共用了哪些東西、每一個實作內部
實際上是怎麼接起來的，以及——這正是這場 bake-off 的重點——把它們並排比較時，各傳輸層之間有什麼差異。本文是
讀真實程式碼寫成的；只要某個實作偏離了 [SPEC](SPEC.md)/[PROTOCOL](PROTOCOL.md) 的約定，文中都會指出來。

以下是契約文件的參照，本文不再重述：**[SPEC.md](SPEC.md)**（CLI 介面、sink、工作階段 (session)、設定鍵）、
**[PROTOCOL.md](PROTOCOL.md)**（envelope、分幀 (framing)、身分 (identity)、IPC）、**[BAKEOFF.md](BAKEOFF.md)**
（實測數據 + 評分表 (scorecard)）。使用方式：**[usage-mesh-rs.md](usage-mesh-rs.md)**、
**[usage-room-go.md](usage-room-go.md)**、**[usage-lan-go.md](usage-lan-go.md)**。

| 實作 | 執行檔 | 語言 | 傳輸層 | 狀態 |
|---|---|---|---|---|
| `mesh-rs` | `clip` | Rust | iroh (QUIC)，直連 + mDNS | 主力 (headliner) |
| `room-go` | `room` | Go | SSH/wish 中央房間 (room) 伺服器 | 主力 (headliner) |
| `experiments/lan-go` | `lan` | Go | quic-go + mDNS | 探路 (probe) |
| `experiments/libp2p-mesh` | `libp2p-mesh` | Go | libp2p gossipsub + mDNS | **已擱置 (parked)** |

---

## 1. 共通架構

每個實作的形狀都一樣：**每台裝置一個常駐程式 (daemon)** + 透過本機 IPC socket 連線的**短命瘦客戶端
(thin client)**。唯一不同的只有常駐程式底下的網路傳輸層。本節談的是真正完全相同的部分（這也正是 bake-off
測試腳本 (harness) 能用同一組指令驅動任何一個實作的原因）。

### 1.1 為什麼常駐程式是必要的（不是風格選擇）

> **圖片與檔案只能透過該裝置上一個原生、常駐的代理程式，才有辦法進到該裝置作業系統的剪貼簿 (clipboard)。**

有兩個硬性限制迫使我們這麼做：

1. **選取區所有權 (selection ownership)。** 在 X11/Wayland 上，*設定*剪貼簿的那個行程**擁有選取區
   (selection)**，而且必須保持存活才能服務後續的貼上動作。一個射後不理 (fire-and-forget) 的 CLI 會在它結束的
   瞬間就把選取區丟掉。
2. **永遠在線的 hand-off。**「收到就自動複製 (auto_copy)」和「先通知我、再一鍵接受」這類行為，必須在其他東西
   都沒在跑的時候仍然運作；而終端跳脫序列 (OSC 52) 只能承載**純文字**（約 74 KB，還會被 tmux 濾掉）——沒有
   圖片，也沒有檔案路徑。所以「每台裝置一個擁有剪貼簿的常駐程式」是躲不掉的。

常駐程式擁有這些東西：網路連線、裝置身分金鑰、受信任對等節點 (peer) 集合（允許清單 (allowlist)）、一個有界的
接收項目環形緩衝區、一份內容定址 (content-addressed) 的 blob 快取，以及**作業系統剪貼簿**。`send`/`recv`/
`paste`/`clear`/`pair`/`join`/`tui`/… 都是不持有任何網路狀態的瘦客戶端，而且在常駐程式沒在跑時會**自動生成
(auto-spawn)** 它（以 detached 方式）。**TUI 同樣只是一個前端**——它自己永遠不會去開剪貼簿（那會讓它變成第二
個擁有者）；它的 `y` 複製動作是繞道常駐程式完成的。

### 1.2 本機 IPC（客戶端 ⇄ 常駐程式）

與傳輸層無關，三個實作的形狀完全一致（PROTOCOL §4）：

- **Socket**：位於 runtime 資料夾中的 Unix domain socket——例如 `$XDG_RUNTIME_DIR/<bin>/daemon.sock`（退回機制
  (fallback) 為暫存資料夾），可用 `--socket` 覆寫。Windows 上規劃採用具名管線 (named pipe)。
- **分幀 (framing)**：請求與回應都採用 **4 位元組大端序長度前綴 + CBOR**。每條連線一個請求，例外是
  `Subscribe` / `recv --follow` / `tui`，它們會保持連線開啟並接收**事件串流**（`Item` / `PeerUp` / `PeerDown` /
  `Toast`）。
- **自動生成 (auto-spawn)**：連線被拒時，客戶端會以 detached 方式啟動 `<bin> daemon`（繼承 `--config-dir` /
  `--socket` / `--room`），然後以短暫退避重試（mesh-rs：50×100 ms；room-go：5 s / 75 ms）。
- **有版本控管**：會檢查 `ipc_version`（目前為 1）；不相符時會明確報錯。

mesh-rs 使用 `interprocess` crate + `ciborium`；room-go 與 shared-go 使用 `net.Listen("unix")` +
`fxamacker/cbor`。同樣的傳輸格式 (wire format)，不同的函式庫。

### 1.3 與傳輸層無關的 envelope

單一的邏輯訊息形狀，**在三個實作中一律以 CBOR 編碼於傳輸格式上**（PROTOCOL 允許實驗性質使用 JSON，但實際上
沒人在用——見 §5 的分歧說明）：

```
Envelope { v:1, msg_id: ULID, type: text|image|file, mime, sender, device_name, ts(unix-ms),
           filename?,  text? | blob{ hash: BLAKE3-hex, size, w?, h? } }
```

- **`msg_id`** 是 **ULID**（可依時間排序）——也是去重 (dedupe) 用的鍵值。
- **`sender`** 是傳輸層的穩定身分（Ed25519 node id / TLS 憑證指紋 (fingerprint) / SSH 金鑰指紋 / PeerId），因此
  允許清單的檢查是由傳輸層完成認證的。
- 選填欄位標記為 `omitempty` / `skip_serializing_if`，而解碼是**容忍未知欄位**的，所以新增一個欄位（例如
  `filename`）仍能與較舊的對等節點保持傳輸格式相容。

**型別嗅探（`--auto`，PROTOCOL §2）** 在各處都一樣：PNG magic（`89 50 4E 47 0D 0A 1A 0A`）或 JPEG（`FF D8 FF`）
→ **image**（JPEG 會轉碼成 PNG，也就是標準傳輸形式；PNG 則逐位元組原樣通過，因此保留了它的 BLAKE3）；`--file`
或非 UTF-8 位元組 → **file**（`application/octet-stream`，mime 由副檔名推測）；其餘只要是合法 UTF-8 → **text**。
任意二進位資料永遠不算錯誤——它*就是*一個合法的檔案。

**圖片規則**：image 在傳輸格式上永遠是 PNG，是唯一能*以圖片形式*進到剪貼簿的型別，而且它的 `blob.hash`
（BLAKE3）**在接收時必須驗證**。**file** 同樣採用內容定址，但**永遠不能**被貼到剪貼簿——`paste`/`y` 複製的是
它的**本機路徑，以剪貼簿文字形式**呈現。

**實作分歧之處——blob 的搬運方式。** 這是唯一有實質意義的傳輸格式差異，而且是刻意為之：

| | 圖片/檔案位元組 | 理由 |
|---|---|---|
| `mesh-rs` | **announce + pull**：廣播小小的 envelope，每個接收端再以 `blob.hash` 為鍵，透過直連的 QUIC 雙向串流把位元組拉回來（`WireMsg::BlobRequest`/`BlobResponse`），做 BLAKE3 驗證後快取 | 永遠不會用位元組灌爆網路；這是 PROTOCOL §1 的目標 |
| `room-go`、`lan-go`、`libp2p-mesh` | **內嵌 `blob_data`**：PNG/檔案位元組直接搭*在* envelope *裡面*（這是有文件記載的 Phase-0 簡化做法） | 最簡單；`MaxFrame` 32 MiB 足以輕鬆容納測試用圖片 |

兩種做法都維持 PNG 為標準形式，也都在接收時驗證 BLAKE3，因此 bake-off 比較的是同一件事。讓 Go 實作也支援
announce+pull 屬於 Phase-1 項目。

### 1.4 notify-first 的自動複製 (auto_copy)、回音抑制 (echo suppression)、sink、工作階段

這四種行為在契約上完全一致（SPEC §3/§8）；每個實作都在自己的常駐程式狀態之上重新實作了一遍。

- **`auto_copy`（`notify` 為預設值 | `on` | `off`）** —— `notify`：先緩衝並發出一個 `Toast`，**不**去動剪貼簿
  （按 `y` 或執行 `paste` 才算接受）；`on`：立刻寫入剪貼簿（檔案除外——永遠不會被自動複製）；`off`：完全不碰。
- **回音／迴圈抑制** —— (1) 以 `msg_id` 去重（一個有界的已見集合，最舊的先淘汰；上限 4096）；(2) 記錄最後一次
  寫入*我們自己*剪貼簿的值的 BLAKE3（`note_written` / `lastCopy`），這樣 `on` 模式就絕不會把它剛剛貼上的東西
  再廣播出去；(3) 收到的項目永遠不再轉播出去。規則 (2) 已經存在並有單元測試，但要等到 `broadcast_on_copy`
  的剪貼簿監看功能落地（Phase 1）之後才會真正被*觸發*——見 §5。
- **附加式 sink** —— 收到的項目會依型別扇出，與剪貼簿彼此獨立：**`save_dir`** 會收到 image/file
  （`<filename|hash>[.png]`，同名時去重為 `name (2).ext`）；**`text_file`** 會在一個
  `\n---\n<device> <ISO8601-ts>\n` 標頭之後附加文字。sink 是在抑制*之後*才執行，而且這個工作階段寫出的所有
  東西都會被記錄下來供 `clear` 使用。
- **工作階段與清除** —— 常駐程式會逐次執行記錄：**暫存 (transient) 儲存區**（blob 快取 + `recv --emit-path`
  產生的檔案 + 記憶體中的緩衝區）、**工作階段開始時的 `text_file` 大小**（也就是截斷位移量，若 sink 在工作
  階段中途被重新設定則會重新錨定），以及**它寫過的 `save_dir` 檔案清單**。`clear` 會清掉暫存內容；
  `clear --all` 還會把 `text_file` 截斷回該位移量，並刪除剛好屬於這個工作階段的 `save_dir` 檔案——永遠不會動到
  工作階段之前就存在的內容，也永遠不會刪掉整個資料夾。`daemon stop` / SIGTERM 會套用 `clear_on_exit` 政策
  （非互動情況下 `ask` → `transient`）；TUI 在退出時也會跳出同一個 `[t]ransient / [a]ll / [n]o` 提示。

### 1.5 headless（無顯示環境）降級

每個常駐程式都會**在啟動時探測剪貼簿一次**（mesh-rs：`arboard::Clipboard::new().is_ok()`，可用
`CLIP_FORCE_HEADLESS` 強制；room-go/shared-go：`clip.Available()`）。在沒有顯示環境的主機上，它會記錄一則警告
並降級：`send`/`recv`/`--emit-path`/探索 (discovery)/傳輸/sink 全都照常運作；只有 `paste` 與 `auto_copy on` 會
變成明確的無動作（絕不會 panic），而 `status` 會回報 `clipboard: unavailable`。這正是跨機器測試能夠以一台
headless Ubuntu 機器為目標的原因。

---

## 2. 各實作的內部細節

### 2.1 `mesh-rs` — `clip` (Rust / iroh)

檔案：`mesh-rs/src/{main,daemon,client,proto,config,clipboard,tui}.rs`。執行檔 `clip`。

- **傳輸層 (transport)** — **iroh，精確鎖定 `=1.0.2`**（其 API 變動頻繁；程式碼是針對 `EndpointId`/`EndpointAddr`、
  `presets::Minimal`、`Router`/`ProtocolHandler`、`open_bi`/`accept_bi` 撰寫的）。Phase-0 endpoint = preset `Minimal` +
  `RelayMode::Disabled`（僅限 LAN）。對等節點 (peer) 之間的訊息是一個走 **直接 QUIC 雙向 (bidi) 串流** 的小型 CBOR enum：
  `WireMsg::Announce(Envelope) | BlobRequest{hash} | BlobResponse{ok,bytes}`。
- **身分 (identity)** — 一把持久化在 `<config-dir>/secret.key` 的 Ed25519 `SecretKey`（32 bytes、`0600`）；其 `EndpointId`
  即為 `sender`。設定資料夾預設為 `directories::ProjectDirs(… "mesh-rs")`。
- **探索 (discovery) / 連線建立** — 兩條並存的路徑：
  - **配對票券 (ticket)**：`pair --new` 回傳 base32(CBOR of `EndpointAddr` = id + 可直連位址，包含
    `loopback:port` 以及探索到的 LAN 位址)。`pair <ticket>` 會解碼並執行 `endpoint.connect(addr, alpn)`。
  - **mDNS 自動探索**（相同 `--room`、不需 ticket），透過搭配用的 crate `iroh-mdns-address-lookup =0.4.0`
    （iroh 1.0.2 把 "discovery" 更名為 *address lookup*）。服務名稱以房間 (room) 為範圍：`clip` + `hex(blake3(room)[:8])`
    → 記錄 `<endpoint>._clip<hex>._udp.local`。
- **房間隔離採雙重強制**：(1) mDNS 服務名稱是每個房間各自獨立的（不同房間永遠看不到彼此）；(2) 房間密鑰被摺進
  **ALPN** — `alpn_for_room = b"mesh-rs/clip/0/" + hex(blake3(room)[:8])` — 因此跨房間的撥號會在 QUIC 交握階段被拒絕。
- **N-peer** — `broadcast()` 會對每個已連線 peer 的 `Connection` 開一條 bidi 串流並寫入一則 `Announce`。這是對每個已連線
  peer 的**成對直接 QUIC**；**iroh-gossip**（真正的 N-peer 公告 mesh）**尚未**接上（BAKEOFF 的 multi-peer 只給 3 分正是這個原因）。
- **圖片／檔案** — announce + pull：`fetch_blob` 開一條串流、送出 `BlobRequest{hash}`、以 BLAKE3 驗證
  `BlobResponse`、快取在記憶體中並寫入 `<config-dir>/blobs/clip-<hash[:16]><ext>`，讓 `recv --emit-path`
  與 `paste` 永遠有路徑可用。**iroh-blobs**（可續傳的內容定址 (content-addressed) 傳輸）則延後處理。
- **剪貼簿 (clipboard)** — `arboard`（`image-data`）；PNG↔RGBA 之間以 `image` crate 橋接。`arboard` 是阻塞式的，而且它的
  handle 不是 `Send`，所以每一次寫入都在 `spawn_blocking` 裡執行。
- **TUI** — 在 tokio runtime 上使用 `ratatui 0.29 + ratatui-image 9 + tui-textarea 0.7 + crossterm 0.28`（整組一起鎖版；
  tui-textarea 把 ratatui 上限卡在 0.29）。透過 ratatui-image 的 `StatefulProtocol` 提供**行內圖片縮圖 (thumbnail)**：
  如果啟動時 `Picker` 探測到真正的終端機圖形協定（Kitty/iTerm2/Sixel）就用它，否則用 Unicode 半格區塊字元，再不然就以中繼資料
  那一行作為文字佔位符。**解碼與縮放都跑在 UI 執行緒之外**（一個 `spawn_blocking` 解碼加上一個 resize worker task），
  所以事件迴圈永遠不會被卡住；若終端機忽略圖形能力查詢，代價是啟動時約 1–2 秒的探測逾時。
- **值得注意的程式碼／陷阱**：
  - **Split-brain 防護**：常駐程式 (daemon) 會**先綁定 IPC socket，再綁（較慢的）iroh endpoint**，因此自動催生它的客戶端
    不會競爭而生出*第二個* daemon；`probe_alive` 讓啟動具備冪等性（若已經有人在監聽就直接結束）。
  - **避免雙向撥號**：收到 mDNS 的 `Discovered` 事件時，只有 **`EndpointId` 較大**的那一方會撥號
    （`if my_id < peer_id { continue }`）；另一方負責接受。一組進行中的 `dialing` 集合再加上 `peers` 檢查，可阻止重複事件
    造成重複連線。
  - **多網卡 / Tailscale 注意事項**：endpoint 會廣告所有非 loopback 位址，但 mDNS multicast 只會跨越實體 LAN 介面 —
    VPN 介面通常不會，所以自動探索需要 LAN multicast（否則就退回使用 ticket；透過 LAN 位址的直接 QUIC 仍然可用）。
  - 另有一個獨立的 `sent` ring 保存本機發出的項目，讓 TUI 能對自己的訊息泡泡執行 `PasteItem`。
  - **信任**：Phase 0 會自動把任何你連上的 peer 加入允許清單 (allowlist)；TOFU-with-approval 屬於 Phase 1。
- **成本** — iroh 的 release 編譯**超過 7 分鐘**、557 個鎖定的 crate；binary release 24 MB（stripped 20 MB）／debug 79 MB。
  這是為了日後能*免費*取得 NAT 穿透 + 中繼 (relay) + 內容定址 blob 所付出的代價。

### 2.2 `room-go` — `room` (Go / wish SSH 伺服器)

檔案：`room-go/cmd/room/{main,remote,flagset}.go`、`room-go/internal/{server,broker,daemon,ipc,wire,config,clip,tui}/`。
執行檔 `room`、模組 `…/room-go`、Go 1.26、**需要 cgo**（剪貼簿後端）。

- **傳輸層** — 一台真正的中央 **SSH 房間伺服器**（`charmbracelet/wish` + `charmbracelet/ssh`，公鑰認證），而不是 TCP
  退回機制 (fallback)。原生客戶端這一層把 **原始 SSH 工作階段 (session) 當成位元組管線**：客戶端把**房間名稱當成 SSH 指令**
  執行（`sess.Start(room)`）；伺服器讀 `sess.Command()[0]` 作為房間名稱，並在該房間成員之間中繼**不透明的長度前綴 CBOR frame**。
  伺服器從不解碼 envelope。
- **身分** — 客戶端的 SSH ed25519 金鑰放在 `<config-dir>/id_ed25519`（OpenSSH PEM，首次 `join` 時產生）；
  `sender` = 其 `FingerprintSHA256`。伺服器以金鑰授權：`--authorized-keys FILE` 可加以限制，否則就是 Phase-0 的
  **全部信任 (trust-all)**（指紋 (fingerprint) 仍會被記錄下來）。設定資料夾 = `os.UserConfigDir()/room`。
- **Broker** — 一份記憶體中的 `map[room]map[*Session]struct{}`，改編自作者 `sshbbs` 專案的
  `internal/chat/broker.go`。**自送防護 (self-send guard)**：`Broadcast` 明確排除發起者
  （`if s == from { continue }`），因此客戶端絕不會回音自己送出的內容。收件者在讀鎖之下取快照，並在鎖之外遞送；
  外送緩衝區（256）滿了就丟棄，而不是讓 fan-out 卡住。
- **N-peer** — 由原生伺服器對房間所有成員做 fan-out（BAKEOFF 的 multi-peer 給 5 分）。
- **圖片／檔案** — envelope 中**內嵌 `blob_data`**，經由伺服器中繼；PNG 為標準格式、以 BLAKE3 驗證、實體化到
  `<config-dir>/blobs/<hash>[.png|ext]`。伺服器端的 pull-by-hash 屬於 Phase 1。
- **剪貼簿** — `golang.design/x/clipboard`（原生支援 PNG、cgo）。`clip.Available()` 用來把關 headless（無顯示環境）降級。
- **連線迴圈** — `connectLoop` 維持 SSH 工作階段存活，斷線時重新撥號（2 秒 backoff）；
  `HostKeyCallback = InsecureIgnoreHostKey`（Phase 0 的 loopback／通道情境；host-key pinning 屬於 Phase 1）。
- **TUI** — `bubbletea + lipgloss + bubbles`，`Subscribe` 到 daemon 的事件串流。**行內圖片渲染是一個有文件記載的 stub**：
  圖片泡泡只顯示中繼資料佔位符（沒有像素）；`y`/`s`/`o` 作用在 daemon 實體化出來的全解析度 PNG 上。
  `renderImageBody` 是為日後 `rasterm` 風格預覽預留的 drop-in。
- **值得注意的程式碼／陷阱**：
  - **Host key 產生的修正**（bake-off 中遇到的真實跨機器 bug）：`wish.WithHostKeyPath` 會委派給
    `charmbracelet/keygen`，而它會**對金鑰的上層資料夾做 chmod** → 當金鑰位於伺服器上像 `/tmp` 這種共用資料夾底下時
    就會 `EPERM`。`room` 改為自行載入／產生 ed25519 host key
    （`hostKeyPEM` + `wish.WithHostKeyPEM`），會建立缺少的上層資料夾，但絕不對既有的資料夾做 chmod。
  - Phase 0 的 `peers` 直接沿用 `status` 的輸出（伺服器不會推送 peer 清單；`status.peers` 為 0）。
  - 在沒有伺服器／沒有 peer 的情況下執行 `send` 會回傳 IPC **代碼 4** — 客戶端會警告但以 0 結束（在 `set -e` 下是安全的）。
- **優勢／成本** — 最精簡的 binary（**10 MB**）與 idle RSS（**8.2 MB**）；也是唯一一個天然通往**伺服器端歷史紀錄**（SQLite）
  以及**零安裝 `ssh room@server` + OSC-52 純文字層**（Phase 1）的實作。代價：你得自己跑並信任一台伺服器
  （除非加上 E2E，否則它看得到明文），而且 LAN 使用無法零設定。

### 2.3 `experiments/lan-go` — `lan` (Go / quic-go + mDNS)，建構於 `shared-go`

檔案：`experiments/shared-go/{wire,ipc,config,clip,daemon,cli,transport}/`、
`experiments/lan-go/internal/lantransport/`。執行檔 `lan`。

**shared-go 的設計 — 單一 daemon、可插拔的傳輸層。** `shared-go` 擁有 wire 之上的一切；一個 probe *只*需要寫出一個實作
以下介面的 transport：

```
Transport = Identity() · Start(ctx) · Broadcast(env) · OnReceive(fn) · Peers() · Close()
```

一個 probe 的 `main` 大約 15 行（`cli.App{BinName, NewTransport}`）。`shared-go/daemon` 提供 ring buffer、
`msg_id` 去重 + last-written-hash 抑制、`notify`/`on`/`off` 自動複製 (auto_copy)、可累加的 sink，以及 session/clear
追蹤 — **語意與 §1.4 完全相同**，因此 `lan` 與 `libp2p-mesh` 在傳輸層之上的行為一致。（兩個主打實作維持完全獨立：
它們**不會** import `shared-go`，而 `shared-go` 也不會 import 它們。）

**lan-go transport**（`lantransport`）：

- **傳輸層** — `quic-go` **v0.60.0** 直接連線。Envelope 以**一條 QUIC 單向 (uni) 串流承載一則**的方式傳送
  （長度前綴 CBOR）。圖片走**內嵌 `blob_data`**（收到時以 BLAKE3 驗證）。
- **身分** — 一份持久化的自簽 **TLS 憑證**（`<config-dir>/tls_cert.pem` + `tls_key.pem`，一把 ECDSA
  P-256 金鑰）；身分就是**憑證 DER 的 SHA-256 指紋**（Syncthing 風格）。Peer 之間透過 QUIC 連線上的
  **雙向 TLS (mutual TLS)** 互相認證（ALPN `clip-lan/0`；TOFU — 接受任何憑證，以指紋作為索引鍵）。
- **探索** — 透過 `grandcat/zeroconf` **v1.0.0** 的 mDNS/DNS-SD：每個 daemon 都廣告一個 `_clip-lan._udp`
  服務，其 **TXT 記錄帶有 `room=` / `fp=` / `port=`（真正的臨時 QUIC 埠）/ `name=`**，同時瀏覽同一個服務，
  只連線到 `room` 相符的 peer。**房間隔離靠的是 TXT 的 `room=` 比對**，而不是 ALPN（ALPN 是每個工具固定的常數）。
- **撥號去重** — 同一台主機上的兩個 daemon 會綁定不同的臨時 UDP 埠（`:0`）並廣告真實的埠號。為了避免互相重複連線，
  由**指紋字典序較小的一方負責撥號**（`if t.fp >= fp { return }`）；另一方接受。（注意這與 mesh-rs 的「較大 id」規則方向
  *相反* — 想法相同、正負號不同。）
- **兩個 Go probe 親手解掉的問題**（BAKEOFF 中最具決策參考價值的發現）：對稱的 mDNS 會讓**兩個 peer 同時嘗試撥號**
  （以較小指紋規則解決），以及 **mDNS 在第一次命中之後就不再重新查詢** — lan-go 的解法是**每一輪瀏覽都用一個全新的
  短生命週期 resolver**（瀏覽 3 秒 + 暫停 1 秒，反覆循環），讓晚到／重新連線的 peer 能被重新探索到 — *而這兩件事 iroh
  都免費幫你藏起來了*。（shared-go daemon 在 `Broadcast` 之前也會寬限等待 peer 出現，所以剛啟動就馬上 `send` 不會被丟掉。）

### 2.4 `experiments/libp2p-mesh` — 已擱置

檔案：`experiments/libp2p-mesh/internal/meshtransport/`。執行檔 `libp2p-mesh`。使用相同的 `shared-go` daemon；
只有 transport 不同。

- **傳輸層／拓樸** — `go-libp2p` **v0.48.0** + `go-libp2p-pubsub` **v0.17.0** 的 **gossipsub**
  （`NewGossipSub` 不帶任何選項，因此 `floodPublish` 維持函式庫預設值 — 開啟）。Envelope 會發佈到 topic
  `clip-exp/<sha256(room)>`（`topic.Publish`，5 秒逾時）。真正用來擋住「啟動後第一次送出的訊息在訂閱傳播完成前被丟掉」的
  防護位於上一層的 **shared-go daemon `waitForPeers`**（每 100 ms 輪詢 (polling) 一次、最多 5 秒，之後再給 300 ms 的
  沉澱寬限）— lan-go 也同樣使用 — 另外 daemon 會在廣播前先把自己的 `msg_id` 標記為已見，藉此抑制 gossipsub 的自我遞送。
- **身分** — 一把持久化的 libp2p 私鑰（`<config-dir>/libp2p.key`，首次執行時產生的 Ed25519 金鑰）；身分
  = **PeerId**。Host 監聽 `/ip4/0.0.0.0/tcp/0`（臨時 TCP 埠）。
- **探索** — libp2p mDNS（`p2p/discovery/mdns`），服務標籤由房間推導而來：`clipexp<sha256(room)[:16]>`。
  撥號去重：由 **PeerId 較小的一方撥號**（`t.host.ID() >= pi.ID → return`）；另一方接受。
  它針對 mDNS 不重新查詢的變通做法是 **`connectPeer` 重試：最多 8 次、每次間隔 750 ms**，因為 libp2p mDNS
  一旦收到回應就不會再依固定間隔重新查詢。
- **結論** — 可以運作，也乾淨俐落地提供了 N-peer，但會拉進整套 pion/WebRTC 堆疊（**約 140 個傳遞相依 →
  37–39 MB**），卻**相對於 `lan-go` 在 Phase 0 毫無優勢**，還得同樣去馴服撥號／探索的問題。**已擱置**，
  除非瀏覽器／多語言的觸及範圍成為硬性需求（那才是它真正的、但在此用不到的優勢）。

---

## 3. 它們如何連線 — 交叉比較

### 3.1 完整的橫向比較

| 面向 | `mesh-rs` (`clip`) | `room-go` (`room`) | `lan-go` (`lan`) | `libp2p-mesh`（擱置中） |
|---|---|---|---|---|
| **拓撲 (topology)** | 真正的 P2P mesh，沒有伺服器 | 中央 SSH 房間 (room) 伺服器 | P2P mesh，沒有伺服器 | gossipsub P2P mesh |
| **傳輸層 (transport)** | iroh `=1.0.2`，直接的 QUIC 雙向串流 | wish/`ssh` 中繼 (relay)（原始的工作階段 (session) 管線） | quic-go v0.60 單向串流 | libp2p + gossipsub |
| **身分 (identity)** | Ed25519 `EndpointId`（`secret.key`） | SSH 公鑰指紋 (fingerprint)（`id_ed25519`） | 自簽 TLS 憑證的 SHA-256 | libp2p `PeerId`（`libp2p.key`） |
| **LAN 探索 (discovery)** | mDNS 位址查詢，以房間為範圍的服務 + ALPN | 無（直接撥接一台伺服器） | `grandcat/zeroconf` `_clip-lan._udp`，TXT `room=` | libp2p mDNS，房間標籤 (room-tag) 服務 |
| **跨網際網路路徑** | iroh 中繼／打洞 (hole punching)（只差一個設定旗標；尚未實作） | 今天就能透過 SSH 通道運作 | DIY 中繼（尚未實作） | libp2p 中繼／NAT（尚未實作） |
| **N 個對等節點 (peer)** | 對每個對等節點建立成對的直接 QUIC（gossip 延後） | 伺服器扇出（原生支援） | 每個對等節點一條 QUIC 連線 | gossipsub topic（原生支援） |
| **圖片／檔案傳輸** | 透過直接串流做 **announce + 以 BLAKE3 拉取** | 透過伺服器內嵌 `blob_data` | 內嵌 `blob_data` | 內嵌 `blob_data` |
| **剪貼簿 (clipboard) 函式庫** | `arboard`（透過 `image` 做 PNG↔RGBA） | `golang.design/x/clipboard` | shared-go `clip`（同一個 Go 函式庫） | 同 lan-go |
| **TUI + 內嵌圖片** | ratatui + ratatui-image — **真正的 Kitty/iTerm2/Sixel 縮圖 (thumbnail)**（half-block 退回機制 (fallback)） | bubbletea — 中繼資料 (metadata) **stub** | bubbletea（shared-go） — 中繼資料 **stub** | 同樣是 stub |
| **headless（無顯示環境）** | ✅ 乾淨地降級 | ✅（伺服器不需要顯示環境） | ✅ | ✅ |
| **`remote` 的做法** | **用 iroh 配對票券 (ticket) 做配對 (pairing)**（`scripts/remote.sh`） | **SSH 通道 + join**（原生 Go） | **共享 LAN 上的 mDNS**（`scripts/remote.sh`） | — |
| **執行檔大小** | 24 MB release（stripped 後 20）／debug 79 | **10 MB** | 13 MB | 37 MB |
| **閒置 RSS** | 18.0 MB | **8.2 MB** | 12.0 MB | 19.9 MB |
| **隱私** | 預設端對端加密 E2E（沒有伺服器） | 伺服器看得到明文（除非加上 E2E） | LAN 上是 E2E | E2E |
| **相依套件重量** | 557 個 crates，iroh 編譯 >7 分鐘 | 小巧的 charm 技術堆疊 | quic-go + zeroconf | ~140 個相依套件（pion/WebRTC） |

數字取自 [BAKEOFF.md](BAKEOFF.md) 的 Phase-0 量測（macOS，本機）。bake-off 的但書依然成立：
只跑 LAN 的 Phase 0 會偏袒零設定的 mesh，還無法反映主打實作之所以存在的整個理由
（mesh-rs 毫不費力的跨網際網路 P2P；room-go 的歷史紀錄 + 免安裝的 SSH 層級）。

### 3.2 三種 `remote` 的啟動＋連線流程，逐步拆解

三者都是 `<tool> remote <ssh-host>`（VSCode Remote-SSH 風格）；實際機制則因傳輸層而異。

| 步驟 | `room remote`（原生 Go） | `clip remote`（透過 `remote.sh`） | `lan remote`（透過 `remote.sh`） |
|---|---|---|---|
| 1. 偵測／安裝 | `ssh uname -sm` → GOOS/GOARCH；交叉編譯 `room`（CGO_ENABLED=0），或把正在執行的執行檔 scp 過去 → `~/.local/bin/room` | `ensure_remote_bin` → `install.sh --remote`（Go 交叉編譯 + scp；clip 則在遠端用 cargo 建置） | 同 clip |
| 2. 啟動遠端側 | 在 `127.0.0.1:<rport>` 上跑 `room server`（setsid+nohup；若已在監聽則重複使用） | 在 `--room` 中啟動遠端的 **clip 常駐程式 (daemon)**（setsid+nohup） | 在 `--room` 中啟動遠端的 **lan 常駐程式**（setsid+nohup） |
| 3. 橋接 | **SSH `-N -L <lport>:127.0.0.1:<rport>`** 通道（丟到背景執行，連接埠會自動遞增） | 透過 ssh **取得常駐程式的 `pair --new --json` 票券** | （什麼都不做 — 直接靠 LAN） |
| 4. 本機端連線 | 透過通道執行 `join room@127.0.0.1:<lport>` | 本機 `clip pair <ticket>` → **直接 QUIC** | 位於同一個 `--room` 的本機常駐程式**會透過 mDNS 自動連上** |
| 5. 狀態／收尾 | 每台主機一份狀態檔，放在 `<config-dir>/remote/` 底下；`--stop` 會殺掉通道行程群組 + 它啟動的伺服器 | 狀態放在 `~/.cache/cpc/remote/<tool>@<host>/`；`down` 會殺掉遠端常駐程式 | 同 clip |

具體來說，`clip remote` / `lan remote` 都很薄：`clip` 執行檔的 `cmd_remote` 會去定位 `scripts/remote.sh`
（依序透過 `CPC_REMOTE_HELPER`、執行檔所在資料夾、`~/.local/libexec/cpc/`，或往上層走找 `scripts/`），然後 `exec`
執行 `bash remote.sh clip <host> …`。`room remote` 完全沒有重新實作 SSH — 它直接呼叫本機的 `ssh`/`scp`，
所以你的 `~/.ssh/config`、金鑰、agent 和 ProxyJump 全都能直接運作。

---

## 4. 跨機器與遠端的底層配管

有四塊東西讓單一台開發機就能驅動（或模擬）真正的多機器 hand-off。

### 4.1 `scripts/remote.sh` — 共用的引擎

一支腳本，三個工具（`scripts/remote.sh <clip|room|lan> <host> [up|down|status] [--room R] [--rport N]
[--lport N]`）。狀態放在 `~/.cache/cpc/remote/<tool>@<host>/` 底下。

- **`room`** → `exec` 原生的 `room remote <host>`（SSH `-L` 通道 + `join`；見 §3.2）。
- **`clip`** → `start_remote_daemon`（setsid+nohup `clip … daemon --foreground`），`remote_ticket` 透過 ssh 取得
  `pair --new --json`，本機 `clip pair <ticket>` → **經由 iroh 的直接 QUIC**。
- **`lan`** → `start_remote_daemon`，接著位於同一個 `--room` 的本機常駐程式**會透過 mDNS 自動連上**（腳本會
  輪詢 (polling) `peers` 最多約 12 秒）。

若 `~/.local/bin/<tool>` 不存在，`ensure_remote_bin` 會透過 `install.sh --remote` 把執行檔 bootstrap 起來。

### 4.2 `scripts/install.sh [--remote HOST]`

建置並安裝到 `~/.local/bin`（或 `--remote` 主機上的對應位置）。**Go 工具**是在本機交叉編譯
（`CGO_ENABLED=0 GOOS/GOARCH`）後 `scp` 過去；**clip** 則是*在遠端*用 cargo 建置（遠端必須有工具鏈 —
iroh 會在那裡編譯）。它同時會把 `remote.sh` + `install.sh` 安裝到 `~/.local/libexec/cpc/`，這樣
`<tool> remote` 即使在儲存庫之外也能解析到引擎。

### 4.3 `docker/` — 在單一主機上模擬跨機器

`docker/build.sh` 會交叉編譯出靜態的 `room` + `lan` 放進 `docker/stage/`；`docker/compose.yml` 把每個容器
當成一台「裝置」來跑：**room-go** = `server` + `alice` + `bob`（中央 SSH 房間），**lan-go** = `lan-a` +
`lan-b`（docker 網路上的 mDNS）。`docker/demo.sh` 會驅動一次真實的文字 + 圖片（有做雜湊檢查）+ 檔案
（透過 `save_dir` sink）的跨容器 hand-off。**誠實的限制**：docker bridge 網路常常會過濾掉 mDNS 群播，
所以 `lan` 可能過不去（demo 會發出警告，而不是直接失敗）— **room-go 才是可靠的沙箱路徑**，因為它撥接的是
一台伺服器，而不是靠群播。

### 4.4 `scripts/xmachine.sh` — 真正的雙機器測試

部署到一台**真實的 LAN 主機**（預設是 `local_ubuntu`，可能是 **headless** 的），並驅動一次貨真價實的跨機器
hand-off（這台 Mac → 那台機器），用雜湊驗證 PNG。Go 實作是交叉編譯 + scp；Rust 則是把原始碼 rsync 過去，
在那台機器上 `cargo build`。room-go 會在那邊啟動一台伺服器並讓兩個客戶端加入；mesh-rs/lan-go 則依賴 mDNS
自動探索（若群播過不去，mesh-rs 還有票券的退回機制可用）。BAKEOFF 裡跨機器那一列量測的就是這件事。

### 4.5 誠實的限制

- **執行檔的 bootstrap** 需要儲存庫（本機要有 `go`/`cargo` 工具鏈才能交叉編譯），**或者**一台架構相符的主機，
  好讓你直接複製正在執行的執行檔 — 否則 `room remote` 會以一則可據以行動的訊息失敗收場。
- **QUIC mesh 工具（`clip`、`lan`）需要共享的 LAN** 才能走零設定路徑；**跨網際網路要走中繼路徑，而那部分被
  延後了**（mesh-rs 的 `internet=on` / iroh 中繼「只差一個設定旗標」，但還沒做；lan-go 的則要自己幹）。
- **`room` 今天就能透過通道運作** — SSH 連得到任何主機，所以 `room remote` 是目前唯一已經跨越網際網路的
  路徑，代價是你得跑一台伺服器並信任它。

---

## 5. 程式碼與契約不一致之處（值得調和）

- **到處都是 CBOR。** PROTOCOL §1 允許實驗階段使用 JSON，但**四個實作在傳輸線上（以及 IPC）都把 envelope
  編碼成 CBOR**。沒有任何實作使用 JSON — `--json` 旗標只影響 CLI 的*輸出*。
- **設定資料夾名稱＝模組名，不是執行檔名。** SPEC §5 說設定資料夾是 `<bin>`，但 mesh-rs 用的是
  `ProjectDirs("mesh-rs")`（執行檔叫 `clip`），room-go 用的是 `os.UserConfigDir()/room`。所以 `clip` 在磁碟上的
  資料夾是 `…/mesh-rs/`，不是 `…/clip/`。
- **撥接的破平規則方向相反。** mesh-rs 由 `EndpointId` **較大**的一方發起撥接；lan-go/libp2p 則由指紋/PeerId
  **較小**的一方發起。兩者都正確（決定性、單一發起方），但這個大小方向在各實作之間並不一致。
- **回音 (echo) 規則 2 處於休眠狀態。** last-written-hash 的抑制邏輯已經實作，（在 mesh-rs 中）也有單元測試，
  但它要等到 `broadcast_on_copy`（監看本機剪貼簿）存在之後才有意義 — 而那還沒做。在 shared-go 裡，`lastCopy`
  雜湊在三個地方被記錄下來，卻**從來沒有被讀取**過；在 mesh-rs 裡 `is_echo` 標著 `#[allow(dead_code)]`。
  所以去重設計有一半是「存在但不作動」，要等到 Phase 1/3 才會啟用。
- **Go TUI 裡的內嵌圖片。** 只有 mesh-rs 會算繪真正的內嵌縮圖；room-go/lan-go/libp2p-mesh 的 TUI 只算繪一個
  中繼資料佔位符（已載明的 stub）。
- **`room` 的 announce+pull。** room-go、lan-go 和 libp2p 都是內嵌 `blob_data`（Phase 0）；只有 mesh-rs 做了
  PROTOCOL §1 的 announce+pull。把 Go 實作改成以雜湊拉取，是 Phase-1 的項目。
