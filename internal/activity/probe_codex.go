package activity

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// CodexProbe notices Codex sessions (~/.codex/sessions/YYYY/MM/DD/*.jsonl) that wrote since the
// last scan. The first line of a session holds its working directory.
type CodexProbe struct {
	git  GitConfig
	root string

	mu     sync.Mutex
	seen   map[string]time.Time
	cwd    map[string]string // session file -> cwd
	last   time.Time
	active int
}

type CodexConfig struct {
	Enabled *bool `json:"enabled"`
}

func NewCodexProbe(git GitConfig) *CodexProbe {
	root := ""
	if h, err := os.UserHomeDir(); err == nil {
		root = filepath.Join(h, ".codex", "sessions")
	}
	return &CodexProbe{git: git, root: root, seen: map[string]time.Time{}, cwd: map[string]string{}}
}

func (p *CodexProbe) Name() string           { return "codex" }
func (p *CodexProbe) Every() time.Duration   { return time.Minute }
func (p *CodexProbe) Flush(time.Time) []Span { return nil }
func (p *CodexProbe) Info() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return fmt.Sprintf("sessions active in the last minute: %d", p.active)
}

func (p *CodexProbe) Tick(ctx context.Context, now time.Time) ([]Span, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := os.Stat(p.root); err != nil {
		return nil, fmt.Errorf("no %s", p.root)
	}
	since := p.last
	p.last = now
	var files []string
	for _, d := range []time.Time{now.AddDate(0, 0, -1), now} { // sessions started yesterday may still run
		m, _ := filepath.Glob(filepath.Join(p.root, d.Format("2006"), d.Format("01"), d.Format("02"), "*.jsonl"))
		files = append(files, m...)
	}
	repos := Repos(p.git.Repos)
	byRepo := map[string]Span{}
	for _, f := range files {
		mark := newest(f)
		old, ok := p.seen[f]
		p.seen[f] = mark
		if since.IsZero() || (ok && !mark.After(old)) || (!ok && !mark.After(since)) {
			continue
		}
		cwd, ok := p.cwd[f]
		if !ok {
			cwd = codexCwd(f)
			p.cwd[f] = cwd
		}
		name, branch := filepath.Base(cwd), ""
		if r := RepoOf(cwd, repos); r != "" {
			name = filepath.Base(r)
			_, branch = GitInfo(r)
		} else if _, b := GitInfo(cwd); b != "" {
			branch = b
		}
		byRepo[name] = Span{Date: dayOf(now), From: clock(dayFloor(since, now)), To: clock(now), Kind: "agent", App: "Codex", Repo: name, Branch: branch}
	}
	out := make([]Span, 0, len(byRepo))
	for _, s := range byRepo {
		out = append(out, s)
	}
	p.active = len(out)
	return out, nil
}

// codexCwd reads "cwd" from the session_meta line (the first line, can be large).
func codexCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64*1024)
	line, err := r.ReadSlice('\n')
	if err != nil && err != bufio.ErrBufferFull && len(line) == 0 {
		return ""
	}
	if err == bufio.ErrBufferFull { // keep reading the rest of a long first line
		var b strings.Builder
		b.Write(line)
		rest, _ := r.ReadString('\n')
		b.WriteString(rest)
		line = []byte(b.String())
	}
	var meta struct {
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &meta) != nil {
		return ""
	}
	return meta.Payload.Cwd
}

// RepoOf returns the repo that holds dir (the longest matching repo path).
func RepoOf(dir string, repos []string) string {
	best := ""
	for _, r := range repos {
		if (dir == r || strings.HasPrefix(dir, r+string(filepath.Separator))) && len(r) > len(best) {
			best = r
		}
	}
	return best
}
