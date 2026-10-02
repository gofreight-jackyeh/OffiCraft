package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Credential is the machine credential on disk, kept alive by this process.
//
// OffiCraft mints a warden-class token with a finite lifetime (30 days by
// default) and offers POST /api/machines/renew-credential, which acts on the
// CALLER's own `sub` and takes no body — so a machine can only ever renew
// itself. We renew at two thirds of the remaining life, the cadence ocwarden
// uses, so a Mac asleep through one renewal window still holds a valid token
// when it wakes.
//
// File format (shared with the Python bridge): {token, expires_at, machine_id},
// expires_at in fractional unix seconds, mode 0600, replaced atomically.
type Credential struct {
	base   string
	path   string
	client *http.Client

	mu        sync.Mutex
	token     string
	expiresAt float64
}

type credentialFile struct {
	Token     string  `json:"token"`
	ExpiresAt float64 `json:"expires_at"`
	MachineID string  `json:"machine_id"`
}

func newCredential(baseURL, path string) (*Credential, error) {
	c := &Credential{
		base:   strings.TrimRight(baseURL, "/"),
		path:   path,
		client: &http.Client{Timeout: 15 * time.Second},
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	var f credentialFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("credential %s: %w", path, err)
	}
	c.token, c.expiresAt = f.Token, f.ExpiresAt
	return c, nil
}

func (c *Credential) Token() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

func (c *Credential) remaining() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Duration((c.expiresAt - unixNow()) * float64(time.Second))
}

func unixNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }

type mintResponse struct {
	Token     string `json:"token"`
	ExpiresIn int64  `json:"expires_in"`
	MachineID string `json:"machine_id"`
}

// call POSTs a JSON body (an empty object when body is nil, as the Python
// bridge sent) and decodes the mint response.
func (c *Credential) call(path string, body any, token string) (mintResponse, error) {
	var r mintResponse
	if body == nil {
		body = struct{}{}
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(payload))
	if err != nil {
		return r, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return r, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return r, fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, path, snippet(raw))
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, fmt.Errorf("%s: bad response: %w", path, err)
	}
	if r.Token == "" {
		return r, fmt.Errorf("%s: response carried no token", path)
	}
	return r, nil
}

// save must be called with c.mu held.
func (c *Credential) save(r mintResponse) error {
	expires := unixNow() + float64(r.ExpiresIn)
	data, _ := json.MarshalIndent(credentialFile{r.Token, expires, r.MachineID}, "", "  ")
	// Atomic: a torn credential file bricks the bridge.
	if err := writeFileAtomic(c.path, data, 0o600); err != nil {
		return fmt.Errorf("store credential: %w", err)
	}
	c.token, c.expiresAt = r.Token, expires
	log.Printf("credential stored for %s, valid %.1f days", r.MachineID, float64(r.ExpiresIn)/86400)
	return nil
}

// Claim is the one-time bootstrap: trade a console-issued claim code for a token.
func (c *Credential) Claim(code string) error {
	r, err := c.call("/api/machines/claim", map[string]string{"code": code}, "")
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.save(r)
}

func (c *Credential) Renew() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, err := c.call("/api/machines/renew-credential", nil, c.token)
	if err != nil {
		return err
	}
	return c.save(r)
}

// keepFresh renews at 2/3 of remaining life and retries hourly on failure.
// Failure is not fatal while the current token still works, so it logs and
// retries rather than taking the bridge down — but once the token does expire
// every delivery 401s, which is why the warning is loud.
func (c *Credential) keepFresh(ctx context.Context) {
	for {
		wait := time.Hour
		if rem := c.remaining(); rem > 0 {
			wait = max(time.Minute, rem/3)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if err := c.Renew(); err != nil {
			log.Printf("WARNING credential renewal failed (%.1f days left): %v",
				max(0, c.remaining().Hours()/24), err)
		}
	}
}

// ---------------------------------------------------------------------------
// OffiCraft delivery
// ---------------------------------------------------------------------------

const deliverRetries = 4

// OffiCraft posts chat messages to one recipient as the machine identity.
type OffiCraft struct {
	url    string
	to     string
	cred   *Credential
	client *http.Client
	// backoffUnit scales the 1.5^attempt retry wait (1 s in production; tests
	// set 0 so retries do not sleep).
	backoffUnit time.Duration
}

func newOffiCraft(baseURL string, cred *Credential, to string) *OffiCraft {
	return &OffiCraft{
		url:         strings.TrimRight(baseURL, "/") + "/api/chat",
		to:          to,
		cred:        cred,
		client:      &http.Client{Timeout: 15 * time.Second},
		backoffUnit: time.Second,
	}
}

// Post delivers one chat message: up to 4 attempts with 1.5^n s backoff. A
// 4xx fails immediately (a bad recipient does not improve with retries),
// except the first 401, which renews the credential and retries — otherwise
// every message after token expiry would dead-letter.
func (o *OffiCraft) Post(body string, meta any) error {
	payload, err := json.Marshal(struct {
		To   string `json:"to"`
		Body string `json:"body"`
		Meta any    `json:"meta"`
	}{o.to, body, meta})
	if err != nil {
		return err
	}
	var last error
	renewed := false
	for attempt := 0; attempt < deliverRetries; attempt++ {
		if attempt > 0 && last != nil {
			time.Sleep(time.Duration(math.Pow(1.5, float64(attempt-1)) * float64(o.backoffUnit)))
		}
		// Header rebuilt every attempt: a renewal between attempts must not be
		// undone by a stale Authorization captured once up front.
		req, err := http.NewRequest(http.MethodPost, o.url, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+o.cred.Token())
		resp, err := o.client.Do(req)
		if err != nil {
			last = err
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		resp.Body.Close()
		code := resp.StatusCode
		switch {
		case code/100 == 2:
			log.Printf("delivered -> %s (HTTP %d)", o.to, code)
			return nil
		case code == http.StatusUnauthorized && !renewed:
			renewed = true
			log.Printf("401 from OffiCraft — renewing credential and retrying")
			if err := o.cred.Renew(); err != nil {
				return fmt.Errorf("401 and renewal failed: %w", err)
			}
			last = nil // retry right away, as the Python bridge did
		case code >= 400 && code < 500:
			return fmt.Errorf("HTTP %d from OffiCraft: %s", code, snippet(raw))
		default:
			last = fmt.Errorf("HTTP %d from OffiCraft: %s", code, snippet(raw))
		}
	}
	return fmt.Errorf("delivery failed after %d attempts: %v", deliverRetries, last)
}

func snippet(b []byte) string {
	if len(b) > 400 {
		b = b[:400]
	}
	return strings.ToValidUTF8(string(b), "�")
}
