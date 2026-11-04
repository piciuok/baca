// Package activity records what was on screen and where code changed during the day, and turns
// it into suggested worklog blocks. Raw data stays local: one JSON line per span in
// <dir>/YYYY-MM-DD.jsonl (mode 600, window titles can be private).
package activity

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Span is a stretch of time with one state. Kinds:
//   - window: App/Title in front, user active
//   - idle: no input for a while (App is what was in front)
//   - meeting: a call window of App titled Title was open (Slack huddle, Zoom, Teams),
//     or a calendar event (App: Calendar)
//   - repo: the git repo Repo (on Branch) changed during the span
//   - claude: a Claude Code session in Repo wrote during the span
//   - agent: a coding agent (App: Claude Code, Codex) worked in Repo
//   - remote: an action in a web service (App: GitLab, Jira, ...) at To; From is a short lead-in
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
	Enabled *bool  `json:"enabled"` // default true
	Dir     string `json:"dir"`     // default: "activity" next to the worklog file
	// RetentionDays deletes activity files older than this many days; 0 keeps everything.
	RetentionDays int            `json:"retentionDays"`
	System        SystemConfig   `json:"system"`
	Git           GitConfig      `json:"git"`
	Claude        ClaudeConfig   `json:"claude"`
	Codex         CodexConfig    `json:"codex"`
	GitLab        GitLabConfig   `json:"gitlab"`
	Jira          JiraConfig     `json:"jira"`
	GitHub        GitHubConfig   `json:"github"`
	Calendar      CalendarConfig `json:"calendar"`
	Slack         SlackConfig    `json:"slack"`

	JiraAuth JiraAuth `json:"-"` // filled from the Jira/Tempo credentials of config.json
}

type SystemConfig struct {
	Enabled    *bool    `json:"enabled"`
	Interval   int      `json:"intervalSeconds"`  // default 5
	IdleAfter  int      `json:"idleAfterSeconds"` // default 180
	IgnoreApps []string `json:"ignoreApps"`       // their window titles are never stored
	// Meetings: app -> regexp of a window title that means a call is on (Slack huddle, Zoom, Teams).
	// During a call no input is normal, so it counts as work, not idle.
	Meetings map[string]string `json:"meetings"`
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
	if len(c.System.IgnoreApps) == 0 { // an empty list from the settings form means "the defaults"
		c.System.IgnoreApps = []string{"1Password", "Bitwarden", "KeePassXC", "Keychain Access", "Dostęp do pęku kluczy"}
	}
	if len(c.System.Meetings) == 0 {
		c.System.Meetings = map[string]string{ // the keys are app names: macOS processes, Linux WM_CLASS
			"Slack":           `(?i)huddle|rozmowa`,
			"zoom.us":         `(?i)zoom meeting|spotkanie|meeting`,
			"Microsoft Teams": `(?i)meeting|spotkanie|call|połączenie|rozmowa`,
		}
		if runtime.GOOS == "linux" {
			c.System.Meetings["zoom"] = `(?i)zoom meeting|spotkanie|meeting`
			c.System.Meetings["teams-for-linux"] = `(?i)meeting|spotkanie|call|połączenie|rozmowa`
		}
	}
}

// Probes builds the enabled probes.
func (c Config) Probes() []Probe {
	if !on(c.Enabled) {
		return nil
	}
	var out []Probe
	if on(c.System.Enabled) {
		out = append(out, NewSystemProbe(c.System, c.Git))
	}
	if on(c.Git.Enabled) && len(c.Git.Repos) > 0 {
		out = append(out, NewGitProbe(c.Git))
	}
	if on(c.Claude.Enabled) {
		out = append(out, NewClaudeProbe(c.Git))
	}
	if on(c.Codex.Enabled) {
		out = append(out, NewCodexProbe(c.Git))
	}
	if on(c.GitLab.Enabled) && len(c.GitLab.Instances) > 0 {
		out = append(out, NewGitLabProbe(c.GitLab))
	}
	if on(c.GitHub.Enabled) && len(c.GitHub.Accounts) > 0 {
		out = append(out, NewGitHubProbe(c.GitHub))
	}
	if on(c.Calendar.Enabled) && len(c.Calendar.URLs) > 0 {
		cal := c.Calendar
		if cal.Email == "" {
			cal.Email = c.JiraAuth.Email
		}
		out = append(out, NewCalendarProbe(cal))
	}
	if on(c.Slack.Enabled) && c.Slack.Token != "" {
		out = append(out, NewSlackProbe(c.Slack))
	}
	if on(c.Jira.Enabled) && c.JiraAuth.Token != "" {
		out = append(out, NewJiraProbe(c.JiraAuth))
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

// Snapshot is a probe that rebuilds whole days from a remote service on every tick; the
// collector replaces the probe's file of each day in Days with what Tick returned.
type Snapshot interface {
	Days(now time.Time) []string
}

func (l Log) snapshotPath(date, probe string) string {
	return filepath.Join(l.Dir, date+"."+probe+".jsonl")
}

// Replace atomically rewrites one probe's snapshot of a day.
func (l Log) Replace(date, probe string, spans []Span) error {
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(l.Dir, "."+date+"."+probe+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	w := bufio.NewWriter(tmp)
	for _, s := range spans {
		s.Date = ""
		b, _ := json.Marshal(s)
		w.Write(b)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), l.snapshotPath(date, probe))
}

// Read returns the day's spans. Touching pieces of one state are merged (long spans are
// written in one-minute pieces), and a window/idle span written later wins over the part of
// earlier ones it overlaps (idle reaches back to the last input).
func (l Log) Read(date string) ([]Span, error) {
	out, err := l.readLocal(date)
	if err != nil {
		return nil, err
	}
	snaps, _ := filepath.Glob(filepath.Join(l.Dir, date+".*.jsonl"))
	for _, p := range snaps {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(b), "\n") {
			var s Span
			if json.Unmarshal([]byte(line), &s) == nil && s.End() >= s.Start() {
				out = append(out, s)
			}
		}
	}
	return out, nil
}

func (l Log) readLocal(date string) ([]Span, error) {
	f, err := os.Open(l.path(date))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// times are parsed once; screen spans (window/idle) arrive in time order, so a later span
	// can only overlap the screen spans at the end of the list: the trim walks back from there
	type item struct {
		s          Span
		start, end int
	}
	var out []item
	var screens []int // indexes into out
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var s Span
		if json.Unmarshal(sc.Bytes(), &s) != nil {
			continue // a torn last line after a crash
		}
		start, end := s.Start(), s.End()
		if end <= start {
			continue // an empty piece
		}
		if screen(s) {
			for k := len(screens) - 1; k >= 0; k-- {
				o := &out[screens[k]]
				if o.end <= start {
					if o.end > o.start { // a live span wholly before this one: so is everything earlier
						break
					}
					continue
				}
				if o.start >= start {
					o.end = o.start // fully covered: dropped below
				} else {
					o.end = start
				}
			}
		}
		// merge with the latest span of the same state that ends where this one starts
		merged := false
		for i := len(out) - 1; i >= 0 && i >= len(out)-8; i-- {
			if sameState(out[i].s, s) && out[i].end >= start-1 && out[i].end <= end {
				out[i].end = end
				merged = true
				break
			}
		}
		if merged {
			continue
		}
		if screen(s) {
			screens = append(screens, len(out))
		}
		out = append(out, item{s, start, end})
	}
	kept := make([]Span, 0, len(out))
	for _, it := range out {
		if it.end > it.start {
			it.s.From, it.s.To = secsClock(it.start), secsClock(it.end)
			kept = append(kept, it.s)
		}
	}
	return kept, sc.Err()
}

func secsClock(s int) string { return fmt.Sprintf("%02d:%02d:%02d", s/3600, s/60%60, s%60) }

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

// Prune removes the day files (and snapshots) of days before cutoff.
func (l Log) Prune(cutoff time.Time) (int, error) {
	files, err := filepath.Glob(filepath.Join(l.Dir, "????-??-??*.jsonl"))
	if err != nil {
		return 0, err
	}
	limit := dayOf(cutoff)
	n := 0
	for _, f := range files {
		if d := filepath.Base(f)[:10]; d < limit {
			if os.Remove(f) == nil {
				n++
			}
		}
	}
	return n, nil
}
