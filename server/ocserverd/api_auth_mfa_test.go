package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// api_auth_mfa_test.go — the owner's second factor and the credential-attempt
// brake, asserted at the HANDLER seam (the wire shape callers actually see).
//
// 🔴 WHAT THESE TESTS EXIST TO CATCH. Before this change /api/login had no
// attempt limit at all and no second factor, so the two defects worth guarding
// against are both silent: a code that can be replayed inside its acceptance
// window (nothing errors — the same six digits simply work twice), and a
// throttle that counts refused attempts as failures (nothing errors — the owner
// is simply locked out by a stranger). Each has a named test below.

const mfaTestPassword = "correct-horse-battery"

// mfaAPI builds a real apiServer on a temp DB with the owner password set.
func mfaAPI(t *testing.T) *apiServer {
	t.Helper()
	db, err := openSQLite(filepath.Join(t.TempDir(), "mfa-test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := runMigrations(db); err != nil {
		t.Fatalf("goose up: %v", err)
	}
	dal := NewDAL(db)
	if err := seedOutOfBox(dal); err != nil {
		t.Fatalf("seed: %v", err)
	}
	api := newAPIServer(dal, NewHub(), []byte(interopSecret), 3600, "../..")
	phc, err := hashPassword(mfaTestPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := dal.PutSetting(settingPasswordHash, phc); err != nil {
		t.Fatalf("store hash: %v", err)
	}
	api.passwordHash = phc
	return api
}

// callJSON invokes a handler directly with a JSON body.
func callJSON(h func(http.ResponseWriter, *http.Request), body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func loginBody(password, code string) string {
	if code == "" {
		return fmt.Sprintf(`{"password":%q}`, password)
	}
	return fmt.Sprintf(`{"password":%q,"code":%q}`, password, code)
}

// offerMFA turns the ship-dark feature flag on — the precondition for enrolling.
// Everything about VERIFICATION is deliberately independent of it, which is what
// TestMFAOfferedFlagNeverDisarmsALiveFactor exists to prove.
func offerMFA(t *testing.T, api *apiServer, on bool) {
	t.Helper()
	rec := callJSON(api.HandleMfaOfferApiAuthMfaOfferPost,
		fmt.Sprintf(`{"offered":%t}`, on))
	if rec.Code != http.StatusOK {
		t.Fatalf("offer(%t): %d %s", on, rec.Code, rec.Body.String())
	}
	if decodeBody[mfaStateDTO](t, rec).Offered != on {
		t.Fatalf("offer(%t) did not stick", on)
	}
}

// armMFA enrols and activates a factor, returning the active secret.
func armMFA(t *testing.T, api *apiServer) string {
	t.Helper()
	offerMFA(t, api, true)
	rec := callJSON(api.HandleMfaEnrollApiAuthMfaEnrollPost, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll: %d %s", rec.Code, rec.Body.String())
	}
	state := decodeBody[mfaStateDTO](t, rec)
	if state.Enrolled {
		t.Fatal("enroll must NOT arm the factor by itself")
	}
	if state.Secret == nil || *state.Secret == "" {
		t.Fatal("enroll returned no secret")
	}
	secret := *state.Secret

	key, err := decodeTOTPSecret(secret)
	if err != nil {
		t.Fatalf("decode enrolled secret: %v", err)
	}
	code := totpCodeAt(key, time.Now().Unix()/totpStepSecs)
	rec = callJSON(api.HandleMfaActivateApiAuthMfaActivatePost,
		fmt.Sprintf(`{"password":%q,"code":%q}`, mfaTestPassword, code))
	if rec.Code != http.StatusOK {
		t.Fatalf("activate: %d %s", rec.Code, rec.Body.String())
	}
	if !decodeBody[mfaStateDTO](t, rec).Enrolled {
		t.Fatal("activate did not report the factor armed")
	}
	return secret
}

// liveCode generates the code an authenticator would show right now.
func liveCode(t *testing.T, secret string) string {
	t.Helper()
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		t.Fatalf("decode secret: %v", err)
	}
	return totpCodeAt(key, time.Now().Unix()/totpStepSecs)
}

// nextCode is the code an authenticator will show on the NEXT 30-second tick.
//
// 🔴 LOGIN TESTS MUST USE THIS, NOT liveCode, and the reason is a real product
// behaviour rather than a test workaround: activation SPENDS the step it proved
// (the replay floor moves to it), precisely so the activation code cannot double
// as the first login. A real owner just waits for the next tick; a test cannot
// afford to sleep 30 seconds, and +1 is inside the accepted skew window, so this
// is the same credential the phone would show — one tick early.
func nextCode(t *testing.T, secret string) string {
	t.Helper()
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		t.Fatalf("decode secret: %v", err)
	}
	return totpCodeAt(key, time.Now().Unix()/totpStepSecs+1)
}

// ── the control: nothing changes for an install that never turns MFA on ──────

func TestLoginWithoutMFAStillWorksAndIgnoresACode(t *testing.T) {
	api := mfaAPI(t)

	if rec := callJSON(api.HandleLoginApiLoginPost, loginBody(mfaTestPassword, "")); rec.Code != http.StatusOK {
		t.Fatalf("plain login: %d %s", rec.Code, rec.Body.String())
	}
	// A client that sends a code to a server with no enrolment must not be
	// punished for it — the wire contract says the field is ignored.
	if rec := callJSON(api.HandleLoginApiLoginPost, loginBody(mfaTestPassword, "123456")); rec.Code != http.StatusOK {
		t.Fatalf("login with a stray code: %d %s", rec.Code, rec.Body.String())
	}
	if rec := callJSON(api.HandleLoginApiLoginPost, loginBody("wrong", "")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d, want 401", rec.Code)
	}
}

// ── the enrol ceremony ──────────────────────────────────────────────────────

func TestMFAEnrolThenActivateArmsTheFactor(t *testing.T) {
	api := mfaAPI(t)
	if api.authMFAEnrolled() {
		t.Fatal("a fresh install must not have a factor armed")
	}
	secret := armMFA(t, api)
	if !api.authMFAEnrolled() {
		t.Fatal("factor not armed after activate")
	}
	// The armed secret must survive a reload from the DB — otherwise MFA
	// silently switches itself off on the next restart.
	reloaded, err := loadAuthSettings(api.dal, Config{}, func(string) {})
	if err != nil {
		t.Fatalf("reload settings: %v", err)
	}
	if reloaded.totpSecret != secret {
		t.Errorf("reloaded secret = %q, want the activated one", reloaded.totpSecret)
	}
}

func TestMFAEnrollRefusedWhileAFactorIsActive(t *testing.T) {
	api := mfaAPI(t)
	armMFA(t, api)
	rec := callJSON(api.HandleMfaEnrollApiAuthMfaEnrollPost, "")
	if rec.Code != http.StatusConflict {
		t.Errorf("enroll over an active factor = %d, want 409 (rotation must disarm first)", rec.Code)
	}
}

func TestMFAActivateWithoutPendingIsAConflict(t *testing.T) {
	api := mfaAPI(t)
	offerMFA(t, api, true)
	rec := callJSON(api.HandleMfaActivateApiAuthMfaActivatePost,
		fmt.Sprintf(`{"password":%q,"code":"123456"}`, mfaTestPassword))
	if rec.Code != http.StatusConflict {
		t.Errorf("activate with nothing pending = %d, want 409", rec.Code)
	}
}

// TestMFAActivateWrongCodeKeepsThePendingSecret — a typo must not force a fresh
// QR scan, or owners learn to abandon the ceremony half-done.
func TestMFAActivateWrongCodeKeepsThePendingSecret(t *testing.T) {
	api := mfaAPI(t)
	offerMFA(t, api, true)
	rec := callJSON(api.HandleMfaEnrollApiAuthMfaEnrollPost, "")
	secret := *decodeBody[mfaStateDTO](t, rec).Secret

	if rec := callJSON(api.HandleMfaActivateApiAuthMfaActivatePost,
		fmt.Sprintf(`{"password":%q,"code":"000000"}`, mfaTestPassword)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong activation code = %d, want 401", rec.Code)
	}
	if api.authMFAEnrolled() {
		t.Fatal("a wrong code armed the factor")
	}
	// The same pending secret must still activate.
	rec = callJSON(api.HandleMfaActivateApiAuthMfaActivatePost,
		fmt.Sprintf(`{"password":%q,"code":%q}`, mfaTestPassword, liveCode(t, secret)))
	if rec.Code != http.StatusOK {
		t.Fatalf("retry after a typo = %d %s", rec.Code, rec.Body.String())
	}
}

// ── login with the factor armed ─────────────────────────────────────────────

func TestLoginWithMFARequiresACorrectCode(t *testing.T) {
	api := mfaAPI(t)
	secret := armMFA(t, api)

	for _, tc := range []struct{ name, password, code string }{
		{"no code", mfaTestPassword, ""},
		{"wrong code", mfaTestPassword, "000000"},
		{"right code, wrong password", "wrong", nextCode(t, secret)},
	} {
		rec := callJSON(api.HandleLoginApiLoginPost, loginBody(tc.password, tc.code))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", tc.name, rec.Code)
		}
		api.loginThrottle.noteSuccess() // isolate each case from the brake
	}

	rec := callJSON(api.HandleLoginApiLoginPost, loginBody(mfaTestPassword, nextCode(t, secret)))
	if rec.Code != http.StatusOK {
		t.Fatalf("password + live code = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if decodeBody[tokenDTO](t, rec).Token == "" {
		t.Error("no token minted on a successful two-factor login")
	}
}

// TestLoginRefusalDoesNotDiscloseWhichFactorFailed is a non-disclosure property,
// not a cosmetic one: a distinguishable refusal confirms a correct password to
// someone who has guessed only that half.
func TestLoginRefusalDoesNotDiscloseWhichFactorFailed(t *testing.T) {
	api := mfaAPI(t)
	secret := armMFA(t, api)

	wrongPassword := callJSON(api.HandleLoginApiLoginPost, loginBody("nope", nextCode(t, secret)))
	api.loginThrottle.noteSuccess()
	wrongCode := callJSON(api.HandleLoginApiLoginPost, loginBody(mfaTestPassword, "000000"))

	if wrongPassword.Code != wrongCode.Code {
		t.Errorf("statuses differ: password %d vs code %d", wrongPassword.Code, wrongCode.Code)
	}
	if wrongPassword.Body.String() != wrongCode.Body.String() {
		t.Errorf("bodies differ — the refusal names which factor failed:\n password: %s\n code:     %s",
			wrongPassword.Body.String(), wrongCode.Body.String())
	}
}

// TestLoginRefusesAReplayedCode is THE replay guard. A TOTP code stays
// cryptographically valid for the whole acceptance window, so nothing but the
// persisted floor makes it single-use — and a regression here is completely
// silent.
func TestLoginRefusesAReplayedCode(t *testing.T) {
	api := mfaAPI(t)
	secret := armMFA(t, api)
	code := nextCode(t, secret)

	if rec := callJSON(api.HandleLoginApiLoginPost, loginBody(mfaTestPassword, code)); rec.Code != http.StatusOK {
		t.Fatalf("first use = %d %s", rec.Code, rec.Body.String())
	}
	rec := callJSON(api.HandleLoginApiLoginPost, loginBody(mfaTestPassword, code))
	if rec.Code == http.StatusOK {
		t.Fatal("the SAME code logged in twice — replay is open")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("replay = %d, want 401", rec.Code)
	}
}

// TestActivationCodeCannotBeReusedAsTheFirstLogin pins a deliberate
// consequence of activate spending the step it proved: the six digits that
// armed the factor are already burnt, so they cannot also open the first
// session. An owner never notices (activate does not log them out, and the next
// tick is 30 seconds away) — but a regression here would mean the activation
// code stays live in anyone's scrollback for the rest of its window.
func TestActivationCodeCannotBeReusedAsTheFirstLogin(t *testing.T) {
	api := mfaAPI(t)
	offerMFA(t, api, true)
	rec := callJSON(api.HandleMfaEnrollApiAuthMfaEnrollPost, "")
	secret := *decodeBody[mfaStateDTO](t, rec).Secret

	activationCode := liveCode(t, secret)
	if rec := callJSON(api.HandleMfaActivateApiAuthMfaActivatePost,
		fmt.Sprintf(`{"password":%q,"code":%q}`, mfaTestPassword, activationCode)); rec.Code != http.StatusOK {
		t.Fatalf("activate: %d %s", rec.Code, rec.Body.String())
	}
	if rec := callJSON(api.HandleLoginApiLoginPost, loginBody(mfaTestPassword, activationCode)); rec.Code == http.StatusOK {
		t.Fatal("the activation code also logged in — it was not spent")
	}
	// The next tick's code still works, so this is a spent STEP, not a broken secret.
	if rec := callJSON(api.HandleLoginApiLoginPost, loginBody(mfaTestPassword, nextCode(t, secret))); rec.Code != http.StatusOK {
		t.Errorf("the next code was refused too = %d; the floor over-collected", rec.Code)
	}
}

// TestReplayFloorSurvivesAReload — the floor is durable, so a restart must not
// reopen the window on a code already spent.
func TestReplayFloorSurvivesAReload(t *testing.T) {
	api := mfaAPI(t)
	secret := armMFA(t, api)
	code := nextCode(t, secret)
	if rec := callJSON(api.HandleLoginApiLoginPost, loginBody(mfaTestPassword, code)); rec.Code != http.StatusOK {
		t.Fatalf("first use: %d", rec.Code)
	}

	reloaded, err := loadAuthSettings(api.dal, Config{}, func(string) {})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := totpVerify(reloaded.totpSecret, code, time.Now().Unix(), reloaded.totpLastStep); ok {
		t.Fatal("a spent code verifies again after a reload — the floor is not durable")
	}
}

// ── disarming ───────────────────────────────────────────────────────────────

func TestMFADisableRequiresBothFactors(t *testing.T) {
	api := mfaAPI(t)
	secret := armMFA(t, api)

	for _, tc := range []struct{ name, body string }{
		{"wrong password", fmt.Sprintf(`{"password":"nope","code":%q}`, nextCode(t, secret))},
		{"wrong code", fmt.Sprintf(`{"password":%q,"code":"000000"}`, mfaTestPassword)},
	} {
		rec := callJSON(api.HandleMfaDisableApiAuthMfaDisablePost, tc.body)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", tc.name, rec.Code)
		}
		if !api.authMFAEnrolled() {
			t.Fatalf("%s: the factor was disarmed anyway", tc.name)
		}
		api.loginThrottle.noteSuccess()
	}

	rec := callJSON(api.HandleMfaDisableApiAuthMfaDisablePost,
		fmt.Sprintf(`{"password":%q,"code":%q}`, mfaTestPassword, nextCode(t, secret)))
	if rec.Code != http.StatusOK {
		t.Fatalf("valid disable = %d %s", rec.Code, rec.Body.String())
	}
	if api.authMFAEnrolled() {
		t.Fatal("factor still armed after a valid disable")
	}
	// And login goes back to password-only.
	if rec := callJSON(api.HandleLoginApiLoginPost, loginBody(mfaTestPassword, "")); rec.Code != http.StatusOK {
		t.Errorf("password-only login after disable = %d", rec.Code)
	}
}

func TestMFADisableWithoutAnActiveFactorIsAConflict(t *testing.T) {
	api := mfaAPI(t)
	rec := callJSON(api.HandleMfaDisableApiAuthMfaDisablePost,
		fmt.Sprintf(`{"password":%q,"code":"123456"}`, mfaTestPassword))
	if rec.Code != http.StatusConflict {
		t.Errorf("disable with nothing armed = %d, want 409", rec.Code)
	}
}

// TestMFADisableClearsEveryStoredKey — a leftover secret or floor would let a
// re-enrolment inherit state from the old one.
func TestMFADisableClearsEveryStoredKey(t *testing.T) {
	api := mfaAPI(t)
	secret := armMFA(t, api)
	rec := callJSON(api.HandleMfaDisableApiAuthMfaDisablePost,
		fmt.Sprintf(`{"password":%q,"code":%q}`, mfaTestPassword, nextCode(t, secret)))
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body.String())
	}
	for _, key := range []string{settingTOTPSecret, settingTOTPPendingSecret, settingTOTPLastStep} {
		got, err := api.dal.GetSetting(key)
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		if got != nil {
			t.Errorf("%s survived the disable: %q", key, *got)
		}
	}
}

// ── the credential-attempt brake, through the login handler ─────────────────

func TestLoginThrottleEventuallyRefusesWith429AndRetryAfter(t *testing.T) {
	api := mfaAPI(t)

	// Spend the free allowance.
	for i := 0; i < throttleFreeFailures; i++ {
		if rec := callJSON(api.HandleLoginApiLoginPost, loginBody("wrong", "")); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d, want 401 (still inside the free allowance)", i+1, rec.Code)
		}
	}
	// One more failure arms the penalty...
	if rec := callJSON(api.HandleLoginApiLoginPost, loginBody("wrong", "")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the allowance-exceeding failure = %d, want 401", rec.Code)
	}
	// ...and the NEXT attempt is refused before any password check.
	rec := callJSON(api.HandleLoginApiLoginPost, loginBody("wrong", ""))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("throttled attempt = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 without a Retry-After header")
	}
}

// TestLoginThrottleRefusesTheCORRECTPasswordToo — the brake must gate on the
// attempt, not on the answer. A throttle that lets a correct password through
// is an oracle: it tells an attacker exactly when they have guessed right.
func TestLoginThrottleRefusesTheCorrectPasswordToo(t *testing.T) {
	api := mfaAPI(t)
	for i := 0; i < throttleFreeFailures+1; i++ {
		callJSON(api.HandleLoginApiLoginPost, loginBody("wrong", ""))
	}
	rec := callJSON(api.HandleLoginApiLoginPost, loginBody(mfaTestPassword, ""))
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("correct password while throttled = %d, want 429 (no oracle)", rec.Code)
	}
}

// TestLoginThrottleClearsOnSuccess — the owner must be able to pay off the
// counter simply by getting it right, which is what makes one global bucket
// tolerable.
func TestLoginThrottleClearsOnSuccess(t *testing.T) {
	api := mfaAPI(t)
	for i := 0; i < throttleFreeFailures; i++ {
		callJSON(api.HandleLoginApiLoginPost, loginBody("wrong", ""))
	}
	if rec := callJSON(api.HandleLoginApiLoginPost, loginBody(mfaTestPassword, "")); rec.Code != http.StatusOK {
		t.Fatalf("login inside the allowance = %d", rec.Code)
	}
	// A fresh run of failures must start from the full allowance again.
	for i := 0; i < throttleFreeFailures; i++ {
		if rec := callJSON(api.HandleLoginApiLoginPost, loginBody("wrong", "")); rec.Code != http.StatusUnauthorized {
			t.Fatalf("post-success failure %d = %d, want 401", i+1, rec.Code)
		}
	}
}

// TestChangePasswordIsThrottled — the ONE seam where a stolen owner token can
// guess the real password with the second factor standing aside (this endpoint
// deliberately demands no code). A successful guess is not a read but a
// takeover: rotating the password stamps password_changed_at, which revokes the
// legitimate owner's own tokens.
func TestChangePasswordIsThrottled(t *testing.T) {
	api := mfaAPI(t)
	body := `{"current_password":"guess","new_password":"a-long-enough-new-one"}`

	for i := 0; i < throttleFreeFailures; i++ {
		if rec := callJSON(api.HandleChangePasswordApiAuthChangePasswordPost, body); rec.Code != http.StatusUnauthorized {
			t.Fatalf("guess %d = %d, want 401 (inside the free allowance)", i+1, rec.Code)
		}
	}
	if rec := callJSON(api.HandleChangePasswordApiAuthChangePasswordPost, body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the allowance-exceeding guess = %d, want 401", rec.Code)
	}
	if rec := callJSON(api.HandleChangePasswordApiAuthChangePasswordPost, body); rec.Code != http.StatusTooManyRequests {
		t.Errorf("throttled password guess = %d, want 429", rec.Code)
	}
}

// TestChangePasswordShortNewPasswordStaysA422 — the throttle must sit AFTER the
// shape check. This is the exact ordering bug that turned set-password's
// documented 409 into a 429 (caught by conformance, not by a unit test).
func TestChangePasswordShortNewPasswordStaysA422(t *testing.T) {
	api := mfaAPI(t)
	// Drive the throttle well past its allowance first.
	for i := 0; i < throttleFreeFailures+2; i++ {
		callJSON(api.HandleChangePasswordApiAuthChangePasswordPost,
			`{"current_password":"guess","new_password":"a-long-enough-new-one"}`)
	}
	rec := callJSON(api.HandleChangePasswordApiAuthChangePasswordPost,
		`{"current_password":"whatever","new_password":"short"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("short new_password while throttled = %d, want 422 — a malformed "+
			"request is not a credential guess and must not be masked by the brake", rec.Code)
	}
}

// TestSetPasswordIsThrottledToo — the first-run claim token is a 32-byte secret
// submitted by an unauthenticated caller, i.e. the same class of guessing target
// as the password.
func TestSetPasswordIsThrottledToo(t *testing.T) {
	api := mfaAPI(t)
	// Reset to the pre-password state and plant a claim token.
	if err := api.dal.DeleteSetting(settingPasswordHash); err != nil {
		t.Fatalf("clear hash: %v", err)
	}
	api.passwordHash = ""
	if err := api.dal.PutSetting(settingClaimToken, "the-real-claim-token"); err != nil {
		t.Fatalf("plant claim token: %v", err)
	}

	body := `{"password":"long-enough-pw","claim_token":"guess"}`
	for i := 0; i < throttleFreeFailures+1; i++ {
		if rec := callJSON(api.HandleSetPasswordApiAuthSetPasswordPost, body); rec.Code != http.StatusUnauthorized {
			t.Fatalf("claim-token guess %d = %d, want 401", i+1, rec.Code)
		}
	}
	if rec := callJSON(api.HandleSetPasswordApiAuthSetPasswordPost, body); rec.Code != http.StatusTooManyRequests {
		t.Errorf("throttled claim-token guess = %d, want 429", rec.Code)
	}
}

// ── the pre-auth probe and the cockpit's read-only view ─────────────────────

func TestAuthStatusPublishesMFARequired(t *testing.T) {
	api := mfaAPI(t)

	req := httptest.NewRequest("GET", "/api/auth/status", nil)
	rec := httptest.NewRecorder()
	api.HandleAuthStatusApiAuthStatusGet(rec, req)
	before := decodeBody[authStatusDTO](t, rec)
	if !before.PasswordSet {
		t.Error("password_set should be true")
	}
	if before.MFARequired {
		t.Error("mfa_required should be false before enrolment")
	}

	armMFA(t, api)

	rec = httptest.NewRecorder()
	api.HandleAuthStatusApiAuthStatusGet(rec, httptest.NewRequest("GET", "/api/auth/status", nil))
	if !decodeBody[authStatusDTO](t, rec).MFARequired {
		t.Error("mfa_required should be true once the factor is armed")
	}
	// The probe is PUBLIC and must never leak the secret itself.
	if strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("auth status body mentions a secret: %s", rec.Body.String())
	}
}

// TestActiveSecretIsNeverEchoedBack — enroll is the ONE moment a secret crosses
// the wire. If activate or disable echoed it, a stolen owner token could read
// out an existing enrolment and clone the factor.
func TestActiveSecretIsNeverEchoedBack(t *testing.T) {
	api := mfaAPI(t)
	offerMFA(t, api, true)
	rec := callJSON(api.HandleMfaEnrollApiAuthMfaEnrollPost, "")
	secret := *decodeBody[mfaStateDTO](t, rec).Secret

	rec = callJSON(api.HandleMfaActivateApiAuthMfaActivatePost,
		fmt.Sprintf(`{"password":%q,"code":%q}`, mfaTestPassword, liveCode(t, secret)))
	if strings.Contains(rec.Body.String(), secret) {
		t.Errorf("activate echoed the secret: %s", rec.Body.String())
	}
	state := decodeBody[mfaStateDTO](t, rec)
	if state.Secret != nil || state.OtpauthURI != nil {
		t.Error("activate must answer null for secret/otpauth_uri")
	}

	rec = callJSON(api.HandleMfaDisableApiAuthMfaDisablePost,
		fmt.Sprintf(`{"password":%q,"code":%q}`, mfaTestPassword, nextCode(t, secret)))
	if strings.Contains(rec.Body.String(), secret) {
		t.Errorf("disable echoed the secret: %s", rec.Body.String())
	}
}

// TestCorruptStoredSecretIsABootError — booting with MFA silently OFF because a
// row got mangled is the one outcome an owner would never notice until it
// mattered.
func TestCorruptStoredSecretIsABootError(t *testing.T) {
	api := mfaAPI(t)
	if err := api.dal.PutSetting(settingTOTPSecret, "not-valid-base32-!!!"); err != nil {
		t.Fatalf("plant: %v", err)
	}
	if _, err := loadAuthSettings(api.dal, Config{}, func(string) {}); err == nil {
		t.Fatal("a corrupt TOTP secret booted cleanly — MFA would be silently off")
	}
}

// ── H1/H2/H3 and the atomicity invariant: the policy layer, under concurrency ──

// TestVerifyAndSpendTOTPIsAtomicUnderConcurrency is the test the change's most
// emphasised invariant went without.
//
// 🔴 WHY IT HAD TO BE WRITTEN, and why -race is not a substitute. Splitting
// verifyAndSpendTOTP into "RLock read the secret+floor / verify unlocked / Lock
// write the floor" — exactly the shape its own comments forbid — left the whole
// 75-test suite GREEN, and passes `go test -race` too, because the defect is a
// logic race on the floor VALUE, not a data race on memory. Nothing but a
// concurrent test can see it.
//
// The property: N goroutines presenting the SAME code must yield exactly ONE
// success. A code is single-use only because the floor advances inside the same
// critical section that verified it.
func TestVerifyAndSpendTOTPIsAtomicUnderConcurrency(t *testing.T) {
	api := mfaAPI(t)
	secret := armMFA(t, api)
	code := nextCode(t, secret)
	now := time.Now().Unix()

	const goroutines = 32
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	results := make([]bool, goroutines)
	for i := 0; i < goroutines; i++ {
		done.Add(1)
		go func(idx int) {
			defer done.Done()
			start.Wait() // release them all at once, to actually contend
			ok, err := api.verifyAndSpendTOTP(code, now)
			if err != nil {
				t.Errorf("goroutine %d: %v", idx, err)
			}
			results[idx] = ok
		}(i)
	}
	start.Done()
	done.Wait()

	accepted := 0
	for _, ok := range results {
		if ok {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("%d of %d concurrent uses of ONE code were accepted, want exactly 1 — "+
			"the verify-and-spend critical section is not atomic, so a code is replayable",
			accepted, goroutines)
	}
}

// TestThrottleBeginReservesUnderConcurrency — H1. The gate must admit at most
// throttleMaxInFlight callers at once. retryAfter alone reserved nothing, so a
// burst walked straight through it: N guesses per window instead of one, and N
// concurrent argon2id verifications at ~19 MiB each.
func TestThrottleBeginReservesUnderConcurrency(t *testing.T) {
	var th credentialThrottle
	const goroutines = 200

	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	var mu sync.Mutex
	admitted := 0
	releases := []func(){}
	for i := 0; i < goroutines; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			release, _, blocked := th.begin(throttleBase)
			if blocked {
				return
			}
			mu.Lock()
			admitted++
			releases = append(releases, release)
			mu.Unlock()
		}()
	}
	start.Done()
	done.Wait()

	// Nothing released yet, so the pool must be exactly full — never more.
	if admitted != throttleMaxInFlight {
		t.Fatalf("%d of %d concurrent callers admitted, want exactly %d — the gate "+
			"is not reserving, so a burst bypasses the backoff entirely",
			admitted, goroutines, throttleMaxInFlight)
	}
	// Releasing frees the slots again.
	for _, r := range releases {
		r()
	}
	if _, _, blocked := th.begin(throttleBase); blocked {
		t.Error("still blocked after every slot was released — releases leak")
	}
}

// TestThrottleReleaseIsIdempotent — a defer plus an explicit call must not
// double-free, or the pool drifts upward and the cap silently stops holding.
func TestThrottleReleaseIsIdempotent(t *testing.T) {
	var th credentialThrottle
	release, _, blocked := th.begin(throttleBase)
	if blocked {
		t.Fatal("fresh throttle blocked")
	}
	for i := 0; i < 5; i++ {
		release()
	}
	// Exactly the pool size must be available — not more.
	got := 0
	for i := 0; i < throttleMaxInFlight+3; i++ {
		if _, _, b := th.begin(throttleBase); !b {
			got++
		}
	}
	if got != throttleMaxInFlight {
		t.Errorf("after %d releases of ONE slot the pool admitted %d, want %d", 5, got, throttleMaxInFlight)
	}
}

// TestThrottleCannotBeSustainedIndefinitely — H2, and the reason
// throttleDecay is pinned to throttleMaxDelay.
//
// A refused attempt cannot extend a block, so an attacker sustaining a lockout
// must land a FRESH failure right after each block expires. With decay == the
// cap, the gap they just waited out is by definition long enough to decay the
// history, so that failure starts from zero and arms nothing. If decay is ever
// set longer than the cap (it was 15m vs 5m), the same trickle keeps the owner
// out forever and the owner can never reach noteSuccess to clear it.
func TestThrottleCannotBeSustainedIndefinitely(t *testing.T) {
	if throttleDecay > throttleMaxDelay {
		t.Fatalf("throttleDecay (%v) > throttleMaxDelay (%v): a trickle of failures "+
			"one-per-block keeps the owner locked out indefinitely", throttleDecay, throttleMaxDelay)
	}
	var th credentialThrottle
	now := throttleBase
	// Climb the ramp exactly the way a sustaining attacker must: fail, wait the
	// block out, fail again. Stop the moment a CAP-length block is armed — the
	// assertion below is only meaningful from there, and stopping at an arbitrary
	// iteration would put us mid-ramp where a new penalty is legitimate.
	reachedCap := false
	for i := 0; i < 100 && !reachedCap; i++ {
		th.noteFailure(now)
		wait, blocked := th.retryAfter(now)
		if !blocked {
			continue
		}
		reachedCap = wait == throttleMaxDelay
		now = now.Add(wait) // wait it out, as an attacker must
	}
	if !reachedCap {
		t.Fatal("never reached a cap-length block — the assertion below would be vacuous")
	}
	if wait, blocked := th.retryAfter(now); blocked {
		t.Fatalf("still blocked immediately after waiting the cap out: %v", wait)
	}
	// The attacker lands a fresh failure the instant it expires. Because the gap
	// they just waited (the cap) is >= decay, the history is forgotten and NO new
	// block arms — the ratchet breaks at the top, every time.
	th.noteFailure(now)
	if wait, blocked := th.retryAfter(now); blocked {
		t.Errorf("a failure landed right after a cap-length block re-armed the ratchet "+
			"(%v) — the lockout is sustainable and the owner can never clear it", wait)
	}
}

// TestThrottleDecayClearsAStaleDeadline — the bug the decay/cap coupling makes
// live: on decay, failures resets to 0 so penaltyFor returns 0 and the block is
// skipped, which would leave a stale FUTURE deadline outliving the history.
func TestThrottleDecayClearsAStaleDeadline(t *testing.T) {
	var th credentialThrottle
	now := throttleBase
	for i := 0; i < throttleFreeFailures+4; i++ {
		th.noteFailure(now)
	}
	if _, blocked := th.retryAfter(now); !blocked {
		t.Fatal("expected a block")
	}
	// A failure after the decay window: history forgotten, so no penalty is owed
	// and the old deadline must be gone with it.
	later := now.Add(throttleDecay + time.Second)
	th.noteFailure(later)
	if wait, blocked := th.retryAfter(later); blocked {
		t.Errorf("a stale deadline (%v) survived the decay that cleared its history", wait)
	}
}

// TestMFAActivateRequiresThePassword — H3. A stolen owner token alone must not
// be able to ARM a factor: the thief would enrol a secret they control, and the
// real owner's password would then answer 401 with no way to disarm it (disable
// needs a live code) — a durable lockout from a transient theft.
func TestMFAActivateRequiresThePassword(t *testing.T) {
	api := mfaAPI(t)
	offerMFA(t, api, true)
	rec := callJSON(api.HandleMfaEnrollApiAuthMfaEnrollPost, "")
	secret := *decodeBody[mfaStateDTO](t, rec).Secret

	// A correct code with the WRONG password must not arm anything.
	rec = callJSON(api.HandleMfaActivateApiAuthMfaActivatePost,
		fmt.Sprintf(`{"password":"not-the-password","code":%q}`, liveCode(t, secret)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("activate with a wrong password = %d, want 401", rec.Code)
	}
	if api.authMFAEnrolled() {
		t.Fatal("a factor was armed WITHOUT the password — a stolen token can lock the owner out")
	}
	// Omitting it entirely is a 422, not a silent pass.
	if rec := callJSON(api.HandleMfaActivateApiAuthMfaActivatePost,
		fmt.Sprintf(`{"code":%q}`, liveCode(t, secret))); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("activate without a password field = %d, want 422", rec.Code)
	}
	if api.authMFAEnrolled() {
		t.Fatal("a factor was armed with no password field at all")
	}
}

// TestMFAActivateRefusalIsIndistinguishable — same non-disclosure property the
// login and disable seams hold: naming which half failed confirms a correct
// password to someone holding only a session.
func TestMFAActivateRefusalIsIndistinguishable(t *testing.T) {
	api := mfaAPI(t)
	offerMFA(t, api, true)
	rec := callJSON(api.HandleMfaEnrollApiAuthMfaEnrollPost, "")
	secret := *decodeBody[mfaStateDTO](t, rec).Secret

	wrongPwd := callJSON(api.HandleMfaActivateApiAuthMfaActivatePost,
		fmt.Sprintf(`{"password":"nope","code":%q}`, liveCode(t, secret)))
	api.loginThrottle.noteSuccess()
	wrongCode := callJSON(api.HandleMfaActivateApiAuthMfaActivatePost,
		fmt.Sprintf(`{"password":%q,"code":"000000"}`, mfaTestPassword))

	if wrongPwd.Code != wrongCode.Code || wrongPwd.Body.String() != wrongCode.Body.String() {
		t.Errorf("activate discloses WHICH factor failed:\n password: %d %s\n code:     %d %s",
			wrongPwd.Code, wrongPwd.Body.String(), wrongCode.Code, wrongCode.Body.String())
	}
}

// TestMFAActivateConflictsAreNotThrottled — the ordering contract set-password
// states in caps: a path that consults no credential must keep its documented
// status even when the budget is exhausted.
func TestMFAActivateConflictsAreNotThrottled(t *testing.T) {
	api := mfaAPI(t)
	offerMFA(t, api, true)
	for i := 0; i < throttleFreeFailures+2; i++ {
		callJSON(api.HandleLoginApiLoginPost, loginBody("wrong", ""))
	}
	// Nothing pending: a 409, never a 429.
	if rec := callJSON(api.HandleMfaActivateApiAuthMfaActivatePost,
		fmt.Sprintf(`{"password":%q,"code":"123456"}`, mfaTestPassword)); rec.Code != http.StatusConflict {
		t.Errorf("activate with nothing pending while throttled = %d, want 409", rec.Code)
	}
}

// TestMFADisableConflictIsNotThrottled — the mirror, and the one the conformance
// auth matrix pins at 409. The throttle gate used to precede this 409.
func TestMFADisableConflictIsNotThrottled(t *testing.T) {
	api := mfaAPI(t)
	for i := 0; i < throttleFreeFailures+2; i++ {
		callJSON(api.HandleLoginApiLoginPost, loginBody("wrong", ""))
	}
	if rec := callJSON(api.HandleMfaDisableApiAuthMfaDisablePost,
		fmt.Sprintf(`{"password":%q,"code":"123456"}`, mfaTestPassword)); rec.Code != http.StatusConflict {
		t.Errorf("disable with nothing armed while throttled = %d, want 409", rec.Code)
	}
}

// TestLoginUnconfiguredSecretIsNotACredentialFailure — a missing signing secret
// is server config, not a credential fact: it must not spend from the budget,
// and it must be settled before any verification.
func TestLoginUnconfiguredSecretIsNotACredentialFailure(t *testing.T) {
	api := mfaAPI(t)
	api.secret = nil

	rec := callJSON(api.HandleLoginApiLoginPost, loginBody(mfaTestPassword, ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unconfigured login = %d, want 401", rec.Code)
	}
	// The budget must be untouched: restore the secret and the free allowance
	// must still be entirely available.
	api.secret = []byte(interopSecret)
	for i := 0; i < throttleFreeFailures; i++ {
		if rec := callJSON(api.HandleLoginApiLoginPost, loginBody("wrong", "")); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d, want 401 — the unconfigured attempt spent from the budget", i+1, rec.Code)
		}
	}
}

// TestMFAActivateFloorIsPersistedBeforeTheSecret pins the write ORDER, which is
// the substitute for a transaction this package does not have. Armed-with-no-floor
// is the one dangerous partial state: after a restart the floor loads as 0 and
// the activation code becomes replayable as a login.
func TestMFAActivateFloorIsPersistedBeforeTheSecret(t *testing.T) {
	api := mfaAPI(t)
	armMFA(t, api)

	floor, err := api.dal.GetSetting(settingTOTPLastStep)
	if err != nil {
		t.Fatalf("read floor: %v", err)
	}
	if floor == nil || *floor == "" || *floor == "0" {
		t.Fatalf("activate left the replay floor at %v — the activation code is "+
			"replayable across a restart", floor)
	}
}

// TestMFAActivateRefusedWhenAFactorIsAlreadyActive — half the state machine the
// file header draws, and it was unpinned: removing the guard left the suite
// green. It is what stops an armed factor being replaced without proving the old
// one, which is the same property enroll's 409 protects from the other side.
func TestMFAActivateRefusedWhenAFactorIsAlreadyActive(t *testing.T) {
	api := mfaAPI(t)
	secret := armMFA(t, api)

	// A perfectly valid code for the ACTIVE secret must still be refused: this
	// endpoint arms a PENDING enrolment, and there is none.
	rec := callJSON(api.HandleMfaActivateApiAuthMfaActivatePost,
		fmt.Sprintf(`{"password":%q,"code":%q}`, mfaTestPassword, nextCode(t, secret)))
	if rec.Code != http.StatusConflict {
		t.Errorf("activate while a factor is active = %d, want 409", rec.Code)
	}
	if !api.authMFAEnrolled() {
		t.Error("the existing factor was disturbed")
	}
}

// TestMFAActivateUpdatesMemoryOnlyAfterEveryDBWrite pins the DB-before-memory
// ordering. Inverting it (memory first) left the suite green, yet it is what
// keeps a partially-written activation from leaving the live snapshot claiming a
// factor the database does not have — MFA would appear ON until the next restart
// silently turned it OFF.
func TestMFAActivateUpdatesMemoryOnlyAfterEveryDBWrite(t *testing.T) {
	api := mfaAPI(t)
	secret := armMFA(t, api)

	// Memory and DB must agree, in both directions.
	stored, err := api.dal.GetSetting(settingTOTPSecret)
	if err != nil {
		t.Fatalf("read secret: %v", err)
	}
	if stored == nil || *stored != secret {
		t.Fatalf("DB secret = %v, memory = %q — they disagree", stored, secret)
	}
	inMemory, floor := func() (string, int64) {
		api.settingsMu.RLock()
		defer api.settingsMu.RUnlock()
		return api.totpSecret, api.totpLastStep
	}()
	if inMemory != *stored {
		t.Errorf("memory secret %q != DB secret %q", inMemory, *stored)
	}
	dbFloor, err := api.dal.GetSetting(settingTOTPLastStep)
	if err != nil {
		t.Fatalf("read floor: %v", err)
	}
	if dbFloor == nil || *dbFloor != strconv.FormatInt(floor, 10) {
		t.Errorf("memory floor %d != DB floor %v", floor, dbFloor)
	}
	// The pending slot must be gone, so a re-enrolment cannot inherit it.
	pending, err := api.dal.GetSetting(settingTOTPPendingSecret)
	if err != nil {
		t.Fatalf("read pending: %v", err)
	}
	if pending != nil {
		t.Errorf("pending secret survived activation: %q", *pending)
	}
}

// TestThrottleScheduleIsAbsolute — the throttle tests were SELF-REFERENTIAL:
// they wrote their expectations as `throttleBaseDelay`, `2*throttleBaseDelay`,
// `throttleMaxDelay`, so changing a constant moved both sides of the assertion
// together and every stated security property went unguarded (raising the cap to
// 50 minutes stayed green).
//
// This pins the ABSOLUTE numbers the comments promise, so a change to any
// constant has to be a deliberate edit here as well.
func TestThrottleScheduleIsAbsolute(t *testing.T) {
	if throttleFreeFailures != 5 {
		t.Errorf("free allowance = %d, want 5 (the documented human-fumbling budget)", throttleFreeFailures)
	}
	if throttleBaseDelay != time.Second {
		t.Errorf("base delay = %v, want 1s", throttleBaseDelay)
	}
	if throttleMaxDelay != 5*time.Minute {
		t.Errorf("cap = %v, want 5m — the comments derive '~12 attempts an hour' "+
			"and 'one coffee break rather than a lockout' from exactly this number",
			throttleMaxDelay)
	}
	if throttleMaxInFlight != 4 {
		t.Errorf("in-flight cap = %d, want 4 (bounds argon2id memory at ~76 MiB)", throttleMaxInFlight)
	}
	// The absolute schedule the "~12 attempts an hour" claim rests on.
	for _, tc := range []struct {
		failures int
		want     time.Duration
	}{
		{5, 0},
		{6, 1 * time.Second},
		{7, 2 * time.Second},
		{8, 4 * time.Second},
		// The doubling is base * 2^(over-1) where over = failures - free, so the
		// cap is first reached at failure 15 (2^10 = 512s clamps to 300s), not 14.
		{13, 128 * time.Second},
		{14, 256 * time.Second},
		{15, 5 * time.Minute},
		{99, 5 * time.Minute},
	} {
		if got := penaltyFor(tc.failures); got != tc.want {
			t.Errorf("penaltyFor(%d) = %v, want %v", tc.failures, got, tc.want)
		}
	}
	// ~12 attempts an hour at the cap, stated as the arithmetic rather than as prose.
	if perHour := int(time.Hour / throttleMaxDelay); perHour != 12 {
		t.Errorf("attempts per hour at the cap = %d, want 12", perHour)
	}
}

// ── the ship-dark feature flag ───────────────────────────────────────────────

// TestMFADefaultsToNotOffered — the whole point of the flag: an install that
// upgrades into this build must be completely unaffected until its owner opts
// in. Nothing about login changes, and the set-up path is closed.
func TestMFADefaultsToNotOffered(t *testing.T) {
	api := mfaAPI(t)

	if api.authMFAOffered() {
		t.Fatal("a fresh install must NOT offer the second factor")
	}
	reloaded, err := loadAuthSettings(api.dal, Config{}, func(string) {})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.mfaOffered {
		t.Error("an absent auth.mfa_offered row must load as false, not true")
	}
	// The set-up path is closed…
	for _, tc := range []struct {
		name string
		rec  *httptest.ResponseRecorder
	}{
		{"enroll", callJSON(api.HandleMfaEnrollApiAuthMfaEnrollPost, "")},
		{"activate", callJSON(api.HandleMfaActivateApiAuthMfaActivatePost,
			fmt.Sprintf(`{"password":%q,"code":"123456"}`, mfaTestPassword))},
	} {
		if tc.rec.Code != http.StatusForbidden {
			t.Errorf("%s while not offered = %d, want 403", tc.name, tc.rec.Code)
		}
	}
	// …and login is byte-for-byte what it was before this feature existed.
	if rec := callJSON(api.HandleLoginApiLoginPost, loginBody(mfaTestPassword, "")); rec.Code != http.StatusOK {
		t.Errorf("password-only login on a dark install = %d, want 200", rec.Code)
	}
	req := httptest.NewRequest("GET", "/api/auth/status", nil)
	rec := httptest.NewRecorder()
	api.HandleAuthStatusApiAuthStatusGet(rec, req)
	if decodeBody[authStatusDTO](t, rec).MFARequired {
		t.Error("a dark install must report mfa_required: false")
	}
}

// 🔴 TestMFAOfferedFlagNeverDisarmsALiveFactor is THE test for this flag.
//
// The flag is a rollout switch, not a security switch. If turning it off also
// turned verification off, it would BE the bypass: anyone holding a stolen owner
// token could withdraw the feature and walk straight past the second factor that
// exists to stop exactly that — undoing the both-factors rule on disable in one
// line. So: withdraw the feature from an ARMED install and assert that nothing
// about verification moves.
func TestMFAOfferedFlagNeverDisarmsALiveFactor(t *testing.T) {
	api := mfaAPI(t)
	secret := armMFA(t, api)

	offerMFA(t, api, false) // withdraw the feature while a factor is armed

	if !api.authMFAEnrolled() {
		t.Fatal("withdrawing the feature disarmed the factor")
	}
	// The login wall must still be told to ask for a code — otherwise it hides
	// the field while the server still demands one, and the owner is locked out.
	rec := httptest.NewRecorder()
	api.HandleAuthStatusApiAuthStatusGet(rec, httptest.NewRequest("GET", "/api/auth/status", nil))
	if !decodeBody[authStatusDTO](t, rec).MFARequired {
		t.Error("mfa_required went false while a factor is still armed — the wall " +
			"would hide the code field and every login would fail with no way to see why")
	}
	// Password alone must still be refused.
	if rec := callJSON(api.HandleLoginApiLoginPost, loginBody(mfaTestPassword, "")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("password-only login while the feature is withdrawn = %d, want 401 — "+
			"the flag became a bypass", rec.Code)
	}
	// …and the code still works.
	if rec := callJSON(api.HandleLoginApiLoginPost, loginBody(mfaTestPassword, nextCode(t, secret))); rec.Code != http.StatusOK {
		t.Fatalf("two-factor login while the feature is withdrawn = %d, want 200", rec.Code)
	}
}

// TestMFADisableWorksWhileTheFeatureIsWithdrawn — the other half of the same
// rule. Taking the off-switch away alongside the on-switch would strand an owner
// with a factor they can no longer remove through the product.
func TestMFADisableWorksWhileTheFeatureIsWithdrawn(t *testing.T) {
	api := mfaAPI(t)
	secret := armMFA(t, api)
	offerMFA(t, api, false)

	rec := callJSON(api.HandleMfaDisableApiAuthMfaDisablePost,
		fmt.Sprintf(`{"password":%q,"code":%q}`, mfaTestPassword, nextCode(t, secret)))
	if rec.Code != http.StatusOK {
		t.Fatalf("disable while the feature is withdrawn = %d %s", rec.Code, rec.Body.String())
	}
	if api.authMFAEnrolled() {
		t.Error("factor still armed after a valid disable")
	}
}

// TestMFAOfferSurvivesAReload — a rollout decision that forgets itself on
// restart would silently re-hide the feature (or re-expose it).
func TestMFAOfferSurvivesAReload(t *testing.T) {
	api := mfaAPI(t)
	offerMFA(t, api, true)

	reloaded, err := loadAuthSettings(api.dal, Config{}, func(string) {})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reloaded.mfaOffered {
		t.Fatal("the flag did not survive a reload")
	}
	offerMFA(t, api, false)
	reloaded, err = loadAuthSettings(api.dal, Config{}, func(string) {})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.mfaOffered {
		t.Error("turning the flag back off did not survive a reload")
	}
}

// TestMFAStateReadsBothBits — the cockpit's one read, and it must never leak a
// secret (that happens exactly once, at enroll).
func TestMFAStateReadsBothBits(t *testing.T) {
	api := mfaAPI(t)
	get := func() mfaStateDTO {
		rec := httptest.NewRecorder()
		api.HandleMfaStateApiAuthMfaGet(rec, httptest.NewRequest("GET", "/api/auth/mfa", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/auth/mfa = %d", rec.Code)
		}
		return decodeBody[mfaStateDTO](t, rec)
	}
	if s := get(); s.Offered || s.Enrolled {
		t.Fatalf("fresh install = %+v, want both false", s)
	}
	secret := armMFA(t, api)
	s := get()
	if !s.Offered || !s.Enrolled {
		t.Errorf("after arming = %+v, want both true", s)
	}
	if s.Secret != nil || s.OtpauthURI != nil {
		t.Error("the state read echoed a secret — it is disclosed only by enroll")
	}
	if rec := httptest.NewRecorder(); true {
		api.HandleMfaStateApiAuthMfaGet(rec, httptest.NewRequest("GET", "/api/auth/mfa", nil))
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("the ACTIVE secret leaked into the state read: %s", rec.Body.String())
		}
	}
}
