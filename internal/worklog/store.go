package worklog

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

var (
	ErrConflict = errors.New("the day changed in the file since it was read")
	ErrLocked   = errors.New("logged entries cannot be changed or removed")
	ErrNotFound = errors.New("entry not found in the file")
)

// Store is the worklog file on disk. Every call re-reads the file, so edits made by hand
// in an editor are picked up, and writes go through a temp file + rename.
type Store struct {
	Path string
	Now  func() time.Time
	mu   sync.Mutex
}

func NewStore(path string) *Store { return &Store{Path: path, Now: time.Now} }

// SetPath switches to another worklog file (settings changed).
func (s *Store) SetPath(p string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Path = p
}

func (s *Store) FilePath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Path
}

type DayView struct {
	Date    string   `json:"date"`
	Header  string   `json:"header"`
	Exists  bool     `json:"exists"`
	Hash    string   `json:"hash"`
	Extra   []string `json:"extra"`
	Entries []Entry  `json:"entries"`
}

type DaySummary struct {
	Date    string `json:"date"`
	Minutes int    `json:"minutes"`
	Entries int    `json:"entries"`
	Logged  int    `json:"logged"`
	Status  string `json:"status"` // logged | partial | open
}

func Hash(d *Day) string {
	if d == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(d.Text()))
	return hex.EncodeToString(sum[:8])
}

func view(date time.Time, d *Day) DayView {
	v := DayView{Date: date.Format("2006-01-02"), Header: Header(date), Entries: []Entry{}, Extra: []string{}}
	if d != nil {
		v.Header, v.Exists, v.Hash = d.Header, true, Hash(d)
		v.Entries = append(v.Entries, d.Entries...)
		v.Extra = append(v.Extra, d.Extra...)
	}
	return v
}

func (s *Store) read() (*File, error) {
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return Parse("", s.Now()), nil
	}
	if err != nil {
		return nil, err
	}
	return Parse(string(b), s.Now()), nil
}

func (s *Store) write(f *File) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(s.Path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), "."+filepath.Base(s.Path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(f.String()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.Path)
}

func (s *Store) GetDay(date time.Time) (DayView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.read()
	if err != nil {
		return DayView{}, err
	}
	return view(date, f.Day(date)), nil
}

// PutDay replaces the open (not logged) entries of a day. base is the hash the client
// loaded; a different current hash means the file changed meanwhile (ErrConflict).
// Logged entries always stay as they are in the file and must all be in entries.
func (s *Store) PutDay(date time.Time, base string, entries []Entry) (DayView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.read()
	if err != nil {
		return DayView{}, err
	}
	d := f.Day(date)
	if Hash(d) != base {
		return view(date, d), ErrConflict
	}
	sent := map[string]bool{}
	var next []Entry
	for _, e := range entries {
		if e.Logged {
			sent[e.Key()] = true
			continue
		}
		e.WorklogID = ""
		next = append(next, e)
	}
	logged := map[string]bool{}
	if d != nil {
		for _, e := range d.Entries {
			if e.Logged {
				if !sent[e.Key()] {
					return view(date, d), ErrLocked
				}
				logged[e.Key()] = true
				next = append(next, e)
			}
		}
	}
	for k := range sent {
		if !logged[k] {
			return view(date, d), ErrLocked
		}
	}
	if err := Validate(next); err != nil {
		return view(date, d), err
	}
	nf := f.SetDay(date, next, s.Now())
	if err := s.write(nf); err != nil {
		return DayView{}, err
	}
	return view(date, nf.Day(date)), nil
}

// MarkLogged marks the open entry with e's times and ticket as logged under worklogID.
func (s *Store) MarkLogged(date time.Time, e Entry, worklogID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.read()
	if err != nil {
		return err
	}
	d := f.Day(date)
	if d == nil {
		return ErrNotFound
	}
	entries := append([]Entry(nil), d.Entries...)
	found := false
	for i := range entries {
		if !entries[i].Logged && entries[i].Key() == e.Key() {
			entries[i].Logged, entries[i].WorklogID, found = true, worklogID, true
			break
		}
	}
	if !found {
		return fmt.Errorf("%w: %s %s-%s", ErrNotFound, e.Ticket, HM(e.Start), HM(e.End))
	}
	return s.write(f.SetDay(date, entries, s.Now()))
}

// Days lists the days in the file, newest first.
func (s *Store) Days() ([]DaySummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.read()
	if err != nil {
		return nil, err
	}
	out := []DaySummary{}
	for _, d := range f.Days {
		sum := DaySummary{Date: d.Key(), Minutes: d.Minutes(), Entries: len(d.Entries)}
		for _, e := range d.Entries {
			if e.Logged {
				sum.Logged++
			}
		}
		switch {
		case sum.Entries > 0 && sum.Logged == sum.Entries:
			sum.Status = "logged"
		case sum.Logged > 0:
			sum.Status = "partial"
		default:
			sum.Status = "open"
		}
		out = append(out, sum)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Date > out[j].Date })
	return out, nil
}

// Tickets lists every ticket in the file, most used first.
func (s *Store) Tickets() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.read()
	if err != nil {
		return nil, err
	}
	count := map[string]int{}
	for _, d := range f.Days {
		for _, e := range d.Entries {
			if e.Ticket != "" {
				count[e.Ticket]++
			}
		}
	}
	out := make([]string, 0, len(count))
	for t := range count {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if count[out[i]] != count[out[j]] {
			return count[out[i]] > count[out[j]]
		}
		return out[i] < out[j]
	})
	return out, nil
}

// changeLogged applies fn to the logged entry with the given Key and writes the day back.
func (s *Store) changeLogged(date time.Time, key string, fn func(*Entry)) (DayView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.read()
	if err != nil {
		return DayView{}, err
	}
	d := f.Day(date)
	if d == nil {
		return view(date, d), ErrNotFound
	}
	entries := append([]Entry(nil), d.Entries...)
	found := false
	for i := range entries {
		if entries[i].Logged && entries[i].Key() == key {
			fn(&entries[i])
			found = true
			break
		}
	}
	if !found {
		return view(date, d), fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err := Validate(entries); err != nil {
		return view(date, d), err
	}
	nf := f.SetDay(date, entries, s.Now())
	if err := s.write(nf); err != nil {
		return DayView{}, err
	}
	return view(date, nf.Day(date)), nil
}

// UpdateLogged changes the ticket and description of a logged entry (after Tempo took the change).
func (s *Store) UpdateLogged(date time.Time, key, ticket, desc string) (DayView, error) {
	return s.changeLogged(date, key, func(e *Entry) { e.Ticket, e.Desc = ticket, desc })
}

// UnmarkLogged turns a logged entry back into an open one (after its worklog was deleted in Tempo).
func (s *Store) UnmarkLogged(date time.Time, key string) (DayView, error) {
	return s.changeLogged(date, key, func(e *Entry) { e.Logged, e.WorklogID = false, "" })
}

// ReplaceDay writes the day's entries as given, logged ones included (Tempo sync). base is the
// hash the plan was made from.
func (s *Store) ReplaceDay(date time.Time, base string, entries []Entry) (DayView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.read()
	if err != nil {
		return DayView{}, err
	}
	d := f.Day(date)
	if Hash(d) != base {
		return view(date, d), ErrConflict
	}
	if err := Validate(entries); err != nil {
		return view(date, d), err
	}
	nf := f.SetDay(date, entries, s.Now())
	if err := s.write(nf); err != nil {
		return DayView{}, err
	}
	return view(date, nf.Day(date)), nil
}

// Earlier returns up to n days before date as text (newest first): style examples for describing.
func (s *Store) Earlier(date time.Time, n int) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.read()
	if err != nil {
		return nil, err
	}
	days := append([]*Day(nil), f.Days...)
	sort.SliceStable(days, func(i, j int) bool { return days[i].Date.After(days[j].Date) })
	var out []string
	for _, d := range days {
		if d.Date.Before(date) && len(d.Entries) > 0 {
			out = append(out, d.Text())
			if len(out) == n {
				break
			}
		}
	}
	return out, nil
}
