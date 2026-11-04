// Package activity records what was on screen and where code changed during the day, and turns
// it into suggested worklog blocks. Raw data stays local: one JSON line per span in
// <dir>/YYYY-MM-DD.jsonl (mode 600, window titles can be private).
package activity

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Span is a stretch of time with one state. Kinds:
//   - window: App/Title in front, user active
//   - idle: no input for a while (App is what was in front)
//   - repo: the git repo Repo (on Branch) changed during the span
//   - claude: a Claude Code session in Repo wrote during the span
type Span struct {
	Date   string `json:"date,omitempty"`  // only in memory; the file name holds the day
	Probe  string `json:"probe,omitempty"` // which probe wrote it
	From   string `json:"from"`            // 15:04:05
	To     string `json:"to"`
	Kind   string `json:"kind"`
	App    string `json:"app,omitempty"`
	Title  string `json:"title,omitempty"`
	Repo   string `json:"repo,omitempty"`
	Branch string `json:"branch,omitempty"`
}

func (s Span) Start() int { return secs(s.From) }
func (s Span) End() int   { return secs(s.To) }

func secs(hms string) int {
	t, err := time.Parse("15:04:05", hms)
	if err != nil {
		return 0
	}
	return t.Hour()*3600 + t.Minute()*60 + t.Second()
}

func clock(t time.Time) string { return t.Format("15:04:05") }

// Config is the "activity" section of config.json.
type Config struct {
	Enabled *bool        `json:"enabled"` // default true
	Dir     string       `json:"dir"`     // default: "activity" next to the worklog file
	System  SystemConfig `json:"system"`
	Git     GitConfig    `json:"git"`
	Claude  ClaudeConfig `json:"claude"`
}

type SystemConfig struct {
	Enabled    *bool    `json:"enabled"`
	Interval   int      `json:"intervalSeconds"`  // default 5
	IdleAfter  int      `json:"idleAfterSeconds"` // default 180
	IgnoreApps []string `json:"ignoreApps"`       // their window titles are never stored
}

type GitConfig struct {
	Enabled *bool    `json:"enabled"`
	Repos   []string `json:"repos"` // globs of git repos / worktrees, e.g. ~/Projects/*
}

type ClaudeConfig struct {
	Enabled *bool `json:"enabled"`
}

func on(b *bool) bool { return b == nil || *b }

func (c *Config) Defaults(worklogFile string) {
	if c.Dir == "" {
		c.Dir = filepath.Join(filepath.Dir(worklogFile), "activity")
	}
	c.Dir = expandHome(c.Dir)
	if c.System.Interval <= 0 {
		c.System.Interval = 5
	}
	if c.System.IdleAfter <= 0 {
		c.System.IdleAfter = 180
	}
	if c.System.IgnoreApps == nil {
		c.System.IgnoreApps = []string{"1Password", "Bitwarden", "KeePassXC", "Keychain Access", "Dostęp do pęku kluczy"}
	}
}

// Probes builds the enabled probes.
func (c Config) Probes() []Probe {
	if !on(c.Enabled) {
		return nil
	}
	var out []Probe
	if on(c.System.Enabled) {
		out = append(out, NewSystemProbe(c.System))
	}
	if on(c.Git.Enabled) && len(c.Git.Repos) > 0 {
		out = append(out, NewGitProbe(c.Git))
	}
	if on(c.Claude.Enabled) {
		out = append(out, NewClaudeProbe(c.Git))
	}
	return out
}

// Log appends spans to the day files.
type Log struct{ Dir string }

func (l Log) path(date string) string { return filepath.Join(l.Dir, date+".jsonl") }

func (l Log) Append(spans ...Span) error {
	if len(spans) == 0 {
		return nil
	}
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return err
	}
	byDay := map[string][]Span{}
	var order []string
	for _, s := range spans {
		if _, ok := byDay[s.Date]; !ok {
			order = append(order, s.Date)
		}
		byDay[s.Date] = append(byDay[s.Date], s)
	}
	for _, d := range order {
		f, err := os.OpenFile(l.path(d), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		w := bufio.NewWriter(f)
		for _, s := range byDay[d] {
			s.Date = ""
			b, _ := json.Marshal(s)
			w.Write(b)
			w.WriteByte('\n')
		}
		err = w.Flush()
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Read returns the day's spans. Touching pieces of one state are merged (long spans are
// written in one-minute pieces), and a window/idle span written later wins over the part of
// earlier ones it overlaps (idle reaches back to the last input).
func (l Log) Read(date string) ([]Span, error) {
	f, err := os.Open(l.path(date))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Span
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var s Span
		if json.Unmarshal(sc.Bytes(), &s) != nil || s.End() <= s.Start() {
			continue // a torn last line after a crash, or an empty piece
		}
		if screen(s) {
			for i := range out {
				if screen(out[i]) && out[i].End() > s.Start() && out[i].Start() < s.End() {
					if out[i].Start() >= s.Start() {
						out[i].To = out[i].From // fully covered: dropped below
					} else {
						out[i].To = s.From
					}
				}
			}
		}
		if n := len(out); n > 0 {
			// merge with the latest span of the same state that ends where this one starts
			merged := false
			for i := n - 1; i >= 0 && i >= n-8; i-- {
				if sameState(out[i], s) && out[i].End() >= s.Start()-1 && out[i].End() <= s.End() {
					out[i].To = s.To
					merged = true
					break
				}
			}
			if merged {
				continue
			}
		}
		out = append(out, s)
	}
	kept := out[:0]
	for _, s := range out {
		if s.End() > s.Start() {
			kept = append(kept, s)
		}
	}
	return kept, sc.Err()
}

func screen(s Span) bool { return s.Kind == "window" || s.Kind == "idle" }

func sameState(a, b Span) bool {
	return a.Kind == b.Kind && a.App == b.App && a.Title == b.Title && a.Repo == b.Repo && a.Branch == b.Branch && a.Probe == b.Probe
}

// expandHome turns ~/x into an absolute path.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
