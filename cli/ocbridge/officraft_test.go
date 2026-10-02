package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOffiCraft serves /api/chat and /api/machines/renew-credential.
type fakeOffiCraft struct {
	srv *httptest.Server

	mu          sync.Mutex
	posts       []postedChat
	chatCalls   int
	chatStatus  func(call int) int // status for the n-th /api/chat call (1-based); nil = 200
	renewStatus int                // 0 = 200
	renews      int
	hook        func() // runs at the start of every /api/chat call, outside the lock
}

type postedChat struct {
	Auth string
	To   string          `json:"to"`
	Body string          `json:"body"`
	Meta json.RawMessage `json:"meta"`
}

func newFakeOffiCraft(t *testing.T) *fakeOffiCraft {
	f := &fakeOffiCraft{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		if f.hook != nil {
			f.hook()
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.chatCalls++
		var p postedChat
		_ = json.NewDecoder(r.Body).Decode(&p)
		p.Auth = r.Header.Get("Authorization")
		status := http.StatusOK
		if f.chatStatus != nil {
			status = f.chatStatus(f.chatCalls)
		}
		if status == http.StatusOK {
			f.posts = append(f.posts, p)
		}
		w.WriteHeader(status)
		fmt.Fprint(w, `{"ok":true}`)
	})
	mux.HandleFunc("/api/machines/renew-credential", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.renewStatus != 0 {
			w.WriteHeader(f.renewStatus)
			fmt.Fprint(w, `{"error":"mint down"}`)
			return
		}
		f.renews++
		fmt.Fprintf(w, `{"token":"t%d","expires_in":2592000,"machine_id":"m-example"}`, f.renews)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOffiCraft) snapshot() (posts []postedChat, calls, renews int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]postedChat(nil), f.posts...), f.chatCalls, f.renews
}

// testCred writes a credential.json holding token t0 and loads it.
func testCred(t *testing.T, base string) *Credential {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credential.json")
	if err := os.WriteFile(path, []byte(`{"token":"t0","expires_at":4102444800,"machine_id":"m-example"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := newCredential(base, path)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func testPoster(t *testing.T, f *fakeOffiCraft) *OffiCraft {
	oc := newOffiCraft(f.srv.URL, testCred(t, f.srv.URL), "example-member")
	oc.backoffUnit = 0
	return oc
}

func TestPost400FailsOnceWithoutRenewal(t *testing.T) {
	f := newFakeOffiCraft(t)
	f.chatStatus = func(int) int { return http.StatusBadRequest }
	if err := testPoster(t, f).Post("x", nil); err == nil {
		t.Fatal("400 must be an error")
	}
	if _, calls, renews := f.snapshot(); calls != 1 || renews != 0 {
		t.Fatalf("400: calls=%d renews=%d, want 1/0", calls, renews)
	}
}

func TestPost401RenewsOnceAndRetriesWithNewToken(t *testing.T) {
	f := newFakeOffiCraft(t)
	var auths []string
	f.chatStatus = func(n int) int {
		if n == 1 {
			return http.StatusUnauthorized
		}
		return http.StatusOK
	}
	oc := testPoster(t, f)
	if err := oc.Post("x", nil); err != nil {
		t.Fatal(err)
	}
	posts, calls, renews := f.snapshot()
	for _, p := range posts {
		auths = append(auths, p.Auth)
	}
	if renews != 1 || calls != 2 {
		t.Fatalf("calls=%d renews=%d, want 2/1", calls, renews)
	}
	if len(auths) != 1 || auths[0] != "Bearer t1" {
		t.Fatalf("retry must carry the renewed token, got %v", auths)
	}
}

func TestPost401RenewalFailureIsReported(t *testing.T) {
	f := newFakeOffiCraft(t)
	f.chatStatus = func(int) int { return http.StatusUnauthorized }
	f.renewStatus = http.StatusServiceUnavailable
	err := testPoster(t, f).Post("x", nil)
	if err == nil || !strings.Contains(err.Error(), "renewal failed") {
		t.Fatalf("want a 'renewal failed' error, got %v", err)
	}
}

func TestPost5xxRetriesToTheLimit(t *testing.T) {
	f := newFakeOffiCraft(t)
	f.chatStatus = func(int) int { return http.StatusServiceUnavailable }
	if err := testPoster(t, f).Post("x", nil); err == nil {
		t.Fatal("5xx must end in an error")
	}
	if _, calls, _ := f.snapshot(); calls != deliverRetries {
		t.Fatalf("5xx calls=%d, want %d", calls, deliverRetries)
	}
}

func TestPostPayloadShape(t *testing.T) {
	f := newFakeOffiCraft(t)
	if err := testPoster(t, f).Post("hello", map[string]string{"source": "test"}); err != nil {
		t.Fatal(err)
	}
	posts, _, _ := f.snapshot()
	if len(posts) != 1 || posts[0].To != "example-member" || posts[0].Body != "hello" ||
		string(posts[0].Meta) != `{"source":"test"}` || posts[0].Auth != "Bearer t0" {
		t.Fatalf("unexpected post: %+v", posts)
	}
}

func TestRenewWritesCredentialAtomically0600(t *testing.T) {
	f := newFakeOffiCraft(t)
	c := testCred(t, f.srv.URL)
	if err := c.Renew(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(c.path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode %v, want 0600", st.Mode().Perm())
	}
	var cf credentialFile
	data, _ := os.ReadFile(c.path)
	if err := json.Unmarshal(data, &cf); err != nil || cf.Token != "t1" || cf.MachineID != "m-example" || cf.ExpiresAt <= unixNow() {
		t.Fatalf("credential file %s (err %v)", data, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(c.path)); len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
}

// A laptop asleep most of the time: the monotonic clock barely moves while the
// wall clock jumps past the 2/3 point. The next wake-up must renew, and no
// single wait may exceed an hour.
func TestKeepFreshRenewsAfterWallClockJump(t *testing.T) {
	f := newFakeOffiCraft(t)
	c := testCred(t, f.srv.URL)
	wall := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	c.wall = func() time.Time { return wall }
	c.mu.Lock()
	c.expiresAt = float64(wall.Add(30 * 24 * time.Hour).Unix()) // renew point: 10 days out
	c.mu.Unlock()
	var waits []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) bool {
		waits = append(waits, d)
		if len(waits) == 1 {
			wall = wall.Add(21 * 24 * time.Hour) // slept through the renewal point
			return true
		}
		return false // stop after the renewal
	}
	c.keepFresh(context.Background())
	if _, _, renews := f.snapshot(); renews != 1 {
		t.Fatalf("renews = %d, want 1 right after waking past the 2/3 point", renews)
	}
	for _, d := range waits {
		if d > time.Hour {
			t.Fatalf("a single wait of %s exceeds the 1 h cap", d)
		}
	}
	if len(waits) != 2 {
		t.Fatalf("waits = %v, want one before and one after the renewal", waits)
	}
	if got := c.expiry() - float64(wall.Unix()); got < 29*86400 {
		t.Fatalf("renewed expiry must be computed from the wall clock, %f s left", got)
	}
}
