package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/websocket"
)

// skipSubtypes are edits/removals/membership noise rather than something a
// colleague said. Kept deliberately small — an unknown subtype is delivered.
var skipSubtypes = map[string]bool{
	"message_changed": true,
	"message_deleted": true,
	"channel_join":    true,
	"channel_leave":   true,
	"bot_message":     true,
}

// ---------------------------------------------------------------------------
// Slack Web API: names
// ---------------------------------------------------------------------------

// nameCache turns opaque ids into names via users.info / conversations.info.
// Best effort — with no bot token (nil cache) or on any error we fall back to
// the id, and the fallback is cached too. A name is never worth failing
// delivery.
type nameCache struct {
	api    string // e.g. https://slack.com/api
	token  string // bot token (xoxb-)
	client *http.Client

	mu       sync.Mutex
	users    map[string]string
	channels map[string]string
}

func newNameCache(api, botToken string) *nameCache {
	if botToken == "" {
		return nil
	}
	return &nameCache{api: api, token: botToken, client: &http.Client{Timeout: 15 * time.Second},
		users: map[string]string{}, channels: map[string]string{}}
}

func (n *nameCache) user(uid string) string {
	if uid == "" {
		return "?"
	}
	if n == nil {
		return uid
	}
	return n.lookup(n.users, "users.info", "user", uid, func(raw json.RawMessage) string {
		var r struct {
			User struct {
				RealName string `json:"real_name"`
				Profile  struct {
					DisplayName string `json:"display_name"`
				} `json:"profile"`
			} `json:"user"`
		}
		_ = json.Unmarshal(raw, &r)
		if r.User.Profile.DisplayName != "" {
			return r.User.Profile.DisplayName
		}
		return r.User.RealName
	})
}

func (n *nameCache) channel(cid string) string {
	if cid == "" {
		return "?"
	}
	if n == nil {
		return cid
	}
	return n.lookup(n.channels, "conversations.info", "channel", cid, func(raw json.RawMessage) string {
		var r struct {
			Channel struct {
				Name string `json:"name"`
			} `json:"channel"`
		}
		_ = json.Unmarshal(raw, &r)
		return r.Channel.Name
	})
}

func (n *nameCache) lookup(cache map[string]string, method, param, id string, pick func(json.RawMessage) string) string {
	n.mu.Lock()
	name, ok := cache[id]
	n.mu.Unlock()
	if ok {
		return name
	}
	name = id
	raw, err := slackCall(n.client, n.api, method, n.token, url.Values{param: {id}})
	if err != nil {
		log.Printf("WARNING %s(%s) failed: %v", method, id, err)
	} else if got := pick(raw); got != "" {
		name = got
	}
	n.mu.Lock()
	cache[id] = name
	n.mu.Unlock()
	return name
}

// slackCall POSTs a form to a Slack Web API method and returns the body when
// the response says ok:true.
func slackCall(client *http.Client, api, method, token string, form url.Values) (json.RawMessage, error) {
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(api, "/")+"/"+method,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, err
	}
	var head struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(buf.Bytes(), &head); err != nil {
		return nil, fmt.Errorf("%s: HTTP %d, unreadable body", method, resp.StatusCode)
	}
	if !head.OK {
		return nil, fmt.Errorf("%s: %s", method, head.Error)
	}
	return buf.Bytes(), nil
}

// ---------------------------------------------------------------------------
// Event handling
// ---------------------------------------------------------------------------

type slackEvent struct {
	Type        string `json:"type"`
	Subtype     string `json:"subtype"`
	BotID       string `json:"bot_id"`
	User        string `json:"user"`
	Channel     string `json:"channel"`
	ChannelType string `json:"channel_type"`
	Text        string `json:"text"`
	TS          string `json:"ts"`
	ThreadTS    string `json:"thread_ts"`
}

type eventsAPIPayload struct {
	EventID        string     `json:"event_id"`
	Event          slackEvent `json:"event"`
	Authorizations []struct {
		IsBot  bool   `json:"is_bot"`
		UserID string `json:"user_id"`
	} `json:"authorizations"`
}

// render is one human-readable line, then the text. A DM has no channel name —
// its D… id tells the reader nothing — so the conversation KIND decides the
// prefix.
func render(ev slackEvent, names *nameCache) string {
	who := names.user(ev.User)
	var head string
	switch ev.ChannelType {
	case "im":
		head = fmt.Sprintf("[Slack 私訊] %s:", who)
	case "mpim":
		head = fmt.Sprintf("[Slack 群組私訊] %s:", who)
	default:
		head = fmt.Sprintf("[Slack #%s] %s:", names.channel(ev.Channel), who)
	}
	if text := strings.TrimSpace(ev.Text); text != "" {
		return head + "\n" + text
	}
	return head + " (無文字內容)"
}

// bridge turns Slack events into OffiCraft chat messages.
type bridge struct {
	oc         *OffiCraft
	seen       *seenStore
	names      *nameCache
	deadletter string
	ignore     map[string]bool
	health     *connectionHealth // nil in tests that do not watch silence

	// In-flight deliveries. Their envelopes are already acked, so on shutdown
	// they must finish or land in deadletter — Slack will not send them again.
	inflight sync.WaitGroup
	mu       sync.Mutex
	pending  map[int]json.RawMessage
	nextID   int
}

// track wraps one events_api handler so drain can wait for it.
func (b *bridge) track(payload json.RawMessage, f func()) func() {
	b.mu.Lock()
	if b.pending == nil {
		b.pending = map[int]json.RawMessage{}
	}
	id := b.nextID
	b.nextID++
	b.pending[id] = payload
	b.mu.Unlock()
	b.inflight.Add(1)
	return func() {
		defer b.inflight.Done()
		defer func() {
			b.mu.Lock()
			delete(b.pending, id)
			b.mu.Unlock()
		}()
		f()
	}
}

// drain waits up to timeout for in-flight deliveries, then writes whatever is
// still unfinished to deadletter. A delivery that completes right after the
// deadline may then exist both in chat and in deadletter — a duplicate is
// preferred over a silent loss.
func (b *bridge) drain(timeout time.Duration) {
	done := make(chan struct{})
	go func() { b.inflight.Wait(); close(done) }()
	select {
	case <-done:
		return
	case <-time.After(timeout):
	}
	b.mu.Lock()
	left := make([]json.RawMessage, 0, len(b.pending))
	for _, p := range b.pending {
		left = append(left, p)
	}
	b.mu.Unlock()
	for _, p := range left {
		log.Printf("ERROR shutdown with delivery still in flight — writing to deadletter")
		if err := appendDeadletter(b.deadletter, errors.New("shutdown before delivery finished"), p); err != nil {
			log.Printf("ERROR deadletter write failed, message LOST: %v (payload %s)", err, snippet(p))
		}
	}
}

// handleEvent processes one events_api payload. It runs after the ack.
func (b *bridge) handleEvent(raw json.RawMessage) {
	var p eventsAPIPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		log.Printf("WARNING unreadable events_api payload: %v", err)
		return
	}
	ev := p.Event
	// Proof the Slack side still delivers, counted before every filter: the
	// owner's own lines and bot chatter are as good a heartbeat as any.
	if ev.Type == "message" && b.health != nil {
		b.health.markEvent(time.Now())
	}
	if ev.Type != "message" || ev.BotID != "" || skipSubtypes[ev.Subtype] {
		return
	}
	// The owner reads what OTHERS said; his own outgoing lines are already
	// his. Dropped before the dedup table so they do not fill it either.
	if ev.User != "" && b.ignore[ev.User] {
		log.Printf("skip own message from %s", ev.User)
		return
	}

	// Dedup on the MESSAGE, not on the envelope. Once a user-token
	// authorization exists alongside the bot one, the same message is visible
	// to both and Slack may mint a separate event_id per authorization —
	// keying on event_id would let that through twice. (channel, ts) names the
	// message itself and is stable across re-delivery and authorization.
	key := p.EventID
	if ev.Channel != "" && ev.TS != "" {
		key = ev.Channel + ":" + ev.TS
	}
	if key == "" {
		log.Printf("WARNING event with no usable dedup key, dropping")
		return
	}
	first, err := b.seen.claim(key, time.Now())
	if err != nil {
		// The in-memory verdict still holds; only cross-restart dedup is at risk.
		log.Printf("ERROR dedup store write failed: %v", err)
	}
	if !first {
		log.Printf("skip duplicate %s", key)
		return
	}
	authz := "bot"
	if len(p.Authorizations) == 0 || !p.Authorizations[0].IsBot {
		uid := "?"
		if len(p.Authorizations) > 0 && p.Authorizations[0].UserID != "" {
			uid = p.Authorizations[0].UserID
		}
		authz = "user " + uid
	}
	log.Printf("accepted %s (authorization: %s)", key, authz)

	meta := map[string]any{
		"source": "slack-socket-mode",
		"slack": map[string]any{
			"event_id":  nullable(p.EventID),
			"channel":   nullable(ev.Channel),
			"user":      nullable(ev.User),
			"ts":        nullable(ev.TS),
			"thread_ts": nullable(ev.ThreadTS),
		},
	}
	if err := b.oc.Post(render(ev, b.names), meta); err != nil {
		// Already acked, so Slack will not send it again — the only way this
		// message survives is on our own disk.
		log.Printf("ERROR DELIVERY FAILED %s: %v", key, err)
		if err := appendDeadletter(b.deadletter, err, raw); err != nil {
			log.Printf("ERROR deadletter write failed, message LOST: %v", err)
		}
	}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func appendDeadletter(path string, cause error, payload json.RawMessage) error {
	var line bytes.Buffer
	enc := json.NewEncoder(&line)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(struct {
		At      float64         `json:"at"`
		Error   string          `json:"error"`
		Payload json.RawMessage `json:"payload"`
	}{unixNow(), cause.Error(), payload}); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(line.Bytes()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// envelope is one Socket Mode frame.
type envelope struct {
	EnvelopeID string          `json:"envelope_id"`
	Type       string          `json:"type"`
	Reason     string          `json:"reason"` // disconnect: refresh_requested | warning | link_disabled
	Payload    json.RawMessage `json:"payload"`
}

// onFrame acks the envelope FIRST — a slow ack is re-delivered, and a
// re-delivery we then also process is a duplicate — then hands events_api
// work to run (a goroutine in production, inline in tests). It returns the
// envelope so the connection loop can react to hello / disconnect.
func (b *bridge) onFrame(raw []byte, ack func(envelopeID string) error, run func(func())) (envelope, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		log.Printf("WARNING unreadable socket frame: %v", err)
		return env, nil
	}
	if env.EnvelopeID != "" {
		if err := ack(env.EnvelopeID); err != nil {
			return env, fmt.Errorf("ack %s: %w", env.EnvelopeID, err)
		}
	}
	switch env.Type {
	case "hello":
		log.Printf("socket mode: hello")
	case "disconnect":
		log.Printf("socket mode: disconnect requested by Slack (%s)", snippet(raw))
	case "events_api":
		payload := env.Payload
		run(b.track(payload, func() { b.handleEvent(payload) }))
	}
	return env, nil
}

// ---------------------------------------------------------------------------
// Socket Mode connection loop
// ---------------------------------------------------------------------------

// socketClient holds the Slack Socket Mode WebSocket, reconnecting forever.
type socketClient struct {
	appToken string
	api      string // https://slack.com/api
	bridge   *bridge
	health   *connectionHealth
	http     *http.Client

	backoffMin, backoffMax time.Duration // 5 s .. 60 s
	pingEvery              time.Duration // client ping cadence
	readTimeout            time.Duration // no bytes this long = half-open socket, drop it
	tickEvery              time.Duration // health tick

	connected atomic.Bool
	dials     atomic.Int64 // successful dials, for tests and logs
}

func newSocketClient(appToken string, b *bridge, h *connectionHealth) *socketClient {
	return &socketClient{
		appToken: appToken, api: "https://slack.com/api", bridge: b, health: h,
		http:       &http.Client{Timeout: 30 * time.Second},
		backoffMin: 5 * time.Second, backoffMax: 60 * time.Second,
		pingEvery: 30 * time.Second, readTimeout: 90 * time.Second,
		tickEvery: healthTick,
	}
}

// run blocks until ctx is cancelled. A failure at any point — including the
// very first connect — goes through the same reconnect loop; it never exits.
func (s *socketClient) run(ctx context.Context) {
	go s.healthLoop(ctx)
	backoff := s.backoffMin
	for ctx.Err() == nil {
		reconnectNow, gotHello, err := s.session(ctx)
		if ctx.Err() != nil {
			return
		}
		s.health.markDown(time.Now(), true)
		if gotHello {
			backoff = s.backoffMin
		}
		if reconnectNow {
			continue
		}
		log.Printf("WARNING socket mode connection lost: %v — reconnecting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, s.backoffMax)
	}
}

func (s *socketClient) healthLoop(ctx context.Context) {
	t := time.NewTicker(s.tickEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.health.tick(time.Now(), s.connected.Load())
		}
	}
}

// session is one connection: open, dial, read until it fails. It returns
// reconnectNow when Slack asked for the reconnect (no backoff), and gotHello
// when the connection got far enough to reset the backoff.
func (s *socketClient) session(ctx context.Context) (reconnectNow, gotHello bool, err error) {
	wsURL, err := s.openURL()
	if err != nil {
		return false, false, fmt.Errorf("apps.connections.open: %w", err)
	}
	ws, conn, err := dialWebsocket(ctx, wsURL)
	if err != nil {
		return false, false, fmt.Errorf("dial: %w", err)
	}
	s.dials.Add(1)
	s.connected.Store(true)
	defer s.connected.Store(false)
	done := make(chan struct{})
	defer close(done)
	// Every close below is of the RAW socket, never ws.Close(): ws.Close()
	// takes the websocket write lock to send a close frame, and a writer stuck
	// on a full send buffer holds that lock — the close would block, the reader
	// would never return, and health would keep seeing "connected".
	go func() { // close on shutdown, and on a half-open socket (no bytes, not even pongs)
		t := time.NewTicker(s.pingEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				conn.Close()
				return
			case <-t.C:
				if conn.idle() > s.readTimeout {
					log.Printf("WARNING socket silent for %s — dropping it", conn.idle().Round(time.Second))
					conn.Close()
					return
				}
				_ = pingCodec.Send(ws, nil)
			}
		}
	}()
	defer conn.Close()

	ack := func(id string) error { return websocket.JSON.Send(ws, map[string]string{"envelope_id": id}) }
	run := func(f func()) { go f() }
	for {
		var frame []byte
		if err := websocket.Message.Receive(ws, &frame); err != nil {
			return false, gotHello, fmt.Errorf("read: %w", err)
		}
		env, err := s.bridge.onFrame(frame, ack, run)
		if err != nil {
			return false, gotHello, err
		}
		switch env.Type {
		case "hello":
			gotHello = true
		case "disconnect":
			// Skip the backoff only for Slack's routine connection rotation on
			// a connection that actually worked. link_disabled, or a disconnect
			// before hello, would otherwise hammer apps.connections.open.
			if gotHello && (env.Reason == "refresh_requested" || env.Reason == "warning") {
				return true, gotHello, nil
			}
			return false, gotHello, fmt.Errorf("disconnect (reason %q, hello %v)", env.Reason, gotHello)
		}
	}
}

// openURL asks Slack for a fresh WebSocket URL with the app-level token.
func (s *socketClient) openURL() (string, error) {
	raw, err := slackCall(s.http, s.api, "apps.connections.open", s.appToken, url.Values{})
	if err != nil {
		return "", err
	}
	var r struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || r.URL == "" {
		return "", errors.New("response carried no url")
	}
	return r.URL, nil
}

// pingCodec sends an empty ping frame; the pong (or any traffic) refreshes
// trackedConn's last-read time.
var pingCodec = websocket.Codec{Marshal: func(any) ([]byte, byte, error) {
	return nil, websocket.PingFrame, nil
}}

// trackedConn records when bytes last arrived — x/net/websocket swallows pong
// frames, so the raw socket is the only place a pong is observable.
type trackedConn struct {
	net.Conn
	last atomic.Pointer[time.Time] // last successful read; keeps the monotonic reading
}

func (c *trackedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		now := time.Now()
		c.last.Store(&now)
	}
	return n, err
}

// Write bounds every write, so a peer that stops reading surfaces as a write
// error instead of a goroutine stuck forever holding the websocket write lock.
// Writes are serialized by that lock, so setting the deadline here is race-free.
func (c *trackedConn) Write(p []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return c.Conn.Write(p)
}

const writeTimeout = 30 * time.Second

func (c *trackedConn) idle() time.Duration {
	return time.Since(*c.last.Load())
}

func dialWebsocket(ctx context.Context, raw string) (*websocket.Conn, *trackedConn, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := websocket.NewConfig(raw, "https://slack.com")
	if err != nil {
		return nil, nil, err
	}
	host, port := u.Hostname(), u.Port()
	d := &net.Dialer{Timeout: 30 * time.Second}
	var nc net.Conn
	switch u.Scheme {
	case "wss":
		if port == "" {
			port = "443"
		}
		td := &tls.Dialer{NetDialer: d, Config: &tls.Config{ServerName: host}}
		nc, err = td.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	case "ws":
		if port == "" {
			port = "80"
		}
		nc, err = d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	default:
		return nil, nil, fmt.Errorf("unsupported websocket scheme %q", u.Scheme)
	}
	if err != nil {
		return nil, nil, err
	}
	tc := &trackedConn{Conn: nc}
	start := time.Now()
	tc.last.Store(&start)
	_ = nc.SetReadDeadline(start.Add(30 * time.Second)) // bound the handshake (writes are bounded per call)
	ws, err := websocket.NewClient(cfg, tc)
	if err != nil {
		nc.Close()
		return nil, nil, err
	}
	_ = nc.SetReadDeadline(time.Time{})
	return ws, tc, nil
}
