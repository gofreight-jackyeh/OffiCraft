package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// fakeSlack serves apps.connections.open and the WebSocket it hands out.
func fakeSlack(t *testing.T, ws websocket.Handler) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var opens atomic.Int64
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	mux.HandleFunc("/api/apps.connections.open", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer xapp-example" {
			fmt.Fprint(w, `{"ok":false,"error":"invalid_auth"}`)
			return
		}
		opens.Add(1)
		fmt.Fprintf(w, `{"ok":true,"url":"ws://%s/link"}`, srv.Listener.Addr())
	})
	mux.Handle("/link", ws)
	t.Cleanup(srv.Close)
	return srv, &opens
}

// startClient runs a socketClient with second-scale-free timings and returns
// it plus a stop func that cancels and waits for run to return.
func startClient(t *testing.T, slackURL string, b *bridge) (*socketClient, *connectionHealth, func()) {
	t.Helper()
	h := &connectionHealth{send: func(string, any) error { return nil }}
	c := newSocketClient("xapp-example", b, h)
	c.api = slackURL + "/api"
	c.backoffMin, c.backoffMax = 10*time.Millisecond, 40*time.Millisecond
	c.pingEvery, c.readTimeout, c.tickEvery = 50*time.Millisecond, 2*time.Second, 20*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.run(ctx); close(done) }()
	return c, h, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("socket client did not stop")
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The 2026-09 failure, end to end: every connection is accepted and dropped
// at once. The client must keep reconnecting (never exit) and health must
// record each drop.
func TestE2E_AcceptThenCloseReconnectsAndRecordsDrops(t *testing.T) {
	var accepted atomic.Int64
	srv, opens := fakeSlack(t, func(ws *websocket.Conn) { accepted.Add(1); ws.Close() })
	f := newFakeOffiCraft(t)
	c, h, stop := startClient(t, srv.URL, testBridge(t, testPoster(t, f)))
	waitFor(t, "3 reconnects and 3 drops", func() bool {
		return accepted.Load() >= 3 && h.dropCount() >= 3
	})
	stop()
	if opens.Load() < 3 || c.dials.Load() < 3 {
		t.Fatalf("opens=%d dials=%d, want >= 3 each", opens.Load(), c.dials.Load())
	}
	h.mu.Lock()
	down := h.down
	h.mu.Unlock()
	if !down {
		t.Fatal("health must consider the connection down")
	}
}

// A startup failure (connections.open refused) goes through the same loop.
func TestE2E_StartupFailureRetriesInsteadOfExiting(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"ok":false,"error":"invalid_auth"}`)
	}))
	t.Cleanup(srv.Close)
	f := newFakeOffiCraft(t)
	_, h, stop := startClient(t, srv.URL, testBridge(t, testPoster(t, f)))
	waitFor(t, "3 open attempts", func() bool { return calls.Load() >= 3 })
	stop()
	if h.dropCount() < 3 {
		t.Fatalf("dial failures must count as drops, got %d", h.dropCount())
	}
}

// One events_api envelope: the ack must come back with its envelope_id and
// one message must arrive at OffiCraft's /api/chat.
func TestE2E_EventIsAckedAndDelivered(t *testing.T) {
	acks := make(chan map[string]string, 1)
	release := make(chan struct{})
	srv, _ := fakeSlack(t, func(ws *websocket.Conn) {
		_ = websocket.Message.Send(ws, `{"type":"hello","num_connections":1}`)
		_ = websocket.Message.Send(ws, `{"envelope_id":"env-e2e","type":"events_api","accepts_response_payload":false,`+
			`"payload":{"event_id":"Ev0EXAMPLE","authorizations":[{"is_bot":true,"user_id":"U0BOT"}],`+
			`"event":{"type":"message","user":"U0EXAMPLE","channel":"C0EXAMPLE","channel_type":"channel",`+
			`"ts":"1789.0001","text":"hello from e2e"}}}`)
		var ack map[string]string
		if err := websocket.JSON.Receive(ws, &ack); err == nil {
			acks <- ack
		}
		<-release // hold the connection open until the test is done
	})
	t.Cleanup(func() { close(release) }) // runs before srv.Close (LIFO)
	f := newFakeOffiCraft(t)
	_, _, stop := startClient(t, srv.URL, testBridge(t, testPoster(t, f)))
	defer stop()

	select {
	case ack := <-acks:
		if len(ack) != 1 || ack["envelope_id"] != "env-e2e" {
			t.Fatalf("ack = %v, want {envelope_id: env-e2e}", ack)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no ack received")
	}
	waitFor(t, "one OffiCraft post", func() bool { p, _, _ := f.snapshot(); return len(p) == 1 })
	p, _, _ := f.snapshot()
	if p[0].Body != "[Slack #C0EXAMPLE] U0EXAMPLE:\nhello from e2e" || p[0].Auth != "Bearer t0" || p[0].To != "example-member" {
		t.Fatalf("post = %+v", p[0])
	}
	if !strings.Contains(string(p[0].Meta), `"event_id":"Ev0EXAMPLE"`) {
		t.Fatalf("meta = %s", p[0].Meta)
	}
}
