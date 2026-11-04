package activity

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type JiraConfig struct {
	Enabled *bool `json:"enabled"`
}

// JiraAuth comes from the Tempo/Jira part of config.json (jiraBaseUrl, accountEmail, tokens.jira).
type JiraAuth struct {
	BaseURL, Email, Token string
}

// JiraProbe turns the user's own changes (status, fields, assignee) and comments in Jira of today
// and yesterday into "remote" spans. Worklog fields are skipped: logging hours is not the work.
type JiraProbe struct {
	auth JiraAuth
	http *http.Client

	mu      sync.Mutex
	account string
	count   int
}

func NewJiraProbe(auth JiraAuth) *JiraProbe {
	return &JiraProbe{auth: auth, http: &http.Client{Timeout: 20 * time.Second}}
}

func (p *JiraProbe) Name() string           { return "jira" }
func (p *JiraProbe) Every() time.Duration   { return 10 * time.Minute }
func (p *JiraProbe) Flush(time.Time) []Span { return nil }
func (p *JiraProbe) Days(now time.Time) []string {
	return []string{dayOf(now.AddDate(0, 0, -1)), dayOf(now)}
}
func (p *JiraProbe) Info() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return fmt.Sprintf("changes and comments today and yesterday: %d", p.count)
}

var worklogFields = map[string]bool{"timespent": true, "timeestimate": true, "timeoriginalestimate": true, "worklogid": true, "worklogtimespent": true, "aggregatetimespent": true, "aggregatetimeestimate": true}

func (p *JiraProbe) call(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(p.auth.BaseURL, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(p.auth.Email+":"+p.auth.Token)))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	res, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("jira %s: HTTP %d", strings.SplitN(path, "?", 2)[0], res.StatusCode)
	}
	return json.Unmarshal(b, out)
}

type jiraTime struct{ time.Time }

func (t *jiraTime) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	v, err := time.Parse("2006-01-02T15:04:05.000-0700", s)
	if err != nil {
		v, err = time.Parse(time.RFC3339, s)
	}
	t.Time = v
	return err
}

func (p *JiraProbe) Tick(ctx context.Context, now time.Time) ([]Span, error) {
	p.mu.Lock()
	acc := p.account
	p.mu.Unlock()
	if acc == "" {
		var me struct {
			AccountID string `json:"accountId"`
		}
		if err := p.call(ctx, http.MethodGet, "/rest/api/3/myself", nil, &me); err != nil {
			return nil, err
		}
		acc = me.AccountID
		p.mu.Lock()
		p.account = acc
		p.mu.Unlock()
	}
	from := now.AddDate(0, 0, -1)
	start := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, now.Location())
	jf, jt := from.Format("2006/01/02"), now.AddDate(0, 0, 1).Format("2006/01/02")
	jql := fmt.Sprintf(`issuekey in updatedBy("%s", "%s", "%s") OR (updated >= "%s" AND (assignee = currentUser() OR reporter = currentUser() OR watcher = currentUser()))`, acc, jf, jt, jf)
	type issue struct {
		Key    string `json:"key"`
		Fields struct {
			Summary string `json:"summary"`
		} `json:"fields"`
	}
	var found struct{ Issues []issue }
	token := ""
	for page := 0; page < 5; page++ { // up to 500 issues touched in two days
		var res struct {
			Issues []issue `json:"issues"`
			Next   string  `json:"nextPageToken"`
		}
		body := map[string]any{"jql": jql, "fields": []string{"summary"}, "maxResults": 100}
		if token != "" {
			body["nextPageToken"] = token
		}
		if err := p.call(ctx, http.MethodPost, "/rest/api/3/search/jql", body, &res); err != nil {
			return nil, err
		}
		found.Issues = append(found.Issues, res.Issues...)
		if token = res.Next; token == "" {
			break
		}
	}
	var errs []string
	var out []Span
	add := func(key, summary, what string, at time.Time, lead time.Duration) {
		at = at.In(now.Location())
		if at.Before(start) || at.After(now) {
			return
		}
		begin := at.Add(-lead)
		if dayOf(begin) != dayOf(at) {
			begin = time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, at.Location())
		}
		out = append(out, Span{Date: dayOf(at), From: clock(begin), To: clock(at), Kind: "remote", App: "Jira", Repo: strings.SplitN(key, "-", 2)[0], Title: key + " " + summary + ": " + what})
	}
	for _, is := range found.Issues {
		var ch struct {
			Total  int `json:"total"`
			Values []struct {
				Author struct {
					AccountID string `json:"accountId"`
				} `json:"author"`
				Created jiraTime `json:"created"`
				Items   []struct {
					Field      string `json:"field"`
					FromString string `json:"fromString"`
					ToString   string `json:"toString"`
				} `json:"items"`
			} `json:"values"`
		}
		// the changelog is oldest first: on a long-lived issue today's changes sit on the last page
		path := "/rest/api/3/issue/" + url.PathEscape(is.Key) + "/changelog?maxResults=100"
		if err := p.call(ctx, http.MethodGet, path, nil, &ch); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if ch.Total > 100 {
			if err := p.call(ctx, http.MethodGet, fmt.Sprintf("%s&startAt=%d", path, ch.Total-100), nil, &ch); err != nil {
				errs = append(errs, err.Error())
				continue
			}
		}
		for _, v := range ch.Values {
			if v.Author.AccountID != acc {
				continue
			}
			var parts []string
			for _, it := range v.Items {
				if worklogFields[strings.ToLower(it.Field)] {
					continue
				}
				if it.Field == "status" {
					parts = append(parts, fmt.Sprintf("status %s → %s", it.FromString, it.ToString))
				} else {
					parts = append(parts, it.Field)
				}
			}
			if len(parts) > 0 {
				add(is.Key, is.Fields.Summary, strings.Join(parts, ", "), v.Created.Time, 2*time.Minute)
			}
		}
		var cm struct {
			Comments []struct {
				Author struct {
					AccountID string `json:"accountId"`
				} `json:"author"`
				Created jiraTime `json:"created"`
			} `json:"comments"`
		}
		if err := p.call(ctx, http.MethodGet, "/rest/api/3/issue/"+url.PathEscape(is.Key)+"/comment?maxResults=100&orderBy=-created", nil, &cm); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		for _, c := range cm.Comments {
			if c.Author.AccountID == acc {
				add(is.Key, is.Fields.Summary, "comment", c.Created.Time, 5*time.Minute)
			}
		}
	}
	p.mu.Lock()
	p.count = len(out)
	p.mu.Unlock()
	if len(errs) > 0 { // one issue that fails (no permission, deleted) does not hide the others
		return out, fmt.Errorf("jira: %d issue(s) could not be read: %s", len(errs), errs[0])
	}
	return out, nil
}
