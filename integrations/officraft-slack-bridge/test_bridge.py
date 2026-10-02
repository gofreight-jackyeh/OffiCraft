"""最小可跑檢查:去重、渲染、重試策略、401 續期。跑法:uv run python test_bridge.py"""
import io, json, pathlib, sys, tempfile, urllib.error
import bridge

fails = []
def check(name, cond):
    print(("PASS " if cond else "FAIL ") + name)
    if not cond: fails.append(name)

# 1) 去重(Socket Mode 重連會重送;跨 process 重啟也要記得)
with tempfile.TemporaryDirectory() as d:
    seen = bridge.SeenEvents(pathlib.Path(d)/"seen.db")
    check("首次出現可通過", seen.claim("Ev1") is True)
    check("重送被擋下", seen.claim("Ev1") is False)
    check("不同事件不受影響", seen.claim("Ev2") is True)
    check("重啟後仍記得", bridge.SeenEvents(pathlib.Path(d)/"seen.db").claim("Ev1") is False)

# 2) 渲染:沒有 bot token 時退回用 id
names = bridge.NameCache(None)
out = bridge.render({"user":"U1","channel":"C1","text":"hello"}, names)
check("渲染含頻道/人/內容", "C1" in out and "U1" in out and "hello" in out)
check("空訊息不產生空白身體", "無文字內容" in bridge.render({"user":"U1","channel":"C1"}, names))
check("私訊不顯示無意義的 channel id",
      "私訊" in bridge.render({"user":"U1","channel":"D1","channel_type":"im","text":"hi"}, names)
      and "D1" not in bridge.render({"user":"U1","channel":"D1","channel_type":"im","text":"hi"}, names))
check("群組私訊有自己的前綴",
      "群組私訊" in bridge.render({"user":"U1","channel":"G1","channel_type":"mpim","text":"hi"}, names))
check("公開頻道仍顯示頻道",
      "#C1" in bridge.render({"user":"U1","channel":"C1","channel_type":"channel","text":"hi"}, names))

class FakeCred:
    def __init__(self): self.token="t0"; self.renews=0
    def renew(self): self.renews+=1; self.token=f"t{self.renews}"

def http_error(url, code, body=b"x"):
    return urllib.error.HTTPError(url, code, "err", {}, io.BytesIO(body))

bridge.DELIVER_BACKOFF = 0

# 3) 400 類錯誤立即失敗(壞 recipient 重試四次也不會變好)
calls=[]
bridge.urllib.request.urlopen = lambda req, timeout=0: (_ for _ in ()).throw(
    calls.append(req.headers.get("Authorization")) or http_error(req.full_url, 400))
cred=FakeCred(); oc=bridge.OffiCraft("http://h", cred, "mira")
try: oc.post("x", {}); check("400 應拋錯", False)
except RuntimeError as e: check("400 只呼叫一次且不續期", len(calls)==1 and cred.renews==0)

# 4) 401 -> 續期一次 -> 帶新 token 重試
calls.clear()
def once_401(req, timeout=0):
    calls.append(req.headers.get("Authorization"))
    if len(calls)==1: raise http_error(req.full_url, 401)
    class R:
        status=200
        def __enter__(self): return self
        def __exit__(self,*a): return False
    return R()
bridge.urllib.request.urlopen = once_401
cred=FakeCred(); oc=bridge.OffiCraft("http://h", cred, "mira")
oc.post("x", {})
check("401 觸發一次續期", cred.renews==1)
check("重試帶的是新 token", calls==["Bearer t0","Bearer t1"])

# 5) 401 續期失敗不可無聲吞掉
class DeadCred(FakeCred):
    def renew(self): raise RuntimeError("mint down")
bridge.urllib.request.urlopen = lambda req, timeout=0: (_ for _ in ()).throw(http_error(req.full_url,401))
try:
    bridge.OffiCraft("http://h", DeadCred(), "mira").post("x", {}); check("續期失敗應拋錯", False)
except RuntimeError as e: check("續期失敗會拋錯並說明原因", "renewal failed" in str(e))

# 6) 5xx 重試到上限
calls.clear()
bridge.urllib.request.urlopen = lambda req, timeout=0: (_ for _ in ()).throw(
    calls.append(1) or http_error(req.full_url, 503))
try: bridge.OffiCraft("http://h", FakeCred(), "mira").post("x", {})
except RuntimeError: pass
check("5xx 重試到上限", len(calls)==bridge.DELIVER_RETRIES)

# 7) 自己發的訊息不轉,別人的照轉
class FakeClient:
    def __init__(self): self.acks=0
    def send_socket_mode_response(self, resp): self.acks+=1
class FakeReq:
    def __init__(self, user): self.type="events_api"; self.envelope_id="e1"; self.payload={
        "event_id":f"Ev-{user}","event":{"type":"message","user":user,"channel":"C1",
        "channel_type":"channel","text":"hi"}}
sent=[]
class FakeOC:
    to="mira"
    def post(self, body, meta): sent.append(body)
import tempfile as _tf, pathlib as _pl
with _tf.TemporaryDirectory() as d:
    h = bridge.make_handler(FakeOC(), bridge.SeenEvents(_pl.Path(d)/"s.db"), bridge.NameCache(None),
                            _pl.Path(d)/"dl.jsonl", frozenset({"UME"}))
    c = FakeClient()
    h(c, FakeReq("UME"));     check("自己發的訊息不轉", sent == [])
    h(c, FakeReq("UOTHER"));  check("別人發的訊息照轉", len(sent) == 1 and "UOTHER" in sent[0])
    check("兩者都有 ack(否則 Slack 會重送)", c.acks == 2)

# 8) 同一則訊息若同時被 bot 與 user 兩種授權看見,只能進去一次
class Req2:
    def __init__(self, eid, is_bot, user="UOTHER"):
        self.type="events_api"; self.envelope_id="e"
        self.payload={"event_id":eid,
                      "authorizations":[{"is_bot":is_bot,"user_id":"U0EXAMPLE"}],
                      "event":{"type":"message","user":user,"channel":"C9",
                               "channel_type":"channel","ts":"1789.0001","text":"same message"}}
sent2=[]
class OC2:
    to="mira"
    def post(self, body, meta): sent2.append(body)
with _tf.TemporaryDirectory() as d:
    h2 = bridge.make_handler(OC2(), bridge.SeenEvents(_pl.Path(d)/"s2.db"), bridge.NameCache(None),
                             _pl.Path(d)/"dl2.jsonl", frozenset({"UME"}))
    c2 = FakeClient()
    h2(c2, Req2("EvBOT", True))     # bot 授權看到
    h2(c2, Req2("EvUSER", False))   # user 授權看到「同一則」,event_id 不同
    check("兩種授權看到同一則訊息只投遞一次", len(sent2) == 1)
    check("兩者都有 ack", c2.acks == 2)

print("\n" + ("全部通過" if not fails else f"失敗 {len(fails)} 項: {fails}"))
sys.exit(1 if fails else 0)
