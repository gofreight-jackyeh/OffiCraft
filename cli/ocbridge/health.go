package main

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// Health timing (port of the Python ConnectionHealth constants).
const (
	alertAfter  = 15 * time.Minute // down this long -> first alert
	remindEvery = time.Hour        // at most one reminder per hour while still down
	stableFor   = 2 * time.Minute  // connected with no drop this long = healthy again
	healthTick  = 30 * time.Second // production tick interval
)

const restartHint = "launchctl kickstart -k gui/$(id -u)/com.officraft.ocbridge"

// connectionHealth says so in chat when the Slack socket stops holding — and
// when it is back.
//
// The failure this exists for is not a dead process (launchd KeepAlive
// restarts that) but a live one stuck in a reconnect loop: every ~6 s the
// socket connects, hits BrokenPipe, and reconnects. The process is up, logs
// look busy, and nothing reaches OffiCraft. That ran 11 days unseen.
//
// So "healthy" means the socket STAYED up: no drop for stableFor while
// connected. Any drop (or a tick that finds no live connection) opens an
// outage; an outage older than alertAfter posts one alert, then at most one
// reminder per remindEvery; once stable again a "recovered" line follows — but
// only if an alert went out, so the routine reconnects Slack itself requests
// every few hours stay silent.
//
// Every method takes `now` so timing is testable without sleeping. In
// production `now` is time.Now(): durations are computed from its monotonic
// reading, which on macOS does not advance while the machine sleeps, so
// waking from a night's sleep is not mistaken for a night-long outage. The
// wall-clock part of the same value is used only for the "since" in messages.
type connectionHealth struct {
	send func(body string, meta any) error // the normal OffiCraft delivery path

	mu        sync.Mutex
	down      bool
	downSince time.Time
	lastDown  time.Time
	drops     int
	alerted   bool // an alert (or reminder) went out for the current outage
	lastAlert time.Time
}

// markDown records a drop. count is false for a tick that merely finds the
// socket down, so one drop seen by both the reader and the tick is not
// counted twice.
func (h *connectionHealth) markDown(now time.Time, count bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.down {
		h.down, h.downSince, h.drops = true, now, 0
	}
	h.lastDown = now
	if count {
		h.drops++
	}
}

// tick is called from the health goroutine only. The lock guards state shared
// with the socket reader; delivery (retries can take a while) happens outside
// it so the reader never waits on OffiCraft.
func (h *connectionHealth) tick(now time.Time, connected bool) {
	if !connected {
		h.markDown(now, false)
	}
	h.mu.Lock()
	if !h.down {
		h.mu.Unlock()
		return
	}
	since, lastDown, drops := h.downSince, h.lastDown, h.drops
	var due, recovered bool
	switch {
	case connected && now.Sub(lastDown) >= stableFor:
		recovered = h.alerted
		h.down, h.alerted = false, false
	case now.Sub(since) < alertAfter || (h.alerted && now.Sub(h.lastAlert) < remindEvery):
		h.mu.Unlock()
		return
	default:
		due = true
	}
	h.mu.Unlock()

	if due {
		body := fmt.Sprintf("[Slack bridge 警示] Socket Mode 連線自 %s 起撐不住(已 %s 分鐘,斷線 %d 次)。"+
			"Slack 訊息目前收不到,且 Slack 不會補送。重啟:%s",
			stamp(since), minutes(now.Sub(since)), drops, restartHint)
		// Marked sent only if delivery worked, so a failed alert is retried on
		// the next tick instead of being lost.
		if h.deliver(body, "down") {
			h.mu.Lock()
			h.alerted, h.lastAlert = true, now
			h.mu.Unlock()
		}
	} else if recovered {
		h.deliver(fmt.Sprintf("[Slack bridge 已恢復] Socket Mode 連線已穩定。斷線約 %s 分鐘(自 %s 起),"+
			"這段期間的 Slack 訊息 Slack 不會補送。", minutes(lastDown.Sub(since)), stamp(since)), "recovered")
	}
}

func (h *connectionHealth) deliver(body, state string) bool {
	if err := h.send(body, map[string]string{"source": "slack-bridge-health", "state": state}); err != nil {
		// The monitor must outlive a failed alert.
		log.Printf("ERROR health alert (%s) not delivered: %v", state, err)
		return false
	}
	return true
}

func (h *connectionHealth) dropCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.drops
}

func stamp(t time.Time) string       { return t.Local().Format("01-02 15:04") }
func minutes(d time.Duration) string { return fmt.Sprintf("%.0f", d.Minutes()) }
