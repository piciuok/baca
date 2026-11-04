// Package config is baca's config.json: the worklog file, working hours, Jira/Tempo
// credentials and the activity probes with their accounts. It is written atomically with
// mode 600, and secrets are masked whenever the config goes to the UI.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"baca/internal/activity"
	"baca/internal/describe"
	"baca/internal/worklog"
)

// Mask stands for a secret in the UI. Saving it back keeps the stored value.
const Mask = "••••••"

type Config struct {
	WorklogFile  string `json:"worklogFile"`
	AccountEmail string `json:"accountEmail"`
	Tokens       struct {
		Jira  string `json:"jira"`
		Tempo string `json:"temp"`
	} `json:"tokens"`
	JiraBaseURL  string          `json:"jiraBaseUrl"`
	TempoBaseURL string          `json:"tempoBaseUrl"`
	Workday      Workday         `json:"workday"`
	Weekdays     []string        `json:"weekdays"` // names for new day headers, Sunday first, any language
	Updates      Updates         `json:"updates"`
	Activity     activity.Config `json:"activity"`
	Describe     describe.Config `json:"describe"`
}

// Updates: whether the app looks for new releases (every 6 h) and takes pre-releases.
type Updates struct {
	Check      *bool `json:"check"`
	Prerelease bool  `json:"prerelease"`
}

func (u Updates) CheckOn() bool { return u.Check == nil || *u.Check }

// Workday is the visible part of the day and the daily target.
type Workday struct {
	Start       string  `json:"start"`       // "06:00": start of the timeline
	End         string  `json:"end"`         // "18:00": end of the timeline
	TargetHours float64 `json:"targetHours"` // 8: the red line
}

var hhmm = regexp.MustCompile(`^([01]\d|2[0-3]):([0-5]\d)$`)

// Minutes returns start, end and target in minutes, with defaults for empty values.
func (w Workday) Minutes() (start, end, target int) {
	start, end, target = 6*60, 18*60, 8*60
	if m := hhmm.FindStringSubmatch(w.Start); m != nil {
		start = atoi(m[1])*60 + atoi(m[2])
	}
	if m := hhmm.FindStringSubmatch(w.End); m != nil {
		end = atoi(m[1])*60 + atoi(m[2])
	}
	if w.TargetHours > 0 {
		target = int(w.TargetHours * 60)
	}
	return
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

func (c Config) Validate() error {
	if c.Workday.Start != "" && !hhmm.MatchString(c.Workday.Start) {
		return fmt.Errorf("working hours: invalid start %q (HH:MM)", c.Workday.Start)
	}
	if c.Workday.End != "" && !hhmm.MatchString(c.Workday.End) {
		return fmt.Errorf("working hours: invalid end %q (HH:MM)", c.Workday.End)
	}
	s, e, t := c.Workday.Minutes()
	if e-s < 60 {
		return errors.New("working hours: the timeline must span at least an hour")
	}
	if t <= 0 || t > 24*60 {
		return errors.New("working hours: the target must be between 0 and 24 h")
	}
	if len(c.Weekdays) != 0 {
		if len(c.Weekdays) != 7 {
			return errors.New("weekdays: give 7 names, Sunday first")
		}
		for _, d := range c.Weekdays {
			if !worklog.WeekdayRE.MatchString(d) {
				return fmt.Errorf("weekdays: %q must be letters only", d)
			}
		}
	}
	// tokens go to these addresses: https only (http just for a local test server)
	urls := map[string]string{"jiraBaseUrl": c.JiraBaseURL, "tempoBaseUrl": c.TempoBaseURL, "slack baseUrl": c.Activity.Slack.BaseURL}
	for i, in := range c.Activity.GitLab.Instances {
		urls[fmt.Sprintf("gitlab instance %d", i+1)] = in.BaseURL
	}
	for i, a := range c.Activity.GitHub.Accounts {
		urls[fmt.Sprintf("github account %d", i+1)] = a.BaseURL
	}
	for i, u := range c.Activity.Calendar.URLs {
		urls[fmt.Sprintf("calendar %d", i+1)] = strings.Replace(u, "webcal://", "https://", 1)
	}
	for name, raw := range urls {
		if err := checkURL(raw); err != nil {
			return fmt.Errorf("%s: %v", name, err)
		}
	}
	// the worklog file is rewritten on every save: never let it point at a shell rc or a dotfile
	if f := strings.TrimSpace(c.WorklogFile); f != "" {
		base := filepath.Base(f)
		if strings.HasPrefix(base, ".") || !(strings.HasSuffix(strings.ToLower(base), ".txt") || strings.HasSuffix(strings.ToLower(base), ".md")) {
			return fmt.Errorf("worklog file: use a .txt or .md file that is not hidden (got %q)", base)
		}
	}
	if a := c.Describe.Agent; a != "" && a != "claude" && a != "codex" {
		return fmt.Errorf("descriptions: agent must be claude or codex, not %q", a)
	}
	for app, expr := range c.Activity.System.Meetings {
		if _, err := regexp.Compile(expr); err != nil {
			return fmt.Errorf("call pattern for %s: %v", app, err)
		}
	}
	return nil
}

func Load(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Save writes the config atomically with mode 600 (it holds tokens).
func Save(path string, c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config.*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
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
	return os.Rename(tmp.Name(), path)
}

func mask(s string) string {
	if s == "" {
		return ""
	}
	return Mask
}

// keep returns the stored secret when the UI sent the mask back.
func keep(sent, stored string) string {
	if sent == Mask {
		return stored
	}
	return strings.TrimSpace(sent)
}

// Masked is the config for the UI: every token and private URL replaced by Mask.
func (c Config) Masked() Config {
	m := c
	m.Tokens.Jira, m.Tokens.Tempo = mask(c.Tokens.Jira), mask(c.Tokens.Tempo)
	a := &m.Activity
	a.GitLab.Instances = append([]activity.GitLabInstance(nil), c.Activity.GitLab.Instances...)
	for i := range a.GitLab.Instances {
		a.GitLab.Instances[i].Token = mask(a.GitLab.Instances[i].Token)
	}
	a.GitHub.Accounts = append([]activity.GitHubAccount(nil), c.Activity.GitHub.Accounts...)
	for i := range a.GitHub.Accounts {
		a.GitHub.Accounts[i].Token = mask(a.GitHub.Accounts[i].Token)
	}
	a.Slack.Token = mask(c.Activity.Slack.Token)
	a.Calendar.URLs = make([]string, len(c.Activity.Calendar.URLs))
	for i, u := range c.Activity.Calendar.URLs {
		a.Calendar.URLs[i] = fmt.Sprintf("%s #%d", mask(u), i+1)
	}
	return m
}

var maskedURL = regexp.MustCompile(`^` + regexp.QuoteMeta(Mask) + ` #(\d+)$`)

// origin is the scheme and host a URL sends to (def when empty), lower-cased.
func origin(raw, def string) string {
	if strings.TrimSpace(raw) == "" {
		raw = def
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

// Merge takes a config edited in the UI and puts the stored secrets back where it sent the mask.
// A stored secret is kept only while it still goes to the same place: the same scheme and host
// (and for Jira the same account). Pointing a token at another host needs the token typed again,
// so a changed URL can never carry a stored token away. List entries are matched by host
// (GitLab), by host and login (GitHub) and by number (calendars, whose URL is the secret).
func Merge(sent, stored Config) Config {
	c := sent
	keepIf := func(sentTok, storedTok string, same bool) string {
		if sentTok == Mask && !same {
			return ""
		}
		return keep(sentTok, storedTok)
	}
	c.Tokens.Jira = keepIf(sent.Tokens.Jira, stored.Tokens.Jira,
		origin(sent.JiraBaseURL, "") == origin(stored.JiraBaseURL, "") && strings.EqualFold(sent.AccountEmail, stored.AccountEmail))
	c.Tokens.Tempo = keepIf(sent.Tokens.Tempo, stored.Tokens.Tempo,
		origin(sent.TempoBaseURL, "https://api.tempo.io") == origin(stored.TempoBaseURL, "https://api.tempo.io"))
	gl := map[string]string{}
	for _, in := range stored.Activity.GitLab.Instances {
		gl[origin(in.BaseURL, "")] = in.Token
	}
	for i, in := range c.Activity.GitLab.Instances {
		c.Activity.GitLab.Instances[i].Token = keep(in.Token, gl[origin(in.BaseURL, "")])
	}
	gh := map[string]string{}
	for _, a := range stored.Activity.GitHub.Accounts {
		gh[strings.ToLower(a.User)+"@"+origin(a.BaseURL, "https://api.github.com")] = a.Token
	}
	for i, a := range c.Activity.GitHub.Accounts {
		c.Activity.GitHub.Accounts[i].Token = keep(a.Token, gh[strings.ToLower(a.User)+"@"+origin(a.BaseURL, "https://api.github.com")])
	}
	c.Activity.Slack.Token = keepIf(sent.Activity.Slack.Token, stored.Activity.Slack.Token,
		origin(sent.Activity.Slack.BaseURL, "https://slack.com") == origin(stored.Activity.Slack.BaseURL, "https://slack.com"))
	var urls []string
	for _, u := range sent.Activity.Calendar.URLs {
		if m := maskedURL.FindStringSubmatch(u); m != nil {
			if i := atoi(m[1]) - 1; i >= 0 && i < len(stored.Activity.Calendar.URLs) {
				urls = append(urls, stored.Activity.Calendar.URLs[i])
			}
			continue
		}
		if u = strings.TrimSpace(u); u != "" {
			urls = append(urls, u)
		}
	}
	c.Activity.Calendar.URLs = urls
	return c
}

// checkURL accepts https URLs, and http only for a loopback host (a local test server).
func checkURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("not a URL: %q", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if h := u.Hostname(); h == "localhost" || h == "127.0.0.1" || h == "::1" {
			return nil
		}
	}
	return fmt.Errorf("use https (got %s://%s)", u.Scheme, u.Host)
}
