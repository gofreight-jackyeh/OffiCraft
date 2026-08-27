package main

// api_auth.go — the credential seams (handlers.handle_login / handle_mint /
// handle_bootstrap): the ONE public business entry (login), the owner-gated
// long-lived agent mint, and the agent boot seam (context fold + member JWT).

import (
	"fmt"
	"net/http"
	"time"
)

// maxAgentTTLSecs caps every long-lived agent token
// (service.config.MAX_AGENT_TTL_SECS — 400 days).
const maxAgentTTLSecs int64 = 400 * 86400

// invalidCredentialsMsg is the ONE refusal text /api/login answers with, for
// every cause. It names both factors so the cockpit can point the owner at the
// right field without the server disclosing which half actually failed.
const invalidCredentialsMsg = "invalid password or code"

// mintAgentToken is the ONE agent-scope boot-JWT mint under both spawn paths:
// scope="agent", sub=the member/worker id, machine_id = the boot host claim
// (omitted when empty). Members bind their durable desired machine; workers
// bind the warden actually picked at dispatch time.
func (s *apiServer) mintAgentToken(sub, machineID string, ttl int64) (string, error) {
	return mintJWT(sub, "agent", ttl, s.secret, time.Now().Unix(), machineID)
}

// mintMemberToken mints a member's boot JWT (service.boot.mint_member_token):
// machine_id = desired_machine_id.
func (s *apiServer) mintMemberToken(m Member, ttl int64) (string, error) {
	return s.mintAgentToken(m.ID, m.DesiredMachineID, ttl)
}

// mintWardenToken mints the permanent machine credential used only by warden
// installation paths. It intentionally cannot accept an arbitrary member: a
// permanent token for an agent or outsource worker would bypass their TTL and
// the 400-day ceiling.
func (s *apiServer) mintWardenToken(m Member) (string, error) {
	if m.Kind != machineKind {
		return "", fmt.Errorf("%w: permanent credentials are warden-only", errInvalidToken)
	}
	return mintJWTWithoutExpiry(m.ID, "agent", s.secret, time.Now().Unix(), "")
}

// POST /api/login — exchange the owner password (and, once enrolled, a TOTP
// code) for an owner-scoped JWT. Verified ONLY against the DB-stored argon2id
// hash (settings.go); the B1 oc.toml plaintext fallback is gone (B2).
//
// EVERY refusal on this route is the SAME flat 401 with the same message — no
// set password, wrong password, missing code and wrong code are indistinguishable
// (the first-run state is only ever disclosed by the B3 /api/auth/status
// endpoint, and `mfa_required` by the same one). Naming which factor failed
// would confirm a correct password to an attacker who has only guessed one
// half.
//
// 🔴 THE UX COST IS REAL AND ACCEPTED: an owner who fat-fingers the 6-digit
// code is told "invalid password or code", not "invalid code". The cockpit
// covers this by wording its inline error to name both fields, which is honest
// without the server disclosing anything.
//
// Failed attempts spend from the shared credential-attempt budget (throttle.go);
// a success clears it.
func (s *apiServer) HandleLoginApiLoginPost(w http.ResponseWriter, r *http.Request) {
	var body LoginDTO
	if !decodeJSONBodyRequired(w, r, &body, "password") {
		return
	}
	// Server CONFIGURATION is settled before any credential work: a missing
	// signing secret is not a credential fact, so it must not spend from the
	// attempt budget, must not burn a TOTP step, and must not be answered with
	// the credential refusal. It used to sit after the whole verification, which
	// made it the one distinguishable refusal on a route whose contract above
	// says every refusal is identical.
	if len(s.secret) == 0 {
		writeError(w, http.StatusUnauthorized, "auth not configured")
		return
	}
	// The brake sits BEFORE argon2id on purpose: at ~19 MiB and ~50 ms a
	// verification, the hash is itself the cheapest denial-of-service on this
	// server. begin (not retryAfter) both checks the deadline and RESERVES an
	// in-flight slot, which is what stops a concurrent burst walking through the
	// gate and running N argon2id verifications at once.
	release, wait, blocked := s.loginThrottle.begin(time.Now())
	if blocked {
		writeThrottled(w, wait)
		return
	}
	defer release()

	hash := s.authPasswordHash()
	if hash == "" || !verifyPassword(body.Password, hash) {
		s.loginThrottle.noteFailure(time.Now())
		writeError(w, http.StatusUnauthorized, invalidCredentialsMsg)
		return
	}
	// Second factor, when one is armed. verifyAndSpendTOTP is a no-op that
	// answers true while MFA is off, so this is the whole branch.
	code := ""
	if body.Code != nil {
		code = *body.Code
	}
	factorOK, err := s.verifyAndSpendTOTP(code, time.Now().Unix())
	if err != nil {
		// The floor could not be persisted, so the code was not really spent.
		// Failing closed here keeps a code from being replayable across the
		// restart that a storage fault tends to be followed by.
		internalError(w, err)
		return
	}
	if !factorOK {
		s.loginThrottle.noteFailure(time.Now())
		writeError(w, http.StatusUnauthorized, invalidCredentialsMsg)
		return
	}
	ttl := s.ownerTokenTTLValue()
	token, err := mintJWT(wireOwnerID, "owner", ttl, s.secret, time.Now().Unix(), "")
	if err != nil {
		// Deliberately BEFORE noteSuccess: a mint failure is a server fault, and
		// clearing the budget on it would let a failed login look like a proven
		// one. The TOTP step is already spent either way (it had to be, to be
		// single-use), so the owner waits for the next tick — unavoidable, but
		// they should not also have their attempt history silently cleared.
		internalError(w, err)
		return
	}
	// Only now is the credential PROVEN all the way to a usable token.
	s.loginThrottle.noteSuccess()
	writeJSON(w, http.StatusOK, tokenDTO{
		Token:     token,
		TokenType: "bearer",
		ExpiresIn: ttl,
		OwnerID:   wireOwnerID,
	})
}

// POST /api/mint — owner-gated (route table requires="owner") mint of a
// long-lived AGENT token for an existing member; ttl capped at 400 days.
func (s *apiServer) HandleMintApiMintPost(w http.ResponseWriter, r *http.Request) {
	var body MintRequestDTO
	if !decodeJSONBodyRequired(w, r, &body, "member_id", "ttl_days") {
		return
	}
	m, err := s.resolveMember(body.MemberId)
	if err != nil {
		writeResolveError(w, err, "member", body.MemberId)
		return
	}
	ttl := int64(body.TtlDays) * 86400
	if ttl > maxAgentTTLSecs {
		ttl = maxAgentTTLSecs
	}
	// The mint here deliberately carries NO machine_id claim (lifecycle.md
	// §1.3 mint table: /api/mint — machine_id "none").
	token, err := mintJWT(m.ID, "agent", ttl, s.secret, time.Now().Unix(), "")
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tokenDTO{
		Token:     token,
		TokenType: "bearer",
		ExpiresIn: ttl,
		OwnerID:   m.ID,
	})
}

// POST /api/bootstrap — assemble an agent's boot package (admin-gated on the
// route table). With member_id (a warden spawn) the response carries a fresh
// member JWT; a UI preview (no member_id) gets token: null (lifecycle.md §2.3).
func (s *apiServer) HandleBootstrapApiBootstrapPost(w http.ResponseWriter, r *http.Request) {
	var body BootstrapRequestDTO
	if !decodeJSONBody(w, r, &body) {
		return
	}
	var member *Member
	if body.MemberId != nil {
		m, err := s.resolveMember(*body.MemberId)
		if err != nil {
			writeResolveError(w, err, "member", *body.MemberId)
			return
		}
		member = m
	}
	boot, err := s.buildBootContext(strOrEmpty(body.Role), member, strOrEmpty(body.TaskType))
	if err != nil {
		internalError(w, err)
		return
	}
	if boot == nil {
		roleKey := resolveBootRoleKey(strOrEmpty(body.Role), member)
		writeError(w, http.StatusNotFound, "role '"+roleKey+"' not found")
		return
	}
	var token *string
	if member != nil && len(s.secret) > 0 {
		minted, err := s.mintMemberToken(*member, s.agentTokenTTLValue())
		if err != nil {
			internalError(w, err)
			return
		}
		token = &minted
	}
	writeJSON(w, http.StatusOK, bootstrapDTO{
		Role:     boot.RoleKey,
		Name:     boot.Name,
		TaskType: boot.TaskType,
		Context:  boot.Context,
		Token:    token,
	})
}
