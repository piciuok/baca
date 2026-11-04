package worklog

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 3, 12, 0, 0, 0, time.Local)

func date(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.Local) }

const sample = `# notes at the top
@Wtorek 03.11
*[07:00 - 07:10] - ABC-752 - CR #~1001~#
[07:10 - 08:40] - ABC-868 - Write the setup guide, check screenshots
# comment in the day
[08:40 - 09:00] - ABC-868 - Continue task


@Piatek 30.10
[07:00 - 07:10] - abc-1 - CR
@Czwartek 01.01
[10:00 - 11:00] - ABC-2 - Daily
`

func TestParse(t *testing.T) {
	f := Parse(sample, now)
	if len(f.Days) != 3 {
		t.Fatalf("days = %d", len(f.Days))
	}
	d := f.Days[0]
	if d.Key() != "2026-11-03" || d.Header != "@Wtorek 03.11" {
		t.Fatalf("day0 = %s %q", d.Key(), d.Header)
	}
	if len(d.Entries) != 3 || len(d.Extra) != 1 {
		t.Fatalf("entries %d extra %d", len(d.Entries), len(d.Extra))
	}
	e := d.Entries[0]
	if !e.Logged || e.WorklogID != "1001" || e.Desc != "CR" || e.Start != 420 || e.End != 430 {
		t.Fatalf("logged entry = %+v", e)
	}
	if d.Entries[2].Desc != "" {
		t.Fatalf("Continue task should parse to empty desc, got %q", d.Entries[2].Desc)
	}
	if f.Days[1].Entries[0].Ticket != "ABC-1" {
		t.Fatalf("ticket not upper-cased")
	}
	// 01.01 below 30.10 in a newest-first file is January of the same year, not 2027
	if got := f.Days[2].Key(); got != "2026-01-01" {
		t.Fatalf("year inference = %s", got)
	}
	if d.Minutes() != 120 {
		t.Fatalf("minutes = %d", d.Minutes())
	}
}

func TestYearRollsBack(t *testing.T) {
	f := Parse("@Środa 04.11\n[07:00 - 08:00] - A-1 - x\n@Piątek 19.12\n[07:00 - 08:00] - A-1 - x\n", now)
	if f.Days[0].Key() != "2026-11-04" || f.Days[1].Key() != "2025-12-19" {
		t.Fatalf("got %s %s", f.Days[0].Key(), f.Days[1].Key())
	}
	f = Parse("@Piątek 19.12.2025\n", now)
	if f.Days[0].Key() != "2025-12-19" {
		t.Fatalf("explicit year: %s", f.Days[0].Key())
	}
}

func TestRoundTripUntouched(t *testing.T) {
	f := Parse(sample, now)
	if f.String() != sample {
		t.Fatalf("round trip changed the file:\n%s", f.String())
	}
}

func TestSetDayReplacesOnlyThatDay(t *testing.T) {
	f := Parse(sample, now)
	entries := append([]Entry(nil), f.Days[1].Entries...)
	entries[0].End = 450
	entries = append(entries, Entry{Start: 450, End: 480, Ticket: "ABC-9", Desc: "fix\nthings"})
	got := f.SetDay(date(2026, 10, 30), entries, now).String()
	want := strings.Replace(sample, "[07:00 - 07:10] - abc-1 - CR\n",
		"[07:00 - 07:30] - ABC-1 - CR\n[07:30 - 08:00] - ABC-9 - fix things\n\n", 1)
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestSetDayKeepsCommentsAndBlanks(t *testing.T) {
	f := Parse(sample, now)
	d := f.Days[0]
	nf := f.SetDay(d.Date, d.Entries, now)
	// comments and notes of the day move right under the header, blank lines stay
	want := strings.Replace(sample, "@Wtorek 03.11\n*[07:00 - 07:10] - ABC-752 - CR #~1001~#\n[07:10 - 08:40] - ABC-868 - Write the setup guide, check screenshots\n# comment in the day\n",
		"@Wtorek 03.11\n# comment in the day\n*[07:00 - 07:10] - ABC-752 - CR #~1001~#\n[07:10 - 08:40] - ABC-868 - Write the setup guide, check screenshots\n", 1)
	if nf.String() != want {
		t.Fatalf("got:\n%s", nf.String())
	}
}

func TestSetDayInsertsInOrder(t *testing.T) {
	f := Parse(sample, now)
	e := []Entry{{Start: 360, End: 420, Ticket: "ABC-1", Desc: "a"}}
	got := f.SetDay(date(2026, 11, 2), e, now).String()
	if !strings.Contains(got, "[08:40 - 09:00] - ABC-868 - Continue task\n\n\n@Poniedziałek 02.11\n[06:00 - 07:00] - ABC-1 - a\n\n@Piatek 30.10") {
		t.Fatalf("insert between days:\n%s", got)
	}
	got = f.SetDay(date(2026, 11, 4), e, now).String()
	if !strings.HasPrefix(got, "# notes at the top\n@Środa 04.11\n[06:00 - 07:00] - ABC-1 - a\n\n@Wtorek 03.11\n") {
		t.Fatalf("insert newest:\n%s", got)
	}
	got = Parse("", now).SetDay(date(2026, 11, 4), e, now).String()
	if got != "@Środa 04.11\n[06:00 - 07:00] - ABC-1 - a\n" {
		t.Fatalf("empty file: %q", got)
	}
}

func TestSetDayRemovesEmptyDay(t *testing.T) {
	f := Parse(sample, now)
	got := f.SetDay(date(2026, 10, 30), nil, now).String()
	if strings.Contains(got, "30.10") || !strings.Contains(got, "Continue task\n\n\n@Czwartek 01.01") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestEntryString(t *testing.T) {
	e := Entry{Start: 420, End: 430, Ticket: "ABC-1", Desc: "", Logged: true, WorklogID: "77"}
	if e.String() != "*[07:00 - 07:10] - ABC-1 - Continue task #~77~#" {
		t.Fatal(e.String())
	}
	back, ok := ParseEntry(e.String())
	if !ok || back != (Entry{Start: 420, End: 430, Ticket: "ABC-1", Logged: true, WorklogID: "77"}) {
		t.Fatalf("%+v", back)
	}
	if (Entry{Start: 0, End: 5}).String() != "[00:00 - 00:05] - ??? - Continue task" {
		t.Fatal("missing ticket")
	}
}

func TestWrappedLines(t *testing.T) {
	text := "@Wtorek 03.11\n[07:00 - 08:00] - ABC-17 - Review the import screen, fix the date parsing and the totals,\nopen PR, move to review\n# comment\n*[08:00 - 08:30] - ABC-2 - Daily, prepare the demo for the client and the team\nand demo #~42~#\n[08:30 - 08:45] - ABC-2 - Daily\nRazem 8h 20m: ABC-2\nloose note\n"
	f := Parse(text, now)
	d := f.Days[0]
	if len(d.Entries) != 3 || len(d.Extra) != 3 || d.Entries[2].Desc != "Daily" {
		t.Fatalf("entries %d extra %v", len(d.Entries), d.Extra)
	}
	if d.Entries[0].Desc != "Review the import screen, fix the date parsing and the totals, open PR, move to review" {
		t.Fatalf("desc = %q", d.Entries[0].Desc)
	}
	if d.Entries[1].Desc != "Daily, prepare the demo for the client and the team and demo" || d.Entries[1].WorklogID != "42" {
		t.Fatalf("logged = %+v", d.Entries[1])
	}
	if f.String() != text {
		t.Fatal("parse must not change the file")
	}
	if !strings.Contains(f.SetDay(d.Date, d.Entries, now).String(), "move to review\n*[08:00 - 08:30] - ABC-2 - Daily, prepare the demo for the client and the team and demo #~42~#\n") {
		t.Fatal("write should join wrapped lines")
	}
}

func TestValidate(t *testing.T) {
	if err := Validate([]Entry{{Start: 60, End: 120}, {Start: 100, End: 130}}); err == nil {
		t.Fatal("overlap not detected")
	}
	if err := Validate([]Entry{{Start: 60, End: 60}}); err == nil {
		t.Fatal("empty entry not detected")
	}
}

func TestStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "worklog.txt")
	s := NewStore(path)
	s.Now = func() time.Time { return now }
	day := date(2026, 11, 3)

	v, err := s.PutDay(day, "", []Entry{{Start: 420, End: 480, Ticket: "ABC-1", Desc: "a"}, {Start: 480, End: 500, Ticket: "ABC-2"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutDay(day, "stale", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	if err := s.MarkLogged(day, v.Entries[0], "555"); err != nil {
		t.Fatal(err)
	}
	v, _ = s.GetDay(day)
	if !v.Entries[0].Logged || v.Entries[0].WorklogID != "555" {
		t.Fatalf("not marked: %+v", v.Entries[0])
	}
	// dropping a logged entry is refused
	if _, err := s.PutDay(day, v.Hash, v.Entries[1:]); !errors.Is(err, ErrLocked) {
		t.Fatalf("want locked, got %v", err)
	}
	// changing a logged entry through the client is ignored - the file version wins
	changed := append([]Entry(nil), v.Entries...)
	changed[0].Desc = "hacked"
	changed[1].End = 510
	v, err = s.PutDay(day, v.Hash, changed)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	want := "@Wtorek 03.11\n*[07:00 - 08:00] - ABC-1 - a #~555~#\n[08:00 - 08:30] - ABC-2 - Continue task\n"
	if string(b) != want {
		t.Fatalf("file:\n%s", b)
	}
	days, _ := s.Days()
	if len(days) != 1 || days[0].Status != "partial" || days[0].Minutes != 90 {
		t.Fatalf("days = %+v", days)
	}
	if err := s.MarkLogged(day, v.Entries[0], "1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("re-marking a logged entry: %v", err)
	}
}

// BACA_REAL=<path> go test -run Real -v ./internal/worklog - checks a real file read-only.
func TestRealFile(t *testing.T) {
	path := os.Getenv("BACA_REAL")
	if path == "" {
		t.Skip("BACA_REAL not set")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f := Parse(string(b), time.Now())
	if f.String() != strings.ReplaceAll(string(b), "\r\n", "\n") {
		t.Fatal("round trip changed the file")
	}
	extra := 0
	for _, d := range f.Days {
		extra += len(d.Extra)
		t.Logf("%s %-16q entries=%d minutes=%d extra=%d", d.Key(), d.Header, len(d.Entries), d.Minutes(), len(d.Extra))
	}
	t.Logf("days=%d unparsed lines=%d", len(f.Days), extra)
}
