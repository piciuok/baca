// Package worklog reads and writes the human-readable worklog file:
//
//	@Wtorek 03.11
//	*[07:00 - 07:10] - ABC-752 - CR #~123456~#
//	[07:10 - 08:40] - ABC-868 - Write the setup guide, ...
//
// Days start with an "@<weekday> DD.MM[.YYYY]" header, newest day on top. An entry
// already logged in Tempo starts with "*" and ends with "#~<tempoWorklogId>~#".
// Every line the parser does not understand is kept as it is, and a write touches
// only the lines of the day being written.
package worklog

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Entry struct {
	Start     int    `json:"start"` // minutes from midnight
	End       int    `json:"end"`
	Ticket    string `json:"ticket"`
	Desc      string `json:"desc"`
	Logged    bool   `json:"logged"`
	WorklogID string `json:"worklogId,omitempty"`
}

func (e Entry) Minutes() int { return e.End - e.Start }

// Key identifies an entry inside a day (the file has no other stable id).
func (e Entry) Key() string { return fmt.Sprintf("%d-%d-%s", e.Start, e.End, e.Ticket) }

// Description is what goes to the file and to Tempo: an empty one means "Continue task".
func (e Entry) Description() string {
	d := strings.Join(strings.Fields(e.Desc), " ")
	if d == "" {
		return "Continue task"
	}
	return d
}

func (e Entry) String() string {
	t := e.Ticket
	if t == "" {
		t = "???"
	}
	s := fmt.Sprintf("[%s - %s] - %s - %s", HM(e.Start), HM(e.End), t, e.Description())
	if e.Logged {
		s = "*" + s
		if e.WorklogID != "" {
			s += " #~" + e.WorklogID + "~#"
		}
	}
	return s
}

type Day struct {
	Date    time.Time // local midnight
	Header  string    // the header line as written in the file
	Extra   []string  // other non-blank lines of the day (comments, notes) - kept on write
	Entries []Entry

	start, end int // lines[start:end] - header, body and trailing blank lines
	blanks     int // trailing blank lines
}

func (d *Day) Key() string { return d.Date.Format("2006-01-02") }

func (d *Day) Minutes() (n int) {
	for _, e := range d.Entries {
		n += e.Minutes()
	}
	return
}

// Text is the day's section without trailing blank lines - the base of its version hash.
func (d *Day) Text() string {
	lines := append([]string{d.Header}, d.Extra...)
	for _, e := range d.Entries {
		lines = append(lines, e.String())
	}
	return strings.Join(lines, "\n")
}

type File struct {
	lines        []string
	finalNewline bool
	Days         []*Day
}

var (
	headerRE = regexp.MustCompile(`^\s*@([\pL\pM]+)\s+(\d{1,2})\.(\d{1,2})(?:\.(\d{2,4}))?\b`)
	tagRE    = regexp.MustCompile(`\s*#~([^~#\s]*)~#\s*$`)
	entryRE  = regexp.MustCompile(`^\s*(\*)?\s*\[(\d{1,2}):(\d{2})\s*-\s*(\d{1,2}):(\d{2})\]\s*-\s*(\S+)\s*-\s*(.*?)\s*$`)
)

// DefaultWeekdays name the days in new headers unless the settings say otherwise.
var DefaultWeekdays = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}

// WeekdayRE is what a weekday name may look like: letters of any script, nothing else.
var WeekdayRE = regexp.MustCompile(`^[\pL\pM]+$`)

var (
	weekdaysMu sync.RWMutex
	weekdays   = DefaultWeekdays
)

// SetWeekdays sets the names (Sunday first) used for new day headers, in any language.
// Parsing never depends on them: a header is read by its date.
func SetWeekdays(names []string) {
	if len(names) != 7 {
		names = DefaultWeekdays
	}
	weekdaysMu.Lock()
	weekdays = append([]string(nil), names...)
	weekdaysMu.Unlock()
}

// Weekdays returns the names in use, Sunday first.
func Weekdays() []string {
	weekdaysMu.RLock()
	defer weekdaysMu.RUnlock()
	return append([]string(nil), weekdays...)
}

func HM(m int) string { return fmt.Sprintf("%02d:%02d", m/60, m%60) }

// Header renders a day header: "@Tuesday 03.11" (the weekday name comes from SetWeekdays).
func Header(d time.Time) string {
	return fmt.Sprintf("@%s %02d.%02d", Weekdays()[d.Weekday()], d.Day(), int(d.Month()))
}

// ParseEntry parses one entry line; ok is false for any other line.
func ParseEntry(line string) (e Entry, ok bool) {
	if m := tagRE.FindStringSubmatchIndex(line); m != nil {
		e.WorklogID = line[m[2]:m[3]]
		line = line[:m[0]]
	}
	m := entryRE.FindStringSubmatch(line)
	if m == nil {
		return Entry{}, false
	}
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	e.Start, e.End = n(m[2])*60+n(m[3]), n(m[4])*60+n(m[5])
	if e.End <= e.Start {
		return Entry{}, false
	}
	e.Ticket = strings.ToUpper(m[6])
	if e.Ticket == "???" {
		e.Ticket = ""
	}
	e.Desc = m[7]
	if e.Desc == "Continue task" {
		e.Desc = ""
	}
	e.Logged = m[1] == "*" || e.WorklogID != ""
	return e, true
}

// Parse reads the file. Headers without a year get the latest year that keeps the
// file in descending order, starting from now (a month of slack for a day out of order).
func Parse(text string, now time.Time) *File {
	f := &File{}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if text != "" {
		f.finalNewline = strings.HasSuffix(text, "\n")
		f.lines = strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	}
	ceiling := now.AddDate(0, 1, 0)
	var cur *Day
	lastRaw := "" // the entry line above (with its continuations), "" when the line above was not an entry
	closeDay := func(i int) {
		if cur == nil {
			return
		}
		cur.end = i
		for j := i - 1; j > cur.start && strings.TrimSpace(f.lines[j]) == ""; j-- {
			cur.blanks++
		}
		f.Days = append(f.Days, cur)
		cur = nil
	}
	for i, line := range f.lines {
		if date, ok := parseHeader(line, ceiling, now.Location()); ok {
			closeDay(i)
			lastRaw = ""
			ceiling = date.AddDate(0, 1, 0)
			cur = &Day{Date: date, Header: strings.TrimSpace(line), start: i}
			continue
		}
		if cur == nil {
			continue
		}
		if e, ok := ParseEntry(line); ok {
			cur.Entries = append(cur.Entries, e)
			lastRaw = line
			continue
		}
		// a line wrapped while copying from a terminal: glue it to the entry right above
		if lastRaw != "" && isContinuation(lastRaw, line) {
			if e, ok := ParseEntry(lastRaw + " " + strings.TrimSpace(line)); ok {
				cur.Entries[len(cur.Entries)-1] = e
				lastRaw += " " + strings.TrimSpace(line)
				continue
			}
		}
		lastRaw = ""
		if strings.TrimSpace(line) != "" {
			cur.Extra = append(cur.Extra, line)
		}
	}
	closeDay(len(f.lines))
	return f
}

func (f *File) String() string {
	if len(f.lines) == 0 {
		return ""
	}
	s := strings.Join(f.lines, "\n")
	if f.finalNewline {
		s += "\n"
	}
	return s
}

// WrapMin is the shortest entry line treated as wrapped: a terminal cuts only long lines.
const WrapMin = 60

// isContinuation tells a wrapped tail of a long entry line from a comment, note, new entry
// or the /worklog summary that follows the blocks.
func isContinuation(prev, line string) bool {
	t := strings.TrimSpace(line)
	if len([]rune(prev)) < WrapMin || t == "" {
		return false
	}
	for _, p := range []string{"#", "@", "[", "*", "Razem ", "Pominięte", "Worklog za", "Inny dzień"} {
		if strings.HasPrefix(t, p) {
			return false
		}
	}
	return true
}

// parseHeader reads "@Wtorek 03.11[.2026]"; without a year it takes the latest one not after ceiling.
func parseHeader(line string, ceiling time.Time, loc *time.Location) (time.Time, bool) {
	m := headerRE.FindStringSubmatch(line)
	if m == nil {
		return time.Time{}, false
	}
	day, _ := strconv.Atoi(m[2])
	month, _ := strconv.Atoi(m[3])
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return time.Time{}, false
	}
	if m[4] != "" {
		y, _ := strconv.Atoi(m[4])
		if y < 100 {
			y += 2000
		}
		return time.Date(y, time.Month(month), day, 0, 0, 0, 0, loc), true
	}
	date := time.Date(ceiling.Year(), time.Month(month), day, 0, 0, 0, 0, loc)
	for date.After(ceiling) {
		date = date.AddDate(-1, 0, 0)
	}
	return date, true
}

func (f *File) Day(date time.Time) *Day {
	k := date.Format("2006-01-02")
	for _, d := range f.Days {
		if d.Key() == k {
			return d
		}
	}
	return nil
}

// SetDay replaces the day's entries (keeping its header and extra lines) or inserts a new
// day before the first older one. A day left with no entries and no extra lines is removed.
// The returned File is re-parsed; the receiver must not be used afterwards.
func (f *File) SetDay(date time.Time, entries []Entry, now time.Time) *File {
	sorted := append([]Entry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Start < sorted[j].Start })

	d := f.Day(date)
	header := Header(date)
	var extra []string
	if d != nil {
		header, extra = d.Header, d.Extra
	}
	section := append([]string{header}, extra...)
	for _, e := range sorted {
		section = append(section, e.String())
	}
	empty := len(sorted) == 0 && len(extra) == 0

	var lines []string
	switch {
	case d != nil:
		tail := f.lines[d.end:]
		if empty {
			section = nil
		} else {
			blanks := d.blanks
			if blanks == 0 && len(tail) > 0 {
				blanks = 1
			}
			for ; blanks > 0; blanks-- {
				section = append(section, "")
			}
		}
		lines = append(append(append([]string{}, f.lines[:d.start]...), section...), tail...)
	case empty:
		return f
	default:
		at := len(f.lines)
		for _, o := range f.Days {
			if o.Date.Before(date) {
				at = o.start
				break
			}
		}
		if at == len(f.lines) && at > 0 && strings.TrimSpace(f.lines[at-1]) != "" {
			section = append([]string{""}, section...)
		}
		if at < len(f.lines) {
			section = append(section, "")
		}
		lines = append(append(append([]string{}, f.lines[:at]...), section...), f.lines[at:]...)
	}
	nf := Parse(strings.Join(lines, "\n"), now)
	nf.finalNewline = true
	return nf
}

// Validate checks that entries are well formed and do not overlap.
func Validate(entries []Entry) error {
	s := append([]Entry(nil), entries...)
	sort.SliceStable(s, func(i, j int) bool { return s[i].Start < s[j].Start })
	for i, e := range s {
		if e.Start < 0 || e.End > 24*60 || e.End <= e.Start {
			return fmt.Errorf("entry %s-%s has invalid times", HM(e.Start), HM(e.End))
		}
		if strings.ContainsAny(e.Ticket, " \t[]") {
			return fmt.Errorf("invalid ticket %q", e.Ticket)
		}
		if i > 0 && e.Start < s[i-1].End {
			return fmt.Errorf("entries %s-%s and %s-%s overlap", HM(s[i-1].Start), HM(s[i-1].End), HM(e.Start), HM(e.End))
		}
	}
	return nil
}
