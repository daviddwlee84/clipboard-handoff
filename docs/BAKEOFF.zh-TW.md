# BAKEOFF — 評估準則與評分表 (evaluation rubric & scorecard)

!!! note "Terminology rule (zh-TW pages)"
    技術名詞首次出現以「中文 (English original)」格式呈現，例：依賴注入
    (dependency injection)。**不自創翻譯**——若無公認譯名直接保留英文
    （如 `embedding`、`tokenizer`）。代碼、API 名、CLI flag、套件名、檔名一律不翻。

以同一份產品契約做出數種實作，好讓我們看出哪一種架構*用起來*最舒服，然後把昂貴的全 OS／1.0 強化工作
**只投資在優勝者身上**。這是一份 **Phase 0 快照**：每個實作都停在相同的 MVP 深度（LAN、2 台裝置、
透過 `send`/`recv`/`paste` 進行文字與圖片 hand-off、notify-first）。請一併讀過但書 —— Phase 0 只走過
LAN 路徑，因此低估了主打實作在 Phase 3 的差異化能力。

## 實測數據 (Phase 0、macOS、本機)

| | `mesh-rs` (Rust/iroh) | `room-go` (Go/wish) | `lan-go` (Go/quic+mDNS) | `libp2p-mesh` (Go) |
|---|---|---|---|---|
| **往返 (round-trip) 測試腳本 (harness)**（`scripts/roundtrip.sh`） | PASS ✅ | PASS ✅ | PASS ✅ | PASS ✅ |
| 文字送達 | ✅ | ✅ | ✅ | ✅ |
| PNG BLAKE3 雜湊相同 | ✅ | ✅ | ✅ | ✅ |
| 單元測試 | 9/9 | wire+broker+daemon | shared-go 測試套件 | shared-go 測試套件 |
| **執行檔大小**（實際建置結果） | **24 MB** release（stripped 後 20 MB）· 79 MB debug | **10 MB** | 13 MB | 37 MB |
| **常駐程式 (daemon) 閒置 RSS** | 18.0 MB | **8.2 MB** | 12.0 MB | 19.9 MB |
| **建置成本** | iroh release 編譯 **> 7 分鐘** | 數秒 | 數秒 | 數秒（約 140 個相依套件） |
| 實際建置的傳輸層 | 配對票券 (ticket) → 直連 QUIC bidi（gossip/blobs 延後） | 經由伺服器的 SSH/wish 中繼 (relay) | quic-go + mDNS 自動探索 (auto-discovery) | gossipsub + mDNS |
| 身分 (identity) | Ed25519 NodeId | SSH 公鑰指紋 (fingerprint) | TLS 憑證指紋 | libp2p PeerId |
| LAN 配對 (pairing) 步驟 | 1（複製配對票券） | 加入伺服器（+ 金鑰） | **0（mDNS 自動）** | **0（mDNS 自動）** |

備註：`libp2p-mesh` 即使只跑 LAN-only TCP，也會拉進完整的 pion/WebRTC 堆疊（約 140 個傳遞相依套件）→ 於是有了 37 MB。
兩個 Go mesh 探索實作都各自被迫親手解掉兩件事 —— **去重撥接 (dedup dialing)**（對稱的 mDNS → 兩個對等節點 (peer)
同時撥接；採「id 較小者撥接」規則）以及**探索 (discovery) 重試**（mDNS 在第一次命中後就不再重新查詢）—— 而這兩件事
**iroh 免費幫你隱藏掉**。這是整場 bake-off 中最具決策參考價值的單一發現。

## 跨機器 —— mac ↔ headless（無顯示環境）Ubuntu 24.04、實體 LAN (`scripts/xmachine.sh`)

三個 1.0 候選目標都被部署到一台**沒有顯示器**的 Ubuntu 機器上（Go：`CGO_ENABLED=0` 交叉編譯 + scp；
Rust：rsync + 在該機器上跑 `cargo build`），並實際跑過一次真正的跨機器 hand-off（本 Mac → Ubuntu），
以雜湊驗證 PNG。全部通過；headless 常駐程式運作正常，並會對（不存在的）剪貼簿 (clipboard) 優雅降級。

| | 跨機器連線能力 | 文字 | 圖片（雜湊相同） | headless |
|---|---|---|---|---|
| `mesh-rs` | iroh mDNS 自動探索（配對票券為退回機制 (fallback)） | ✅ | ✅ | ✅ 剪貼簿優雅降級 |
| `room-go` | 客戶端撥接伺服器的 LAN IP | ✅ | ✅ | ✅（伺服器不需顯示環境） |
| `lan-go` | mDNS 自動探索 | ✅ | ✅ | ✅ |

但書：`mesh-rs` 的 mDNS 自動探索在多網路介面的主機上（Tailscale + LAN）對時序很敏感；配對票券這條退回機制
（透過 LAN 位址直連 QUIC）則很可靠。`room-go` 需要一個真正的修正 —— 它產生 SSH host key 的方式會去 chmod
`/tmp`（在伺服器上會 EPERM）；現已修正。


## 評分表 (scorecard)

分數 1（差）– 5（優），依據上面 Phase 0 的證據評定。標記 **[built]** 的列由 Phase 0 實際驗證過；
**[arch]** 的列則是對一項尚未實作的 Phase 3 功能評估其架構潛力。權重可調整。

| 面向 | W | `mesh-rs` | `room-go` | `lan-go` | `libp2p-mesh` |
|---|---:|:---:|:---:|:---:|:---:|
| 配對成本 **[built]** | 3 | 4 | 3 | 5 | 5 |
| 首則訊息所需時間 **[built]** | 2 | 4 | 4 | 4 | 3 |
| LAN 零設定 **[built]** | 3 | 3¹ | 2 | 5 | 5 |
| 離線 LAN **[built]** | 2 | 5 | 4 | 5 | 5 |
| 跨網際網路 **[arch]** | 2 | 5 | 5 | 2 | 4 |
| 圖片保真度 **[built]** | 3 | 5 | 5 | 5 | 5 |
| 延遲體感 **[built]** | 2 | 5 | 4 | 5 | 4 |
| 免安裝方案 **[arch]** | 1 | 2 | 4² | 2 | 2 |
| 資源使用（閒置 RSS） **[built]** | 1 | 3 | 5 | 4 | 3 |
| 執行檔大小 **[built]** | 1 | 3³ | 5 | 4 | 2 |
| 隱私（預設 E2E？） **[arch]** | 2 | 5 | 2⁴ | 5 | 5 |
| 歷史紀錄／保存 **[arch]** | 1 | 2 | 5⁵ | 2 | 2 |
| 建置／維運負擔 **[built]** | 2 | 5 | 2 | 5 | 5 |
| 多對等節點 **[built]** | 2 | 3⁶ | 5 | 4 | 5 |
| 開發／維護成本 **[built]** | 1 | 2 | 4 | 5 | 2 |
| **加權總分 / 140** | | **112 (80%)** | **105 (75%)** | **122 (87%)** | **118 (84%)** |

¹ iroh 支援本地 mDNS 探索；Phase 0 的實作用的是配對票券。啟用本地探索（Phase 1 的小幅增補）
可把這格拉到約 5。² room-go 那層純 `ssh room@server`（Phase 1）在客戶端完全不需要安裝任何東西即可傳文字。
³ mesh-rs 的 release 執行檔是 24 MB（stripped 後 20 MB），room-go 則是 10 MB。⁴ 除非加上客戶端側的 E2E，
否則伺服器看得到明文。⁵ 伺服器端的 SQLite 歷史紀錄很自然（Phase 1）—— 其他實作沒有共用的持久化儲存。
⁶ mesh-rs 的 Phase 0 是成對直連 QUIC；要做 N 個對等節點需要 iroh-gossip（Phase 1）。

## 如何解讀這些數字（重要）

Phase 0 的總分**獎勵零設定的 LAN**，而且**還無法把主打實作之所以存在的整個理由算進去**：
`mesh-rs` 那種毫不費力的*跨網際網路* P2P（沒有伺服器、沒有基礎設施、E2E），以及 `room-go` 的*保存歷史紀錄* +
*免安裝 SSH* 這一層，都是 Phase 3／Phase 1 的功能，只以 [arch] 潛力計分。所以原始排名
（`lan-go` > `libp2p-mesh` > `mesh-rs` > `room-go`）真正說的是：**「在 LAN 上，那個克難的零設定 mesh
體感最好、成本最低。」** 這是一個貨真價實、有用的結果 —— 但它不是產品的全貌。

## 質性評估

- **`mesh-rs`** —— 最貼合「我自己的裝置、在任何地方、不需伺服器、預設隱私」這個定位。Phase 0 的代價很明顯：
  79 MB 的 debug 執行檔、超過 7 分鐘的 iroh release 編譯、API 變動頻繁（已釘住 `iroh =1.0.2`）。但它是唯一一個
  日後能*免費*拿到跨網際網路 NAT 穿透 (NAT traversal) + 中繼 + 內容定址 (content-addressed) blob 的實作，
  而且那兩個 Go 探索實作親手解掉的困難 mesh 問題，在這裡根本不存在。
- **`room-go`** —— 最精簡的執行檔（10 MB）與閒置 RSS（8.2 MB），也是唯一一個能自然走向**歷史紀錄**與
  **免安裝**客戶端的實作。代價：你得自己跑並信任一台伺服器（除非加上 E2E，否則它看得到明文），而且 LAN
  使用並非零設定（必須有一台連得到的伺服器）。
- **`lan-go`** —— 意外驚喜。在 LAN 上零設定、體積極小、程式碼最簡單、Phase 0 分數最高。它的弱點正好就是
  iroh 幫你隱藏掉的那塊：它的**跨網際網路方案得自己來**（自架中繼／打洞 (hole punching)）—— 一旦離開 LAN，
  你就開始重造 `mesh-rs`。
- **`libp2p-mesh`** —— 能動，gossipsub 乾淨地提供 N 個對等節點的能力，但花了 **37 MB／約 140 個相依套件**
  卻在 Phase 0 沒有任何勝過 `lan-go` 的優勢，還得處理同樣的撥接／探索馴化工作。只有在瀏覽器／多語言觸及率
  成為硬性需求時才有說服力（那才是它真正的優勢，本次沒用上）。

## 決策

**入圍決選的是 `mesh-rs` 與 `room-go`** —— 這兩個彼此對立的*產品*賭注。實驗已經完成了它們的任務：
- `lan-go` 證明了 LAN 體驗可以極佳而且幾乎免費 —— 它是一條很強的**內嵌 LAN 快速路徑**，但單靠它撐不起
  一個涵蓋整個網際網路的產品。
- `libp2p-mesh` 證明了 libp2p 在這裡買到的東西不多，卻要付出龐大的體積／複雜度稅 —— 除非瀏覽器很重要，
  否則**先擱置**。

以最重要的那條軸線來挑優勝者（這決定了誰能拿到 Phase 3–4 的強化）：
- **不要伺服器、到哪都能用、預設隱私** → **`mesh-rs`**（把 `lan-go` 的 mDNS 零設定併進來，當作 iroh
  本地探索的快速路徑）。
- **一台小伺服器沒問題；想要免安裝 SSH + 保存歷史紀錄** → **`room-go`**。

_優勝者：待使用者選定。_
