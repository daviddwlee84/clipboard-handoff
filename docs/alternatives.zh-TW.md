# 先行專案與替代方案 (Prior art & alternatives)

!!! note "Terminology rule (zh-TW pages)"
    技術名詞首次出現以「中文 (English original)」格式呈現，例：依賴注入
    (dependency injection)。**不自創翻譯**——若無公認譯名直接保留英文
    （如 `embedding`、`tokenizer`）。代碼、API 名、CLI flag、套件名、檔名一律不翻。

本專案並非憑空出現：它源自三個先行專案 (prior art)，這些專案解決的是同一個問題的一個很銳利的版本 ——
*「我 SSH 進了一台機器，我想把剪貼簿 (clipboard) 裡的圖片貼進這個工作階段 (session)。」* 本頁向它們致意、
說明**它們如何運作**，並把它們（以及更廣的生態系）與我們所建造的東西放在一起對照。

## 啟發本專案的三個先行專案

| | 這是什麼 |
|---|---|
| [透過 SSH 把剪貼簿圖片貼進 Claude Code](https://alexanderzeitler.com/articles/paste-clipboard-images-into-claude-code-over-ssh/) | Alexander Zeitler 對這個問題與反向通道 (reverse tunnel) 解法的完整說明 |
| [`AlexZeitler/claude-ssh-image-skill`](https://github.com/AlexZeitler/claude-ssh-image-skill) | `ccimgd`（本機常駐程式 (daemon)）+ `ccimg`（遠端客戶端）+ 一個 Claude Code skill |
| [`AlexZeitler/sshimg.nvim`](https://github.com/AlexZeitler/sshimg.nvim) | 同樣的構想接進 Neovim（本機常駐程式 `imgd`） |

### 它們如何運作（資料路徑）

這兩個工具之所以存在，是因為一項我們也同樣撞上的硬性限制：

> **遠端**主機上的程式無法讀取或寫入**本機**的剪貼簿。唯一*做得到*的終端機跳脫序列 (terminal escape)
> （OSC 52）**只支援文字** —— 它沒有圖片的形式。

所以它們改用頻外 (out-of-band) 的方式搬移位元組，走你手上已經有的那條 SSH 連線：

- **`claude-ssh-image-skill`（拉取式，pull）。** 常駐程式 `ccimgd` 跑在你的**筆電**上，監聽
  `127.0.0.1:9998`。它以 `pngpaste`（macOS）/ `wl-paste`（Wayland）/ `xclip`（X11）讀取本機剪貼簿裡的
  圖片，並以 JSON 承載 base64 PNG 對外提供。你的 SSH 工作階段帶著一條**反向通道**
  （`ssh -R 9998:localhost:9998 host`，或在 `~/.ssh/config` 裡寫一個 `RemoteForward`）。在遠端，`ccimg`
  連向 `127.0.0.1:9998` —— 通道會把它轉送*回筆電* —— 收下 PNG，然後寫出一個 Claude Code 可以 `Read`
  的暫存檔。
- **`sshimg.nvim`（推送式，push）。** 形狀相同，只是觸發方向相反：遠端的 Neovim 透過反向通道通知本機的
  `imgd` 常駐程式，`imgd` 再把剪貼簿裡的 PNG 用 `scp` 傳到遠端主機，Neovim 則插入一個指向它的 markdown
  連結。

**我們從它們身上學到什麼：** 那個核心的架構教訓 ——
*在擁有剪貼簿的那台機器上，一個原生、常駐的 agent 是無法迴避的；終端機跳脫序列沒辦法承載圖片。*
這個結論形塑了本專案「常駐程式加上多個瘦客戶端 (thin client)」的設計（見
[`implementation.md`](implementation.md) §1.1）。

**我們的不同之處：** 它們的方向是**本機 → 遠端**（把我筆電的剪貼簿內容送進遠端工作階段）。我們的則是
**任一裝置 → 任一裝置**，雙向皆可，而且接收端可以寫入它自己的作業系統剪貼簿。我們也傳送文字與任意檔案，
不只是圖片，而且完全不需要存在一個 SSH 工作階段。

### 並列比較

| | `ccimg` / `sshimg.nvim` | 本專案 (`clip` / `room` / `lan`) |
|---|---|---|
| **解決的問題** | 把本機剪貼簿的圖片貼進單一遠端工作階段 | 你各個裝置之間的通用 hand-off |
| **方向** | 本機 → 遠端 | 雙向，任一對等節點 (peer) → 任一對等節點 |
| **拓撲** | 1:1，綁定單一 SSH 工作階段 | 同時 N 台裝置（mesh 或房間 (room)） |
| **傳輸** | 既有的 SSH 連線 + 反向通道（`-R`） | iroh QUIC（`clip`）、SSH room 伺服器（`room`）、quic+mDNS（`lan`） |
| **內容** | 圖片 | 文字 · 圖片 · **任意檔案** |
| **落地形式** | 遠端上的一個暫存檔（路徑交給編輯器／agent） | **作業系統剪貼簿**，以及／或一個資料夾，以及／或一個以附加方式寫入的檔案 |
| **設定** | 一個本機常駐程式 + 每個工作階段一條 `-R` 通道 | 配對 (pairing) 一次（`clip remote <host>`、配對票券 (ticket) 或 mDNS）；常駐程式是常駐的 |
| **需要 SSH 嗎？** | 需要，設計上如此 | 不需要（`clip`／`lan`）；`room` 以 SSH 作為它的傳輸層 |
| **額外功能** | Claude Code skill／nvim 整合 | 通訊軟體式 TUI、工作階段清除、notify-first、自動剪貼簿同步 |

**用它們的，如果**你想要的是針對恰好一種流程、盡可能最小的東西 —— 把筆電剪貼簿的圖片送進遠端的
Claude Code／Neovim 工作階段，走一條你本來就有的 SSH 連線。機制比較少，也沒有東西需要配對。

**用這個，如果**你想要同時連著好幾台裝置、雙向皆可、除了圖片還要能傳文字／檔案，而且內容要直接落在接收端的
剪貼簿上（或落在一個資料夾裡），而不是變成一個路徑。

## 更廣的生態系

其他人們會想到的做法，以及它們為什麼不符合本專案的目標（在你自己的機器之間，以終端機為先的方式 hand-off
文字**與**圖片）：

| 做法 | 它做什麼 | 為什麼不用（就這個目標而言） |
|---|---|---|
| **OSC 52**（終端機跳脫序列） | 讓終端機從遠端程式寫入本機剪貼簿 | **只支援文字**，約 74 KB 上限，會被 tmux 與許多終端機剝除或限制。我們只把它當成文字的附加好處。 |
| **kitty 的 OSC 5522** | kitty 的擴充剪貼簿協定 —— *可以*承載 `image/png` | 需要所有人都用 **kitty**；受權限控管；沒有其他終端機實作它。 |
| **AirDrop / Handoff** | 我們正在模仿的使用體驗 | 只限 Apple；沒有 Linux／Windows；無法從終端機以指令稿驅動。 |
| **KDE Connect / Warpinator / LocalSend / PairDrop** | 區域網路 (LAN) 上的裝置對裝置檔案與剪貼簿分享 | 以圖形介面為先；不是可以接管線 (pipe) 的 CLI/TUI；常常綁定特定桌面環境。 |
| **Syncthing** | 裝置之間持續進行的資料夾同步 | 形狀是資料夾，不是剪貼簿；沒有「現在就貼這個」的那個時刻。不過它的探索 (discovery)／中繼 (relay) 設計*確實*啟發了我們的設計。 |
| **Tailscale / Taildrop** | mesh VPN + 檔案傳送 | 極佳的底層基礎，但需要一個控制平面／帳號；以檔案為導向，而非以剪貼簿為導向。 |
| **magic-wormhole / croc** | 透過一段代碼片語進行的一次性加密檔案傳輸 | 每次傳輸都要握手；沒有常駐連線，也沒有剪貼簿整合。 |
| **`ssh host pbcopy` / 透過 SSH 用 `xclip`** | 自己動手的一行指令 | 單向、依作業系統而異、實務上只能傳文字，而且每次都需要一個 SSH 工作階段。 |
| **雲端剪貼簿管理工具** | 透過廠商的伺服器同步 | 第三方看得到你的剪貼簿；通常沒有 CLI；不是 local-first。 |

## 我們自己的做法落在哪裡

我們針對同一份契約做了三種實作，正是為了比較這些取捨 —— 一個真正的 P2P mesh、一個中央的 SSH room，
以及一個極簡的 LAN mesh。量測後的比較見 [`BAKEOFF.md`](BAKEOFF.md)，各實作如何建造則見
[`implementation.md`](implementation.md)。
