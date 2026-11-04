package activity

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// SystemProbe samples the front app, its window title and the idle time. A title that names a
// watched repo (IDE: "repo – file", VS Code: "file — repo") gets the repo and its branch, and open
// call windows (Slack huddle, Zoom, Teams) are recorded as meetings.
type SystemProbe struct {
	cfg     SystemConfig
	git     GitConfig
	sample  func(ctx context.Context, wantTitle bool) (sample, error)
	windows func(ctx context.Context, apps []string) ([][2]string, error)
	meetRE  map[string]*regexp.Regexp

	repos     []string // watched repos, longest name first
	reposAt   time.Time
	meet      *Span // open meeting span
	meetSince time.Time
	meetAt    time.Time

	mu       sync.Mutex
	cur      *Span     // open window/idle span
	curSince time.Time // when cur was last written
	lastTick time.Time
	titles   string    // "yes" | "no" | "" (unknown yet)
	titleOff time.Time // titles failed: try again after this
}

func NewSystemProbe(cfg SystemConfig, git GitConfig) *SystemProbe {
	p := &SystemProbe{cfg: cfg, git: git, sample: probe, windows: windows, meetRE: map[string]*regexp.Regexp{}}
	for app, expr := range cfg.Meetings {
		if re, err := regexp.Compile(expr); err == nil {
			p.meetRE[app] = re
		}
	}
	return p
}

func (p *SystemProbe) Name() string         { return "system" }
func (p *SystemProbe) Every() time.Duration { return time.Duration(p.cfg.Interval) * time.Second }

func (p *SystemProbe) Info() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.titles == "" {
		return osName
	}
	return fmt.Sprintf("%s · window titles: %s", osName, p.titles)
}

func (p *SystemProbe) Flush(now time.Time) []Span {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append(p.close(now), p.closeMeet(now)...)
}

func (p *SystemProbe) closeMeet(end time.Time) []Span {
	m := p.meet
	p.meet = nil
	if m == nil || dayOf(end) != m.Date || clock(end) <= m.From {
		return nil
	}
	m.To = clock(end)
	return []Span{*m}
}

// repoIn finds a watched repo named in a window title (whole name, longest first).
func (p *SystemProbe) repoIn(title string, now time.Time) string {
	if len(p.git.Repos) == 0 || title == "" {
		return ""
	}
	if now.Sub(p.reposAt) > time.Minute {
		p.repos, p.reposAt = Repos(p.git.Repos), now
		sort.SliceStable(p.repos, func(i, j int) bool { return len(filepath.Base(p.repos[i])) > len(filepath.Base(p.repos[j])) })
	}
	for _, r := range p.repos {
		if containsWord(title, filepath.Base(r)) {
			return r
		}
	}
	return ""
}

func containsWord(s, w string) bool {
	word := func(b byte) bool {
		return b == '-' || b == '_' || b == '.' || (b >= '0' && b <= '9') || (b|0x20 >= 'a' && b|0x20 <= 'z')
	}
	for i := 0; ; {
		j := strings.Index(s[i:], w)
		if j < 0 {
			return false
		}
		a, b := i+j, i+j+len(w)
		if (a == 0 || !word(s[a-1])) && (b == len(s) || !word(s[b])) {
			return true
		}
		i = a + 1
	}
}

// meetings checks the call windows every 30 s and keeps the meeting span open while one is on.
func (p *SystemProbe) meetings(ctx context.Context, now time.Time) []Span {
	if len(p.meetRE) == 0 || now.Sub(p.meetAt) < 30*time.Second {
		if p.meet != nil && now.Sub(p.meetSince) >= time.Minute {
			p.meet.To = clock(now)
			piece := *p.meet
			p.meet.From, p.meetSince = p.meet.To, now
			return []Span{piece}
		}
		return nil
	}
	p.meetAt = now
	apps := make([]string, 0, len(p.meetRE))
	for a := range p.meetRE {
		apps = append(apps, a)
	}
	sort.Strings(apps)
	wins, err := p.windows(ctx, apps)
	if err != nil {
		return nil
	}
	var found *Span
	for _, w := range wins {
		if re := p.meetRE[w[0]]; re != nil && re.MatchString(w[1]) {
			found = &Span{Date: dayOf(now), Kind: "meeting", App: w[0], Title: w[1]}
			break
		}
	}
	if found != nil && p.meet != nil && p.meet.App == found.App && p.meet.Title == found.Title && p.meet.Date == found.Date {
		if now.Sub(p.meetSince) >= time.Minute {
			p.meet.To = clock(now)
			piece := *p.meet
			p.meet.From, p.meetSince = p.meet.To, now
			return []Span{piece}
		}
		return nil
	}
	out := p.closeMeet(now)
	if found != nil {
		found.From = clock(now)
		p.meet, p.meetSince = found, now
	}
	return out
}

// close ends the open span at end and returns it for writing.
func (p *SystemProbe) close(end time.Time) []Span {
	c := p.cur
	p.cur = nil
	if c == nil || dayOf(end) != c.Date || clock(end) <= c.From {
		return nil
	}
	c.To = clock(end)
	return []Span{*c}
}

func (p *SystemProbe) Tick(ctx context.Context, now time.Time) ([]Span, error) {
	p.mu.Lock()
	wantTitle := now.After(p.titleOff)
	p.mu.Unlock()

	s, err := p.sample(ctx, wantTitle)

	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Span
	// the machine slept or the process stalled: the open span ends at the last tick
	if !p.lastTick.IsZero() && now.Sub(p.lastTick) > 3*p.Every() {
		out = append(out, p.close(p.lastTick)...)
	}
	p.lastTick = now
	if err != nil {
		return append(out, p.close(now)...), err
	}
	if wantTitle {
		p.titles = "yes"
		if s.TitleErr != nil {
			p.titles, p.titleOff = "no", now.Add(5*time.Minute)
			err = s.TitleErr
		}
	}
	for _, a := range p.cfg.IgnoreApps {
		if strings.EqualFold(a, s.App) {
			s.Title = ""
		}
	}

	out = append(out, p.meetings(ctx, now)...)
	next := Span{Date: dayOf(now), Kind: "window", App: s.App, Title: s.Title}
	if r := p.repoIn(s.Title, now); r != "" {
		next.Repo = filepath.Base(r)
		_, next.Branch = GitInfo(r)
	}
	from := now
	if s.Idle >= time.Duration(p.cfg.IdleAfter)*time.Second {
		next.Kind, next.Title, next.Repo, next.Branch = "idle", "", "", ""
		from = now.Add(-s.Idle) // idle began at the last input, not when we noticed
	}
	c := p.cur
	if c != nil && c.Date == next.Date && c.Kind == next.Kind && c.App == next.App && c.Title == next.Title && c.Branch == next.Branch {
		if now.Sub(p.curSince) >= time.Minute { // long spans go out in pieces, Log.Read merges them
			c.To = clock(now)
			out = append(out, *c)
			c.From, p.curSince = c.To, now
		}
		return out, err
	}
	known := c != nil // without a span before, idle cannot reach back past this tick
	out = append(out, p.close(now)...)
	if !known || dayOf(from) != next.Date || from.After(now) {
		from = now
	}
	next.From = clock(from)
	p.cur, p.curSince = &next, now
	return out, err
}

// sample is one look at the screen.
type sample struct {
	App      string
	Title    string
	Idle     time.Duration
	TitleErr error // titles not readable (permission, missing tool)
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return "", &probeError{name: name, msg: strings.TrimSpace(string(ee.Stderr))}
		}
		return "", &probeError{name: name, msg: err.Error()}
	}
	return strings.TrimSpace(string(out)), nil
}

type probeError struct{ name, msg string }

func (e *probeError) Error() string { return e.name + ": " + e.msg }

func dayOf(t time.Time) string { return t.Format("2006-01-02") }

func splitWindows(out string) [][2]string {
	var res [][2]string
	for _, line := range strings.Split(out, "\n") {
		if app, title, ok := strings.Cut(line, "\t"); ok && strings.TrimSpace(title) != "" {
			res = append(res, [2]string{app, title})
		}
	}
	return res
}
