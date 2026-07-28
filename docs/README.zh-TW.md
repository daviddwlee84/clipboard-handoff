# docs/

!!! note "Terminology rule (zh-TW pages)"
    技術名詞首次出現以「中文 (English original)」格式呈現，例：依賴注入
    (dependency injection)。**不自創翻譯**——若無公認譯名直接保留英文
    （如 `embedding`、`tokenizer`）。代碼、API 名、CLI flag、套件名、檔名一律不翻。

跨平台剪貼簿 (clipboard) / 檔案 hand-off 工具的文件。三個可用的實作共用**同一份產品契約**
（相同的 CLI、相同的傳輸模型）；不同的只有執行檔名稱與傳輸方式。

## 我該用哪一個工具？

| 工具 | 執行檔 | 傳輸方式 | 適合的情境… |
|---|---|---|---|
| **[mesh-rs](usage-mesh-rs.md)** | `clip` | iroh P2P (QUIC) | 你想要**免伺服器、隨處可用、預設私密**——自己的裝置，不論是在區域網路 (LAN) 內或跨越網際網路。 |
| **[room-go](usage-room-go.md)** | `room` | SSH/wish 中央伺服器 | **架一台小伺服器沒問題**，而且你想要歷史記錄 ＋ 一層免安裝的 SSH 使用方式。 |
| **[lan-go](usage-lan-go.md)** | `lan` | quic + mDNS（實驗性） | 一個**最小化、僅限區域網路的試作**——在單一 LAN 上零設定，沒有走網際網路的路徑。 |

不確定要用哪個？個人多裝置使用請從 **`clip`** (mesh-rs) 開始；如果你已經在運行伺服器，就用
**`room`** (room-go)。`lan` 是 bake-off 的實驗品，不是要正式推出的目標。

## 內容

**共用契約**（讀完這些就能理解任何一種實作）：
- **[SPEC.md](SPEC.md)** — 產品契約：CLI 介面（§2）、sink 與自動複製 (auto_copy)（§3）、設定鍵
  （§5）、作業系統支援矩陣（§6）、工作階段 (session) 與清除（§8）。
- **[PROTOCOL.md](PROTOCOL.md)** — 傳輸 envelope、型別嗅探、身分 (identity)／房間 (room)／信任、本機 IPC。
- **[BAKEOFF.md](BAKEOFF.md)** — 評估準則、實測到的事實，以及最終入選者的決策。
- **[implementation.md](implementation.md)** — 實際是怎麼建構的：共用的常駐程式 (daemon)／IPC／envelope
  架構、各實作的內部細節（mesh-rs、room-go、lan-go，以及擱置中的 libp2p-mesh）、跨傳輸方式的
  比較表格，還有跨機器／`remote` 的底層串接。比使用指南更深入。
- **[alternatives.md](alternatives.md)** — 先行專案 (prior art) 與替代方案：啟發本專案的 `ccimg` /
  `sshimg.nvim` 專案（以及它們的運作方式）、我們有何不同，還有更廣的生態系
  （OSC 52、kitty OSC 5522、AirDrop、Syncthing、Taildrop、LocalSend、magic-wormhole……）。

**各工具的使用指南**（安裝、連線、完整指令參考、sink、工作階段、TUI 按鍵、
疑難排解）：
- **[usage-mesh-rs.md](usage-mesh-rs.md)** — `clip`（Rust/iroh P2P）：mDNS 自動探索 (auto-discovery) ＋
  配對票券 (ticket) 退回機制 (fallback)、行內圖片 TUI、headless（無顯示環境）注意事項。
- **[usage-room-go.md](usage-room-go.md)** — `room`（Go/wish 伺服器）：啟動伺服器、`join`、
  以 SSH 金鑰為基礎的身分模型。
- **[usage-lan-go.md](usage-lan-go.md)** — `lan`（Go/quic+mDNS）：零設定的區域網路探索 (discovery)
  （實驗性）。

使用指南會交叉連結到 SPEC/PROTOCOL，而不是重述完整的契約內容。repo 層級的概觀請見
[根目錄 README](https://github.com/daviddwlee84/clipboard-handoff/blob/main/README.md)。
