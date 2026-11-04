package worklog

import (
	"strings"
	"testing"
)

func lg(start, end int, ticket, desc, id string) Entry {
	return Entry{Start: start, End: end, Ticket: ticket, Desc: desc, Logged: true, WorklogID: id}
}

func TestSync(t *testing.T) {
	local := []Entry{
		lg(420, 480, "ABC-1", "a", "1"),         // 07:00-08:00, same in Tempo
		lg(480, 510, "ABC-2", "b", "2"),         // 08:00-08:30, changed in Tempo
		lg(510, 540, "ABC-3", "c", "3"),         // 08:30-09:00, gone from Tempo
		{Start: 540, End: 660, Ticket: "ABC-4"}, // 09:00-11:00 open, Tempo #4 cuts its middle
		{Start: 660, End: 680, Ticket: "ABC-5"}, // 11:00-11:20 open, untouched
		lg(700, 720, "ABC-6", "f", "6"),         // 11:40-12:00, Tempo #7 overlaps it
	}
	remote := []Entry{
		lg(420, 480, "ABC-1", "a", "1"),
		lg(480, 515, "ABC-2", "b fixed", "2"),
		lg(570, 600, "ABC-9", "", "4"),
		lg(700, 720, "ABC-6", "f", "6"),
		lg(710, 730, "ABC-8", "", "7"),
	}
	plan := PlanSync(local, remote)
	var got []string
	for _, c := range plan {
		got = append(got, c.ID+":"+c.Kind)
	}
	if strings.Join(got, " ") != "2:changed 3:deleted 4:new 7:conflict" {
		t.Fatalf("plan %v", got)
	}
	if len(plan[2].Trims) != 1 || plan[2].Trims[0].Ticket != "ABC-4" || plan[3].Reason != "overlaps logged ABC-6 11:40–12:00" {
		t.Fatalf("%+v", plan)
	}
	// #2 changed to 08:00-08:35 also cuts into #3, which is gone from Tempo
	out, err := ApplySync(local, plan, map[string]bool{"2": true, "3": true, "4": true, "6": true, "7": true})
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, e := range out {
		lines = append(lines, e.String())
	}
	want := strings.Join([]string{
		"*[07:00 - 08:00] - ABC-1 - a #~1~#",
		"*[08:00 - 08:35] - ABC-2 - b fixed #~2~#",
		"[08:35 - 09:00] - ABC-3 - c",
		"[09:00 - 09:30] - ABC-4 - Continue task",
		"*[09:30 - 10:00] - ABC-9 - Continue task #~4~#",
		"[10:00 - 11:00] - ABC-4 - Continue task",
		"[11:00 - 11:20] - ABC-5 - Continue task",
		"*[11:40 - 12:00] - ABC-6 - f #~6~#",
	}, "\n")
	if strings.Join(lines, "\n") != want {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(lines, "\n"), want)
	}
	// nothing accepted: the file stays as it was
	if out, _ := ApplySync(local, plan, nil); len(out) != len(local) {
		t.Fatalf("%v", out)
	}
}
