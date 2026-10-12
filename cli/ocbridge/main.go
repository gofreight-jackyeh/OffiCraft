// Command ocbridge forwards Slack messages and GitHub review requests into
// OffiCraft chat as a machine identity. It replaces the Python
// officraft-slack-bridge (bridge.py + github_watch.py) and reads the same
// config.toml and credential.json, so a deployment switches binaries without
// re-claiming. Context, deployment and operational facts: README.md.
//
// Deps: the stdlib plus golang.org/x/net/websocket (Slack Socket Mode) and
// github.com/BurntSushi/toml (config). Dedup state is plain JSON — no sqlite.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

var subcommands = []struct{ name, help string }{
	{"slack", "hold the Slack Socket Mode connection and forward messages (long-running)"},
	{"github-watch", "one pass: report new GitHub review requests, then exit"},
	{"claim <code>", "trade a console claim code for the machine credential (one-time)"},
}

func usage(out io.Writer) {
	fmt.Fprintln(out, "usage: ocbridge <subcommand>")
	fmt.Fprintln(out, "  Slack / GitHub -> OffiCraft chat bridge.")
	fmt.Fprintln(out, "  config: $BRIDGE_CONFIG or ~/.officraft-slack-bridge/config.toml")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "subcommands:")
	for _, s := range subcommands {
		fmt.Fprintf(out, "  %-14s %s\n", s.name, s.help)
	}
}

// realMain is the testable entrypoint: argv without the program name.
func realMain(argv []string, env func(string) string, out, errOut io.Writer) int {
	if len(argv) == 0 {
		usage(out)
		return 0
	}
	switch argv[0] {
	case "-h", "--help", "help":
		usage(out)
		return 0
	case "slack", "github-watch", "claim":
	default:
		fmt.Fprintf(errOut, "ocbridge: unknown subcommand %q\n\n", argv[0])
		usage(errOut)
		return 2
	}
	if argv[0] == "claim" && len(argv) != 2 {
		fmt.Fprintln(errOut, "usage: ocbridge claim <claim-code>")
		return 2
	}

	cfg, err := loadConfig(configPath(env))
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	stateDir := cfg.stateDir(env("HOME"))
	credPath := filepath.Join(stateDir, "credential.json")

	switch argv[0] {
	case "claim":
		if cfg.OffiCraft.BaseURL == "" {
			fmt.Fprintf(errOut, "config %s: missing required key [officraft].base_url\n", cfg.path)
			return 1
		}
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			fmt.Fprintln(errOut, err)
			return 1
		}
		cred, err := newCredential(cfg.OffiCraft.BaseURL, credPath)
		if err == nil {
			err = cred.Claim(argv[1])
		}
		if err != nil {
			fmt.Fprintln(errOut, "claim failed:", err)
			return 1
		}
		fmt.Fprintf(out, "claimed; credential written to %s\n", credPath)
		return 0

	case "github-watch":
		newPoster := func(to string) (*OffiCraft, error) {
			if cfg.OffiCraft.BaseURL == "" || to == "" {
				return nil, fmt.Errorf("config %s: [officraft].base_url and a recipient ([github].to or [officraft].to) are required", cfg.path)
			}
			cred, err := loadedCredential(cfg, credPath)
			if err != nil {
				return nil, err
			}
			return newOffiCraft(cfg.OffiCraft.BaseURL, cred, to), nil
		}
		if err := githubWatch(cfg, stateDir, ghSearch, newPoster); err != nil {
			fmt.Fprintln(errOut, "github-watch:", err)
			return 1
		}
		return 0

	default: // slack
		if err := cfg.validateSlack(); err != nil {
			fmt.Fprintln(errOut, err)
			return 1
		}
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			fmt.Fprintln(errOut, err)
			return 1
		}
		cred, err := loadedCredential(cfg, credPath)
		if err != nil {
			fmt.Fprintln(errOut, err)
			return 1
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		runSlack(ctx, cfg, stateDir, cred)
		log.Printf("shutting down")
		return 0
	}
}

func loadedCredential(cfg *Config, path string) (*Credential, error) {
	cred, err := newCredential(cfg.OffiCraft.BaseURL, path)
	if err != nil {
		return nil, err
	}
	if cred.Token() == "" {
		return nil, fmt.Errorf("no credential at %s — run:  ocbridge claim <claim-code>", path)
	}
	return cred, nil
}

// runSlack wires the long-running bridge and blocks until ctx is cancelled.
func runSlack(ctx context.Context, cfg *Config, stateDir string, cred *Credential) {
	const slackAPI = "https://slack.com/api"
	seenPath := filepath.Join(stateDir, "seen.json")
	seen, err := openSeen(seenPath, 7*24*time.Hour)
	if err != nil {
		// Starting empty only risks a duplicate on re-delivery; refusing to
		// start would lose every message until someone notices.
		log.Printf("ERROR %v — starting with an empty dedup table", err)
		seen = &seenStore{path: seenPath, keep: 7 * 24 * time.Hour, m: map[string]float64{}}
	}
	names := newNameCache(slackAPI, cfg.Slack.BotToken)
	if names == nil {
		log.Printf("WARNING no [slack].bot_token — user/channel ids will not be resolved to names")
	}
	ignore := map[string]bool{}
	for _, u := range cfg.Slack.IgnoreUserIDs {
		ignore[u] = true
	}
	oc := newOffiCraft(cfg.OffiCraft.BaseURL, cred, cfg.OffiCraft.To)
	health := &connectionHealth{send: oc.Post, silenceAfter: cfg.silenceAfter(), lastEvent: time.Now()}
	b := &bridge{oc: oc, seen: seen, names: names,
		deadletter: filepath.Join(stateDir, "deadletter.jsonl"), ignore: ignore, health: health}

	go cred.keepFresh(ctx)
	log.Printf("connecting to Slack (socket mode); delivering to %s", oc.to)
	newSocketClient(cfg.Slack.AppToken, b, health).run(ctx)
	// SIGTERM (including the alert's own `kickstart -k` hint) must not drop
	// messages that were acked but not yet delivered.
	b.drain(shutdownGrace)
}

const shutdownGrace = 10 * time.Second

func main() {
	log.SetFlags(log.LstdFlags)
	os.Exit(realMain(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}
