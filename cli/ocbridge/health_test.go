package main

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// healthRecorder stands in for OffiCraft delivery.
type healthRecorder struct {
	states []string
	bodies []string
	fail   bool
}

func (r *healthRecorder) send(body string, meta any) error {
	if r.fail {
		return errors.New("officraft down")
	}
	r.states = append(r.states, meta.(map[string]string)["state"])
	r.bodies = append(r.bodies, body)
	return nil
}

var t0 = time.Date(2026, 9, 1, 9, 0, 0, 0, time.Local)

func at(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }

func newHealth() (*connectionHealth, *healthRecorder) {
	r := &healthRecorder{}
	return &connectionHealth{send: r.send}, r
}

// flap reproduces the 2026-09 failure shape: a drop every 6 s, a tick every
// 30 s that happens to land while the socket is (briefly) connected.
func flap(h *connectionHealth, start, secs int) {
	for s := start; s < start+secs; s++ {
		if s%6 == 0 {
			h.markDown(at(s), true)
		}
		if s%30 == 0 {
			h.tick(at(s), true)
		}
	}
}

func steady(h *connectionHealth, start, secs int) {
	for s := start; s < start+secs; s += 30 {
		h.tick(at(s), true)
	}
}

func TestHealth1_ConnectThenDropEvery6sAlertsExactlyOnceWithin15Min(t *testing.T) {
	h, r := newHealth()
	flap(h, 0, 15*60+30)
	if !reflect.DeepEqual(r.states, []string{"down"}) {
		t.Fatalf("want exactly one alert within 15 min, got %v", r.states)
	}
	want := "[Slack bridge 警示] Socket Mode 連線自 09-01 09:00 起撐不住(已 15 分鐘,斷線 151 次)。" +
		"Slack 訊息目前收不到,且 Slack 不會補送。重啟:launchctl kickstart -k gui/$(id -u)/com.officraft.ocbridge"
	if r.bodies[0] != want {
		t.Fatalf("alert text\n got %q\nwant %q", r.bodies[0], want)
	}
}

func TestHealth2_RecoveredMessageAfterRecovery(t *testing.T) {
	h, r := newHealth()
	flap(h, 0, 20*60)
	steady(h, 20*60, 5*60)
	if !reflect.DeepEqual(r.states, []string{"down", "recovered"}) {
		t.Fatalf("want down then recovered, got %v", r.states)
	}
	if !strings.HasPrefix(r.bodies[1], "[Slack bridge 已恢復] Socket Mode 連線已穩定。斷線約 20 分鐘(自 09-01 09:00 起)") {
		t.Fatalf("recovered text: %q", r.bodies[1])
	}
	steady(h, 25*60, 60*60)
	if len(r.states) != 2 {
		t.Fatalf("nothing more once healthy, got %v", r.states)
	}
}

func TestHealth3_NoRepeatWithinAnHourReminderAfterAnHour(t *testing.T) {
	h, r := newHealth()
	flap(h, 0, 15*60+30)
	flap(h, 15*60+30, 45*60) // still down at 60 min
	if len(r.states) != 1 {
		t.Fatalf("no repeat within an hour of the alert, got %v", r.states)
	}
	flap(h, 60*60+30, 16*60) // an hour after the first alert
	if !reflect.DeepEqual(r.states, []string{"down", "down"}) {
		t.Fatalf("want one reminder after an hour, got %v", r.states)
	}
	steady(h, 76*60+30, 5*60)
	if !reflect.DeepEqual(r.states, []string{"down", "down", "recovered"}) {
		t.Fatalf("want recovered after the reminder, got %v", r.states)
	}
}

func TestHealth4_SilentForANormalDayAndAShortReconnect(t *testing.T) {
	h, r := newHealth()
	steady(h, 0, 24*3600)
	if len(r.states) != 0 {
		t.Fatalf("a normal day must be silent, got %v", r.states)
	}
	h.markDown(at(100000), true) // a routine Slack-requested reconnect
	steady(h, 100020, 10*60)
	if len(r.states) != 0 {
		t.Fatalf("a short reconnect must send neither alert nor recovered, got %v", r.states)
	}
}

func TestHealthTickOnlyDisconnectAlerts(t *testing.T) {
	h, r := newHealth()
	for s := 0; s <= 16*60; s += 30 { // never connects, no read-error callbacks at all
		h.tick(at(s), false)
	}
	if !reflect.DeepEqual(r.states, []string{"down"}) {
		t.Fatalf("tick-observed outage must alert, got %v", r.states)
	}
	if h.dropCount() != 0 {
		t.Fatalf("tick-observed down must not count as a drop, got %d", h.dropCount())
	}
}

func TestHealthFailedAlertRetriedNextTick(t *testing.T) {
	h, r := newHealth()
	r.fail = true
	flap(h, 0, 15*60+30)
	if h.alerted {
		t.Fatal("a failed alert must not be recorded as sent")
	}
	r.fail = false
	flap(h, 15*60+30, 60)
	if !reflect.DeepEqual(r.states, []string{"down"}) {
		t.Fatalf("want the alert re-sent on the next tick, got %v", r.states)
	}
}

func TestHealthDropsNotDoubleCounted(t *testing.T) {
	h, _ := newHealth()
	for s := 0; s <= 16*60; s += 30 { // reader sees the drop AND the tick sees it down
		h.markDown(at(s), true)
		h.tick(at(s), false)
	}
	if got, want := h.dropCount(), 16*2+1; got != want {
		t.Fatalf("drops = %d, want %d (one per real drop)", got, want)
	}
}

func TestHealthFailedRecoveredRetriedNextTick(t *testing.T) {
	h, r := newHealth()
	flap(h, 0, 20*60)
	r.fail = true
	steady(h, 20*60, 5*60)
	if !reflect.DeepEqual(r.states, []string{"down"}) {
		t.Fatalf("recovered must not be recorded while delivery fails, got %v", r.states)
	}
	r.fail = false
	steady(h, 25*60, 30*60)
	if !reflect.DeepEqual(r.states, []string{"down", "recovered"}) {
		t.Fatalf("want recovered re-sent exactly once, got %v", r.states)
	}
}

func TestHealthNewOutageCancelsPendingRecovered(t *testing.T) {
	h, r := newHealth()
	flap(h, 0, 20*60)
	r.fail = true
	steady(h, 20*60, 5*60) // recovered owed but undelivered
	h.markDown(at(25*60), true)
	r.fail = false
	steady(h, 25*60+30, 5*60) // short new outage that heals without an alert
	if !reflect.DeepEqual(r.states, []string{"down"}) {
		t.Fatalf("a new outage must cancel the pending recovered, got %v", r.states)
	}
}

// --- connected but silent (no Slack message events) ---

const day = 24 * 3600

func newSilence() (*connectionHealth, *healthRecorder) {
	r := &healthRecorder{}
	return &connectionHealth{send: r.send, silenceAfter: defaultSilenceAfter, lastEvent: at(0)}, r
}

func TestSilence1_ConnectedNoEventsFor24hAlertsOnce(t *testing.T) {
	h, r := newSilence()
	steady(h, 0, day+30*60)
	if !reflect.DeepEqual(r.states, []string{"silent"}) {
		t.Fatalf("want exactly one silence alert, got %v", r.states)
	}
	if !strings.HasPrefix(r.bodies[0], "[Slack bridge 警示] Slack 連線正常,但自 09-01 09:00 起已 24 小時沒收到任何訊息事件") {
		t.Fatalf("silence alert text: %q", r.bodies[0])
	}
}

func TestSilence2_EventAfterAlertSendsOneRecovered(t *testing.T) {
	h, r := newSilence()
	steady(h, 0, day+30)
	h.markEvent(at(day + 100))
	steady(h, day+120, 2*3600)
	if !reflect.DeepEqual(r.states, []string{"silent", "silent-recovered"}) {
		t.Fatalf("want silent then one silent-recovered, got %v", r.states)
	}
	if !strings.HasPrefix(r.bodies[1], "[Slack bridge 已恢復] 又收到 Slack 訊息事件了(自 09-01 09:00 起中斷)") {
		t.Fatalf("recovered text: %q", r.bodies[1])
	}
}

func TestSilence3_23h59mIsQuiet(t *testing.T) {
	h, r := newSilence()
	steady(h, 0, day-60+1) // last tick at 23h59m
	if len(r.states) != 0 {
		t.Fatalf("under 24h must be silent, got %v", r.states)
	}
}

func TestSilence4_FilteredMessagesCountAsAlive(t *testing.T) {
	// Real clock: handleEvent stamps with time.Now(). Each event is dropped by
	// a different filter, yet must still prove Slack is delivering.
	for name, ev := range map[string]map[string]any{
		"own (ignore_user_ids)":   {"type": "message", "user": "U0SELF", "channel": "C0EXAMPLE", "ts": "1789.0001"},
		"bot_id":                  {"type": "message", "bot_id": "B0EXAMPLE", "channel": "C0EXAMPLE", "ts": "1789.0002"},
		"subtype message_changed": {"type": "message", "subtype": "message_changed", "channel": "C0EXAMPLE", "ts": "1789.0003"},
	} {
		f := newFakeOffiCraft(t)
		b := testBridge(t, testPoster(t, f), "U0SELF")
		r := &healthRecorder{}
		b.health = &connectionHealth{send: r.send, silenceAfter: defaultSilenceAfter, lastEvent: time.Now().Add(-25 * time.Hour)}
		raw, err := json.Marshal(map[string]any{"envelope_id": "env-" + name, "type": "events_api",
			"payload": map[string]any{"event_id": "Ev-" + name, "event": ev}})
		if err != nil {
			t.Fatal(err)
		}
		var acks []string
		feed(t, b, raw, &acks)
		b.health.tick(time.Now(), true)
		if posts, _, _ := f.snapshot(); len(posts) != 0 {
			t.Fatalf("%s: must still be filtered, got %d posts", name, len(posts))
		}
		if len(r.states) != 0 {
			t.Fatalf("%s: a filtered message must count as alive, got %v", name, r.states)
		}
	}
}

func TestSilence5_DisconnectedOnlyDisconnectAlerts(t *testing.T) {
	h, r := newSilence()
	for s := 0; s <= day+3600; s += 30 { // 25 h down, no events either
		h.tick(at(s), false)
	}
	steady(h, day+3600+30, 3600) // reconnects and holds
	for _, s := range r.states {
		if s == "silent" {
			t.Fatalf("silence must not alert during or right after a disconnect, got %v", r.states)
		}
	}
	if r.states[0] != "down" || r.states[len(r.states)-1] != "recovered" {
		t.Fatalf("the disconnect alert owns the outage, got %v", r.states)
	}
	// Outage time does not count: silence alerts after 24 h of CONNECTED time.
	n := len(r.states)
	steady(h, day+2*3600+30, day-2*3600)
	if len(r.states) != n {
		t.Fatalf("outage time must not count as silence, got %v", r.states[n:])
	}
	steady(h, 2*day+30, 2*3600) // outage was 25h02m incl. the 2-min stability wait
	if !reflect.DeepEqual(r.states[n:], []string{"silent"}) {
		t.Fatalf("want one silence alert after 24 h connected, got %v", r.states[n:])
	}
}

func TestSilence6_RemindOncePer24hNotHourly(t *testing.T) {
	h, r := newSilence()
	steady(h, 0, 2*day-60+1) // up to 47h59m
	if !reflect.DeepEqual(r.states, []string{"silent"}) {
		t.Fatalf("no reminder within 24 h of the alert, got %v", r.states)
	}
	steady(h, 2*day-30, 60)
	if !reflect.DeepEqual(r.states, []string{"silent", "silent"}) {
		t.Fatalf("want one reminder at 48 h, got %v", r.states)
	}
}

func TestSilenceFailedRecoveredRetried(t *testing.T) {
	h, r := newSilence()
	steady(h, 0, day+30)
	h.markEvent(at(day + 100))
	r.fail = true
	steady(h, day+120, 60)
	r.fail = false
	steady(h, day+180, 3600)
	if !reflect.DeepEqual(r.states, []string{"silent", "silent-recovered"}) {
		t.Fatalf("want silent-recovered re-sent exactly once, got %v", r.states)
	}
}

func TestSilenceReminderIntervalExcludesOutage(t *testing.T) {
	h, r := newSilence()
	steady(h, 0, day+30) // silence alert at 24h
	for s := day + 30; s <= 2*day+30; s += 30 {
		h.tick(at(s), false) // then a 24h disconnect
	}
	n := len(r.states)
	steady(h, 2*day+60, 3600) // reconnects and holds
	for _, s := range r.states[n:] {
		if s == "silent" {
			t.Fatalf("no silence reminder right after a disconnect, got %v", r.states[n:])
		}
	}
	steady(h, 2*day+3660, day)
	if got := r.states[len(r.states)-1]; got != "silent" {
		t.Fatalf("reminder resumes after 24h of connected time, got %v", r.states[n:])
	}
}

func TestSilenceEventDuringAlertSendStillRecovers(t *testing.T) {
	r := &healthRecorder{}
	h := &connectionHealth{silenceAfter: defaultSilenceAfter, lastEvent: at(0)}
	h.send = func(body string, meta any) error {
		if meta.(map[string]string)["state"] == "silent" {
			h.markEvent(at(day + 1)) // an event lands while the alert is in flight
		}
		return r.send(body, meta)
	}
	steady(h, 0, day+3600)
	if !reflect.DeepEqual(r.states, []string{"silent", "silent-recovered"}) {
		t.Fatalf("the alert went out, so its recovered line is owed, got %v", r.states)
	}
}

func TestSilenceNewRoundCancelsUnsentRecovered(t *testing.T) {
	r := &healthRecorder{}
	failRecovered := true
	h := &connectionHealth{silenceAfter: defaultSilenceAfter, lastEvent: at(0)}
	h.send = func(body string, meta any) error {
		if failRecovered && meta.(map[string]string)["state"] == "silent-recovered" {
			return errors.New("officraft down")
		}
		return r.send(body, meta)
	}
	steady(h, 0, day+30)
	h.markEvent(at(day + 100)) // recovered owed, but every attempt fails...
	steady(h, day+120, day+3600)
	failRecovered = false // ...until a new silence round has already alerted
	steady(h, 2*day+3720, 3600)
	if !reflect.DeepEqual(r.states, []string{"silent", "silent"}) {
		t.Fatalf("a new silence alert must cancel the stale recovered, got %v", r.states)
	}
}
