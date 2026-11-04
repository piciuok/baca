// Package tempo logs worklogs to Tempo:
// the Jira issue id comes from Jira, the worklog goes to Tempo's /4/worklogs.
// Credentials come from baca's config.json (tokens.jira, tokens.temp).
package tempo

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

const (
	defaultTempoBase = "https://api.tempo.io"
)

type Config struct {
	AccountEmail string `json:"accountEmail"`
	Tokens       struct {
		Jira  string `json:"jira"`
		Tempo string `json:"temp"`
	} `json:"tokens"`
	JiraBaseURL  string `json:"jiraBaseUrl"`
	TempoBaseURL string `json:"tempoBaseUrl"`
}

func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return Prepare(c)
}

// Prepare fills the default Tempo URL and checks that the credentials are there.
func Prepare(c Config) (Config, error) {
	if c.TempoBaseURL == "" {
		c.TempoBaseURL = defaultTempoBase
	}
	if c.JiraBaseURL == "" || c.AccountEmail == "" || c.Tokens.Jira == "" || c.Tokens.Tempo == "" {
		return c, fmt.Errorf("missing the Jira address, e-mail, Jira token or Tempo token (Settings → Jira and Tempo)")
	}
	return c, nil
}

type Client struct {
	cfg  Config
	http *http.Client

	mu        sync.Mutex
	accountID string
	issues    map[string]string
}

func New(cfg Config) *Client {
	return &Client{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second}, issues: map[string]string{}}
}

func (c *Client) JiraBase() string { return c.cfg.JiraBaseURL }

type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body) }

func (c *Client) do(ctx context.Context, method, base, path string, q url.Values, body any, auth string, out any) error {
	u, err := url.Parse(strings.TrimRight(base, "/") + path)
	if err != nil {
		return err
	}
	if q != nil {
		u.RawQuery = q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode >= 300 {
		return &HTTPError{Status: res.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

func (c *Client) jira(ctx context.Context, path string, out any) error {
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(c.cfg.AccountEmail+":"+c.cfg.Tokens.Jira))
	return c.do(ctx, http.MethodGet, c.cfg.JiraBaseURL, path, nil, nil, auth, out)
}

func (c *Client) tempo(ctx context.Context, method, path string, q url.Values, body, out any) error {
	return c.do(ctx, method, c.cfg.TempoBaseURL, path, q, body, "Bearer "+c.cfg.Tokens.Tempo, out)
}

// AccountID is the Jira account of the token owner (cached).
func (c *Client) AccountID(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.accountID != "" {
		return c.accountID, nil
	}
	var me struct {
		AccountID string `json:"accountId"`
	}
	if err := c.jira(ctx, "/rest/api/3/myself", &me); err != nil {
		return "", fmt.Errorf("Jira account: %w", err)
	}
	if me.AccountID == "" {
		return "", fmt.Errorf("Jira account: no accountId")
	}
	c.accountID = me.AccountID
	return me.AccountID, nil
}

// IssueID resolves a ticket key (ABC-123) to Jira's numeric issue id (cached).
func (c *Client) IssueID(ctx context.Context, key string) (string, error) {
	c.mu.Lock()
	id, ok := c.issues[key]
	c.mu.Unlock()
	if ok {
		return id, nil
	}
	var issue struct {
		ID string `json:"id"`
	}
	if err := c.jira(ctx, "/rest/api/3/issue/"+url.PathEscape(key), &issue); err != nil {
		return "", fmt.Errorf("%s in Jira: %w", key, err)
	}
	c.mu.Lock()
	c.issues[key] = issue.ID
	c.mu.Unlock()
	return issue.ID, nil
}

type Worklog struct {
	ID          string
	IssueID     string
	StartDate   string // 2026-11-03
	StartTime   string // 07:00:00
	Seconds     int
	Description string
}

// flexID reads an id Tempo may send as a number or a string.
type flexID string

func (f *flexID) UnmarshalJSON(b []byte) error {
	*f = flexID(strings.Trim(string(b), `"`))
	return nil
}

// Existing lists the user's Tempo worklogs of one day - used to skip duplicates.
func (c *Client) Existing(ctx context.Context, accountID string, day time.Time) ([]Worklog, error) {
	d := day.Format("2006-01-02")
	var res struct {
		Results []struct {
			ID    flexID `json:"tempoWorklogId"`
			Issue struct {
				ID flexID `json:"id"`
			} `json:"issue"`
			StartDate string `json:"startDate"`
			StartTime string `json:"startTime"`
			Seconds   int    `json:"timeSpentSeconds"`
		} `json:"results"`
	}
	q := url.Values{"from": {d}, "to": {d}, "limit": {"1000"}}
	if err := c.tempo(ctx, http.MethodGet, "/4/worklogs/user/"+url.PathEscape(accountID), q, nil, &res); err != nil {
		return nil, fmt.Errorf("worklogs from Tempo: %w", err)
	}
	out := make([]Worklog, 0, len(res.Results))
	for _, r := range res.Results {
		out = append(out, Worklog{ID: string(r.ID), IssueID: string(r.Issue.ID), StartDate: r.StartDate, StartTime: r.StartTime, Seconds: r.Seconds})
	}
	return out, nil
}

type NewWorklog struct {
	AccountID   string
	IssueID     string
	Start       time.Time
	Seconds     int
	Description string
}

// Create posts a worklog and returns its tempoWorklogId.
func (c *Client) Create(ctx context.Context, w NewWorklog) (string, error) {
	var issue any = w.IssueID
	if n, err := strconv.Atoi(w.IssueID); err == nil {
		issue = n
	}
	body := map[string]any{
		"authorAccountId":  w.AccountID,
		"issueId":          issue,
		"startDate":        w.Start.Format("2006-01-02"),
		"startTime":        w.Start.Format("15:04:05"),
		"timeSpentSeconds": w.Seconds,
		"description":      w.Description,
	}
	var res struct {
		ID flexID `json:"tempoWorklogId"`
	}
	if err := c.tempo(ctx, http.MethodPost, "/4/worklogs", nil, body, &res); err != nil {
		return "", err
	}
	if res.ID == "" {
		return "", fmt.Errorf("Tempo returned no tempoWorklogId")
	}
	return string(res.ID), nil
}

// Get reads one worklog by its tempoWorklogId.
func (c *Client) Get(ctx context.Context, id string) (Worklog, error) {
	var r struct {
		ID    flexID `json:"tempoWorklogId"`
		Issue struct {
			ID flexID `json:"id"`
		} `json:"issue"`
		StartDate   string `json:"startDate"`
		StartTime   string `json:"startTime"`
		Seconds     int    `json:"timeSpentSeconds"`
		Description string `json:"description"`
	}
	if err := c.tempo(ctx, http.MethodGet, "/4/worklogs/"+url.PathEscape(id), nil, nil, &r); err != nil {
		return Worklog{}, err
	}
	return Worklog{ID: string(r.ID), IssueID: string(r.Issue.ID), StartDate: r.StartDate, StartTime: r.StartTime, Seconds: r.Seconds, Description: r.Description}, nil
}

// Update overwrites a worklog. issueId is sent only when the worklog moves to another issue,
// so a plain description fix never depends on Tempo accepting that field.
func (c *Client) Update(ctx context.Context, id string, w NewWorklog, moveIssue bool) error {
	body := map[string]any{
		"authorAccountId":  w.AccountID,
		"startDate":        w.Start.Format("2006-01-02"),
		"startTime":        w.Start.Format("15:04:05"),
		"timeSpentSeconds": w.Seconds,
		"description":      w.Description,
	}
	if moveIssue {
		body["issueId"] = w.IssueID
		if n, err := strconv.Atoi(w.IssueID); err == nil {
			body["issueId"] = n
		}
	}
	return c.tempo(ctx, http.MethodPut, "/4/worklogs/"+url.PathEscape(id), nil, body, nil)
}

// Delete removes a worklog; one that is already gone counts as deleted.
func (c *Client) Delete(ctx context.Context, id string) error {
	err := c.tempo(ctx, http.MethodDelete, "/4/worklogs/"+url.PathEscape(id), nil, nil, nil)
	var he *HTTPError
	if errors.As(err, &he) && he.Status == http.StatusNotFound {
		return nil
	}
	return err
}
