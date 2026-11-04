package activity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type GitLabConfig struct {
	Enabled   *bool            `json:"enabled"`
	Instances []GitLabInstance `json:"instances"`
}

// GitLabInstance is one GitLab. Without token the probe asks glab for it
// (glab config get token --host <host>), so the token stays in glab's keychain/config.
type GitLabInstance struct {
	BaseURL string `json:"baseUrl"`
	Token   string `json:"token"`
}

// GitLabProbe turns the user's GitLab events (pushes, MR comments, approvals, merges) of today
// and yesterday into "remote" spans. Each event is a point in time; the span reaches back by
// a short lead-in, the time the action likely took.
type GitLabProbe struct {
	cfg  GitLabConfig
	http *http.Client

	mu       sync.Mutex
	tokens   map[string]string
	projects map[string]string // base|id -> path
	count    int
}

func NewGitLabProbe(cfg GitLabConfig) *GitLabProbe {
	return &GitLabProbe{cfg: cfg, http: &http.Client{Timeout: 20 * time.Second}, tokens: map[string]string{}, projects: map[string]string{}}
}

func (p *GitLabProbe) Name() string           { return "gitlab" }
func (p *GitLabProbe) Every() time.Duration   { return 5 * time.Minute }
func (p *GitLabProbe) Flush(time.Time) []Span { return nil }
func (p *GitLabProbe) Days(now time.Time) []string {
	return []string{dayOf(now.AddDate(0, 0, -1)), dayOf(now)}
}
func (p *GitLabProbe) Info() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return fmt.Sprintf("events today and yesterday: %d", p.count)
}

type gitlabEvent struct {
	ProjectID   int       `json:"project_id"`
	Action      string    `json:"action_name"`
	TargetType  string    `json:"target_type"`
	TargetIID   int       `json:"target_iid"`
	TargetTitle string    `json:"target_title"`
	CreatedAt   time.Time `json:"created_at"`
	PushData    *struct {
		Ref         string `json:"ref"`
		RefType     string `json:"ref_type"`
		CommitTitle string `json:"commit_title"`
		CommitCount int    `json:"commit_count"`
	} `json:"push_data"`
	Note *struct {
		NoteableType string `json:"noteable_type"`
	} `json:"note"`
}

// lead is how long before the event the work on it likely started.
func lead(action string) time.Duration {
	switch {
	case strings.HasPrefix(action, "pushed"), action == "opened", action == "created":
		return 10 * time.Minute
	case action == "accepted", action == "deleted", action == "closed", action == "joined":
		return 2 * time.Minute
	default: // commented on, approved
		return 5 * time.Minute
	}
}

func (p *GitLabProbe) Tick(ctx context.Context, now time.Time) ([]Span, error) {
	from := now.AddDate(0, 0, -1)
	var out []Span
	var errs []string
	for _, inst := range p.cfg.Instances {
		spans, err := p.fetch(ctx, inst, from, now)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		out = append(out, spans...)
	}
	p.mu.Lock()
	p.count = len(out)
	p.mu.Unlock()
	if len(errs) > 0 {
		return out, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return out, nil
}

func (p *GitLabProbe) token(ctx context.Context, inst GitLabInstance) (string, error) {
	if inst.Token != "" {
		return inst.Token, nil
	}
	u, err := url.Parse(inst.BaseURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("gitlab: invalid baseUrl %q", inst.BaseURL)
	}
	p.mu.Lock()
	t, ok := p.tokens[u.Host]
	p.mu.Unlock()
	if ok {
		return t, nil
	}
	// GITLAB_TOKEN is gitlab.com's; other instances get glab's token for their own host
	if u.Hostname() == "gitlab.com" {
		t = os.Getenv("GITLAB_TOKEN")
	}
	if t == "" {
		t, err = run(ctx, "glab", "config", "get", "token", "--host", u.Host)
		if err != nil || t == "" {
			return "", fmt.Errorf("gitlab %s: no token (token in the config, GITLAB_TOKEN or glab auth login)", u.Host)
		}
	}
	p.mu.Lock()
	p.tokens[u.Host] = t
	p.mu.Unlock()
	return t, nil
}

func (p *GitLabProbe) get(ctx context.Context, inst GitLabInstance, path string, q url.Values, out any) (http.Header, error) {
	tok, err := p.token(ctx, inst)
	if err != nil {
		return nil, err
	}
	u := strings.TrimRight(inst.BaseURL, "/") + "/api/v4" + path
	if q != nil {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("PRIVATE-TOKEN", tok)
	res, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode != http.StatusOK {
		if res.StatusCode == http.StatusUnauthorized && inst.Token == "" { // a rotated glab token
			if u, err := url.Parse(inst.BaseURL); err == nil {
				p.mu.Lock()
				delete(p.tokens, u.Host)
				p.mu.Unlock()
			}
		}
		return nil, fmt.Errorf("gitlab %s: HTTP %d", path, res.StatusCode)
	}
	return res.Header, json.Unmarshal(b, out)
}

func (p *GitLabProbe) project(ctx context.Context, inst GitLabInstance, id int) string {
	key := inst.BaseURL + "|" + strconv.Itoa(id)
	p.mu.Lock()
	name, ok := p.projects[key]
	p.mu.Unlock()
	if ok {
		return name
	}
	var pr struct {
		Path string `json:"path"`
	}
	if _, err := p.get(ctx, inst, "/projects/"+strconv.Itoa(id), nil, &pr); err != nil || pr.Path == "" {
		return strconv.Itoa(id)
	}
	p.mu.Lock()
	p.projects[key] = pr.Path
	p.mu.Unlock()
	return pr.Path
}

func (p *GitLabProbe) fetch(ctx context.Context, inst GitLabInstance, from, now time.Time) ([]Span, error) {
	// after/before are exclusive dates
	q := url.Values{"after": {from.AddDate(0, 0, -1).Format("2006-01-02")}, "before": {now.AddDate(0, 0, 1).Format("2006-01-02")}, "per_page": {"100"}}
	var events []gitlabEvent
	for page := 1; page <= 10; page++ {
		q.Set("page", strconv.Itoa(page))
		var batch []gitlabEvent
		h, err := p.get(ctx, inst, "/events", q, &batch)
		if err != nil {
			return nil, err
		}
		events = append(events, batch...)
		if h.Get("X-Next-Page") == "" {
			break
		}
	}
	start := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, now.Location())
	var out []Span
	for _, e := range events {
		at := e.CreatedAt.In(now.Location())
		if at.Before(start) || at.After(now) {
			continue
		}
		s := Span{Date: dayOf(at), To: clock(at), Kind: "remote", App: "GitLab", Repo: p.project(ctx, inst, e.ProjectID)}
		begin := at.Add(-lead(e.Action))
		if dayOf(begin) != s.Date {
			begin = time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, at.Location())
		}
		s.From = clock(begin)
		what := e.Action
		switch {
		case e.PushData != nil:
			s.Branch = e.PushData.Ref
			what = fmt.Sprintf("%s %s", e.Action, e.PushData.Ref)
			if e.PushData.CommitTitle != "" {
				what += ": " + e.PushData.CommitTitle
			}
		case e.TargetType == "MergeRequest":
			what = fmt.Sprintf("%s MR !%d %s", e.Action, e.TargetIID, e.TargetTitle)
		case e.Note != nil && e.Note.NoteableType == "MergeRequest":
			what = fmt.Sprintf("%s MR %s", e.Action, e.TargetTitle)
		case e.TargetTitle != "":
			what = fmt.Sprintf("%s %s %s", e.Action, e.TargetType, e.TargetTitle)
		}
		s.Title = what
		out = append(out, s)
	}
	return out, nil
}
