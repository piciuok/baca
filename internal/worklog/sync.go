package worklog

import (
	"fmt"
	"sort"
)

// Change is one difference between Tempo and the worklog file for a day. Tempo is the source of
// truth for logged worklogs (that is what got booked); open blocks are the file's own.
type Change struct {
	ID     string  `json:"id"`              // tempoWorklogId
	Kind   string  `json:"kind"`            // new | changed | deleted | conflict
	Tempo  *Entry  `json:"tempo,omitempty"` // the worklog as Tempo has it (new, changed, conflict)
	Local  *Entry  `json:"local,omitempty"` // the entry in the file (changed, deleted)
	Trims  []Entry `json:"trims,omitempty"` // open blocks it cuts into or removes
	Reason string  `json:"reason,omitempty"`
}

func overlaps(a, b Entry) bool { return a.Start < b.End && b.Start < a.End }

func same(a, b Entry) bool {
	return a.Start == b.Start && a.End == b.End && a.Ticket == b.Ticket && a.Description() == b.Description()
}

func span(e Entry) string { return fmt.Sprintf("%s %s–%s", orDash(e.Ticket), HM(e.Start), HM(e.End)) }

func orDash(s string) string {
	if s == "" {
		return "???"
	}
	return s
}

// PlanSync compares the day's entries with Tempo's worklogs (Logged, with WorklogID):
//   - new: in Tempo, not in the file (logged in Tempo directly);
//   - changed: the same worklog id with another time, ticket or description in Tempo;
//   - deleted: logged in the file, gone from Tempo (it would become an open block again);
//   - conflict: a Tempo worklog that cannot go into the file because it overlaps another logged
//     entry or another Tempo worklog; it has to be fixed in Tempo.
//
// Open blocks that a new or changed worklog overlaps are listed in Trims: Tempo wins there.
func PlanSync(local, remote []Entry) []Change {
	byID := map[string]Entry{}
	for _, e := range local {
		if e.Logged && e.WorklogID != "" {
			byID[e.WorklogID] = e
		}
	}
	inTempo := map[string]bool{}
	var out []Change
	for i, r := range remote {
		inTempo[r.WorklogID] = true
		r := r
		c := Change{ID: r.WorklogID, Tempo: &r}
		if l, ok := byID[r.WorklogID]; ok {
			if same(l, r) {
				continue
			}
			l := l
			c.Kind, c.Local = "changed", &l
		} else {
			c.Kind = "new"
		}
		for j, o := range remote {
			if j != i && overlaps(r, o) {
				c.Kind, c.Reason = "conflict", "overlaps Tempo worklog #"+o.WorklogID+" ("+span(o)+")"
			}
		}
		for _, l := range local {
			switch {
			case !overlaps(r, l):
			case l.Logged && l.WorklogID != r.WorklogID && remoteHas(remote, l.WorklogID):
				// the other logged entry stays (Tempo has it too): both cannot be in the file
				c.Kind, c.Reason = "conflict", "overlaps logged "+span(l)
			case !l.Logged, l.WorklogID != r.WorklogID: // open, or logged but gone from Tempo
				c.Trims = append(c.Trims, l)
			}
		}
		if c.Kind == "conflict" {
			c.Trims = nil
		}
		out = append(out, c)
	}
	for _, l := range local {
		if l.Logged && l.WorklogID != "" && !inTempo[l.WorklogID] {
			l := l
			out = append(out, Change{ID: l.WorklogID, Kind: "deleted", Local: &l})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return start(out[i]) < start(out[j]) })
	return out
}

func remoteHas(remote []Entry, id string) bool {
	for _, r := range remote {
		if r.WorklogID == id {
			return true
		}
	}
	return false
}

func start(c Change) int {
	if c.Tempo != nil {
		return c.Tempo.Start
	}
	return c.Local.Start
}

// ApplySync applies the accepted changes (by worklog id) to the day's entries: deleted worklogs
// become open blocks, changed ones take Tempo's values, new ones are added, and open blocks are
// cut around accepted Tempo worklogs (pieces shorter than 5 minutes go). Conflicts never apply.
func ApplySync(local []Entry, changes []Change, accept map[string]bool) ([]Entry, error) {
	entries := append([]Entry(nil), local...)
	var booked []Entry
	gone := map[string]bool{} // logged in the file, gone from Tempo: open blocks for cutting
	for _, c := range changes {
		if c.Kind == "deleted" {
			gone[c.ID] = true
		}
	}
	for _, c := range changes {
		if !accept[c.ID] || c.Kind == "conflict" {
			continue
		}
		switch c.Kind {
		case "deleted":
			for i := range entries {
				if entries[i].Logged && entries[i].WorklogID == c.ID {
					entries[i].Logged, entries[i].WorklogID = false, ""
				}
			}
		case "changed":
			for i := range entries {
				if entries[i].Logged && entries[i].WorklogID == c.ID {
					entries[i] = *c.Tempo
				}
			}
			booked = append(booked, *c.Tempo)
		case "new":
			entries = append(entries, *c.Tempo)
			booked = append(booked, *c.Tempo)
		}
	}
	for _, b := range booked {
		var next []Entry
		for _, e := range entries {
			if (e.Logged && !gone[e.WorklogID]) || !overlaps(e, b) {
				next = append(next, e)
				continue
			}
			e.Logged, e.WorklogID = false, ""
			if b.Start-e.Start >= 5 {
				left := e
				left.End = b.Start
				next = append(next, left)
			}
			if e.End-b.End >= 5 {
				right := e
				right.Start = b.End
				next = append(next, right)
			}
		}
		entries = next
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Start < entries[j].Start })
	if err := Validate(entries); err != nil {
		return nil, err
	}
	return entries, nil
}
