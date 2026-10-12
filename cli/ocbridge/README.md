# ocbridge

Slack **Socket Mode** 與 GitHub review request → OffiCraft chat。不使用 `/in`,不需要公開網址。
folder = `module ocbridge` = binary `ocbridge`(root CLAUDE.md §10)。取代先前的 Python
officraft-slack-bridge(`bridge.py` + `github_watch.py`),沿用同一份 config 與憑證。

```
Slack ──WebSocket(outbound)──▶ ocbridge slack ──POST /api/chat──▶ OffiCraft(127.0.0.1:7755)
gh api search/issues ◀── ocbridge github-watch(每小時一次)──POST /api/chat──▶ OffiCraft
```

- `ocbridge slack` — 常駐:握住 Socket Mode 連線,把訊息轉成聊天(launchd `KeepAlive`)。
- `ocbridge github-watch` — 跑一輪就結束:回報新的 review request(launchd `StartInterval 3600`)。
- `ocbridge claim <code>` — 一次性:用控制台的 claim code 換機器憑證。
- `ocbridge --help`(或不帶參數)— 用法,exit 0。

## 檔案地圖

- `main.go` — 子命令分派、slack 模式的組裝。
- `config.go` — config.toml(與 Python 版同鍵)、state_dir、原子寫檔。
- `officraft.go` — 機器憑證(claim / renew / 2/3 壽命續期)與 `POST /api/chat` 投遞策略。
- `slack.go` — Socket Mode 連線迴圈(apps.connections.open → wss → ack → 重連)、訊息過濾、去重、渲染、deadletter。
- `health.go` — 連線健康警示(見下)。
- `seen.go` — JSON 去重表。
- `githubwatch.go` — review request 查詢、渲染、seeding。
- `deploy/*.plist` — launchd 範本(見「常駐」)。
- `config.example.toml` — 設定範例。

## 為什麼是這個形狀

* **Socket Mode 沒有公開端點**:bridge 主動連出去,所以不經 cloudflared。協定直接用原始
  WebSocket 實作(`apps.connections.open` 換一次性 wss URL → 連線 → 每個 envelope 先 ack),
  不帶 Slack SDK;唯二外部依賴是 `golang.org/x/net/websocket` 與 `github.com/BurntSushi/toml`。
* **不走 `/in`**:`/in` 會把 payload 原封不動當成聊天內容,成員得自己解 Slack 的 JSON。走
  `POST /api/chat` 可以先渲染成人看得懂的一行,並把結構化欄位放進 `meta.slack`。
* **身分是一台「機器」**:`/api/chat` 對寄件者沒有 kind 檢查,而 machine 憑證是唯一能
  **自助續期**的(`POST /api/machines/renew-credential`,只能續自己的),所以不需要有人
  定期手動重簽。代價:機器清單會多一台永遠 `Online:false` 的 `slack-bridge`,那是裝飾性的,
  reconcile 不會對它做任何事。
* **GitHub 用輪詢不用 webhook**:GitHub 無法只訂閱單一 action,`pull_request` webhook 會把
  opened/synchronize/labeled/closed 全送來(每則 ~26 KB),search API 一次就答得出
  「哪些 open PR 在等我 review」。透過 `gh` CLI 呼叫,這個 process 從不經手 GitHub token。

## 先決條件

Slack app(任一 workspace,**內部 app**;Socket Mode 的 app 不能上架 Marketplace):

1. 開啟 **Socket Mode**。
2. 建 **app-level token**(`xapp-…`),scope `connections:write`。
3. **Bot token scopes**:`channels:history`(公開頻道)、`users:read`(解析寄件者名稱);
   私訊加 `im:history`,私有頻道加 `groups:history`,群組私訊加 `mpim:history`。
4. **Event Subscriptions** 訂閱 `message.channels`(要被 @ 才收就用 `app_mention`)。
   Socket Mode 開著時**不需要填 Request URL**。
5. 安裝 app,並**把 bot 邀進**你要監看的每個頻道 — 沒邀進去收不到。

github-watch 另需已登入的 `gh` CLI(`gh auth login`;憑證在 `~/.config/gh/hosts.yml`,
launchd job 讀得到)。

## 安裝

```bash
cd cli/ocbridge && go build -o ocbridge . && install -m 755 ocbridge ~/bin/ocbridge   # 位置自選

mkdir -p ~/.officraft-slack-bridge
cp config.example.toml ~/.officraft-slack-bridge/config.toml
chmod 600 ~/.officraft-slack-bridge/config.toml
$EDITOR ~/.officraft-slack-bridge/config.toml   # 填 app_token / bot_token / to / [github]
```

config 路徑可用 `BRIDGE_CONFIG` 覆蓋;`[bridge].state_dir` 省略時用 config 所在資料夾。

取得 OffiCraft 憑證(一次性):

1. 控制台 **監控 › 機器 › 加機器**,名字取 `slack-bridge`。
2. 它會給一行 `curl -fsSL '.../install.sh?code=XXXX' | bash` — **不要執行這行**
   (執行會在這台機器上裝一個 warden)。只要 `code=` 後面那段。
3. claim code 是**一次性、約 10 分鐘就過期**:

```bash
ocbridge claim XXXX
# → claimed; credential written to ~/.officraft-slack-bridge/credential.json (0600)
```

之後 token 由 `ocbridge slack` 自己在剩餘壽命 2/3 處續期(失敗每小時重試),
投遞時遇到 401 也會先續期一次再重試。

## 常駐

`deploy/` 下是兩份 launchd **範本**,先把佔位字串換掉:

- `__OCBRIDGE_BIN__` → binary 的絕對路徑(例如 `/Users/<you>/bin/ocbridge`)
- `__HOME__` → 你的家目錄(例如 `/Users/<you>`)

```bash
for p in com.officraft.ocbridge com.officraft.ocbridge.github-watch; do
  sed -e "s#__OCBRIDGE_BIN__#$HOME/bin/ocbridge#g" -e "s#__HOME__#$HOME#g" \
    deploy/$p.plist > ~/Library/LaunchAgents/$p.plist
  launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/$p.plist
done
launchctl list | grep com.officraft.ocbridge
tail -f ~/.officraft-slack-bridge/ocbridge.err.log     # log 都寫 stderr
```

停止:`launchctl bootout gui/$(id -u)/com.officraft.ocbridge`(github-watch 同理)。
github-watch 的 plist PATH 要找得到 `gh`(Apple silicon 的 Homebrew 在 `/opt/homebrew/bin`)。

## 從 Python bridge 遷移

- **沿用**:`config.toml`(同鍵、同預設路徑、同 `BRIDGE_CONFIG`)與 `credential.json`
  (同格式 `{token, expires_at, machine_id}`)— **不需要重新 claim**。
- **重新開始**:去重表改成 `seen.json` / `github_seen.json`,舊的 `seen.db` /
  `github_seen.db` / `github_seeded` 不讀。Slack 端影響只有切換那一刻若剛好重送會多一則;
  github-watch 因為 `github_seen.json` 不存在,第一輪**只 seed、不回報**,不會把整個積壓再報一次。
- **launchd label 換了**:舊 job 先 `launchctl bootout gui/$(id -u)/<舊 label>`,**再把舊 Python
  bridge 的 plist(你當初安裝它用的那個 label)從 `~/Library/LaunchAgents/` 刪掉**,最後才裝
  `deploy/` 的新範本。只 bootout 不刪檔,下次登入舊 job 會被重新載入,變成兩條 Socket Mode 連線
  同時跑,同一個 Slack app 的事件會被隨機分到其中一條 —— 新 bridge 只收到一部分。
  github-watch 的舊 plist 同理。

## 運作上要知道的事

* **離線期間的訊息會遺失**。Slack 對 Socket Mode **不做緩衝**(官方文件:
  "you may lose events until you establish a connection")。Mac 睡眠、重開機、網路斷線那段
  時間的訊息沒有任何補送機制。這是選 Socket Mode 換來的代價 — HTTP webhook 反而有 Slack 重送。
* **先 ack、再處理**。每個 envelope 一收到就回 `{"envelope_id": …}`,投遞在背景做;
  慢 ack 會被 Slack 重送。
* **重連永不放棄**。讀取錯誤、連線被關、`apps.connections.open` 或 dial 失敗(包含啟動時
  的第一次)都走同一個重連迴圈:5 秒起跳、每次加倍、上限 60 秒;收到 `hello` 就重設。
  Slack 送 `disconnect` 時,只有「已收到 hello」且 reason 是 `refresh_requested` / `warning`
  (例行換線)才立即重連;`link_disabled` 或還沒 hello 就被斷,一律走 backoff,
  不狂打 `apps.connections.open`。另外每 30 秒送一次 ping,90 秒沒收到任何 bytes(含 pong)
  就判定半開連線、直接關掉底層 socket 重連(不走 websocket close,避免卡在寫入鎖上);
  每次寫入也有 30 秒上限。
* **關機不丟訊息**。SIGTERM(包括警示訊息裡的 `kickstart -k`)時,已 ack、還在投遞中的訊息
  最多等 10 秒;等不到的寫進 `deadletter.jsonl`(極少數情況下會同時出現在聊天與 deadletter,
  寧可重複不可遺失)。
* **去重靠我們自己**。OffiCraft 的聊天入口沒有去重,而 Slack 會重送沒 ack 的 envelope。
  `seen.json` 擋掉重送,且**跨重啟有效**(重啟後立刻重連正是最容易重送的時刻);
  超過 7 天的鍵會被清掉(github 180 天)。檔案損毀時 slack 模式記 ERROR 並以空表啟動。
* **投遞策略**:最多 4 次、間隔 1.5^n 秒;4xx 立即失敗(壞 recipient 重試也不會變好),
  唯一例外是 401 — 續期一次再重試。
* **投遞失敗會落地**。因為 envelope 已經 ack 過、Slack 不會再送,投不進去的訊息以
  `{at, error, payload}` 一行一筆寫到 `deadletter.jsonl`,不會無聲消失。
* 忽略 bot 自己發的訊息與 `message_changed` / `message_deleted` / `channel_join` /
  `channel_leave` / `bot_message` 這類雜訊,避免迴圈與噪音。未知 subtype 照轉。
* github-watch:個人指名一條查詢、每個 `[github].teams` 一條;同一個 PR 以 URL 去重、
  第一個找到的原因為準;一則最多列 20 筆,其餘以「…另有 N 筆未列出」帶過;
  送到 `[github].to`(省略則 `[officraft].to`)。**投遞失敗時不記為已回報**,下一輪會再報。

## 連線健康警示

process 活著、卻卡在「連上即斷、每幾秒重連」時(2026-09 曾這樣靜默 11 天:每次連上立刻
BrokenPipe、約 6 秒重連一次),log 看起來很忙,但什麼都進不來。`health.go` 專門抓這個:

* **健康 = 連上後撐滿 2 分鐘沒有斷線**。讀取錯誤 / 關閉 / dial 失敗都算斷線(計次);
  每 30 秒的檢查若發現沒有活連線,也算斷線中,但**不另外計次**(同一次斷線不重複算)。
* 斷線持續 **15 分鐘** → 走一般投遞路徑發一則
  `[Slack bridge 警示] Socket Mode 連線自 <MM-DD HH:MM> 起撐不住(已 N 分鐘,斷線 K 次)…`
  給 `[officraft].to`,內含重啟指令
  `launchctl kickstart -k gui/$(id -u)/com.officraft.ocbridge`。
* 仍未恢復時**每小時最多再提醒一則**;穩定後發一則 `[Slack bridge 已恢復]` — 只在先前
  真的發過警示時才發,所以 Slack 自己要求的例行重連(幾秒就好)不會發任何訊息。
* 警示與「已恢復」送不出去(例如 OffiCraft 本身掛了)都不算已送,記 ERROR log、下一次檢查
  再試;若在補送「已恢復」之前又開始新的斷線,就取消那則,改由新斷線自己的警示說話;
  發送時不持有鎖,不會卡住 socket 讀取。
* 時間以 monotonic clock 計:macOS 睡眠期間不前進,睡一晚醒來不會被當成一整晚的斷線。
  (反過來,憑證續期以 wall clock 為準、每次最多等 1 小時,常睡眠的筆電也不會拖過到期日。)
* process 整個掛掉由 launchd `KeepAlive` 處理,不在這裡。

**連上了卻沒有訊息**(2026-10 曾這樣 16 天:連線一直健康,Slack 一則 message 事件都沒送,
問題在 Slack App 端)是另一種失敗,同一支 `health.go` 另外盯:

* 只要收到 `events_api` 的 `message` 事件就算「還活著」,**在任何過濾之前**計 — 你自己發的、
  bot 發的、重送的都算(證明的是 Slack 那頭還在送)。
* 連線正常且 **24 小時**(`[slack].silence_alert_hours`,選填,省略、0 或負數 = 24)沒有任何事件 →
  發一則 `[Slack bridge 警示] Slack 連線正常,但自 <MM-DD HH:MM> 起已 N 小時沒收到任何訊息事件…`,
  指向 Slack App 的 Event Subscriptions / 授權。仍然沒有時**每 24 小時最多再提醒一則**(不是每小時);
  下一則事件進來就發一則 `[Slack bridge 已恢復]`。送不出去的處理同上(下次檢查重送)。
* **斷線期間由斷線警示負責**:斷線中不發沒訊息警示;斷線(含恢復前那 2 分鐘穩定期)的時間
  **不算進**沒訊息的時數,所以斷線恢復後也不會馬上補一則 — 要再連續連線 24 小時仍沒事件才發。
  已發過沒訊息警示時,「24 小時提醒一次」的間隔同樣扣掉斷線時間。
* 計時從 process 啟動開始,**不跨重啟保存**:重啟會把時鐘歸零,沒訊息要在重啟後滿 24 小時才發現。

## 誰的訊息會被轉進去

* **只轉別人說的話**。`[slack].ignore_user_ids` 裡的人(預設是你自己)發的訊息會被丟掉 —
  你自己說過的話不需要再回報一次。**但仍然照常 ack**:不 ack,Slack 會一直重送。
* **去重以 `(channel, ts)` 為鍵,不是 `event_id`**(缺 channel/ts 才退回 event_id)。一旦同時
  存在 bot 與 user 兩種授權,同一則訊息會被兩邊看到,而 Slack 可能為每個授權各發一個
  `event_id` —— 用 event_id 當鍵會讓它漏過去變成兩則。`(channel, ts)` 指的是訊息本身。
* log 會記下每則訊息是從哪個授權進來的(`accepted <key> (authorization: bot | user U…)`)。

## bot token 看得到什麼(範圍限制)

bot 只看得到**自己在場**的對話:被 `/invite` 邀進去的頻道、以及別人跟 bot 的私訊。
**它看不到你和第三人的私訊** —— 那需要以你本人身分授權的 user token(`xoxp-`)。

要接上個人私訊,先驗這件事(官方文件沒寫死 user 授權的事件會不會走 Socket Mode,
而且有非官方回報說會被靜默丟棄):

1. app 設定加 **User Token Scope** `im:history`,重新安裝完成 OAuth。
2. 私訊任何人一句話。
3. 看 log:出現 `skip own message from <你的 id>` 就代表 user 授權事件**確實走 socket**
   (bot 不在那個對話裡,事件只可能來自 user 授權),而且因為被過濾器擋下,
   **沒有任何內容進入 officraft**。沒出現就是被丟棄,那條路不通。

⚠️ user `im:history` 是**全有全無**:你所有私訊(收的與發的)都會進來,Slack 端沒有
「只收某幾個人」的設定,要篩只能在這支 bridge 裡做。同一張 token 也能回頭撈歷史訊息。

## 測試

```bash
cd cli/ocbridge && go test ./... -count=1
```

stdlib `testing`,無網路:OffiCraft 與 Slack 都是 `httptest` fake。涵蓋去重(含重啟、跨授權)、
渲染、400/401/5xx 投遞策略、自我訊息過濾仍 ack、deadletter、github 渲染/查詢/行數上限/seeding、
連線健康(`TestHealth1`–`TestHealth4` 對應 ①–④,另有 tick-only 斷線、警示失敗重送、斷線不重複計)、
連上卻沒訊息(`TestSilence1`–`TestSilence6`:24h 警示一次、事件來了恢復一次、23h59m 不發、
自己的訊息算活著、斷線時只有斷線警示、48h 內只提醒一次),
以及 end-to-end(`TestE2E_*`:fake Slack 接了就斷 → 持續重連且健康記到斷線;送一個
events_api → 收到 ack、OffiCraft 收到一則)。CI 第 7 道 golang gate 自動涵蓋本 module
(`bin/ci.sh` 掃 `cli/*/go.mod`)。
