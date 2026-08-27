package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// base is a fixed instant; every test drives the clock explicitly so none of
// this depends on wall time.
var throttleBase = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

// TestPenaltyForIsFreeThenDoublesThenCaps pins the whole schedule in one table.
// The free allowance is what keeps an owner's typo from costing anything; the
// doubling is what makes sustained guessing pointless; the cap is what stops it
// from becoming a lockout.
func TestPenaltyForIsFreeThenDoublesThenCaps(t *testing.T) {
	for _, tc := range []struct {
		failures int
		want     time.Duration
	}{
		{0, 0},
		{1, 0},
		{throttleFreeFailures, 0}, // the last free one
		{throttleFreeFailures + 1, throttleBaseDelay},
		{throttleFreeFailures + 2, 2 * throttleBaseDelay},
		{throttleFreeFailures + 3, 4 * throttleBaseDelay},
		{throttleFreeFailures + 4, 8 * throttleBaseDelay},
		{100, throttleMaxDelay},
		{10000, throttleMaxDelay}, // must saturate, never wrap
	} {
		if got := penaltyFor(tc.failures); got != tc.want {
			t.Errorf("penaltyFor(%d) = %v, want %v", tc.failures, got, tc.want)
		}
	}
}

// TestPenaltyForNeverWrapsToZero is the overflow guard. A naive `1 << over`
// wraps at 63 doublings and hands an attacker a penalty of zero — exactly at
// the point the attack has been running longest.
func TestPenaltyForNeverWrapsToZero(t *testing.T) {
	for _, failures := range []int{62, 63, 64, 65, 1 << 20} {
		if got := penaltyFor(failures); got != throttleMaxDelay {
			t.Errorf("penaltyFor(%d) = %v, want the cap %v", failures, got, throttleMaxDelay)
		}
	}
}

func TestThrottleAllowsTheFreeAllowanceWithoutDelay(t *testing.T) {
	var th credentialThrottle
	for i := 0; i < throttleFreeFailures; i++ {
		th.noteFailure(throttleBase)
		if _, blocked := th.retryAfter(throttleBase); blocked {
			t.Fatalf("blocked after only %d failure(s) — the free allowance is %d",
				i+1, throttleFreeFailures)
		}
	}
	// One past the allowance must bite.
	th.noteFailure(throttleBase)
	wait, blocked := th.retryAfter(throttleBase)
	if !blocked {
		t.Fatal("not blocked after exceeding the free allowance")
	}
	if wait != throttleBaseDelay {
		t.Errorf("first penalty = %v, want %v", wait, throttleBaseDelay)
	}
}

func TestThrottleUnblocksWhenTheWaitElapses(t *testing.T) {
	var th credentialThrottle
	for i := 0; i < throttleFreeFailures+1; i++ {
		th.noteFailure(throttleBase)
	}
	if _, blocked := th.retryAfter(throttleBase); !blocked {
		t.Fatal("expected a block")
	}
	if _, blocked := th.retryAfter(throttleBase.Add(throttleBaseDelay)); blocked {
		t.Error("still blocked exactly at the deadline; the wait should be over")
	}
}

// TestThrottleBlockedAttemptsDoNotDeepenThePenalty is the anti-amplification
// property. If a refused attempt counted as a failure, anyone hammering the
// endpoint could drive the penalty to the cap and pin it there — handing the
// attacker a lockout tool against the owner.
func TestThrottleBlockedAttemptsDoNotDeepenThePenalty(t *testing.T) {
	var th credentialThrottle
	for i := 0; i < throttleFreeFailures+1; i++ {
		th.noteFailure(throttleBase)
	}
	first, blocked := th.retryAfter(throttleBase)
	if !blocked {
		t.Fatal("expected a block")
	}
	for i := 0; i < 50; i++ {
		th.retryAfter(throttleBase)
	}
	again, _ := th.retryAfter(throttleBase)
	if again != first {
		t.Errorf("penalty moved from %v to %v just from reading it", first, again)
	}
}

// TestThrottleSuccessClearsHistory — this is what keeps one global bucket from
// being an owner lockout: proving you hold the credential ends the suspicion.
func TestThrottleSuccessClearsHistory(t *testing.T) {
	var th credentialThrottle
	for i := 0; i < throttleFreeFailures+4; i++ {
		th.noteFailure(throttleBase)
	}
	if _, blocked := th.retryAfter(throttleBase); !blocked {
		t.Fatal("expected a block before the success")
	}

	th.noteSuccess()

	if _, blocked := th.retryAfter(throttleBase); blocked {
		t.Fatal("still blocked after a successful authentication")
	}
	// And the NEXT failure must start from the free allowance again, not from
	// the old depth.
	th.noteFailure(throttleBase)
	if _, blocked := th.retryAfter(throttleBase); blocked {
		t.Error("the first failure after a success was penalised; history was not cleared")
	}
}

// TestThrottleDecaysAfterQuiet stops the counter being a ratchet that an owner
// can never pay off.
func TestThrottleDecaysAfterQuiet(t *testing.T) {
	var th credentialThrottle
	for i := 0; i < throttleFreeFailures+4; i++ {
		th.noteFailure(throttleBase)
	}
	deep, _ := th.retryAfter(throttleBase)

	// One more failure, but long after the quiet period.
	later := throttleBase.Add(throttleDecay + time.Minute)
	th.noteFailure(later)
	if _, blocked := th.retryAfter(later); blocked {
		t.Fatalf("a single failure after %v of quiet was still penalised (previous depth %v)",
			throttleDecay, deep)
	}
}

func TestThrottleZeroValueIsUnblocked(t *testing.T) {
	var th credentialThrottle
	if _, blocked := th.retryAfter(throttleBase); blocked {
		t.Fatal("a fresh throttle must not block")
	}
}

// TestWriteThrottledShape pins the wire face: 429, a Retry-After the client can
// act on without parsing prose, and the SAME error envelope as every other
// refusal (code derived from the status, so the closed vocabulary is unchanged).
func TestWriteThrottledShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeThrottled(rec, 42*time.Second)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if got := rec.Header().Get("Retry-After"); got != "42" {
		t.Errorf("Retry-After = %q, want %q", got, "42")
	}
	body := decodeBody[struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}](t, rec)
	if want := errorCodeForStatus(http.StatusTooManyRequests); body.Error.Code != want {
		t.Errorf("envelope code = %q, want %q (the status-derived code)", body.Error.Code, want)
	}
	if body.Error.Message == "" {
		t.Error("envelope message is empty")
	}
}

// TestWriteThrottledRoundsUpAndFloorsAtOne — a sub-second wait must never
// render as "Retry-After: 0", which a client reads as "go ahead now".
func TestWriteThrottledRoundsUpAndFloorsAtOne(t *testing.T) {
	for _, tc := range []struct {
		wait time.Duration
		want string
	}{
		{0, "1"},
		{1 * time.Millisecond, "1"},
		{1 * time.Second, "1"},
		{1500 * time.Millisecond, "2"},
		{throttleMaxDelay, strconv.Itoa(int(throttleMaxDelay.Seconds()))},
	} {
		rec := httptest.NewRecorder()
		writeThrottled(rec, tc.wait)
		if got := rec.Header().Get("Retry-After"); got != tc.want {
			t.Errorf("wait %v → Retry-After %q, want %q", tc.wait, got, tc.want)
		}
	}
}
