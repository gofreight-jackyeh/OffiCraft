package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// github-watch: GitHub review requests -> OffiCraft chat.
//
// Runs ONE pass and exits; launchd re-runs it hourly. A long-lived process
// would buy nothing here — there is no connection to hold, and a crash between
// runs would be invisible.
//
// Why polling rather than a webhook: GitHub cannot subscribe to a single
// ACTION, so a pull_request webhook delivers every opened/synchronize/labeled/
// closed event too, at ~26 KB of raw JSON each, into a chat body that folds at
// 4,000 chars. The search API answers the actual question — "which open PRs
// request my review" — in one call.
//
// A RUN WITHOUT github_seen.json SEEDS, IT DOES NOT REPORT. The standing
// backlog was >100 PRs; sending that as a welcome message would bury the
// signal this exists to deliver. This also covers the migration from the
// Python watcher, whose sqlite state is not carried over.

// maxLines: one chat body caps at 4,000 chars and a PR line is ~120. Stop well
// short and say how many were held back, rather than silently losing the tail.
const maxLines = 20

const githubKeep = 180 * 24 * time.Hour

type ghItem struct {
	HTMLURL string `json:"html_url"`
	Number  int    `json:"number"`
	Title   string `json:"title"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
}

type ghQuery struct{ why, q string }

type ghFound struct {
	why  string
	item ghItem
}

// ghSearch runs one search/issues page via the gh CLI. gh is used rather than
// the REST API so this process never handles the GitHub token: gh reads its
// own credential from ~/.config/gh/hosts.yml, which a launchd job can reach.
func ghSearch(query string) ([]ghItem, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "gh", "api", "-X", "GET", "search/issues",
		"--field", "q="+query, "--field", "per_page=100")
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("gh failed (%v): %s", err, msg)
	}
	var r struct {
		Items []ghItem `json:"items"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		return nil, fmt.Errorf("gh output: %w", err)
	}
	return r.Items, nil
}

func ghQueries(cfg *Config) []ghQuery {
	excl := make([]string, 0, len(cfg.GitHub.ExcludeAuthors))
	for _, a := range cfg.GitHub.ExcludeAuthors {
		excl = append(excl, "-author:"+a)
	}
	ex := strings.Join(excl, " ")
	const base = "is:open is:pr archived:false"
	out := []ghQuery{{"個人指名", strings.TrimSpace(base + " review-requested:@me " + ex)}}
	for _, team := range cfg.GitHub.Teams {
		short := team[strings.LastIndex(team, "/")+1:]
		out = append(out, ghQuery{"team " + short,
			strings.TrimSpace(base + " team-review-requested:" + team + " " + ex)})
	}
	return out
}

func ghRender(found []ghFound) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[GitHub] %d 筆新的 review request\n\n", len(found))
	lines := make([]string, 0, maxLines+1)
	for i, f := range found {
		if i == maxLines {
			break
		}
		repo := "?"
		if parts := strings.Split(f.item.HTMLURL, "/"); f.item.HTMLURL != "" {
			repo = strings.Join(parts[min(3, len(parts)):min(5, len(parts))], "/")
		}
		title := []rune(strings.TrimSpace(f.item.Title))
		if len(title) > 90 {
			title = title[:90]
		}
		login := f.item.User.Login
		if login == "" {
			login = "?"
		}
		lines = append(lines, fmt.Sprintf("• %s #%d — %s\n  %s · 作者 %s · %s",
			repo, f.item.Number, string(title), f.why, login, f.item.HTMLURL))
	}
	if len(found) > maxLines {
		lines = append(lines, fmt.Sprintf("…另有 %d 筆未列出", len(found)-maxLines))
	}
	b.WriteString(strings.Join(lines, "\n"))
	return b.String()
}

// githubWatch is one pass. search is injected so tests run without gh;
// newPoster builds the OffiCraft client only when there is something to send.
func githubWatch(cfg *Config, stateDir string, search func(string) ([]ghItem, error),
	newPoster func(to string) (*OffiCraft, error)) error {
	if !cfg.hasGitHub {
		return fmt.Errorf("%s: missing [github] section", cfg.path)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	seen, err := openSeen(filepath.Join(stateDir, "github_seen.json"), githubKeep)
	if err != nil {
		return fmt.Errorf("%w (delete it to reseed; the next run then reports nothing)", err)
	}

	var found []ghFound
	byURL := map[string]bool{}
	for _, q := range ghQueries(cfg) {
		items, err := search(q.q)
		if err != nil {
			return err
		}
		log.Printf("%s: %d 筆 (%s)", q.why, len(items), q.q)
		for _, it := range items {
			// A PR requested BOTH personally and via a team is still one PR;
			// the url is its identity and the first reason found wins.
			if it.HTMLURL == "" || byURL[it.HTMLURL] {
				continue
			}
			byURL[it.HTMLURL] = true
			found = append(found, ghFound{q.why, it})
		}
	}

	now := time.Now()
	var fresh []ghFound
	for _, f := range found {
		if seen.add(f.item.HTMLURL, now) {
			fresh = append(fresh, f)
		}
	}

	if !seen.existed {
		if err := seen.save(now); err != nil {
			return err
		}
		log.Printf("first run: seeded %d existing requests, reporting none", len(found))
		return nil
	}
	if len(fresh) == 0 {
		log.Printf("no new review requests (%d open in total)", len(found))
		return nil
	}

	to := cfg.GitHub.To
	if to == "" {
		to = cfg.OffiCraft.To
	}
	oc, err := newPoster(to)
	if err != nil {
		return err
	}
	urls := make([]string, len(fresh))
	for i, f := range fresh {
		urls[i] = f.item.HTMLURL
	}
	if err := oc.Post(ghRender(fresh), map[string]any{
		"source": "github-review-watch",
		"github": map[string]any{"count": len(fresh), "urls": urls},
	}); err != nil {
		// Not saved: the same requests are reported on the next run.
		return err
	}
	log.Printf("reported %d new review request(s)", len(fresh))
	return seen.save(now)
}
