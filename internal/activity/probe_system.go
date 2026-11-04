package activity

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// SystemProbe samples the front app, its window title and the idle time.
type SystemProbe struct {
	cfg    SystemConfig
	sample func(ctx context.Context, wantTitle bool) (sample, error)

	mu       sync.Mutex
	cur      *Span     // open window/idle span
	curSince time.Time // when cur was last written
	lastTick time.Time
	titles   string    // "yes" | "no" | "" (unknown yet)
	titleOff time.Time // titles failed: try again after this
}

func NewSystemProbe(cfg SystemConfig) *SystemProbe {
	return &SystemProbe{cfg: cfg, sample: probe}
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
	return p.close(now)
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

	next := Span{Date: dayOf(now), Kind: "window", App: s.App, Title: s.Title}
	from := now
	if s.Idle >= time.Duration(p.cfg.IdleAfter)*time.Second {
		next.Kind, next.Title = "idle", ""
		from = now.Add(-s.Idle) // idle began at the last input, not when we noticed
	}
	c := p.cur
	if c != nil && c.Date == next.Date && c.Kind == next.Kind && c.App == next.App && c.Title == next.Title {
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
