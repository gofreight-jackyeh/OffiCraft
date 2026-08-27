package main

import (
	"strings"
	"testing"
)

// rfc6238Secret is the RFC 6238 Appendix B seed ("12345678901234567890",
// 20 ASCII bytes) in the base32 form this package stores secrets as.
const rfc6238Secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

// TestTOTPCodeAtMatchesRFC6238Vectors pins the generator against the SHA-1
// vectors published in RFC 6238 Appendix B. This is the test that actually
// proves interoperability: an authenticator app is not something we can call
// from a test, so agreeing with the SPEC the apps implement is the only real
// evidence that a phone's code will verify here.
//
// The RFC prints 8-digit codes; this implementation emits 6, which is the
// same truncation with a smaller modulus — hence the low 6 digits of each
// published value.
func TestTOTPCodeAtMatchesRFC6238Vectors(t *testing.T) {
	key, err := decodeTOTPSecret(rfc6238Secret)
	if err != nil {
		t.Fatalf("decode seed: %v", err)
	}
	for _, tc := range []struct {
		unix int64
		want string // low 6 digits of the RFC's 8-digit vector
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	} {
		got := totpCodeAt(key, tc.unix/totpStepSecs)
		if got != tc.want {
			t.Errorf("totpCodeAt(T=%d, step=%d) = %q, RFC 6238 says %q",
				tc.unix, tc.unix/totpStepSecs, got, tc.want)
		}
	}
}

func TestTOTPVerifyAcceptsCurrentStep(t *testing.T) {
	const now int64 = 1111111111
	key, _ := decodeTOTPSecret(rfc6238Secret)
	code := totpCodeAt(key, now/totpStepSecs)

	step, ok := totpVerify(rfc6238Secret, code, now, 0)
	if !ok {
		t.Fatalf("current-step code rejected")
	}
	if want := now / totpStepSecs; step != want {
		t.Errorf("matched step = %d, want %d", step, want)
	}
}

// TestTOTPVerifyToleratesClockSkew covers the window in BOTH directions — a
// phone that is a step fast and one that is a step slow — and then pins the
// edge: two steps out must fail. Without the negative half the window could
// silently widen to any size and this test would still pass.
func TestTOTPVerifyToleratesClockSkew(t *testing.T) {
	const now int64 = 1111111111
	key, _ := decodeTOTPSecret(rfc6238Secret)
	current := now / totpStepSecs

	for _, delta := range []int64{-1, 0, 1} {
		code := totpCodeAt(key, current+delta)
		if _, ok := totpVerify(rfc6238Secret, code, now, 0); !ok {
			t.Errorf("code from step %+d rejected, inside the accepted window", delta)
		}
	}
	for _, delta := range []int64{-2, 2} {
		code := totpCodeAt(key, current+delta)
		if _, ok := totpVerify(rfc6238Secret, code, now, 0); ok {
			t.Errorf("code from step %+d ACCEPTED, outside the window", delta)
		}
	}
}

// TestTOTPVerifyRefusesReplay is the one that matters most: a code stays
// cryptographically valid for the whole ~90s window, so replay defence is
// entirely the caller's floor. If this breaks, a shoulder-surfed code becomes a
// login.
func TestTOTPVerifyRefusesReplay(t *testing.T) {
	const now int64 = 1111111111
	key, _ := decodeTOTPSecret(rfc6238Secret)
	current := now / totpStepSecs
	code := totpCodeAt(key, current)

	step, ok := totpVerify(rfc6238Secret, code, now, 0)
	if !ok {
		t.Fatalf("first use rejected")
	}
	if _, ok := totpVerify(rfc6238Secret, code, now, step); ok {
		t.Fatal("SAME code accepted a second time at the recorded floor — replay is open")
	}
	// The floor must not burn the *rest* of the window: a later step still works.
	next := totpCodeAt(key, current+1)
	if _, ok := totpVerify(rfc6238Secret, next, now, step); !ok {
		t.Error("the next step's code was refused; the floor over-collected")
	}
}

// TestTOTPVerifyRefusesStepsAtOrBelowFloor guards the boundary directly: the
// floor is "already spent", so equality must be refused, not just <.
func TestTOTPVerifyRefusesStepsAtOrBelowFloor(t *testing.T) {
	const now int64 = 1111111111
	key, _ := decodeTOTPSecret(rfc6238Secret)
	current := now / totpStepSecs

	// Floor set to the newest step in the window ⇒ nothing in it can be spent.
	for _, delta := range []int64{-1, 0, 1} {
		code := totpCodeAt(key, current+delta)
		if _, ok := totpVerify(rfc6238Secret, code, now, current+1); ok {
			t.Errorf("step %+d accepted despite a floor above it", delta)
		}
	}
}

func TestTOTPVerifyRejectsWrongAndMalformedInput(t *testing.T) {
	const now int64 = 1111111111
	for _, tc := range []struct {
		name, secret, code string
	}{
		{"wrong code", rfc6238Secret, "000000"},
		{"empty code", rfc6238Secret, ""},
		{"blank code", rfc6238Secret, "   "},
		{"non-numeric", rfc6238Secret, "abcdef"},
		{"empty secret", "", "123456"},
		{"secret not base32", "not-base32-!!", "123456"},
	} {
		if _, ok := totpVerify(tc.secret, tc.code, now, 0); ok {
			t.Errorf("%s: accepted", tc.name)
		}
	}
}

// TestTOTPVerifyAcceptsDisplayedSeparators — authenticator apps render "123 456"
// and users paste exactly that.
func TestTOTPVerifyAcceptsDisplayedSeparators(t *testing.T) {
	const now int64 = 1111111111
	key, _ := decodeTOTPSecret(rfc6238Secret)
	code := totpCodeAt(key, now/totpStepSecs)

	for _, typed := range []string{
		code[:3] + " " + code[3:],
		code[:3] + "-" + code[3:],
		" " + code + " ",
	} {
		if _, ok := totpVerify(rfc6238Secret, typed, now, 0); !ok {
			t.Errorf("code typed as %q rejected", typed)
		}
	}
}

func TestDecodeTOTPSecretIsCaseAndSpaceInsensitive(t *testing.T) {
	want, err := decodeTOTPSecret(rfc6238Secret)
	if err != nil {
		t.Fatalf("baseline decode: %v", err)
	}
	for _, variant := range []string{
		strings.ToLower(rfc6238Secret),
		rfc6238Secret[:4] + " " + rfc6238Secret[4:],
		rfc6238Secret[:4] + "-" + rfc6238Secret[4:],
	} {
		got, err := decodeTOTPSecret(variant)
		if err != nil {
			t.Errorf("decode %q: %v", variant, err)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("decode %q gave a different key", variant)
		}
	}
}

// TestNewTOTPSecretIsFreshAndUsable checks the two properties that matter: the
// mint does not repeat itself, and what it produces actually round-trips
// through the verifier.
func TestNewTOTPSecretIsFreshAndUsable(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 16; i++ {
		secret, err := newTOTPSecret()
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if seen[secret] {
			t.Fatalf("newTOTPSecret repeated a secret — not random")
		}
		seen[secret] = true

		key, err := decodeTOTPSecret(secret)
		if err != nil {
			t.Fatalf("minted secret does not decode: %v", err)
		}
		const now int64 = 1700000000
		if _, ok := totpVerify(secret, totpCodeAt(key, now/totpStepSecs), now, 0); !ok {
			t.Fatal("a freshly minted secret's own code did not verify")
		}
	}
}

// TestTOTPEnrollmentURIShapeIsWhatAuthenticatorsParse pins the Key URI Format
// fields. A malformed URI produces a QR that scans into a silently wrong
// credential, which surfaces only later as "MFA is broken".
func TestTOTPEnrollmentURIShapeIsWhatAuthenticatorsParse(t *testing.T) {
	uri := totpEnrollmentURI(rfc6238Secret, "OffiCraft", "owner")

	if !strings.HasPrefix(uri, "otpauth://totp/") {
		t.Fatalf("wrong scheme/type: %s", uri)
	}
	for _, want := range []string{
		"secret=" + rfc6238Secret,
		"issuer=OffiCraft",
		"algorithm=SHA1",
		"digits=6",
		"period=30",
	} {
		if !strings.Contains(uri, want) {
			t.Errorf("URI missing %q: %s", want, uri)
		}
	}
	// The canonical Key URI label is a literal `issuer:account` path segment —
	// PathEscape leaves ':' alone, which is what authenticators expect.
	if !strings.Contains(uri, "otpauth://totp/OffiCraft:owner?") {
		t.Errorf("label should be the literal issuer:account pair: %s", uri)
	}
}

// TestTOTPEnrollmentURIKeepsTheLabelUnambiguous — the label's own separator is
// the colon, so an owner-supplied org name containing one must not be able to
// forge a different issuer/account split in the app's list.
func TestTOTPEnrollmentURIKeepsTheLabelUnambiguous(t *testing.T) {
	uri := totpEnrollmentURI(rfc6238Secret, "Acme:Evil", "owner")
	if strings.Contains(uri, "Acme:Evil") {
		t.Errorf("a colon in the issuer survived into the label: %s", uri)
	}
	if !strings.Contains(uri, "otpauth://totp/Acme%20Evil:owner?") {
		t.Errorf("expected the colon neutralised and the label still well-formed: %s", uri)
	}
}

// TestTOTPEnrollmentURIEscapesSpaces guards the other half of the PathEscape
// choice: a studio name with a space must not become a '+' (which apps would
// display literally).
func TestTOTPEnrollmentURIEscapesSpaces(t *testing.T) {
	uri := totpEnrollmentURI(rfc6238Secret, "My Studio", "owner")
	if !strings.Contains(uri, "otpauth://totp/My%20Studio:owner?") {
		t.Errorf("space should be %%20 in the label: %s", uri)
	}
}

// TestDecodeTOTPSecretRefusesAnEmptyKey — the guard this pins is LOAD-BEARING
// and was previously untested: base32 decodes "" to a zero-length key with NO
// error, so without the emptiness check totpVerify would HMAC with an empty key
// and accept codes anyone can compute. Removing the guard used to leave the whole
// suite green (TestCorruptStoredSecretIsABootError only plants invalid base32,
// which trips the decoder and never reaches this path).
func TestDecodeTOTPSecretRefusesAnEmptyKey(t *testing.T) {
	for _, in := range []string{"", "   ", "-", "\t", " - \t "} {
		if _, err := decodeTOTPSecret(in); err == nil {
			t.Errorf("decodeTOTPSecret(%q) returned no error — an empty key would "+
				"make every code verifiable by anyone", in)
		}
	}
	// And the verifier must refuse them too, not merely the decoder.
	for _, in := range []string{"", "   ", "-"} {
		if _, ok := totpVerify(in, "123456", 1700000000, 0); ok {
			t.Errorf("totpVerify accepted a code against the empty secret %q", in)
		}
	}
}
