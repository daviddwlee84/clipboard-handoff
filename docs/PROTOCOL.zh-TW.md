# PROTOCOL — envelope、分幀與本機 IPC (wire envelope, framing, and local IPC)

!!! note "Terminology rule (zh-TW pages)"
    技術名詞首次出現以「中文 (English original)」格式呈現，例：依賴注入
    (dependency injection)。**不自創翻譯**——若無公認譯名直接保留英文
    （如 `embedding`、`tokenizer`）。代碼、API 名、CLI flag、套件名、檔名一律不翻。

本文件定義所有實作共用、**與傳輸層無關 (transport-agnostic)** 的訊息模型，以及瘦客戶端 (thin client) 與
常駐程式 (daemon) 之間的本機 IPC 契約。各個傳輸層（iroh gossip/blobs、wish/SSH、quic+mDNS、
libp2p gossipsub）各以自己的方式承載這些 envelope（訊息封裝格式），但 envelope 的形狀與圖片規則是共通的，
如此 bake-off（本專案的多實作對比）才能在相同基準上比較。

## 1. envelope

邏輯欄位（二進位實作在傳輸格式 (wire format) 上以 CBOR 編碼；實驗性實作只要欄位相符，使用 JSON 亦可）：

```
Envelope {
  v:          u16          // protocol version, currently 1
  msg_id:     string       // ULID (lexicographically sortable, time-based) — the dedupe key
  type:       "text" | "image" | "file"
  mime:       string       // text/plain; charset=utf-8 | image/png | best-effort for files (application/octet-stream)
  sender:     string       // stable peer identity (see §3): Ed25519 node id / TLS cert fp / ssh key fp
  device_name:string       // human label, advisory only
  ts:         u64          // unix milliseconds at the sender
  filename?:  string       // image/file: original file name, advisory (used by folder/save sinks)
  // exactly one of the following, per `type`:
  text?:      string       // type=text: the UTF-8 payload, inline
  blob?: {                 // type=image|file: content-addressed reference
    hash:     string       // BLAKE3 hex of the bytes — also the integrity check
    size:     u64          // byte length
    w?:       u32          // image only
    h?:       u32          // image only
  }
}
```

規則：
- **`text`** 內容一律**內嵌 (inline)** 在 `text` 欄位中。（內嵌的軟性上限是 1 MiB；更大的文字可由實作自行決定
  改以 blob 傳送，並帶上 `mime: text/plain` — Phase 0 不要求。）
- **`image`** 在傳輸上**一律是 PNG**，而且是唯一能*以圖片形式*貼進作業系統剪貼簿 (clipboard) 的類型。
  傳送端編碼成 PNG；接收端把 PNG 解碼成作業系統原生的剪貼簿圖片格式。`blob.hash` 是這份 PNG 位元組的
  BLAKE3，接收時必須 (MUST) 驗證。
- **`file`** 是任意位元組，附上盡力而為 (best-effort) 的 `mime` 與 `filename`。它和圖片一樣採內容定址
  (content-addressed)，但**不能**貼到剪貼簿；它會被導向資料夾 sink（見 SPEC §3）或 `recv --emit-path`。
  `blob.hash` 於接收時驗證。
- 圖片/檔案的位元組**不會**在 gossip/廣播頻道上大量散播。傳送端廣播的是那份小小的 envelope（公告）；
  每個接收端再以 `blob.hash` 為鍵，透過直連串流**拉取 (pull)** 位元組（mesh：iroh-blobs 或一條直連的
  QUIC 串流；房間 (room)：向伺服器抓取）。實驗性實作可以 (MAY) 為求簡便，把小張圖片以 inline 分塊傳送。
- **Phase 0 的 inline 擴充：** 實作可以 (MAY) 把 PNG 位元組直接內嵌在選用的 `blob_data` 欄位（與
  `blob.hash`/`size`/`w`/`h` 並列），取代「先公告再拉取」的做法，用於小張測試圖片。PNG 仍是標準格式，
  `blob.hash` 接收時仍要驗證。`room-go` 的 Phase 0 採用這種做法；announce+pull 是 Phase 1 的目標。
  只認得 announce+pull 的接收端必須能優雅地忽略未知欄位。

## 2. 型別嗅探 (type sniffing)（`--auto`）

- 若有給路徑參數或 `--name`，`filename` 就由它決定。
- 若開頭位元組符合 **PNG** 簽章（`89 50 4E 47 0D 0A 1A 0A`）或 **JPEG**（`FF D8 FF`）→ 視為圖片
  （JPEG 會轉碼成 PNG，也就是標準的傳輸格式）。`--image` 可強制當成圖片。
- 否則，若設了 `--file`（或以 pipe 傳入二進位／非 UTF-8 內容並附上 `--name`）→ 視為**檔案 (file)**
  （除非能從副檔名得知更合適的 mime，否則用 `application/octet-stream`）。
- 否則，若整份內容都是合法的 UTF-8 → 視為**文字 (text)**。`--text` 可強制當成文字。
- 否則（二進位，且沒有 `--file`/`--name`）→ 視為**檔案**，mime 為 `application/octet-stream`。

## 3. 身分 (identity)、房間、信任

- **身分**是每台裝置固定不變的金鑰指紋 (fingerprint)：
  - `mesh-rs`：iroh 的 **Ed25519 NodeId**。
  - `lan-go`：自簽 **TLS 憑證 (TLS cert)** 的 SHA-256 指紋（會持久保存；Syncthing 風格）。
  - `libp2p-mesh`：libp2p 的 **PeerId**。
  - `room-go`：客戶端的 **SSH 公開金鑰 (SSH public-key)** 指紋（伺服器依金鑰授權）。
- **房間 (room)** = 一組共用的祕密字串。mesh 實作以 `hash(room_secret)` 推導出 gossip/topic id；room 實作
  則使用伺服器上的具名頻道。只有持有該祕密者（mesh）／頻道允許清單 (allowlist) 中的金鑰（room）才能參與。
- **允許清單（TOFU）：** 每個常駐程式都會持久保存一組受信任的 `sender` id。來自未知傳送端的第一個 envelope
  會被**暫扣待核准**；在使用者核准（CLI 提示／TUI）之前，來自它的任何內容都不會自動複製 (auto_copy)。
  每個 envelope 都由傳輸層的對等節點 (peer) 身分驗證過（簽章 gossip／雙向 TLS／SSH），因此 `sender`
  足以信賴，可用於允許清單檢查。

## 4. 本機 IPC（客戶端 ⇄ 常駐程式）

傳輸方式：Unix domain socket（macOS/Linux），位於 `$XDG_RUNTIME_DIR/<bin>/daemon.sock`
（退回機制 (fallback) 為暫存資料夾）／具名管線 (named pipe) `\\.\pipe\<bin>-daemon`（Windows）。
分幀 (framing)：**長度前綴**（4 位元組大端序長度）的 CBOR 請求/回應，外加一條事件串流。
每條連線只處理一個請求，`recv --follow` / `tui` 除外——它們會保持連線開啟並接收事件串流。

請求（客戶端 → 常駐程式）：
```
Req = Send{ type, mime, bytes }            // bytes = raw stdin; daemon sniffs + broadcasts
    | RecvLatest{ kind: any|text|image }   // returns the newest buffered item (or blocks for next if --follow)
    | Paste                                 // daemon writes latest buffered item to OS clipboard
    | Subscribe                             // opens an event stream (recv --follow, tui)
    | Peers | Status
    | ConfigSet{ key, value } | ConfigGet{ key }
    | PairNew | Pair{ ticket } | Join{ target }
```
回應／事件（常駐程式 → 客戶端）：
```
Resp  = Ok{ ... } | Err{ code, message }
Event = Item{ envelope, local_path? }      // a received item (local_path set once blob is fetched)
      | PeerUp{ id, name } | PeerDown{ id }
      | PairPrompt{ id, name }             // unknown peer awaiting approval
      | Toast{ text }
```

若沒有常駐程式在監聽，客戶端會**啟動 (spawn)** `BIN daemon`（detached），等待 socket 出現（短暫的退避重試），
然後重試。`--foreground` 會讓常駐程式跑在客戶端的終端機裡，方便除錯。

## 5. 版本管理 (versioning)

只要 envelope 出現破壞相容性的變更，`v` 就會遞增。常駐程式收到未知主版本 `v` 的 envelope 時，會以 `Err` 拒絕。
IPC 也一併做版本管理；IPC 版本不相符的客戶端與常駐程式必須明確報錯，而不是默默失敗。
