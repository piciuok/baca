package activity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// GitProbe notices repos whose git state changed (index, HEAD, reflog) since the last scan:
// staging, commits, checkouts, IDE refreshes after edits. The branch names the ticket.
type GitProbe struct {
	cfg GitConfig

	mu    sync.Mutex
	marks map[string]time.Time
	last  time.Time
	count int
}

func NewGitProbe(cfg GitConfig) *GitProbe { return &GitProbe{cfg: cfg, marks: map[string]time.Time{}} }

func (p *GitProbe) Name() string           { return "git" }
func (p *GitProbe) Every() time.Duration   { return time.Minute }
func (p *GitProbe) Flush(time.Time) []Span { return nil }
func (p *GitProbe) Info() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return fmt.Sprintf("%d repo", p.count)
}

func (p *GitProbe) Tick(ctx context.Context, now time.Time) ([]Span, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	since := recent(p.last, now, p.Every())
	p.last = now
	repos := Repos(p.cfg.Repos)
	p.count = len(repos)
	var out []Span
	for _, r := range repos {
		gitDir, branch := GitInfo(r)
		if gitDir == "" {
			continue
		}
		mark := newest(filepath.Join(gitDir, "index"), filepath.Join(gitDir, "HEAD"), filepath.Join(gitDir, "logs", "HEAD"))
		if old, ok := p.marks[r]; ok && mark.After(old) && !since.IsZero() {
			out = append(out, Span{Date: dayOf(now), From: clock(dayFloor(since, now)), To: clock(now), Kind: "repo", Repo: filepath.Base(r), Branch: branch})
		}
		p.marks[r] = mark
	}
	if len(p.cfg.Repos) > 0 && len(repos) == 0 {
		return out, fmt.Errorf("no git.repos pattern matches a repository")
	}
	return out, nil
}

// Repos expands globs to directories that hold a .git entry (repos and worktrees).
func Repos(globs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, g := range globs {
		matches, _ := filepath.Glob(expandHome(g))
		for _, m := range matches {
			if _, err := os.Stat(filepath.Join(m, ".git")); err == nil && !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	sort.Strings(out)
	return out
}

// GitInfo finds the git dir of a repo or worktree (.git may be a "gitdir: ..." file) and its branch.
func GitInfo(repo string) (gitDir, branch string) {
	gitDir = filepath.Join(repo, ".git")
	st, err := os.Stat(gitDir)
	if err != nil {
		return "", ""
	}
	if !st.IsDir() {
		b, err := os.ReadFile(gitDir)
		if err != nil {
			return "", ""
		}
		p := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
		if !filepath.IsAbs(p) {
			p = filepath.Join(repo, p)
		}
		gitDir = p
	}
	head, _ := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	h := strings.TrimSpace(string(head))
	if strings.HasPrefix(h, "ref: refs/heads/") {
		branch = strings.TrimPrefix(h, "ref: refs/heads/")
	}
	return gitDir, branch
}

func newest(paths ...string) time.Time {
	var t time.Time
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil && st.ModTime().After(t) {
			t = st.ModTime()
		}
	}
	return t
}

// dayFloor keeps a span that started yesterday inside today.
func dayFloor(t, now time.Time) time.Time {
	if dayOf(t) != dayOf(now) {
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	}
	return t
}

// recent is where a minute probe's span may start: the previous scan, but never more than two
// intervals back - after a sleep (wall clock: the monotonic one stops while a Mac sleeps) the
// first change would otherwise claim the whole night.
func recent(last, now time.Time, every time.Duration) time.Time {
	if !last.IsZero() && now.Round(0).Sub(last.Round(0)) > 2*every {
		return now.Add(-2 * every)
	}
	return last
}
