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

type GitHubConfig struct {
	Enabled  *bool           `json:"enabled"`
	Accounts []GitHubAccount `json:"accounts"`
}

// GitHubAccount is a GitHub login whose events are read. The token comes from Token,
// GITHUB_TOKEN or gh (gh auth token --user <User>), so with several gh accounts the right one is used.
type GitHubAccount struct {
	User    string `json:"user"`
	Token   string `json:"token"`
	BaseURL string `json:"baseUrl"` // default https://api.github.com (GitHub Enterprise: https://host/api/v3)
}

// GitHubProbe turns the user's GitHub events (pushes, PR reviews and comments, opened/merged PRs)
// of today and yesterday into "remote" spans.
type GitHubProbe struct {
	cfg  GitHubConfig
	http *http.Client

	mu     sync.Mutex
	tokens map[string]string
	count  int
}

func NewGitHubProbe(cfg GitHubConfig) *GitHubProbe {
	for i := range cfg.Accounts {
		if cfg.Accounts[i].BaseURL == "" {
			cfg.Accounts[i].BaseURL = "https://api.github.com"
		}
	}
	return &GitHubProbe{cfg: cfg, http: &http.Client{Timeout: 20 * time.Second}, tokens: map[string]string{}}
}

func (p *GitHubProbe) Name() string           { return "github" }
func (p *GitHubProbe) Every() time.Duration   { return 5 * time.Minute }
func (p *GitHubProbe) Flush(time.Time) []Span { return nil }
func (p *GitHubProbe) Days(now time.Time) []string {
	return []string{dayOf(now.AddDate(0, 0, -1)), dayOf(now)}
}
func (p *GitHubProbe) Info() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	users := make([]string, len(p.cfg.Accounts))
	for i, a := range p.cfg.Accounts {
		users[i] = a.User
	}
	return fmt.Sprintf("%s · events today and yesterday: %d", strings.Join(users, ", "), p.count)
}

func (p *GitHubProbe) getToken(ctx context.Context, a GitHubAccount) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t := p.tokens[a.User]; t != "" {
		return t, nil
	}
	// GITHUB_TOKEN and gh's token belong to one host: never send them anywhere else
	host := "github.com"
	if u, err := url.Parse(a.BaseURL); err == nil && u.Hostname() != "" && u.Hostname() != "api.github.com" {
		host = u.Hostname()
	}
	t := a.Token
	if t == "" && host == "github.com" {
		t = os.Getenv("GITHUB_TOKEN")
	}
	if t == "" {
		var err error
		if t, err = run(ctx, "gh", "auth", "token", "--hostname", host, "--user", a.User); err != nil || t == "" {
			return "", fmt.Errorf("github %s: no token (token in the settings, GITHUB_TOKEN or gh auth login)", a.User)
		}
	}
	p.tokens[a.User] = t
	return t, nil
}

type githubEvent struct {
	Type string `json:"type"`
	Repo struct {
		Name string `json:"name"`
	} `json:"repo"`
	CreatedAt time.Time `json:"created_at"`
	Payload   struct {
		Action  string `json:"action"`
		Ref     string `json:"ref"`
		Commits []struct {
			Message string `json:"message"`
		} `json:"commits"`
		PullRequest *struct {
			Number int    `json:"number"`
			Title  string `json:"title"`
			Merged bool   `json:"merged"`
			Head   struct {
				Ref string `json:"ref"`
			} `json:"head"`
		} `json:"pull_request"`
		Review *struct {
			State string `json:"state"`
		} `json:"review"`
		Issue *struct {
			Number int    `json:"number"`
			Title  string `json:"title"`
		} `json:"issue"`
	} `json:"payload"`
}

func (p *GitHubProbe) Tick(ctx context.Context, now time.Time) ([]Span, error) {
	var out []Span
	var errs []string
	for _, a := range p.cfg.Accounts {
		if a.User == "" {
			continue
		}
		spans, err := p.fetch(ctx, a, now)
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

func (p *GitHubProbe) fetch(ctx context.Context, a GitHubAccount, now time.Time) ([]Span, error) {
	tok, err := p.getToken(ctx, a)
	if err != nil {
		return nil, err
	}
	from := now.AddDate(0, 0, -1)
	start := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, now.Location())
	var out []Span
	for page := 1; page <= 3; page++ { // newest first, 100 per page
		u := strings.TrimRight(a.BaseURL, "/") + "/users/" + url.PathEscape(a.User) + "/events?per_page=100&page=" + strconv.Itoa(page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/vnd.github+json")
		res, err := p.http.Do(req)
		if err != nil {
			return nil, err
		}
		b, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			if res.StatusCode == http.StatusUnauthorized { // a rotated token: ask gh again next time
				p.mu.Lock()
				delete(p.tokens, a.User)
				p.mu.Unlock()
			}
			return nil, fmt.Errorf("github %s: HTTP %d", a.User, res.StatusCode)
		}
		var events []githubEvent
		if err := json.Unmarshal(b, &events); err != nil {
			return nil, err
		}
		older := false
		for _, e := range events {
			at := e.CreatedAt.In(now.Location())
			if at.Before(start) {
				older = true
				continue
			}
			if at.After(now) {
				continue
			}
			what, branch, lead := githubWhat(e)
			if what == "" {
				continue
			}
			begin := at.Add(-lead)
			if dayOf(begin) != dayOf(at) {
				begin = time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, at.Location())
			}
			repo := e.Repo.Name
			if i := strings.LastIndex(repo, "/"); i >= 0 {
				repo = repo[i+1:]
			}
			out = append(out, Span{Date: dayOf(at), From: clock(begin), To: clock(at), Kind: "remote", App: "GitHub", Repo: repo, Branch: branch, Title: what})
		}
		if older || len(events) < 100 {
			break
		}
	}
	return out, nil
}

// githubWhat describes an event; an empty text means the event is not work worth showing.
func githubWhat(e githubEvent) (what, branch string, lead time.Duration) {
	pl := e.Payload
	pr := func(verb string) string {
		if pl.PullRequest == nil {
			return verb + " PR"
		}
		branch = pl.PullRequest.Head.Ref
		return fmt.Sprintf("%s PR #%d %s", verb, pl.PullRequest.Number, pl.PullRequest.Title)
	}
	switch e.Type {
	case "PushEvent":
		branch = strings.TrimPrefix(pl.Ref, "refs/heads/")
		what = "pushed to " + branch
		if n := len(pl.Commits); n > 0 {
			what += ": " + strings.SplitN(pl.Commits[n-1].Message, "\n", 2)[0]
		}
		return what, branch, 10 * time.Minute
	case "PullRequestReviewEvent":
		state := "reviewed"
		if pl.Review != nil && pl.Review.State != "" {
			state = strings.ToLower(pl.Review.State)
		}
		return pr(state), branch, 5 * time.Minute
	case "PullRequestReviewCommentEvent":
		return pr("commented on"), branch, 5 * time.Minute
	case "PullRequestEvent":
		verb := pl.Action
		if pl.Action == "closed" && pl.PullRequest != nil && pl.PullRequest.Merged {
			verb = "merged"
		}
		lead = 2 * time.Minute
		if verb == "opened" {
			lead = 10 * time.Minute
		}
		return pr(verb), branch, lead
	case "IssueCommentEvent":
		if pl.Issue != nil {
			return fmt.Sprintf("commented on #%d %s", pl.Issue.Number, pl.Issue.Title), "", 5 * time.Minute
		}
	case "IssuesEvent":
		if pl.Issue != nil {
			return fmt.Sprintf("%s issue #%d %s", pl.Action, pl.Issue.Number, pl.Issue.Title), "", 5 * time.Minute
		}
	case "CreateEvent":
		if pl.Ref != "" {
			return "created " + pl.Ref, pl.Ref, 2 * time.Minute
		}
	}
	return "", "", 0
}
