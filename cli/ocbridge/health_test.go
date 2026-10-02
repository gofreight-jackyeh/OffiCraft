package main

import (
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
