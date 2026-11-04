package activity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type SlackConfig struct {
	Enabled *bool `json:"enabled"`
	// Token is a Slack user token (xoxp-…) with the search:read scope.
	Token   string `json:"token"`
	BaseURL string `json:"baseUrl"` // default https://slack.com/api
}

// SlackProbe turns the messages you sent today and yesterday (search.messages from:me) into
// "remote" spans; a ticket key in a message counts for its time. Time spent reading Slack and
// huddles come from the system probe (window titles, call windows).
type SlackProbe struct {
	cfg  SlackConfig
	http *http.Client

	mu    sync.Mutex
	count int
}

func NewSlackProbe(cfg SlackConfig) *SlackProbe {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://slack.com/api"
	}
	return &SlackProbe{cfg: cfg, http: &http.Client{Timeout: 20 * time.Second}}
}

func (p *SlackProbe) Name() string           { return "slack" }
func (p *SlackProbe) Every() time.Duration   { return 10 * time.Minute }
func (p *SlackProbe) Flush(time.Time) []Span { return nil }
func (p *SlackProbe) Days(now time.Time) []string {
	return []string{dayOf(now.AddDate(0, 0, -1)), dayOf(now)}
}
func (p *SlackProbe) Info() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return fmt.Sprintf("messages today and yesterday: %d", p.count)
}

func (p *SlackProbe) Tick(ctx context.Context, now time.Time) ([]Span, error) {
	from := now.AddDate(0, 0, -1)
	start := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, now.Location())
	// after: is exclusive, so ask from the day before yesterday
	q := url.Values{"query": {"from:me after:" + from.AddDate(0, 0, -1).Format("2006-01-02")}, "count": {"100"}, "sort": {"timestamp"}}
	var out []Span
	for page := 1; page <= 5; page++ {
		q.Set("page", strconv.Itoa(page))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.cfg.BaseURL, "/")+"/search.messages?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
		res, err := p.http.Do(req)
		if err != nil {
			return nil, err
		}
		b, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
		res.Body.Close()
		var r struct {
			OK       bool   `json:"ok"`
			Error    string `json:"error"`
			Messages struct {
				Matches []struct {
					Ts      string `json:"ts"`
					Text    string `json:"text"`
					Channel struct {
						Name string `json:"name"`
						IsIM bool   `json:"is_im"`
					} `json:"channel"`
				} `json:"matches"`
				Paging struct {
					Pages int `json:"pages"`
				} `json:"paging"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("slack: HTTP %d", res.StatusCode)
		}
		if !r.OK {
			return nil, fmt.Errorf("slack: %s", r.Error)
		}
		for _, m := range r.Messages.Matches {
			sec, err := strconv.ParseFloat(m.Ts, 64)
			if err != nil {
				continue
			}
			at := time.Unix(int64(sec), 0).In(now.Location())
			if at.Before(start) || at.After(now) {
				continue
			}
			begin := at.Add(-2 * time.Minute)
			if dayOf(begin) != dayOf(at) {
				begin = time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, at.Location())
			}
			where := "#" + m.Channel.Name
			if m.Channel.IsIM {
				where = "DM"
			}
			text := strings.Join(strings.Fields(m.Text), " ")
			if r := []rune(text); len(r) > 120 {
				text = string(r[:120]) + "…"
			}
			out = append(out, Span{Date: dayOf(at), From: clock(begin), To: clock(at), Kind: "remote", App: "Slack", Repo: where, Title: text})
		}
		if page >= r.Messages.Paging.Pages {
			break
		}
	}
	p.mu.Lock()
	p.count = len(out)
	p.mu.Unlock()
	return out, nil
}
