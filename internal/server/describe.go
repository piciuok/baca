package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"baca/internal/activity"
	"baca/internal/describe"
	"baca/internal/worklog"
)

// DescribeProposal is the agent's description of a block next to the one it has now.
type DescribeProposal struct {
	Key string `json:"key"`
	Old string `json:"old"`
	New string `json:"new"`
}

// describeDay asks the agent for descriptions of the day's open blocks (all with a ticket, or
// the given keys). It writes nothing: the UI fills empty descriptions and asks about the rest.
func (s *Server) describeDay(w http.ResponseWriter, r *http.Request) {
	date, err := parseDate(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err, nil)
		return
	}
	var body struct {
		Keys []string `json:"keys"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, err, nil)
		return
	}
	if !s.describeMu.TryLock() {
		fail(w, http.StatusConflict, errors.New("descriptions are being written already"), nil)
		return
	}
	defer s.describeMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), describe.Timeout)
	defer cancel()

	st := s.st()
	req, blocks, err := s.describeRequest(ctx, st, date, body.Keys)
	if err != nil {
		fail(w, http.StatusUnprocessableEntity, err, nil)
		return
	}
	run := s.DescribeRunner
	if run == nil {
		run = describe.AgentRunner(st.Describe)
	}
	out, err := run(ctx, describe.Render(req))
	if err != nil {
		fail(w, http.StatusBadGateway, err, nil)
		return
	}
	props, err := describe.Parse(out, blocks)
	if err != nil {
		fail(w, http.StatusBadGateway, err, nil)
		return
	}
	cur := map[string]string{}
	for _, b := range blocks {
		cur[b.Key] = b.Current
	}
	res := []DescribeProposal{}
	for _, p := range props {
		res = append(res, DescribeProposal{Key: p.Key, Old: cur[p.Key], New: p.Desc})
	}
	writeJSON(w, http.StatusOK, map[string]any{"proposals": res, "reviewed": describe.Reviewed(st.Describe)})
}

func hm(m int) string { return fmt.Sprintf("%02d:%02d", m/60, m%60) }

func clock(t time.Time) int { return t.Hour()*60 + t.Minute() }

// describeRequest gathers the evidence of each block: commits and agent prompts in watched repos
// around its time (or naming its ticket), window titles and service events during it.
func (s *Server) describeRequest(ctx context.Context, st *State, date time.Time, keys []string) (describe.Request, []describe.Block, error) {
	v, err := s.Store.GetDay(date)
	if err != nil {
		return describe.Request{}, nil, err
	}
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
	}
	earlier, _ := s.Store.Earlier(date, 3)
	seenBefore := func(t string) bool {
		for _, d := range earlier {
			if strings.Contains(d, "] - "+t+" - ") {
				return true
			}
		}
		return false
	}
	entries := append([]worklog.Entry(nil), v.Entries...)
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Start < entries[j].Start })
	firstOf := map[string]string{}
	for _, e := range entries {
		if _, ok := firstOf[e.Ticket]; !ok {
			firstOf[e.Ticket] = e.Key()
		}
	}

	repos := activity.Repos(st.GitRepos)
	commits := describe.Commits(ctx, repos, date)
	var prompts []describe.Prompt
	for _, p := range describe.Prompts(ctx, date) {
		if activity.RepoOf(p.Cwd, repos) != "" { // work outside the watched repos stays out
			prompts = append(prompts, p)
		}
	}
	spans, _ := st.Activity.Read(date.Format("2006-01-02"))

	var blocks []describe.Block
	for _, e := range entries {
		if e.Logged || e.Ticket == "" || (len(want) > 0 && !want[e.Key()]) {
			continue
		}
		b := describe.Block{Key: e.Key(), Ticket: e.Ticket, From: hm(e.Start), To: hm(e.End), Current: e.Desc,
			FirstOfTicket: firstOf[e.Ticket] == e.Key(), TicketSeenBefore: seenBefore(e.Ticket)}
		near := func(t time.Time) bool { m := clock(t); return m >= e.Start-10 && m < e.End+10 }
		// ABC-1 in a branch or subject: abc-1, ABC1, but never ABC-12
		prefix, num, _ := strings.Cut(e.Ticket, "-")
		ticketRE := regexp.MustCompile(`(?i)(?:^|[^a-z0-9])` + regexp.QuoteMeta(prefix) + `-?0*` + regexp.QuoteMeta(num) + `(?:$|[^0-9])`)
		var during, named []string // commits during the block first, so the cap keeps them
		for _, c := range commits {
			line := fmt.Sprintf("%s %s (%s): %s", c.At.Format("15:04"), c.Repo, c.Branch, c.Subject)
			switch {
			case near(c.At):
				during = append(during, line)
			case ticketRE.MatchString(c.Branch) || ticketRE.MatchString(c.Subject):
				named = append(named, line)
			}
		}
		b.Evidence.Commits = append(during, named...)
		for _, p := range prompts {
			if near(p.At) {
				b.Evidence.Prompts = append(b.Evidence.Prompts, fmt.Sprintf("%s %s in %s: %s", p.At.Format("15:04"), p.Agent, filepath.Base(p.Cwd), p.Text))
			}
		}
		titles := map[string]int{}
		for _, sp := range spans {
			a, z := sp.Start()/60, sp.End()/60
			lo, hi := max(a, e.Start), min(z, e.End)
			if hi <= lo && !(sp.Kind == "remote" && z >= e.Start && z <= e.End) {
				continue
			}
			switch sp.Kind {
			case "window":
				if sp.Title != "" {
					titles[sp.App+" · "+sp.Title] += sp.End() - sp.Start()
				}
			case "remote", "meeting":
				b.Evidence.Events = append(b.Evidence.Events, fmt.Sprintf("%s %s %s: %s", sp.To[:5], sp.App, sp.Repo, sp.Title))
			}
		}
		type tw struct {
			t string
			s int
		}
		var tws []tw
		for t, sec := range titles {
			if sec >= 60 {
				tws = append(tws, tw{t, sec})
			}
		}
		sort.Slice(tws, func(i, j int) bool { return tws[i].s > tws[j].s })
		for i, x := range tws {
			if i == 8 {
				break
			}
			b.Evidence.Windows = append(b.Evidence.Windows, fmt.Sprintf("%dm %s", (x.s+30)/60, x.t))
		}
		b.Evidence.Commits = capList(b.Evidence.Commits, 20)
		b.Evidence.Prompts = capList(b.Evidence.Prompts, 20)
		b.Evidence.Events = capList(b.Evidence.Events, 20)
		blocks = append(blocks, b)
	}
	if len(blocks) == 0 {
		return describe.Request{}, nil, errors.New("no open block with a ticket to describe")
	}
	day := worklog.Header(date)[1:] + "." + date.Format("2006")
	return describe.Request{Day: day, Earlier: earlier, Blocks: blocks, Style: st.Describe.Style}, blocks, nil
}

func capList(l []string, n int) []string {
	if len(l) > n {
		return l[:n]
	}
	return l
}
