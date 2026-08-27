package main

// throttle.go — the brute-force brake on the credential-guessing surface:
// EVERY seam where a caller submits a secret and the server says yes or no.
//
// The authoritative list is the call sites, not this comment: grep for
// `loginThrottle.begin`. An earlier version of this line enumerated "the three
// PUBLIC/owner seams" and was already wrong when written (it omitted
// mfa/disable) and wronger later (change-password, mfa/activate) — a hardcoded
// count in prose about a set that grows, which is exactly what the root
// CLAUDE.md 〈文件鐵律〉 forbids. If you need the set, ask the compiler.
//
// WHY THIS EXISTS: before it, /api/login had no attempt limit of any kind. The
// only brake was argon2id's own ~50ms cost, which is a CPU brake, not a policy
// one. Under the shipped security model (loopback bind, exposure via a tunnel
// the owner opens themselves — docs/guide/mobile.md) that was defensible; the
// moment an owner follows our OWN mobile instructions and puts a tunnel in
// front of this, it becomes an unlimited online password-guessing oracle that
// leaves no trace. This is the policy brake.
//
// 🔴 ONE GLOBAL BUCKET, NOT PER-CLIENT — and that is a conclusion about the
// deployment, not laziness. The server binds loopback only; every request that
// is not from this machine arrives through a tunnel or reverse proxy, so
// r.RemoteAddr is 127.0.0.1 for the owner's phone, the owner's laptop and an
// attacker alike, and X-Forwarded-For is attacker-controlled text. Bucketing on
// any of those would produce a per-attacker bucket the attacker chooses — a
// counter that reads like a limit and enforces nothing. A single global bucket
// is the only one that actually holds here.
//
// THE COST, STATED PLAINLY: a global bucket means a stranger who can reach the
// login page can also delay the OWNER's login, up to the cap. That is a real
// denial of service and it is the deliberate trade — a bounded wait for the
// owner, who knows their password and gets the counter reset the instant they
// succeed, against unlimited guessing for everyone else.
//
// 🔴 AND IT IS WORSE THAN "a bounded wait", SO SAY SO. A refused attempt does
// not extend the block (see retryAfter), but nothing stops an attacker landing
// a FRESH failure the moment a block expires — so a trickle of a dozen requests
// an hour can keep the owner out for as long as the attacker keeps it up. The
// owner cannot buy their way out either: they are refused BEFORE their password
// is verified, so noteSuccess is unreachable while blocked.
//
// An earlier version of this comment claimed "the cap exists precisely so this
// can never become an indefinite lockout, and there is no admin action needed
// to clear it: it decays on its own." That was FALSE, and load-bearing — it
// would tell the next reader no escape hatch was needed. What is actually true:
//   * throttleDecay is pinned at throttleMaxDelay (see the constant), so a
//     cap-length block ALWAYS decays the history behind it. The ratchet breaks
//     at the top every time and the attacker must re-climb it, which leaves the
//     owner real windows instead of a sealed door.
//   * the counter is process-local (api_stub.go), so restarting ocserverd
//     clears it outright. That is the escape hatch, it needs host shell, and it
//     is the same trust substitution `ocserverd mfa-disable` makes.
// Both facts are owner-facing and are documented in docs/guide/mobile.md.

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	// throttleFreeFailures is how many consecutive failures cost nothing — the
	// budget for human fumbling, spent before any delay appears.
	//
	// 5 is the conventional allowance (OWASP's guidance sits at 5–10 before a
	// lockout), and it is the right number here for a specific reason: a
	// password manager's generated password mistyped by hand, plus a TOTP code
	// that expired mid-typing, is two failures that are nobody's fault. A
	// tighter budget of 3 punishes an owner having a bad minute, and buys
	// almost nothing — an attacker's cost is set by the DOUBLING and the cap
	// below, not by whether the ramp starts on the 4th attempt or the 6th.
	throttleFreeFailures = 5
	// throttleBaseDelay is the penalty on the first failure PAST the free
	// budget; each further failure doubles it.
	throttleBaseDelay = 1 * time.Second
	// throttleMaxDelay caps the penalty. 5 minutes holds a sustained attacker
	// to ~12 attempts an hour — against even a weak 8-character password that
	// is centuries, and against a 6-digit TOTP code it is ~9 years — while
	// bounding the owner's worst case at one coffee break rather than a
	// lockout only a shell can lift.
	throttleMaxDelay = 5 * time.Minute
	// throttleMaxInFlight bounds how many credential verifications may be in
	// progress at once, and it is what makes the backoff above mean anything.
	//
	// 🔴 WITHOUT IT THE BRAKE IS BYPASSABLE AND THE STATED DoS DEFENCE IS
	// FICTION. retryAfter is deliberately read-only and noteFailure lands only
	// AFTER argon2id returns, so N simultaneous requests all read "not blocked",
	// all verify, and only then record N failures: the attacker gets N guesses
	// per window instead of one, and N concurrent argon2id verifications at
	// ~19 MiB each (password.go) — a few thousand is tens of GB and the process
	// is OOM-killed by one unauthenticated burst. Measured shape: 500 concurrent
	// wrong-password POSTs used to yield ~500 verifications, not ~6.
	//
	// 4 rather than 1: a single slot would 429 the loser of a genuine two-device
	// race, and 4 still bounds memory at ~76 MiB and holds a sustained attacker
	// to 4 guesses per window — against an 8-character password that is still
	// hopeless for them.
	throttleMaxInFlight = 4
	// throttleBurstWait is the Retry-After handed to a caller refused for
	// concurrency rather than for a deadline. There is no deadline to report, and
	// the slots free in argon2id time, so it says "a moment".
	throttleBurstWait = 1 * time.Second
	// throttleDecay forgets the failure history once this long has passed since
	// the last failure. Without it the counter is a ratchet: an owner who
	// fumbled five times last Tuesday would still be paying for it today, and
	// the penalty would only ever grow over the life of the install.
	//
	// 🔴 IT IS PINNED TO throttleMaxDelay ON PURPOSE, and the coupling is a
	// security property, not a coincidence. A blocked attempt cannot extend the
	// block, so an attacker sustaining a lockout has to land a fresh failure
	// right after each block expires. With decay == the cap, the gap they just
	// waited out is BY DEFINITION long enough to decay the history, so the
	// failure they land starts from zero and arms no penalty: the ratchet
	// breaks at the top every single time. Set decay LONGER than the cap (it
	// used to be 15m vs 5m) and that same trickle keeps the owner out forever.
	//
	// Shorter blocks stay inside the window, so the ramp below still works —
	// only the cap-length block resets. TestThrottleCannotBeSustainedIndefinitely
	// pins the relationship; do not change one of these without the other.
	throttleDecay = throttleMaxDelay
)

// credentialThrottle is the failure counter behind the credential seams. Safe
// for concurrent use; the zero value is a ready, empty throttle.
type credentialThrottle struct {
	mu          sync.Mutex
	failures    int
	lastFailure time.Time
	nextAllowed time.Time
	// inFlight counts credential verifications currently running. See
	// throttleMaxInFlight — this is the half that survives concurrency.
	inFlight int
}

// penaltyFor is the delay owed after `failures` consecutive failures. Pure, so
// the schedule is testable without a clock.
func penaltyFor(failures int) time.Duration {
	over := failures - throttleFreeFailures
	if over <= 0 {
		return 0
	}
	// Shift in float space: `1<<over` overflows int64 at 63 doublings, and a
	// long-running attack WILL get there. math.Pow saturates into +Inf instead,
	// which the cap below then clamps — an attacker cannot wrap the penalty
	// around to zero.
	delay := float64(throttleBaseDelay) * math.Pow(2, float64(over-1))
	if delay >= float64(throttleMaxDelay) || math.IsInf(delay, 1) {
		return throttleMaxDelay
	}
	return time.Duration(delay)
}

// retryAfter reports how long the caller must wait, and whether it must wait at
// all. Read-only: a blocked attempt is NOT itself a failure. Counting it would
// let anyone hammering the endpoint drive the penalty to the cap and hold it
// there — turning the brake into the attacker's own lockout tool.
//
// ⚠️ INSPECTION ONLY — handlers must call begin instead. This reserves nothing,
// so on its own it is bypassed by any concurrent burst.
func (t *credentialThrottle) retryAfter(now time.Time) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.nextAllowed.IsZero() || !now.Before(t.nextAllowed) {
		return 0, false
	}
	return t.nextAllowed.Sub(now), true
}

// begin is THE gate every credential seam must call. It answers (release, wait,
// blocked): on a refusal `release` is nil and `wait` is what to put in
// Retry-After; on admission the caller MUST `defer release()`.
//
// 🔴 USE THIS, NOT retryAfter, IN A HANDLER. retryAfter only reads the
// deadline; it reserves nothing, so a burst walks straight through it (see
// throttleMaxInFlight). begin both checks the deadline AND takes an in-flight
// slot under the same lock, which is what makes the two checks atomic with
// respect to each other.
//
// release is idempotent, so a `defer` plus an early explicit call cannot
// double-free a slot and let the pool drift upward over time.
func (t *credentialThrottle) begin(now time.Time) (func(), time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.nextAllowed.IsZero() && now.Before(t.nextAllowed) {
		return nil, t.nextAllowed.Sub(now), true
	}
	if t.inFlight >= throttleMaxInFlight {
		return nil, throttleBurstWait, true
	}
	t.inFlight++
	released := false
	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if released {
			return
		}
		released = true
		t.inFlight--
	}, 0, false
}

// noteFailure records a rejected credential and arms the next penalty.
//
// Callers pass the time the failure was DETERMINED, not the time the request
// started: stamping the deadline from the request start refunds the attacker
// the argon2id time they just spent (measured ~250ms under -race), discounting
// the first 1s penalty by a quarter.
func (t *credentialThrottle) noteFailure(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.lastFailure.IsZero() && now.Sub(t.lastFailure) >= throttleDecay {
		// History forgotten (see throttleDecay). Clearing nextAllowed is NOT
		// optional: with failures back to 0, penaltyFor returns 0 and the block
		// below is skipped, so a stale future deadline would otherwise outlive
		// the history it came from and keep the caller refused with an empty
		// counter. Latent while decay > cap; LIVE now that they are equal.
		t.failures = 0
		t.nextAllowed = time.Time{}
	}
	t.failures++
	t.lastFailure = now
	if penalty := penaltyFor(t.failures); penalty > 0 {
		t.nextAllowed = now.Add(penalty)
	}
}

// noteSuccess clears the history. Proving you hold the credential ends the
// suspicion — this is what keeps a global bucket from being an owner lockout.
func (t *credentialThrottle) noteSuccess() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failures = 0
	t.lastFailure = time.Time{}
	t.nextAllowed = time.Time{}
}

// writeThrottled answers a rate-limited credential attempt: 429 with a
// Retry-After header (HTTP's own vocabulary for this, so a client does not have
// to parse prose) through the SAME error envelope every other refusal uses.
//
// The message states the wait and stops there — no hint about whether the
// submitted secret was close, and no suggestion of another endpoint to try.
// 429 maps to `client_error` through the existing errorCodeForStatus fallback,
// so the closed envelope-code vocabulary
// (docs/design/api-error-envelope.codes.json) does not move for this.
func writeThrottled(w http.ResponseWriter, wait time.Duration) {
	secs := int64(math.Ceil(wait.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
	writeError(w, http.StatusTooManyRequests,
		"too many failed credential attempts; retry in "+strconv.FormatInt(secs, 10)+"s")
}
