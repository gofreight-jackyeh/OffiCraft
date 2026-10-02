package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSeenDedupSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seen.json")
	s, err := openSeen(path, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	claim := func(s *seenStore, k string) bool {
		ok, err := s.claim(k, now)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !claim(s, "Ev1") {
		t.Fatal("first sighting must pass")
	}
	if claim(s, "Ev1") {
		t.Fatal("re-delivery must be blocked")
	}
	if !claim(s, "Ev2") {
		t.Fatal("a different event is unaffected")
	}
	s2, err := openSeen(path, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.existed || claim(s2, "Ev1") {
		t.Fatal("dedup must survive a restart")
	}
}

func TestSeenPrunesByAge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seen.json")
	s, _ := openSeen(path, 7*24*time.Hour)
	old := time.Now().Add(-8 * 24 * time.Hour)
	s.add("old", old)
	if _, err := s.claim("new", time.Now()); err != nil {
		t.Fatal(err)
	}
	s2, _ := openSeen(path, 7*24*time.Hour)
	if _, ok := s2.m["old"]; ok {
		t.Fatal("entry older than keep must be pruned")
	}
	if _, ok := s2.m["new"]; !ok {
		t.Fatal("fresh entry must be kept")
	}
}

func TestRender(t *testing.T) {
	var names *nameCache // no bot token: ids are shown as-is
	out := render(slackEvent{User: "U0EXAMPLE", Channel: "C0EXAMPLE", Text: "hello"}, names)
	if !strings.Contains(out, "C0EXAMPLE") || !strings.Contains(out, "U0EXAMPLE") || !strings.Contains(out, "hello") {
		t.Fatalf("render must carry channel, user and text: %q", out)
	}
	if out := render(slackEvent{User: "U0EXAMPLE", Channel: "C0EXAMPLE"}, names); !strings.Contains(out, "(無文字內容)") {
		t.Fatalf("empty message must not render an empty body: %q", out)
	}
	im := render(slackEvent{User: "U0EXAMPLE", Channel: "D0EXAMPLE", ChannelType: "im", Text: "hi"}, names)
	if !strings.HasPrefix(im, "[Slack 私訊] U0EXAMPLE:") || strings.Contains(im, "D0EXAMPLE") {
		t.Fatalf("DM must say 私訊 and hide the meaningless channel id: %q", im)
	}
	if mp := render(slackEvent{User: "U0EXAMPLE", Channel: "G0EXAMPLE", ChannelType: "mpim", Text: "hi"}, names); !strings.HasPrefix(mp, "[Slack 群組私訊]") {
		t.Fatalf("group DM has its own prefix: %q", mp)
	}
	if ch := render(slackEvent{User: "U0EXAMPLE", Channel: "C0EXAMPLE", ChannelType: "channel", Text: "hi"}, names); ch != "[Slack #C0EXAMPLE] U0EXAMPLE:\nhi" {
		t.Fatalf("public channel still shows the channel: %q", ch)
	}
}

func testBridge(t *testing.T, oc *OffiCraft, ignore ...string) *bridge {
	t.Helper()
	dir := t.TempDir()
	seen, err := openSeen(filepath.Join(dir, "seen.json"), 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ig := map[string]bool{}
	for _, u := range ignore {
		ig[u] = true
	}
	return &bridge{oc: oc, seen: seen, deadletter: filepath.Join(dir, "deadletter.jsonl"), ignore: ig}
}

// frame builds a Socket Mode events_api envelope.
func frame(t *testing.T, envID, eventID, user string, isBot bool) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"envelope_id": envID,
		"type":        "events_api",
		"payload": map[string]any{
			"event_id":       eventID,
			"authorizations": []map[string]any{{"is_bot": isBot, "user_id": "U0EXAMPLE"}},
			"event": map[string]any{"type": "message", "user": user, "channel": "C0EXAMPLE",
				"channel_type": "channel", "ts": "1789.0001", "text": "same message"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// feed runs one frame through onFrame with an inline runner, counting acks.
func feed(t *testing.T, b *bridge, raw []byte, acks *[]string) {
	t.Helper()
	if _, err := b.onFrame(raw, func(id string) error { *acks = append(*acks, id); return nil }, func(f func()) { f() }); err != nil {
		t.Fatal(err)
	}
}

func TestIgnoreOwnMessageButStillAck(t *testing.T) {
	f := newFakeOffiCraft(t)
	b := testBridge(t, testPoster(t, f), "U0SELF")
	var acks []string
	own := frame(t, "env-own", "Ev-own", "U0SELF", true)
	feed(t, b, own, &acks)
	if posts, _, _ := f.snapshot(); len(posts) != 0 {
		t.Fatalf("own message must not be forwarded: %+v", posts)
	}
	other := []byte(strings.Replace(string(frame(t, "env-other", "Ev-other", "U0EXAMPLE", true)), "1789.0001", "1789.0002", 1))
	feed(t, b, other, &acks)
	posts, _, _ := f.snapshot()
	if len(posts) != 1 || !strings.Contains(posts[0].Body, "U0EXAMPLE") {
		t.Fatalf("others' message must be forwarded: %+v", posts)
	}
	if len(acks) != 2 || acks[0] != "env-own" || acks[1] != "env-other" {
		t.Fatalf("both envelopes must be acked (else Slack re-sends): %v", acks)
	}
}

func TestSameMessageSeenByBotAndUserAuthorizationDeliveredOnce(t *testing.T) {
	f := newFakeOffiCraft(t)
	b := testBridge(t, testPoster(t, f))
	var acks []string
	feed(t, b, frame(t, "env-1", "EvBOT", "U0OTHER", true), &acks)   // seen via the bot authorization
	feed(t, b, frame(t, "env-2", "EvUSER", "U0OTHER", false), &acks) // same message via the user one, new event_id
	posts, _, _ := f.snapshot()
	if len(posts) != 1 {
		t.Fatalf("one message across two authorizations must be delivered once, got %d", len(posts))
	}
	if len(acks) != 2 {
		t.Fatalf("both must be acked: %v", acks)
	}
	var meta struct {
		Source string            `json:"source"`
		Slack  map[string]string `json:"slack"`
	}
	if err := json.Unmarshal(posts[0].Meta, &meta); err != nil || meta.Source != "slack-socket-mode" ||
		meta.Slack["event_id"] != "EvBOT" || meta.Slack["channel"] != "C0EXAMPLE" || meta.Slack["ts"] != "1789.0001" {
		t.Fatalf("meta.slack: %s (%v)", posts[0].Meta, err)
	}
}

func TestDedupAcrossRestart(t *testing.T) {
	f := newFakeOffiCraft(t)
	b := testBridge(t, testPoster(t, f))
	var acks []string
	feed(t, b, frame(t, "env-1", "Ev1", "U0OTHER", true), &acks)
	reopened, err := openSeen(b.seen.path, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	b.seen = reopened // a restarted process re-reads seen.json
	feed(t, b, frame(t, "env-1", "Ev1", "U0OTHER", true), &acks)
	if posts, _, _ := f.snapshot(); len(posts) != 1 {
		t.Fatalf("re-delivery after restart must be dropped, got %d posts", len(posts))
	}
}

func TestSkipsBotsAndNoiseSubtypes(t *testing.T) {
	f := newFakeOffiCraft(t)
	b := testBridge(t, testPoster(t, f))
	for i, ev := range []string{
		`{"type":"message","bot_id":"B0EXAMPLE","channel":"C0EXAMPLE","ts":"1.1","text":"x"}`,
		`{"type":"message","subtype":"message_changed","channel":"C0EXAMPLE","ts":"1.2"}`,
		`{"type":"message","subtype":"channel_join","channel":"C0EXAMPLE","ts":"1.3"}`,
		`{"type":"reaction_added","channel":"C0EXAMPLE","ts":"1.4"}`,
	} {
		b.handleEvent(json.RawMessage(`{"event_id":"Ev` + string(rune('a'+i)) + `","event":` + ev + `}`))
	}
	if _, calls, _ := f.snapshot(); calls != 0 {
		t.Fatalf("bot / noise events must not be delivered, got %d calls", calls)
	}
}

func TestDeliveryFailureGoesToDeadletter(t *testing.T) {
	f := newFakeOffiCraft(t)
	f.chatStatus = func(int) int { return http.StatusServiceUnavailable }
	b := testBridge(t, testPoster(t, f))
	var acks []string
	feed(t, b, frame(t, "env-dl", "EvDL", "U0OTHER", true), &acks) // must not panic
	data, err := os.ReadFile(b.deadletter)
	if err != nil {
		t.Fatalf("deadletter not written: %v", err)
	}
	var line struct {
		At      float64         `json:"at"`
		Error   string          `json:"error"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(data, &line); err != nil || line.At == 0 || line.Error == "" ||
		!strings.Contains(string(line.Payload), "same message") {
		t.Fatalf("deadletter line %s (%v)", data, err)
	}
	if len(acks) != 1 {
		t.Fatal("the envelope must still be acked")
	}
}

func TestConfigCompat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, []byte(`
[slack]
app_token = "xapp-example"
ignore_user_ids = "U0EXAMPLE"
[officraft]
base_url = "http://127.0.0.1:7755"
to = "example-member"
[bridge]
state_dir = "~/state"
`), 0o600)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.validateSlack(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Slack.IgnoreUserIDs) != 1 || cfg.Slack.IgnoreUserIDs[0] != "U0EXAMPLE" {
		t.Fatalf("a bare-string ignore_user_ids must be accepted: %v", cfg.Slack.IgnoreUserIDs)
	}
	if got := cfg.stateDir("/home/octocat"); got != "/home/octocat/state" {
		t.Fatalf("state_dir ~ expansion: %s", got)
	}
	cfg.Bridge.StateDir = ""
	if got := cfg.stateDir("/home/octocat"); got != dir {
		t.Fatalf("default state_dir is the config folder: %s", got)
	}
	cfg.Slack.AppToken = "xoxb-wrong"
	if cfg.validateSlack() == nil {
		t.Fatal("a non-xapp app_token must be rejected")
	}
	if cfg.hasGitHub {
		t.Fatal("no [github] table here")
	}
}

func TestHelpExitsZero(t *testing.T) {
	for _, args := range [][]string{nil, {"--help"}} {
		var out, errOut strings.Builder
		if code := realMain(args, func(string) string { return "" }, &out, &errOut); code != 0 || !strings.Contains(out.String(), "github-watch") {
			t.Fatalf("%v: code %d, out %q", args, code, out.String())
		}
	}
}
