package activity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ClaudeProbe notices Claude Code sessions that wrote to their transcript since the last scan
// (~/.claude/projects/<path with / and . as ->/*.jsonl) and maps them to a known repo.
type ClaudeProbe struct {
	git  GitConfig
	root string

	mu     sync.Mutex
	seen   map[string]time.Time
	last   time.Time
	active int
}

func NewClaudeProbe(git GitConfig) *ClaudeProbe {
	root := ""
	if h, err := os.UserHomeDir(); err == nil {
		root = filepath.Join(h, ".claude", "projects")
	}
	return &ClaudeProbe{git: git, root: root, seen: map[string]time.Time{}}
}

func (p *ClaudeProbe) Name() string           { return "claude" }
func (p *ClaudeProbe) Every() time.Duration   { return time.Minute }
func (p *ClaudeProbe) Flush(time.Time) []Span { return nil }
func (p *ClaudeProbe) Info() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return fmt.Sprintf("sessions active in the last minute: %d", p.active)
}

func (p *ClaudeProbe) Tick(ctx context.Context, now time.Time) ([]Span, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := os.Stat(p.root); err != nil {
		return nil, fmt.Errorf("no %s", p.root)
	}
	since := p.last
	p.last = now
	repos := map[string]string{}
	for _, r := range Repos(p.git.Repos) {
		repos[claudeKey(r)] = r
	}
	dirs, _ := filepath.Glob(filepath.Join(p.root, "*"))
	var out []Span
	for _, d := range dirs {
		files, _ := filepath.Glob(filepath.Join(d, "*.jsonl"))
		mark := newest(files...)
		if old, ok := p.seen[d]; ok && mark.After(old) && !since.IsZero() {
			name, branch := filepath.Base(d), ""
			if r, ok := repos[filepath.Base(d)]; ok {
				name = filepath.Base(r)
				_, branch = GitInfo(r)
			}
			out = append(out, Span{Date: dayOf(now), From: clock(dayFloor(since, now)), To: clock(now), Kind: "agent", App: "Claude Code", Repo: name, Branch: branch})
		}
		p.seen[d] = mark
	}
	p.active = len(out)
	return out, nil
}

// claudeKey is how Claude Code names a project dir: the path with / . and _ turned into -.
func claudeKey(path string) string {
	return strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(path)
}
