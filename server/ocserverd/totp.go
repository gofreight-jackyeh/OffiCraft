package main

// totp.go — RFC 6238 TOTP (RFC 4226 HOTP over a time counter), the owner's
// second login factor. Hand-rolled on the standard library ON PURPOSE: the
// whole algorithm is an HMAC, a truncation and a modulo, and this repo does not
// add a dependency for what forty lines of stdlib do (root CLAUDE.md, Lazy
// ladder rung 5/6). No secrets leave this file's callers.
//
// HMAC-SHA1, 6 digits, 30-second step — NOT a choice, an interop constraint.
// Google Authenticator, 1Password, Authy and the iOS/macOS keychain generator
// all implement exactly that triple as their default, and several ignore the
// `algorithm`/`digits` parameters in an otpauth:// URI entirely. Emitting
// SHA-256 or 8 digits produces a QR those apps happily accept and then compute
// the WRONG code from, which is a support nightmare that looks like "MFA is
// broken". SHA-1 inside HMAC is not the SHA-1 collision problem: HMAC-SHA1 has
// no practical break, and the value being protected lives for 30 seconds.
//
// The verification WINDOW and the REPLAY floor are the two halves that make
// this safe, and they pull in opposite directions — see totpVerify.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
)

const (
	// totpStepSecs is the RFC 6238 default time step.
	totpStepSecs int64 = 30
	// totpDigits is the code length every mainstream authenticator assumes.
	totpDigits = 6
	// totpSecretLen is the raw secret size in bytes. 20 bytes = 160 bits is
	// the RFC 4226 §4 recommendation and encodes to exactly 32 base32 chars
	// with no padding — the shape authenticator apps expect to be handed.
	totpSecretLen = 20
	// totpSkewSteps is how many steps EITHER SIDE of now are accepted. 1 gives
	// a ~90-second acceptance window, which absorbs ordinary phone clock drift
	// and the seconds a human spends typing. Widening it multiplies the codes
	// valid at any instant (a brute-force gain) for no usability gain that
	// matters; narrowing it to 0 rejects a phone whose clock is 3 seconds off.
	totpSkewSteps int64 = 1
)

// totpBase32 is the alphabet every authenticator reads, unpadded (apps choke on
// the '=' padding far more often than they handle it).
var totpBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// newTOTPSecret mints a fresh base32-encoded TOTP secret.
func newTOTPSecret() (string, error) {
	raw := make([]byte, totpSecretLen)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return totpBase32.EncodeToString(raw), nil
}

// decodeTOTPSecret decodes a stored base32 secret. Whitespace and lowercase are
// tolerated because a human may have typed the secret back in by hand, and
// base32 is case-insensitive by definition — refusing "abc def" while accepting
// "ABCDEF" would be a gratuitous failure, not a security property.
func decodeTOTPSecret(secret string) ([]byte, error) {
	cleaned := strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "\t", "").Replace(secret))
	raw, err := totpBase32.DecodeString(cleaned)
	if err != nil {
		return nil, fmt.Errorf("totp secret is not valid base32: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("totp secret is empty")
	}
	return raw, nil
}

// totpCodeAt computes the RFC 4226 code for one explicit counter value. Split
// out from totpVerify so the tests can drive the RFC 6238 Appendix B vectors
// directly at a known counter.
func totpCodeAt(key []byte, counter int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(counter))
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)

	// RFC 4226 §5.4 dynamic truncation: the low nibble of the last byte picks
	// the 4-byte window, and the top bit is masked off (a sign-free 31-bit int).
	offset := sum[len(sum)-1] & 0x0f
	truncated := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff

	mod := uint32(1)
	for i := 0; i < totpDigits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", totpDigits, truncated%mod)
}

// normalizeTOTPCode strips the separators authenticator apps display ("123 456")
// so a copy-paste of what the user SEES verifies. It deliberately does not
// validate shape — totpVerify's constant-time compare against a generated code
// is the only judge of correctness, and a length check here would just be a
// second, divergent opinion about what a code looks like.
func normalizeTOTPCode(code string) string {
	return strings.NewReplacer(" ", "", "-", "", "\t", "").Replace(strings.TrimSpace(code))
}

// totpVerify checks code against secret at time `now` (unix seconds) and
// returns the STEP the code matched, or ok=false.
//
// 🔴 THE RETURNED STEP IS NOT A DETAIL — it is the replay defence, and the
// caller MUST persist it. A TOTP code stays valid for the whole acceptance
// window, so without a floor the same six digits can be replayed for ~90
// seconds; that is the difference between "an attacker who shoulder-surfs one
// code gets nothing" and "gets a login". `minStep` is the last step this
// credential already spent: any candidate at or below it is refused even when
// the HMAC matches. Pass 0 for "nothing spent yet".
//
// The scan runs the FULL window every time and never breaks early, so the work
// done — and therefore the time taken — does not depend on which step matched,
// nor on whether any did. The per-candidate compare is constant-time for the
// same reason.
func totpVerify(secret, code string, now, minStep int64) (int64, bool) {
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		return 0, false
	}
	want := normalizeTOTPCode(code)
	if want == "" {
		return 0, false
	}

	current := now / totpStepSecs
	matched := int64(0)
	found := false
	for step := current - totpSkewSteps; step <= current+totpSkewSteps; step++ {
		if step <= minStep {
			// Already spent (or before the floor) — generate nothing and claim
			// nothing. Not an early exit: the loop still runs its full range.
			continue
		}
		if subtle.ConstantTimeCompare([]byte(totpCodeAt(key, step)), []byte(want)) == 1 && !found {
			matched = step
			found = true
		}
	}
	return matched, found
}

// totpEnrollmentURI renders the otpauth:// URI an authenticator app consumes.
// `issuer` labels the account in the app's list; it is prefixed onto the label
// as well as passed as a parameter, which is the de-facto convention (Google's
// Key URI Format) every app parses.
//
// The algorithm/digits/period parameters are emitted EXPLICITLY even though
// they are all the defaults: apps that read them get confirmation, and apps
// that ignore them fall back to those same values. Emitting them costs nothing
// and removes a class of "why is my code wrong" that is otherwise invisible.
// The colon is stripped from both halves first: it is the label's OWN separator,
// so a colon inside an owner-supplied org name ("A:B") would render a label an
// app parses as a different issuer/account split. Cosmetic, not a security
// property — but the cost of preventing it is one replace, and the symptom
// (a confusingly named entry the owner may delete as a stranger's) is not.
func totpEnrollmentURI(secret, issuer, account string) string {
	issuer = strings.ReplaceAll(issuer, ":", " ")
	account = strings.ReplaceAll(account, ":", " ")
	// PathEscape, not QueryEscape: the label is a path segment, so a space must
	// become %20 rather than '+'. It deliberately leaves ':' as-is, which is the
	// canonical `issuer:account` form every authenticator parses.
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprintf("%d", totpDigits))
	q.Set("period", fmt.Sprintf("%d", totpStepSecs))
	return "otpauth://totp/" + label + "?" + q.Encode()
}
