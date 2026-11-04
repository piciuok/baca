package activity

import (
	"regexp"
	"sort"
	"strings"
)

const Slot = 5 * 60 // seconds; suggestions use the same 5-minute grid as the timeline

var (
	titleTicket  = regexp.MustCompile(`\b([A-Z][A-Z0-9]{1,9})-(\d{1,6})\b`)
	branchTicket = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])([a-z]{2,10})-?(\d{1,6})(?:$|[^0-9])`)
	notTickets   = map[string]bool{"UTF": true, "ISO": true, "SHA": true, "RFC": true, "HTTP": true, "TLS": true, "MD": true, "COVID": true, "WIN": true}
)

// Tickets finds ticket keys in a window title or a branch name. With known prefixes (from the
// worklog) only those count; branch names may lack the dash (abc717 → ABC-717).
func Tickets(s string, branch bool, known map[string]bool) []string {
	var out []string
	add := func(prefix, num string) {
		prefix = strings.ToUpper(prefix)
		if notTickets[prefix] || (len(known) > 0 && !known[prefix]) {
			return
		}
		out = append(out, prefix+"-"+strings.TrimLeft(num, "0"))
	}
	if branch {
		for _, m := range branchTicket.FindAllStringSubmatch(s, -1) {
			add(m[1], m[2])
		}
		return out
	}
	for _, m := range titleTicket.FindAllStringSubmatch(s, -1) {
		add(m[1], m[2])
	}
	return out
}

// AppSpan is a stretch with one app in front, for the activity bar.
type AppSpan struct {
	Start int    `json:"start"` // seconds of the day
	End   int    `json:"end"`
	App   string `json:"app"`
	Idle  bool   `json:"idle,omitempty"`
}

type Share struct {
	Name    string `json:"name"`
	Minutes int    `json:"minutes"`
}

// Suggestion is a proposed worklog block.
type Suggestion struct {
	Start   int      `json:"start"` // minutes of the day, on the 5-minute grid
	End     int      `json:"end"`
	Ticket  string   `json:"ticket"`
	Sources []string `json:"sources"` // where the ticket came from: "window title", "changes in repo (branch)", ...
	Apps    []Share  `json:"apps"`
	Titles  []Share  `json:"titles"`
	Meeting string   `json:"meeting,omitempty"` // call window title when the block is a meeting
}

type Day struct {
	Apps        []AppSpan    `json:"apps"`
	Suggestions []Suggestion `json:"suggestions"`
}

// Analyze merges the window spans per app for the bar and proposes blocks: every 5-minute
// slot with at least 2 active minutes (or code activity) gets the ticket with the most
// evidence - seconds of a title naming it, double for repo/Claude activity on its branch.
// A slot without evidence continues the ticket of the slot before it.
func Analyze(spans []Span, known map[string]bool) Day {
	d := Day{Apps: []AppSpan{}, Suggestions: []Suggestion{}}
	const slots = 24 * 3600 / Slot
	active := make([]int, slots)
	code := make([]bool, slots)
	votes := make([]map[string]int, slots)
	src := make([]map[string]map[string]bool, slots)
	apps := make([]map[string]int, slots)
	meets := make([]map[string]int, slots)
	titles := make([]map[string]int, slots)
	vote := func(i int, t, from string, w int) {
		if votes[i] == nil {
			votes[i], src[i] = map[string]int{}, map[string]map[string]bool{}
		}
		votes[i][t] += w
		if src[i][t] == nil {
			src[i][t] = map[string]bool{}
		}
		src[i][t][from] = true
	}
	add := func(m *[]map[string]int, i int, k string, v int) {
		if (*m)[i] == nil {
			(*m)[i] = map[string]int{}
		}
		(*m)[i][k] += v
	}

	sorted := append([]Span(nil), spans...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Start() < sorted[j].Start() })
	for _, s := range sorted {
		a, b := s.Start(), s.End()
		if b <= a {
			continue
		}
		if s.Kind == "window" || s.Kind == "idle" {
			idle := s.Kind == "idle"
			if n := len(d.Apps); n > 0 && d.Apps[n-1].App == s.App && d.Apps[n-1].Idle == idle && d.Apps[n-1].End >= a-Slot/10 {
				d.Apps[n-1].End = max(d.Apps[n-1].End, b)
			} else {
				d.Apps = append(d.Apps, AppSpan{Start: a, End: b, App: s.App, Idle: idle})
			}
		}
		var tickets []string
		var from string
		switch s.Kind {
		case "window":
			tickets, from = Tickets(s.Title, false, known), "window title"
			if s.Branch != "" { // an IDE window of a watched repo: its branch counts too
				bt := Tickets(s.Branch, true, known)
				if len(tickets) == 0 && len(bt) > 0 {
					tickets, from = bt, s.App+" in "+s.Repo+" ("+s.Branch+")"
				}
			}
		case "meeting":
			from = s.App
		case "repo", "agent":
			tickets = Tickets(s.Branch, true, known)
			if s.Kind == "repo" {
				from = "changes in " + s.Repo
			} else {
				from = s.App + " in " + s.Repo
			}
			if s.Branch != "" {
				from += " (" + s.Branch + ")"
			}
		}
		for i := a / Slot; i <= (b-1)/Slot && i < slots; i++ {
			lo, hi := max(a, i*Slot), min(b, (i+1)*Slot)
			if hi <= lo {
				continue
			}
			ov := hi - lo
			switch s.Kind {
			case "window":
				active[i] += ov
				add(&apps, i, s.App, ov)
				if s.Title != "" {
					add(&titles, i, s.App+" · "+s.Title, ov)
				}
			case "meeting":
				code[i] = true
				add(&meets, i, s.App+" · "+s.Title, ov)
			case "repo", "agent":
				code[i] = true
				ov *= 2
			}
			for _, t := range tickets {
				vote(i, t, from, ov)
			}
		}
	}

	ticketOf := make([]string, slots)
	on := make([]bool, slots)
	for i := range slots {
		on[i] = active[i] >= 120 || code[i]
		if !on[i] {
			continue
		}
		// a call that fills most of the slot makes it a meeting block of its own
		if mt, mw := topKey(meets[i]); mw >= Slot/2 {
			ticketOf[i] = meetingKey + mt
			continue
		}
		best, bw := "", 0
		for t, w := range votes[i] {
			if w > bw || (w == bw && t < best) {
				best, bw = t, w
			}
		}
		if best == "" && i > 0 && on[i-1] && !strings.HasPrefix(ticketOf[i-1], meetingKey) {
			best = ticketOf[i-1]
		}
		ticketOf[i] = best
	}
	for i := 0; i < slots; {
		if !on[i] {
			i++
			continue
		}
		j := i
		for j < slots && on[j] && ticketOf[j] == ticketOf[i] {
			j++
		}
		sg := Suggestion{Start: i * Slot / 60, End: j * Slot / 60, Ticket: ticketOf[i], Sources: []string{}}
		if strings.HasPrefix(sg.Ticket, meetingKey) {
			sg.Meeting, sg.Ticket = strings.TrimPrefix(sg.Ticket, meetingKey), ""
		}
		appSum, titleSum, srcSet := map[string]int{}, map[string]int{}, map[string]bool{}
		for k := i; k < j; k++ {
			for a, v := range apps[k] {
				appSum[a] += v
			}
			for t, v := range titles[k] {
				titleSum[t] += v
			}
			for s := range src[k][sg.Ticket] {
				srcSet[s] = true
			}
		}
		for s := range srcSet {
			sg.Sources = append(sg.Sources, s)
		}
		sort.Strings(sg.Sources)
		sg.Apps, sg.Titles = top(appSum, 4), top(titleSum, 5)
		d.Suggestions = append(d.Suggestions, sg)
		i = j
	}
	return d
}

const meetingKey = "\x00meeting:"

func topKey(m map[string]int) (string, int) {
	best, bw := "", 0
	for k, v := range m {
		if v > bw || (v == bw && k < best) {
			best, bw = k, v
		}
	}
	return best, bw
}

func top(m map[string]int, n int) []Share {
	out := make([]Share, 0, len(m))
	for k, v := range m {
		if v >= 30 {
			out = append(out, Share{Name: k, Minutes: (v + 30) / 60})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Minutes != out[j].Minutes {
			return out[i].Minutes > out[j].Minutes
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}
