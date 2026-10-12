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

	defaultSilenceAfter = 24 * time.Hour // connected but no message event this long -> alert
	silenceRemindEvery  = 24 * time.Hour // not hourly: a Slack-side outage can last days
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

	// A "recovered" line owed for the last outage, retried each tick until it
	// is delivered; a new outage cancels it (its alert will speak instead).
	recoverPending          bool
	recoverSince, recoverTo time.Time

	// Silence: the socket holds, yet Slack delivers no message events (2026-10:
	// 16 days, Slack App side). lastEvent is the last events_api `message`
	// seen BEFORE any filtering — the owner's own lines prove the pipe works.
	// The caller seeds it with the process start: the clock is NOT persisted,
	// so a restart resets it and a silence only alerts after silenceAfter of
	// uptime (upgrade path: persist lastEvent in state_dir). Config always
	// supplies silenceAfter > 0 (omitted/0/negative = 24h); only the zero value
	// used by disconnect-only tests skips the check. paused is outage time
	// since lastEvent, which does not count as silence — that time belongs to
	// the disconnect alert, so one outage never yields both alerts (also not
	// right after it heals). The reminder interval is shifted the same way.
	silenceAfter          time.Duration
	lastEvent             time.Time
	paused                time.Duration
	silenceAlerted        bool
	lastSilenceAlert      time.Time
	silenceRecoverPending bool
	silenceRecoverSince   time.Time
}

// markEvent records a Slack message event. A silence that was alerted owes
// one "recovered" line, delivered (and retried) by tick.
func (h *connectionHealth) markEvent(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.silenceAlerted {
		h.silenceAlerted = false
		h.silenceRecoverPending, h.silenceRecoverSince = true, h.lastEvent
	}
	h.lastEvent, h.paused = now, 0
}

// markDown records a drop. count is false for a tick that merely finds the
// socket down, so one drop seen by both the reader and the tick is not
// counted twice.
func (h *connectionHealth) markDown(now time.Time, count bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.down {
		h.down, h.downSince, h.drops = true, now, 0
		h.recoverPending = false
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
	if h.down {
		since, lastDown, drops := h.downSince, h.lastDown, h.drops
		switch {
		case connected && now.Sub(lastDown) >= stableFor:
			if h.alerted {
				h.recoverPending, h.recoverSince, h.recoverTo = true, since, lastDown
			}
			h.down, h.alerted = false, false
			// Exclude the outage (including the stability wait) from the
			// silence clock; an event inside it already restarted the clock.
			h.paused += now.Sub(later(since, h.lastEvent))
			if h.silenceAlerted {
				h.lastSilenceAlert = h.lastSilenceAlert.Add(now.Sub(later(since, h.lastSilenceAlert)))
			}
		case now.Sub(since) < alertAfter || (h.alerted && now.Sub(h.lastAlert) < remindEvery):
			h.mu.Unlock()
			return
		default:
			h.mu.Unlock()
			body := fmt.Sprintf("[Slack bridge 警示] Socket Mode 連線自 %s 起撐不住(已 %s 分鐘,斷線 %d 次)。"+
				"Slack 訊息目前收不到,且 Slack 不會補送。重啟:%s",
				stamp(since), minutes(now.Sub(since)), drops, restartHint)
			// Marked sent only if delivery worked, so a failed alert is
			// retried on the next tick instead of being lost.
			if h.deliver(body, "down") {
				h.mu.Lock()
				h.alerted, h.lastAlert = true, now
				h.mu.Unlock()
			}
			return
		}
	}
	pending, since, until := h.recoverPending, h.recoverSince, h.recoverTo
	silentFor := now.Sub(h.lastEvent) - h.paused // monotonic: sleep is not silence
	silenceDue := connected && h.silenceAfter > 0 && silentFor >= h.silenceAfter &&
		(!h.silenceAlerted || now.Sub(h.lastSilenceAlert) >= silenceRemindEvery)
	lastEvent := h.lastEvent
	silencePending, silenceSince := h.silenceRecoverPending, h.silenceRecoverSince
	h.mu.Unlock()

	if silencePending && h.deliver(fmt.Sprintf("[Slack bridge 已恢復] 又收到 Slack 訊息事件了(自 %s 起中斷),"+
		"中斷期間的訊息 Slack 不會補送。", stamp(silenceSince)), "silent-recovered") {
		h.mu.Lock()
		if h.silenceRecoverSince.Equal(silenceSince) {
			h.silenceRecoverPending = false
		}
		h.mu.Unlock()
	}
	if silenceDue && h.deliver(fmt.Sprintf("[Slack bridge 警示] Slack 連線正常,但自 %s 起已 %s 小時沒收到任何訊息事件"+
		"(連你自己發的都沒有)。多半是 Slack App 端的問題:Event Subscriptions 訂閱或 App 授權被拿掉,"+
		"請到 Slack App 設定檢查;重啟 bridge 通常沒用。", stamp(lastEvent), hours(now.Round(0).Sub(lastEvent.Round(0)))), "silent") {
		h.mu.Lock()
		if h.lastEvent.Equal(lastEvent) {
			h.silenceAlerted, h.lastSilenceAlert = true, now
			h.silenceRecoverPending = false // a new silence speaks for itself
		} else { // an event arrived while sending: owe the recovered line
			h.silenceRecoverPending, h.silenceRecoverSince = true, lastEvent
		}
		h.mu.Unlock()
	}

	if pending && h.deliver(fmt.Sprintf("[Slack bridge 已恢復] Socket Mode 連線已穩定。斷線約 %s 分鐘(自 %s 起),"+
		"這段期間的 Slack 訊息 Slack 不會補送。", minutes(until.Sub(since)), stamp(since)), "recovered") {
		h.mu.Lock()
		if h.recoverSince.Equal(since) { // not superseded meanwhile
			h.recoverPending = false
		}
		h.mu.Unlock()
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
func hours(d time.Duration) string   { return fmt.Sprintf("%.0f", d.Hours()) }

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
