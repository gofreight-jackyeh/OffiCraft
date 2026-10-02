package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config mirrors the config.toml the Python bridge already reads, key for key,
// so a deployment can switch binaries without touching its config. See
// config.example.toml for the annotated shape.
type Config struct {
	Slack struct {
		AppToken      string     `toml:"app_token"`
		BotToken      string     `toml:"bot_token"`
		IgnoreUserIDs stringList `toml:"ignore_user_ids"`
	} `toml:"slack"`
	OffiCraft struct {
		BaseURL string `toml:"base_url"`
		To      string `toml:"to"`
	} `toml:"officraft"`
	Bridge struct {
		StateDir string `toml:"state_dir"`
	} `toml:"bridge"`
	GitHub struct {
		Teams          []string `toml:"teams"`
		ExcludeAuthors []string `toml:"exclude_authors"`
		To             string   `toml:"to"`
	} `toml:"github"`

	path      string // the file it was read from
	hasGitHub bool   // a [github] table is present
}

// stringList accepts either `"U1"` or `["U1", "U2"]` — the Python bridge
// tolerated a bare string for ignore_user_ids, so existing configs may use it.
type stringList []string

func (s *stringList) UnmarshalTOML(v any) error {
	switch x := v.(type) {
	case string:
		*s = []string{x}
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			str, ok := e.(string)
			if !ok {
				return fmt.Errorf("expected a list of strings, got %T", e)
			}
			out = append(out, str)
		}
		*s = out
	default:
		return fmt.Errorf("expected a string or a list of strings, got %T", v)
	}
	return nil
}

// configPath: $BRIDGE_CONFIG, else ~/.officraft-slack-bridge/config.toml.
func configPath(env func(string) string) string {
	if p := env("BRIDGE_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(env("HOME"), ".officraft-slack-bridge", "config.toml")
}

func loadConfig(path string) (*Config, error) {
	cfg := &Config{path: path}
	md, err := toml.DecodeFile(path, cfg)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	cfg.hasGitHub = md.IsDefined("github")
	return cfg, nil
}

// validateSlack is the bridge's startup check (Python load_config).
func (c *Config) validateSlack() error {
	for _, k := range []struct{ name, val string }{
		{"[slack].app_token", c.Slack.AppToken},
		{"[officraft].base_url", c.OffiCraft.BaseURL},
		{"[officraft].to", c.OffiCraft.To},
	} {
		if k.val == "" {
			return fmt.Errorf("config %s: missing required key %s", c.path, k.name)
		}
	}
	if !strings.HasPrefix(c.Slack.AppToken, "xapp-") {
		return fmt.Errorf("config %s: [slack].app_token must be an app-level token (xapp-...)", c.path)
	}
	return nil
}

// stateDir: [bridge].state_dir with ~ expanded, else the config file's folder.
func (c *Config) stateDir(home string) string {
	d := c.Bridge.StateDir
	if d == "" {
		return filepath.Dir(c.path)
	}
	if d == "~" {
		return home
	}
	if strings.HasPrefix(d, "~/") {
		return filepath.Join(home, d[2:])
	}
	return d
}

// writeFileAtomic replaces path in one rename, so a crash mid-write never
// leaves a torn credential or dedup file behind. The temp file is created
// 0600 by os.CreateTemp; perm is applied explicitly anyway.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
