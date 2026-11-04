package activity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type CalendarConfig struct {
	Enabled *bool `json:"enabled"`
	// URLs are private iCal addresses (Google: "Secret address in iCal format", Outlook:
	// "Publish calendar" → ICS). Treat them like passwords.
	URLs []string `json:"urls"`
	// Email marks your own attendance: meetings you declined are skipped. Default: accountEmail.
	Email string `json:"email"`
}

// CalendarProbe turns calendar events of today and yesterday into "meeting" spans, recurring
// ones included (RRULE DAILY/WEEKLY/MONTHLY with INTERVAL, COUNT, UNTIL, BYDAY, EXDATE and
// moved occurrences). All-day events, cancelled and declined meetings are skipped.
type CalendarProbe struct {
	cfg  CalendarConfig
	http *http.Client

	mu    sync.Mutex
	count int
}

func NewCalendarProbe(cfg CalendarConfig) *CalendarProbe {
	return &CalendarProbe{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second}}
}

func (p *CalendarProbe) Name() string           { return "calendar" }
func (p *CalendarProbe) Every() time.Duration   { return 15 * time.Minute }
func (p *CalendarProbe) Flush(time.Time) []Span { return nil }
func (p *CalendarProbe) Days(now time.Time) []string {
	return []string{dayOf(now.AddDate(0, 0, -1)), dayOf(now)}
}
func (p *CalendarProbe) Info() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return fmt.Sprintf("meetings today and yesterday: %d", p.count)
}

func (p *CalendarProbe) Tick(ctx context.Context, now time.Time) ([]Span, error) {
	var out []Span
	var errs []string
	for i, u := range p.cfg.URLs {
		// the URL is a secret: errors name the calendar by number, never by address
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.Replace(u, "webcal://", "https://", 1), nil)
		if err != nil {
			errs = append(errs, fmt.Sprintf("calendar %d: not a valid URL", i+1))
			continue
		}
		res, err := p.http.Do(req)
		if err != nil {
			errs = append(errs, fmt.Sprintf("calendar %d: cannot fetch (network or TLS error)", i+1))
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(res.Body, 32<<20))
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			errs = append(errs, fmt.Sprintf("calendar %d: HTTP %d", i+1, res.StatusCode))
			continue
		}
		out = append(out, CalendarSpans(string(b), p.cfg.Email, now)...)
	}
	p.mu.Lock()
	p.count = len(out)
	p.mu.Unlock()
	if len(errs) > 0 {
		return out, errors.New(strings.Join(errs, "; "))
	}
	return out, nil
}

type icsProp struct {
	params map[string]string
	value  string
}

type icsEvent struct {
	props map[string][]icsProp
}

func (e icsEvent) get(name string) (icsProp, bool) {
	v := e.props[name]
	if len(v) == 0 {
		return icsProp{}, false
	}
	return v[0], true
}

func parseICS(text string) []icsEvent {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\n ", "")
	text = strings.ReplaceAll(text, "\n\t", "")
	var events []icsEvent
	var cur *icsEvent
	depth := 0 // VALARM etc. inside VEVENT
	for _, line := range strings.Split(text, "\n") {
		switch {
		case line == "BEGIN:VEVENT":
			cur, depth = &icsEvent{props: map[string][]icsProp{}}, 0
			continue
		case line == "END:VEVENT":
			if cur != nil {
				events = append(events, *cur)
			}
			cur = nil
			continue
		case cur == nil:
			continue
		case strings.HasPrefix(line, "BEGIN:"):
			depth++
			continue
		case strings.HasPrefix(line, "END:"):
			depth--
			continue
		case depth > 0:
			continue
		}
		head, value, ok := cutOutsideQuotes(line, ':') // CN="Doe: Jan" is not the value
		if !ok {
			continue
		}
		parts := splitOutsideQuotes(head, ';')
		pr := icsProp{params: map[string]string{}, value: value}
		for _, p := range parts[1:] {
			if k, v, ok := strings.Cut(p, "="); ok {
				pr.params[strings.ToUpper(k)] = strings.Trim(v, `"`)
			}
		}
		name := strings.ToUpper(parts[0])
		cur.props[name] = append(cur.props[name], pr)
	}
	return events
}

func cutOutsideQuotes(s string, sep byte) (string, string, bool) {
	q := false
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '"':
			q = !q
		case s[i] == sep && !q:
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

func splitOutsideQuotes(s string, sep byte) []string {
	var out []string
	for {
		a, b, ok := cutOutsideQuotes(s, sep)
		out = append(out, a)
		if !ok {
			return out
		}
		s = b
	}
}

var isoDuration = regexp.MustCompile(`^([+-])?P(?:(\d+)W)?(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?$`)

// parseDuration reads an iCal DURATION: PT1H30M, P1D, P1DT2H, P2W.
func parseDuration(v string) (time.Duration, bool) {
	m := isoDuration.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(v)))
	if m == nil {
		return 0, false
	}
	n := func(i int) time.Duration { x, _ := strconv.Atoi(m[i]); return time.Duration(x) }
	d := n(2)*7*24*time.Hour + n(3)*24*time.Hour + n(4)*time.Hour + n(5)*time.Minute + n(6)*time.Second
	if m[1] == "-" {
		d = -d
	}
	return d, true
}

var windowsZones = map[string]string{
	"Central European Standard Time": "Europe/Warsaw", "Central Europe Standard Time": "Europe/Budapest",
	"W. Europe Standard Time": "Europe/Berlin", "Romance Standard Time": "Europe/Paris",
	"GMT Standard Time": "Europe/London", "UTC": "UTC", "Eastern Standard Time": "America/New_York",
	"Pacific Standard Time": "America/Los_Angeles", "FLE Standard Time": "Europe/Kiev",
}

// icsTime reads DTSTART-like values in the event's own zone (UTC for "Z", TZID, or local for a
// floating time): a recurring event repeats at its own wall-clock time, so the series is expanded
// there and only the occurrences are converted. allDay is true for VALUE=DATE.
func icsTime(p icsProp, local *time.Location) (t time.Time, allDay bool, err error) {
	v := p.value
	if p.params["VALUE"] == "DATE" || len(v) == 8 {
		t, err = time.ParseInLocation("20060102", v, local)
		return t, true, err
	}
	if strings.HasSuffix(v, "Z") {
		t, err = time.Parse("20060102T150405Z", v)
		return t, false, err
	}
	loc := local
	if tz := p.params["TZID"]; tz != "" {
		if z, ok := windowsZones[tz]; ok {
			tz = z
		}
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	t, err = time.ParseInLocation("20060102T150405", v, loc)
	return t, false, err
}

// instant keys an occurrence by its moment, whatever zone it is written in.
func instant(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func icsText(s string) string {
	return strings.NewReplacer(`\n`, " ", `\N`, " ", `\,`, ",", `\;`, ";", `\\`, `\`).Replace(s)
}

var weekdays = map[string]time.Weekday{"SU": time.Sunday, "MO": time.Monday, "TU": time.Tuesday, "WE": time.Wednesday, "TH": time.Thursday, "FR": time.Friday, "SA": time.Saturday}

// occurrences lists the starts of a recurring event up to until (inclusive of overlap checks by caller).
func occurrences(start time.Time, rule string, until time.Time) []time.Time {
	r := map[string]string{}
	for _, part := range strings.Split(rule, ";") {
		if k, v, ok := strings.Cut(part, "="); ok {
			r[strings.ToUpper(k)] = v
		}
	}
	interval, _ := strconv.Atoi(r["INTERVAL"])
	if interval < 1 {
		interval = 1
	}
	count, _ := strconv.Atoi(r["COUNT"])
	last := until
	if u := r["UNTIL"]; u != "" {
		if t, _, err := icsTime(icsProp{value: u}, start.Location()); err == nil && t.Before(last) {
			last = t
			if len(u) == 8 { // a date: the whole day counts
				last = t.AddDate(0, 0, 1).Add(-time.Second)
			}
		}
	}
	type byDay struct {
		pos int // 0: every such weekday; 1: the first in the month; -1: the last
		wd  time.Weekday
	}
	var days []time.Weekday
	var byDays []byDay
	for _, d := range strings.Split(r["BYDAY"], ",") {
		name := strings.TrimLeft(d, "+-0123456789")
		wd, ok := weekdays[name]
		if !ok {
			continue
		}
		pos, _ := strconv.Atoi(strings.TrimPrefix(d[:len(d)-len(name)], "+"))
		days = append(days, wd)
		byDays = append(byDays, byDay{pos, wd})
	}
	var out []time.Time
	emit := func(t time.Time) bool {
		if t.Before(start) {
			return true
		}
		if t.After(last) || (count > 0 && len(out) >= count) {
			return false
		}
		out = append(out, t)
		return true
	}
	at := func(base time.Time, y int, m time.Month, d int) time.Time {
		return time.Date(y, m, d, base.Hour(), base.Minute(), base.Second(), 0, base.Location())
	}
	for i := 0; i < 5000; i++ {
		switch r["FREQ"] {
		case "DAILY":
			if !emit(start.AddDate(0, 0, i*interval)) {
				return out
			}
		case "WEEKLY":
			if len(days) == 0 {
				if !emit(start.AddDate(0, 0, 7*i*interval)) {
					return out
				}
				continue
			}
			// the week (from Monday) of the i-th interval
			monday := start.AddDate(0, 0, -((int(start.Weekday())+6)%7)+7*i*interval)
			week := make([]time.Time, 0, len(days))
			for _, wd := range days {
				d := monday.AddDate(0, 0, (int(wd)+6)%7)
				week = append(week, at(start, d.Year(), d.Month(), d.Day()))
			}
			sort.Slice(week, func(a, b int) bool { return week[a].Before(week[b]) })
			for _, t := range week {
				if !emit(t) {
					return out
				}
			}
		case "MONTHLY":
			first := time.Date(start.Year(), start.Month()+time.Month(i*interval), 1, 0, 0, 0, 0, start.Location())
			if len(byDays) == 0 {
				t := at(start, first.Year(), first.Month(), start.Day())
				if t.Day() != start.Day() { // no such day this month
					continue
				}
				if !emit(t) {
					return out
				}
				continue
			}
			// BYDAY=1MO (first Monday), -1FR (last Friday), TU (every Tuesday)
			dim := first.AddDate(0, 1, -1).Day()
			var month []time.Time
			for _, b := range byDays {
				var matches []int
				for d := 1; d <= dim; d++ {
					if time.Date(first.Year(), first.Month(), d, 0, 0, 0, 0, first.Location()).Weekday() == b.wd {
						matches = append(matches, d)
					}
				}
				switch {
				case b.pos == 0:
					for _, d := range matches {
						month = append(month, at(start, first.Year(), first.Month(), d))
					}
				case b.pos > 0 && b.pos <= len(matches):
					month = append(month, at(start, first.Year(), first.Month(), matches[b.pos-1]))
				case b.pos < 0 && -b.pos <= len(matches):
					month = append(month, at(start, first.Year(), first.Month(), matches[len(matches)+b.pos]))
				}
			}
			sort.Slice(month, func(a, b int) bool { return month[a].Before(month[b]) })
			for _, t := range month {
				if !emit(t) {
					return out
				}
			}
		default:
			return append(out, start)
		}
	}
	return out
}

// CalendarSpans returns the meetings of the day of now and the day before.
func CalendarSpans(ics, email string, now time.Time) []Span {
	local := now.Location()
	from := time.Date(now.Year(), now.Month(), now.Day()-1, 0, 0, 0, 0, local)
	to := from.AddDate(0, 0, 2)
	email = strings.ToLower(email)

	type occ struct {
		start, end time.Time
		title      string
	}
	moved := map[string]bool{} // uid|original start of moved/cancelled occurrences
	var single []icsEvent
	var series []icsEvent
	for _, e := range parseICS(ics) {
		if rid, ok := e.get("RECURRENCE-ID"); ok {
			uid, _ := e.get("UID")
			if t, _, err := icsTime(rid, local); err == nil {
				moved[uid.value+"|"+instant(t)] = true
			}
			single = append(single, e)
			continue
		}
		if _, ok := e.get("RRULE"); ok {
			series = append(series, e)
		} else {
			single = append(single, e)
		}
	}
	skip := func(e icsEvent) bool {
		if st, ok := e.get("STATUS"); ok && strings.EqualFold(st.value, "CANCELLED") {
			return true
		}
		if email == "" {
			return false
		}
		for _, a := range e.props["ATTENDEE"] {
			if strings.EqualFold(strings.TrimPrefix(strings.ToLower(a.value), "mailto:"), email) && strings.EqualFold(a.params["PARTSTAT"], "DECLINED") {
				return true
			}
		}
		return false
	}
	length := func(e icsEvent, start time.Time) (time.Duration, bool) {
		if en, ok := e.get("DTEND"); ok {
			if t, _, err := icsTime(en, local); err == nil {
				return t.Sub(start), true
			}
		}
		if d, ok := e.get("DURATION"); ok {
			if dur, ok := parseDuration(d.value); ok {
				return dur, true
			}
		}
		return 0, false
	}
	var occs []occ
	add := func(e icsEvent, start time.Time, d time.Duration) {
		end := start.Add(d)
		if end.After(from) && start.Before(to) {
			sum, _ := e.get("SUMMARY")
			occs = append(occs, occ{start.In(local), end.In(local), icsText(sum.value)})
		}
	}
	for _, e := range single {
		ds, ok := e.get("DTSTART")
		if !ok || skip(e) {
			continue
		}
		start, allDay, err := icsTime(ds, local)
		if err != nil || allDay {
			continue
		}
		if d, ok := length(e, start); ok {
			add(e, start, d)
		}
	}
	for _, e := range series {
		ds, ok := e.get("DTSTART")
		if !ok || skip(e) {
			continue
		}
		start, allDay, err := icsTime(ds, local)
		if err != nil || allDay {
			continue
		}
		d, ok := length(e, start)
		if !ok {
			continue
		}
		uid, _ := e.get("UID")
		ex := map[string]bool{}
		for _, x := range e.props["EXDATE"] {
			for _, v := range strings.Split(x.value, ",") {
				if t, _, err := icsTime(icsProp{params: x.params, value: v}, local); err == nil {
					ex[instant(t)] = true
				}
			}
		}
		rule, _ := e.get("RRULE")
		for _, t := range occurrences(start, rule.value, to) {
			k := instant(t)
			if ex[k] || moved[uid.value+"|"+k] {
				continue
			}
			add(e, t, d)
		}
	}
	var out []Span
	for _, o := range occs {
		for day := from; day.Before(to); day = day.AddDate(0, 0, 1) {
			next := day.AddDate(0, 0, 1)
			a, b := o.start, o.end
			if a.Before(day) {
				a = day
			}
			if b.After(next) {
				b = next.Add(-time.Second)
			}
			if !b.After(a) {
				continue
			}
			out = append(out, Span{Date: dayOf(day), From: clock(a), To: clock(b), Kind: "meeting", App: "Calendar", Title: o.title})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date+out[i].From < out[j].Date+out[j].From })
	return out
}
