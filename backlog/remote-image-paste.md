# 遠端 agent 圖片貼上：Herdr、Moshi 與低侵入 SSH/Mosh 整合

**Date**: 2026-09-11

**Status**: P? — research only；尚未實作或部署本筆記中的新介面

**Effort**: M（先驗證最小流程；完整跨 terminal 自動插入可能更大）

**Related**: [TODO](../TODO.md) · [既有替代方案](../docs/alternatives.md) · [mesh-rs CLI](../mesh-rs/src/main.rs)

## 結論與需求

主要需求是「本機截圖 → 遠端 coding agent」，希望保留普通 SSH/Mosh 的使用方式、支援 Herdr，
而且由使用者明確選擇何時開啟、暫停和關閉圖片功能。2026-09-11 的建議是：

1. 桌面上的 Herdr 工作先驗收原生 `herdr --remote` 圖片貼上。
2. 普通 SSH/Mosh 先做「單次上傳 → 回傳遠端路徑」；圖片上傳按需發生，沒有持續剪貼簿監看。
3. 需要減少重複選主機時，再用獨立 session wrapper 記住目標、提供開關、管理退出回收。
4. 自動把路徑插進指定輸入框，交給可選的 terminal／Herdr adapter；這與傳檔是兩項能力。

clipboard-handoff 的多裝置文字／圖片／檔案傳輸仍有價值。這個需求先補交付與輸入整合，
不需要先重做 mesh，也不需要先完成所有 OS、terminal 和 agent 的整合。

本次只有文件與程式碼檢查；沒有讀取實際剪貼簿圖片、建立遠端連線、修改 SSH/Herdr 設定、
註冊快捷鍵或執行跨機傳圖測試。

## 為什麼普通 Cmd/Ctrl+V 通常不成立

剪貼簿 (clipboard) 圖片在本機；agent 的程序與檔案系統在遠端。普通 terminal 的文字貼上
通常只把文字送進 pseudo-terminal (PTY)，不會自動把 OS 剪貼簿的 PNG 上傳到遠端。
Cmd+V 常由 terminal 處理；Ctrl+V 的實際行為依 terminal 綁定與內部應用程式而定。
若遠端程式自行讀 OS 剪貼簿，讀到的是遠端主機的環境。

SSH 能傳二進位資料。缺的是這段整合：

```text
使用者明確觸發
  → 本機讀取圖片
  → 將圖片傳到 agent 能讀取的檔案系統
  → 確認落盤並取得確切路徑
  → 插入附件／路徑
  → 使用者補充文字並送出
```

remote session manager 不是必要條件。terminal、外掛、單次執行的本機 helper、
或 Herdr 的本機 client 都可以提供橋接。要進入模型的視覺上下文，仍須由 agent 的
圖片附件／讀圖工具載入；單純貼路徑不代表所有 agent 都一定會載入。

**校正舊筆記的概括說法：** 本 repo 舊文件把「常駐原生 agent 無法避免」和
「terminal escapes 不能傳圖片」寫得過於絕對。持續同步通常需要持續運行的元件；
單次上傳只需在觸發時讀取剪貼簿。OSC 52 不是通用圖片貼上機制，但 kitty 的 OSC 5522
支援圖片等任意 MIME 資料；`kitten clipboard -g picture.png` 可透過 SSH 取得圖片。
terminal 和中間的 mux 必須相容，Mosh 路徑也要另外驗證。
參考 [kitty clipboard protocol](https://sw.kovidgoyal.net/kitty/clipboard/) 與
[clipboard kitten](https://sw.kovidgoyal.net/kitty/kittens/clipboard/)。

## 已確認的外部方案與本機狀態

| 方案 | 已確認的能力 | 對這個需求的意義 |
|---|---|---|
| Herdr 本機 `--remote` client | 能橋接本機圖片剪貼簿至遠端 | 桌面 Herdr 的優先路徑 |
| `ssh host` 後在遠端啟動 Herdr | Herdr 在遠端，沒有本機剪貼簿存取能力 | 還需要本機橋接 |
| Moshi | SCP 上傳至 `~/.moshi/uploads/`，再把遠端路徑放入 composer 或 terminal | 手機附件上傳的成熟操作模式 |
| kitty clipboard kitten | 支援經 SSH 讀寫任意剪貼簿資料，讀取通常會要求授權 | 已使用 kitty 的人可評估；不作跨 terminal 預設 |

Herdr 的連線差異見 [How to work with Herdr](https://herdr.dev/docs/how-to-work/)。
Moshi 的 remote clipboard 選項放的是遠端**路徑**；上傳的檔案會留在 host 上，可從 Files 管理。
見 [Moshi image and file paste](https://getmoshi.app/docs/image-paste)。
Moshi + Herdr 的附件路徑是否在目前 agent/模式中正確載入，仍需實測。

本機檢查結果：

- `herdr --version`：`herdr 0.9.0`。
- `herdr --default-config`：`[keys] remote_image_paste = "ctrl+v"`，註明只在 `herdr --remote` 啟用。
- 當時有效的 `~/.config/herdr/config.toml` 沒有覆寫此鍵，因此使用預設。
- CLI help 有 `herdr pane send-text <pane_id> <text>`、`herdr pane send-keys` 和
  `herdr agent prompt`。貼附件不應直接呼叫會附帶 Enter 的 `agent prompt`。
  `send-text` 的實際 paste 編碼與 agent 輸入框相容性，需在 adapter spike 中驗證。

可先人工驗收的既有功能（`workbox` 換成 SSH alias，在本機 terminal 執行）：

```sh
herdr --remote workbox
```

將截圖複製到本機剪貼簿，聚焦遠端 agent 輸入框，按 Control+V。只有 help/config 和官方
能力描述已確認，這次沒有做完整傳圖驗收。

## clipboard-handoff 現有能力與缺口

以下是目前 Rust `clip` 的程式碼事實，不能假設每個 Go 實作完全一致：

| 項目 | 現況／設計含義 | 程式碼 |
|---|---|---|
| 讀取剪貼簿圖片 | 已有 `clip send --clipboard-image`，由 daemon 讀取並編碼 PNG | [main.rs](../mesh-rs/src/main.rs)、[clipboard.rs](../mesh-rs/src/clipboard.rs) |
| Headless 接收 | `clip recv --latest-image --emit-path` 能取得接收端本地檔案路徑 | [client.rs](../mesh-rs/src/client.rs) |
| 送達確認 | `reached` 計數的是 announcement 寫出；沒有接收方落盤 ACK；沒有 peer 時會警告但 exit 0 | [client.rs](../mesh-rs/src/client.rs)、[daemon.rs](../mesh-rs/src/daemon.rs) |
| 精確目標 | `send` 廣播給已連線 peers，尚無指定 host/session/pane 的 attach 介面 | [main.rs](../mesh-rs/src/main.rs)、[proto.rs](../mesh-rs/src/proto.rs) |
| 自動廣播 | `broadcast_on_copy` 已有 watcher，預設 `off`；不適合作為每次明確貼圖的預設 | [config.rs](../mesh-rs/src/config.rs)、[daemon.rs](../mesh-rs/src/daemon.rs) |
| 網際網路 | endpoint 明確設成 `RelayMode::Disabled`，目前仍是 LAN-first；`internet` 設定名不代表 relay 已接好 | [daemon.rs](../mesh-rs/src/daemon.rs) |
| 保留期限 | 接收圖片在 transient blob cache；clear／退出策略可能清掉，需要另外保留已附加圖片 | [config.rs](../mesh-rs/src/config.rs)、[daemon.rs](../mesh-rs/src/daemon.rs) |

不能用「send 成功 → 立刻 recv latest」充當可靠的附件交付。舊圖、新圖、其他裝置送來的圖可能競爭；
要用此次 `msg_id` 關聯遠端落盤結果，再將對應路徑交給指定 agent。

## 低侵入方案比較

| 方案 | 操作與開關 | 背景元件 | 取捨 |
|---|---|---|---|
| A. 單次本機上傳 | 明確呼叫 helper；完成就結束；使用者貼上回傳路徑 | 不需要常駐服務 | 最容易跨 terminal；通常兩步，可能需另一個本機 shell／啟動器 |
| B. 按連線啟用 wrapper | 明確用 `sshclip`／`moshclip` 進入；可暫停；退出收回 | 可只有目標登記與 supervisor，圖片按需傳輸 | 適合日常；自動插入仍需 adapter |
| C. Terminal profile + 專用快捷鍵 | 只在選定 profile/session 啟用，使用者可換 profile 或停用 | terminal 外掛／腳本 | 能融入輸入框，但依賴 terminal 的 API |
| D. 臨時 reverse tunnel + 遠端 pull | wrapper 啟動本機圖片服務及反向通道；遠端命令／skill 按需拉取 | 僅該連線期間的 helper 與 SSH 通道 | 適合從遠端 `/paste-image` 觸發；需要存取授權和生命週期管理 |
| E. PTY wrapper 攔截專用鍵 | wrapper 包住 ssh/mosh 並辨識特定 key sequence | 全程經過自己的 PTY wrapper | 較通用，但 raw mode、resize、signals、paste 協定的維護成本高 |

A、B、D 可以共用圖片擷取與上傳原語。B 的重點是管理目標與開關，不必一開始就寫 E。
若沒有可靠的 terminal adapter，B 可退回 A 的「複製路徑後手動貼上」。

iTerm2 官方有 profile key action、script function 和 coprocess；coprocess 的 stdout
能作為 session 的鍵盤輸入，是 C 的實際整合點，但不是所有 terminal 的共通 API。
見 [iTerm2 key mappings](https://iterm2.com/3.4/documentation-preferences-profiles-keys.html)
與 [coprocesses](https://iterm2.com/documentation-coprocesses.html)。實作前應依安裝版本重查。

## 建議分階段介面（提案，尚不存在）

### 第一步：單次上傳

候選命令名稱，**不是現有 CLI，用於討論設計**：

```text
clip attach --host workbox --clipboard-image
clip attach --host workbox --clipboard-image --copy-path
```

預設在 stdout 回傳確認已落盤的絕對路徑，進度寫 stderr。`--copy-path` 是明確選擇將
本機剪貼簿改成路徑文字，再由使用者執行一般文字貼上；這會取代原本的圖片剪貼簿內容，應告知。
另保留輸入檔案的路徑，讓不能讀剪貼簿的環境也可使用。

傳輸優先用現有 SSH/SFTP/SCP 認證與 host alias，Mosh session 也可另開短暫 SSH 上傳。
圖片不需要經過互動式 terminal 的資料流。Mosh 可連不代表當下 SSH 上傳必定可用；
遇到 SSH 無法連線時回報上傳失敗，保留原 terminal session 和使用者輸入。

### 第二步：獨立 wrapper，依連線控制

**以下同樣是提案，並未安裝 alias 或實作 wrapper：**

```text
ssh workbox                 → 原有流程
sshclip workbox             → 登記本次圖片目標，啟動 ssh
moshclip workbox            → 登記本次圖片目標，啟動 mosh
```

`sshclip` 可是 alias 指向可執行 wrapper，也可用 shell function；資源管理由 wrapper 完成。
原本的 `ssh`／`mosh` 命令保持原有行為。SSH 的 `Host workbox-clip` 別名可以提供專用連線選項，
但它本身不會提供完整的本機 helper 啟停、熱鍵與回收機制。

wrapper 的候選行為：

1. 建立 session ID，記錄目標 SSH alias、自己的程序識別、terminal session／pane 識別。
2. 圖片功能預設在此明確啟用的連線中處於 ready；ready 只表示可觸發，不讀取也不廣播剪貼簿。
3. 專用操作只在使用者觸發時讀一張圖片。可提供 session `status`、`pause`、`resume`、`off`。
4. 按鍵時固定目標與 session generation；傳輸中切換 pane、重新連線、關閉功能，都不能把
   完成的結果插入其他 session。目標已失效時只報告結果，供人工處理。
5. 一般退出、明確 off、連線程序終了時，撤銷此 session 的讀取能力並收回自有背景資源。

**alias 的界線：** 本機 shell function 或 ZLE widget 不會在 ssh/mosh 已佔用前景、
terminal 進入 raw mode 時自然收到按鍵。若要在原輸入框一鍵完成，仍需 C 的 terminal adapter、
Herdr 本機端，或 E 的 PTY wrapper。只靠設 environment variable／alias 不足以攔截 Cmd+V。

第一版採用專用「Attach image」動作，由使用者選擇快捷鍵；保留普通貼上的習慣。
也不假設新開的 terminal 腳本能繼承另一個 session 動態設定的環境；跨程序查找須經過
session ID 和受限的本機登記機制。多個 session 不得共用一個全域「最新主機」。

### 第三步：可選的自動插入 adapter

Herdr adapter 用確切 host + session/socket + pane 作為目標，再驗證該 pane 的 agent。
一般 terminal adapter 只能操作自己能明確識別的 session；不可根據視窗標題或當下焦點猜測。
agent 停在 approval/question UI 時，不注入附件文字。輸入框中的既有文字須保留，預設不送 Enter。

本機 Herdr pane 裡的 `ssh` 和遠端 Herdr 裡的 agent pane 是不同目標層級；巢狀 SSH、container、
sudo 切換使用者等情況需要明確映射，無法識別就要求指定落點／手動貼上。
檔案最終必須位於 agent 的可讀範圍；host 上的 `/tmp/foo.png` 不一定在 container 中存在。

## 「關閉連線後收回」的精確語意

### 普通 SSH

wrapper 可以管理前景 SSH 子程序並在它退出後 cleanup，或單獨管理專用的 `ssh -N` tunnel。
使用 `trap`／等價的 supervisor 處理正常退出及可捕捉的終止訊號；不要以 shell 全域 trap
污染使用者原有 shell。

連線共享 (ControlMaster) 是需要明確處理的地方。`ControlPersist` 可能使 master 在互動 client
結束後繼續運行，因此一個 shell 退出不代表其 forwarding 自動消失。推薦 wrapper 使用自己的
ControlPath／私有通道生命週期，或精確追蹤並取消它建立的 forwarding。
OpenSSH 有 `ssh -O forward`、`ssh -O cancel` 與 `ssh -O exit`；不要對使用者共用的 master
直接執行 exit，否則會影響其他連線。見 [ssh(1)](https://man.openbsd.org/ssh) 和
[ssh_config(5)](https://man.openbsd.org/ssh_config)。

若採方案 D，`ExitOnForwardFailure=yes` 可使初始 forwarding 建立失敗時結束，
但不等於圖片服務已健康，也不涵蓋每次轉送最終目的地的連線失敗；需要獨立 readiness check。
上述參數僅是設計依據，這次沒有建立 tunnel。

### Mosh

Mosh 用 SSH 啟動遠端 server，之後關閉 bootstrap SSH，互動走 UDP。
因此只在 `mosh --ssh=...` 裡塞 `-R`，不能保證有跟著 Mosh 存活的圖片通道。
見 [Mosh 官方說明](https://mosh.org/) 的 How Mosh works。

推薦先採 A/B：每次貼圖獨立 SSH 上傳，不維持圖片 tunnel。若確實需要遠端 pull，
wrapper 另外管理一條 sidecar SSH tunnel，與 Mosh client 共存，並提供斷線狀態與明確重連策略。

Mosh 的暫時斷網／休眠是正常使用情況。這裡的「關閉」預設定義為本機 Mosh client 結束
或使用者 off；暫時無網路時暫停傳圖，不當成退出。若要求「失聯 N 秒即撤銷」，必須另外
定義 heartbeat／lease；代價是漫遊恢復後可能需要重新啟用。

### 異常退出、授權與檔案保留

`trap` 無法保證處理 SIGKILL、當機或斷電。session registry 要能判定 owner 是否仍有效，
輔以租約 (lease)/過期機制回收孤兒狀態。網路中斷也不一定被 SSH 立即察覺，不能承諾瞬時收回。

若採遠端 pull，能接到 forwarding 的程序不應直接獲得任意讀取剪貼簿的權力：
本機服務限 loopback／受限 socket，每個 session 使用獨立、可撤銷的 capability，
憑證以受限檔案／安全通道交付，不放進提示詞或日誌。這是對「隨時可停用」的實作要求；
沒有必要為單次本機 push 預先增加這套服務。

off／退出要停止新讀取與新上傳，失效的非同步結果不得再自動注入。已送到遠端的 bytes 無法
藉撤銷功能收回；已附加圖片也不應隨 helper 退出就刪除，agent 可能尚未讀完。
圖片使用獨立保留策略（明確清除／可設定期限／使用中保留），與 bridge 的生命週期分開。

## 最小實作與驗收順序

以下是後續實作的驗收項目，不是這次已通過的測試：

1. **Herdr 原生路徑**：一張已知圖片完整傳送，agent 確實讀到；保留既有提示文字、無多餘 Enter。
2. **單次上傳**：PNG 擷取、確認落盤、唯一檔名、原子完成後才公布路徑；以 hash 驗證接收的 PNG。
   SSH 錯誤、空剪貼簿、非圖片輸入、不可寫目錄均回傳失敗；不可插入前次路徑。
3. **使用者控制**：只有明確動作才讀圖片；pause/off 後不再讀取、上傳或自動注入；普通文字貼上正常。
4. **多連線競爭**：不同 host、同 host 多 pane、傳輸中切換焦點、重連、關閉目標時都不串錯。
5. **生命週期**：一般退出、HUP/TERM、SIGKILL 後孤兒回收；只取消自己的 tunnel，其他 SSH master 不受影響。
6. **Mosh**：bootstrap SSH 結束後仍能按需上傳；漫遊中失敗可見，恢復後重試；本機 client 退出後功能撤銷。
7. **agent 可讀性**：container/sandbox 的路徑映射正確，已附加圖片在 agent 讀取前不被 cache 清理。

仍待決定：首個 terminal adapter、使用者偏好的專用快捷鍵、是否真的需要遠端 pull、附件保留期限，
以及要先做獨立 helper 還是納入 `clip attach`。這些都不阻礙先驗收 Herdr 和單次上傳原語。

## 決策紀錄與來源

2026-09-11：先保存研究；建議優先 Herdr 原生路徑及單次本機 push，再加按連線啟用的 wrapper。
未選定新 transport、未承諾實作排程、未部署全域快捷鍵或常駐剪貼簿服務。

- [Herdr remote workflow](https://herdr.dev/docs/how-to-work/)：本機 client 與純遠端 session 的差別。
- [Moshi image/file paste](https://getmoshi.app/docs/image-paste)：上傳後交付檔案路徑。
- [OpenSSH ssh(1)](https://man.openbsd.org/ssh)、[ssh_config(5)](https://man.openbsd.org/ssh_config)：forwarding、connection sharing 與 teardown。
- [Mosh](https://mosh.org/)：SSH bootstrap、UDP 互動、暫時失聯與漫遊。
- [kitty clipboard protocol](https://sw.kovidgoyal.net/kitty/clipboard/)、[clipboard kitten](https://sw.kovidgoyal.net/kitty/kittens/clipboard/)：圖片 MIME 傳輸的例外與 terminal 依賴。
- [iTerm2 coprocesses](https://iterm2.com/documentation-coprocesses.html)：可選的 terminal session 輸入 adapter。
- [既有 prior art 比較](../docs/alternatives.md)：`ccimgd/ccimg`、`sshimg.nvim` 的 reverse-tunnel 思路；其 token／session 回收能力尚未在本次查核。
