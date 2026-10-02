# officraft-slack-bridge

Slack **Socket Mode** → OffiCraft chat。不使用 `/in`,不需要公開網址。

```
Slack ──WebSocket(outbound)──▶ bridge.py ──POST /api/chat──▶ OffiCraft(127.0.0.1:7755)
```

## 為什麼是這個形狀

* **Socket Mode 沒有公開端點**:bridge 主動連出去,所以不經 cloudflared。
* **不走 `/in`**:`/in` 會把 payload 原封不動當成聊天內容,成員得自己解 Slack 的 JSON。走
  `POST /api/chat` 可以先渲染成人看得懂的一行,並把結構化欄位放進 `meta.slack`。
* **身分是一台「機器」**:`/api/chat` 對寄件者沒有 kind 檢查,而 machine 憑證是唯一能
  **自助續期**的(`POST /api/machines/renew-credential`,只能續自己的),所以不需要有人
  每年手動重簽。代價:機器清單會多一台永遠 `Online:false` 的 `slack-bridge`,那是裝飾性的,
  reconcile 不會對它做任何事。

## 先決條件

Slack app(任一 workspace,**內部 app**;Socket Mode 的 app 不能上架 Marketplace):

1. 開啟 **Socket Mode**。
2. 建 **app-level token**(`xapp-…`),scope `connections:write`。
3. **Bot token scopes**:`channels:history`(公開頻道)、`users:read`(解析寄件者名稱);
   私訊加 `im:history`,私有頻道加 `groups:history`,群組私訊加 `mpim:history`。
4. **Event Subscriptions** 訂閱 `message.channels`(要被 @ 才收就用 `app_mention`)。
   Socket Mode 開著時**不需要填 Request URL**。
5. 安裝 app,並**把 bot 邀進**你要監看的每個頻道 — 沒邀進去收不到。

## 安裝

```bash
cd ~/officraft-slack-bridge
uv sync

mkdir -p ~/.officraft-slack-bridge
cp config.example.toml ~/.officraft-slack-bridge/config.toml
chmod 600 ~/.officraft-slack-bridge/config.toml
$EDITOR ~/.officraft-slack-bridge/config.toml   # 填 app_token / bot_token / to
```

取得 OffiCraft 憑證(一次性):

1. 控制台 **監控 › 機器 › 加機器**,名字取 `slack-bridge`。
2. 它會給一行 `curl -fsSL '.../install.sh?code=XXXX' | bash` — **不要執行這行**
   (執行會在這台機器上裝一個 warden)。只要 `code=` 後面那段。
3. claim code 是**一次性、約 10 分鐘就過期**:

```bash
uv run python bridge.py claim XXXX
# → credential written to ~/.officraft-slack-bridge/credential.json (0600)
```

之後 token 由 bridge 自己在壽命 2/3 處續期,不需要再管。

## 常駐

```bash
cp com.jackyeh.officraft-slack-bridge.plist ~/Library/LaunchAgents/
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.jackyeh.officraft-slack-bridge.plist
launchctl list | grep officraft-slack-bridge
tail -f ~/.officraft-slack-bridge/bridge.out.log
```

停止:`launchctl bootout gui/$(id -u)/com.jackyeh.officraft-slack-bridge`

## 運作上要知道的事

* **離線期間的訊息會遺失**。Slack 對 Socket Mode **不做緩衝**(官方文件:
  "you may lose events until you establish a connection")。Mac 睡眠、重開機、網路斷線那段
  時間的訊息沒有任何補送機制。這是選 Socket Mode 換來的代價 — HTTP webhook 反而有 Slack 重送。
* **去重靠我們自己**。OffiCraft 的聊天入口沒有去重,而 Slack 會重送沒 ack 的 envelope。
  `seen.db` 以 `event_id` 擋掉重送,且**跨重啟有效**(重啟後立刻重連正是最容易重送的時刻)。
* **投遞失敗會落地**。因為 envelope 已經 ack 過、Slack 不會再送,投不進去的訊息寫到
  `deadletter.jsonl`,不會無聲消失。
* **401 會自動續期一次再重試**,避免 token 到期後每則訊息都進 deadletter。
* 忽略 bot 自己發的訊息與 `message_changed` / `channel_join` 這類雜訊,避免迴圈與噪音。

## 誰的訊息會被轉進去

* **只轉別人說的話**。`[slack].ignore_user_ids` 裡的人(預設是你自己)發的訊息會被丟掉 —
  你自己說過的話不需要再回報一次。**但仍然照常 ack**:不 ack,Slack 會一直重送。
* **去重以 `(channel, ts)` 為鍵,不是 `event_id`**。一旦同時存在 bot 與 user 兩種授權,
  同一則訊息會被兩邊看到,而 Slack 可能為每個授權各發一個 `event_id` —— 用 event_id 當鍵
  會讓它漏過去變成兩則。`(channel, ts)` 指的是訊息本身,對重送與授權都穩定。
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
uv run python test_bridge.py    # 19 項:去重(含跨授權)、渲染、重試策略、401 續期、自我訊息過濾
```
