package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func ghTestItem(n int) ghItem {
	it := ghItem{HTMLURL: fmt.Sprintf("https://github.com/example-org/example-repo/pull/%d", n), Number: n,
		Title: "feat(ci): example pull request [ABC-123]"}
	it.User.Login = "octocat"
	return it
}

func TestGitHubRender(t *testing.T) {
	out := ghRender([]ghFound{{"個人指名", ghTestItem(396)}})
	for _, want := range []string{
		"[GitHub] 1 筆新的 review request",
		"example-org/example-repo #396",
		"octocat", "個人指名",
		"https://github.com/example-org/example-repo/pull/396",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("render missing %q:\n%s", want, out)
		}
	}
	if !strings.HasPrefix(out, "[GitHub] 1 筆新的 review request") {
		t.Fatalf("header first: %q", out)
	}
}

func TestGitHubRenderLineCap(t *testing.T) {
	var many []ghFound
	for i := 0; i < 30; i++ {
		many = append(many, ghFound{"個人指名", ghTestItem(i)})
	}
	out := ghRender(many)
	if !strings.Contains(out, fmt.Sprintf("…另有 %d 筆未列出", 30-maxLines)) {
		t.Fatalf("overflow must say how many were held back:\n%s", out)
	}
	if got := strings.Count(out, "• "); got != maxLines {
		t.Fatalf("listed %d lines, want %d", got, maxLines)
	}
	if utf8.RuneCountInString(out) >= 4000 {
		t.Fatalf("body must stay under the 4,000-char chat cap, got %d", utf8.RuneCountInString(out))
	}
}

func TestGitHubQueries(t *testing.T) {
	cfg := &Config{}
	cfg.GitHub.Teams = []string{"example-org/a", "example-org/b"}
	cfg.GitHub.ExcludeAuthors = []string{"app/renovate"}
	qs := ghQueries(cfg)
	if len(qs) != 3 {
		t.Fatalf("personal + one per team, got %d", len(qs))
	}
	if !strings.Contains(qs[0].q, "review-requested:@me") || qs[0].why != "個人指名" {
		t.Fatalf("personal query: %+v", qs[0])
	}
	if !strings.Contains(qs[1].q, "team-review-requested:example-org/a") || qs[1].why != "team a" {
		t.Fatalf("team query: %+v", qs[1])
	}
	for _, q := range qs {
		if !strings.Contains(q.q, "-author:app/renovate") || !strings.HasPrefix(q.q, "is:open is:pr archived:false") {
			t.Fatalf("query %q", q.q)
		}
	}
}

// TestGitHubWatchSeedsThenReports covers the run lifecycle: a run without
// github_seen.json reports nothing; the next run reports only new PRs, once,
// with the first reason found; a failed post leaves them for the next run.
func TestGitHubWatchSeedsThenReports(t *testing.T) {
	f := newFakeOffiCraft(t)
	dir := t.TempDir()
	cfg := &Config{hasGitHub: true}
	cfg.GitHub.Teams = []string{"example-org/a"}
	cfg.OffiCraft.To = "example-member"

	results := map[string][]ghItem{}
	search := func(q string) ([]ghItem, error) {
		if strings.Contains(q, "team-review-requested") {
			return results["team"], nil
		}
		return results["me"], nil
	}
	var postedTo []string
	poster := func(to string) (*OffiCraft, error) {
		postedTo = append(postedTo, to)
		oc := newOffiCraft(f.srv.URL, testCred(t, f.srv.URL), to)
		oc.backoffUnit = 0
		return oc, nil
	}

	results["me"] = []ghItem{ghTestItem(1), ghTestItem(2)}
	if err := githubWatch(cfg, dir, search, poster); err != nil {
		t.Fatal(err)
	}
	if _, calls, _ := f.snapshot(); calls != 0 || len(postedTo) != 0 {
		t.Fatal("the seeding run must report nothing")
	}
	if _, err := os.Stat(filepath.Join(dir, "github_seen.json")); err != nil {
		t.Fatal("seeding must write github_seen.json")
	}

	// PR 3 is requested both personally and via the team: one line, first reason.
	results["me"] = []ghItem{ghTestItem(1), ghTestItem(2), ghTestItem(3)}
	results["team"] = []ghItem{ghTestItem(3), ghTestItem(4)}
	f.chatStatus = func(int) int { return http.StatusServiceUnavailable }
	if err := githubWatch(cfg, dir, search, poster); err == nil {
		t.Fatal("a failed post must surface as an error")
	}
	f.chatStatus = nil
	if err := githubWatch(cfg, dir, search, poster); err != nil {
		t.Fatal(err)
	}
	posts, _, _ := f.snapshot()
	if len(posts) != 1 {
		t.Fatalf("want one report after the retry, got %d", len(posts))
	}
	body := posts[0].Body
	if !strings.HasPrefix(body, "[GitHub] 2 筆新的 review request") ||
		!strings.Contains(body, "#3 — ") || !strings.Contains(body, "#4 — ") || strings.Contains(body, "#1 — ") {
		t.Fatalf("report body:\n%s", body)
	}
	if strings.Count(body, "pull/3") != 1 || !strings.Contains(body, "個人指名 · 作者 octocat · https://github.com/example-org/example-repo/pull/3") {
		t.Fatalf("PR 3 must appear once with the first reason:\n%s", body)
	}
	if postedTo[len(postedTo)-1] != "example-member" {
		t.Fatalf("recipient falls back to [officraft].to, got %v", postedTo)
	}

	if err := githubWatch(cfg, dir, search, poster); err != nil {
		t.Fatal(err)
	}
	if posts, _, _ := f.snapshot(); len(posts) != 1 {
		t.Fatal("already-reported PRs must not be reported again")
	}
}

func TestGitHubWatchNeedsSection(t *testing.T) {
	if err := githubWatch(&Config{}, t.TempDir(), nil, nil); err == nil || !strings.Contains(err.Error(), "[github]") {
		t.Fatalf("want missing [github] error, got %v", err)
	}
}
