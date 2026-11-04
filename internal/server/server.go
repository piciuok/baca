// Package server serves the bar UI and its JSON API on localhost.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"baca/internal/activity"
	"baca/internal/config"
	"baca/internal/tempo"
	"baca/internal/update"
	"baca/internal/worklog"
)

type Server struct {
	Store    *worklog.Store
	Web      fs.FS           // must hold index.html
	Settings Settings        // nil: no settings screen
	Updates  *update.Manager // nil: no update checks
	Version  string
	Token    string // required on every request, see guard

	state atomic.Pointer[State]
	logMu sync.Mutex
}

// State is what the settings can change while the server runs; it is swapped as a whole.
type State struct {
	Tempo     *tempo.Client // nil when Jira/Tempo credentials are missing
	TempoErr  string        // why Tempo is not available
	Activity  activity.Log  // where the probes write
	Collector *activity.Collector
	Workday   config.Workday
}

// Settings reads and applies config.json for the settings screen.
type Settings interface {
	Get() config.Config
	Apply(config.Config) error
	Path() string
}

func (s *Server) SetState(st *State) { s.state.Store(st) }

func (s *Server) st() *State {
	if st := s.state.Load(); st != nil {
		return st
	}
	return &State{}
}

var ticketRE = regexp.MustCompile(`^[A-Z][A-Z0-9]*-\d+$`)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /api/config", s.config)
	mux.HandleFunc("GET /api/days", s.days)
	mux.HandleFunc("GET /api/days/{date}", s.getDay)
	mux.HandleFunc("PUT /api/days/{date}", s.putDay)
	mux.HandleFunc("POST /api/days/{date}/log", s.logDay)
	mux.HandleFunc("POST /api/days/{date}/logged/update", s.updateLogged)
	mux.HandleFunc("POST /api/days/{date}/logged/delete", s.unlogEntry)
	mux.HandleFunc("GET /api/tickets", s.tickets)
	mux.HandleFunc("GET /api/days/{date}/tempo", s.tempoPlan)
	mux.HandleFunc("POST /api/days/{date}/tempo", s.tempoApply)
	mux.HandleFunc("GET /api/activity/{date}", s.activity)
	mux.HandleFunc("GET /api/settings", s.getSettings)
	mux.HandleFunc("PUT /api/settings", s.putSettings)
	mux.HandleFunc("POST /api/update/check", s.checkUpdate)
	mux.HandleFunc("POST /api/update/install", s.installUpdate)
	return s.guard(mux)
}

// guard keeps everything else away from the UI and the API:
//   - the Host must be a loopback name (no DNS rebinding);
//   - the page needs ?t=<token> and every API call the X-Baca-Token header, so other local
//     users and processes cannot read the worklog or the activity (window titles); a custom
//     header also means no cross-site request can reach the API (no CORS preflight passes);
//   - the page is never framed and never leaks its URL (with the token) in a Referer.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "localhost" && host != "127.0.0.1" && host != "::1" {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		token := r.Header.Get("X-Baca-Token")
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			token = r.URL.Query().Get("t")
		}
		if s.Token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.Token)) != 1 {
			http.Error(w, "missing or invalid token — open baca from the menu bar icon or run: baca open", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type apiError struct {
	Error string           `json:"error"`
	Day   *worklog.DayView `json:"day,omitempty"`
}

func fail(w http.ResponseWriter, status int, err error, day *worklog.DayView) {
	writeJSON(w, status, apiError{Error: err.Error(), Day: day})
}

func parseDate(r *http.Request) (time.Time, error) {
	return time.ParseInLocation("2006-01-02", r.PathValue("date"), time.Local)
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFileFS(w, r, s.Web, "index.html")
}

func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	st := s.st()
	start, end, target := st.Workday.Minutes()
	cfg := map[string]any{"file": s.Store.FilePath(), "tempo": st.Tempo != nil, "tempoError": st.TempoErr,
		"workday": map[string]int{"start": start, "end": end, "target": target}, "settings": s.Settings != nil, "version": s.Version,
		"weekdays": worklog.Weekdays()}
	if s.Updates != nil {
		cfg["update"] = s.Updates.Info()
	}
	if st.Tempo != nil {
		cfg["jiraBase"] = st.Tempo.JiraBase()
	}
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) days(w http.ResponseWriter, r *http.Request) {
	days, err := s.Store.Days()
	if err != nil {
		fail(w, http.StatusInternalServerError, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, days)
}

func (s *Server) tickets(w http.ResponseWriter, r *http.Request) {
	t, err := s.Store.Tickets()
	if err != nil {
		fail(w, http.StatusInternalServerError, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) getDay(w http.ResponseWriter, r *http.Request) {
	date, err := parseDate(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err, nil)
		return
	}
	v, err := s.Store.GetDay(date)
	if err != nil {
		fail(w, http.StatusInternalServerError, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) putDay(w http.ResponseWriter, r *http.Request) {
	date, err := parseDate(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err, nil)
		return
	}
	var body struct {
		Base    string          `json:"base"`
		Entries []worklog.Entry `json:"entries"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, err, nil)
		return
	}
	v, err := s.Store.PutDay(date, body.Base, body.Entries)
	switch {
	case errors.Is(err, worklog.ErrConflict):
		fail(w, http.StatusConflict, err, &v)
	case err != nil && v.Date != "":
		fail(w, http.StatusUnprocessableEntity, err, &v)
	case err != nil:
		fail(w, http.StatusInternalServerError, err, nil)
	default:
		writeJSON(w, http.StatusOK, v)
	}
}

type LogResult struct {
	Entry     worklog.Entry `json:"entry"`
	Status    string        `json:"status"` // logged | existing | skipped | error
	WorklogID string        `json:"worklogId,omitempty"`
	Error     string        `json:"error,omitempty"`
}

// logDay sends the day's open entries to Tempo. hash must match the day the user confirmed.
// Each entry is marked in the file right after Tempo accepts it, so an interrupted run never
// logs the same entry twice; worklogs already in Tempo (same issue, start and length) are
// only marked.
func (s *Server) logDay(w http.ResponseWriter, r *http.Request) {
	date, err := parseDate(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err, nil)
		return
	}
	tc, tempoErr := s.st().Tempo, s.st().TempoErr
	if tc == nil {
		fail(w, http.StatusServiceUnavailable, errors.New("Tempo unavailable: "+tempoErr), nil)
		return
	}
	var body struct {
		Hash string `json:"hash"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, err, nil)
		return
	}
	if !s.logMu.TryLock() {
		fail(w, http.StatusConflict, errors.New("logging is already running"), nil)
		return
	}
	defer s.logMu.Unlock()

	v, err := s.Store.GetDay(date)
	if err != nil {
		fail(w, http.StatusInternalServerError, err, nil)
		return
	}
	if v.Hash != body.Hash {
		fail(w, http.StatusConflict, worklog.ErrConflict, &v)
		return
	}
	// finish the run even if the page goes away mid-way
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Minute)
	defer cancel()

	account, err := tc.AccountID(ctx)
	if err != nil {
		fail(w, http.StatusBadGateway, err, &v)
		return
	}
	existing, err := tc.Existing(ctx, account, date)
	if err != nil {
		fail(w, http.StatusBadGateway, err, &v)
		return
	}

	results := []LogResult{}
	for _, e := range v.Entries {
		if e.Logged {
			continue
		}
		res := LogResult{Entry: e}
		results = append(results, s.logEntry(ctx, tc, date, account, &existing, res))
	}
	day, _ := s.Store.GetDay(date)
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "day": day})
}

func (s *Server) logEntry(ctx context.Context, tc *tempo.Client, date time.Time, account string, existing *[]tempo.Worklog, res LogResult) LogResult {
	e := res.Entry
	if !ticketRE.MatchString(e.Ticket) {
		res.Status, res.Error = "skipped", "no valid ticket"
		return res
	}
	issueID, err := tc.IssueID(ctx, e.Ticket)
	if err != nil {
		res.Status, res.Error = "error", err.Error()
		return res
	}
	start := date.Add(time.Duration(e.Start) * time.Minute)
	secs := e.Minutes() * 60
	for i, x := range *existing {
		if x.IssueID == issueID && x.StartDate == start.Format("2006-01-02") && x.StartTime == start.Format("15:04:05") && x.Seconds == secs {
			res.Status, res.WorklogID = "existing", x.ID
			*existing = append((*existing)[:i], (*existing)[i+1:]...)
			break
		}
	}
	if res.WorklogID == "" {
		id, err := tc.Create(ctx, tempo.NewWorklog{AccountID: account, IssueID: issueID, Start: start, Seconds: secs, Description: e.Description()})
		if err != nil {
			res.Status, res.Error = "error", err.Error()
			return res
		}
		res.Status, res.WorklogID = "logged", id
	}
	if err := s.Store.MarkLogged(date, e, res.WorklogID); err != nil {
		res.Error = "in Tempo (#" + res.WorklogID + "), but not marked in the file: " + err.Error()
	}
	return res
}

type loggedReq struct {
	Hash   string `json:"hash"`
	Key    string `json:"key"`
	Ticket string `json:"ticket"`
	Desc   string `json:"desc"`
}

// withLogged runs fn for one logged entry of the day the user confirmed (hash), holding the
// Tempo lock. fn talks to Tempo first and changes the file only after Tempo accepted it.
func (s *Server) withLogged(w http.ResponseWriter, r *http.Request, fn func(ctx context.Context, tc *tempo.Client, date time.Time, req loggedReq, e worklog.Entry) (worklog.DayView, int, error)) {
	date, err := parseDate(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err, nil)
		return
	}
	tc, tempoErr := s.st().Tempo, s.st().TempoErr
	if tc == nil {
		fail(w, http.StatusServiceUnavailable, errors.New("Tempo unavailable: "+tempoErr), nil)
		return
	}
	var req loggedReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, err, nil)
		return
	}
	if !s.logMu.TryLock() {
		fail(w, http.StatusConflict, errors.New("another Tempo operation is running"), nil)
		return
	}
	defer s.logMu.Unlock()
	v, err := s.Store.GetDay(date)
	if err != nil {
		fail(w, http.StatusInternalServerError, err, nil)
		return
	}
	if v.Hash != req.Hash {
		fail(w, http.StatusConflict, worklog.ErrConflict, &v)
		return
	}
	var entry *worklog.Entry
	for i := range v.Entries {
		if v.Entries[i].Logged && v.Entries[i].Key() == req.Key {
			entry = &v.Entries[i]
		}
	}
	if entry == nil || entry.WorklogID == "" {
		fail(w, http.StatusNotFound, errors.New("no such logged entry with a Tempo id"), &v)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Minute)
	defer cancel()
	day, status, err := fn(ctx, tc, date, req, *entry)
	if err != nil {
		if day.Date == "" {
			day = v
		}
		fail(w, status, err, &day)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"day": day})
}

// updateLogged sends a new ticket/description of a logged entry to Tempo, then to the file.
func (s *Server) updateLogged(w http.ResponseWriter, r *http.Request) {
	s.withLogged(w, r, func(ctx context.Context, tc *tempo.Client, date time.Time, req loggedReq, e worklog.Entry) (worklog.DayView, int, error) {
		ticket := strings.ToUpper(strings.TrimSpace(req.Ticket))
		desc := strings.Join(strings.Fields(req.Desc), " ")
		if desc == "Continue task" {
			desc = ""
		}
		if !ticketRE.MatchString(ticket) {
			return worklog.DayView{}, http.StatusUnprocessableEntity, errors.New("invalid ticket: " + ticket)
		}
		if ticket == e.Ticket && desc == e.Desc {
			v, err := s.Store.GetDay(date)
			return v, http.StatusInternalServerError, err
		}
		account, err := tc.AccountID(ctx)
		if err != nil {
			return worklog.DayView{}, http.StatusBadGateway, err
		}
		issueID, err := tc.IssueID(ctx, ticket)
		if err != nil {
			return worklog.DayView{}, http.StatusBadGateway, err
		}
		ne := e
		ne.Ticket, ne.Desc = ticket, desc
		nw := tempo.NewWorklog{AccountID: account, IssueID: issueID, Start: date.Add(time.Duration(e.Start) * time.Minute), Seconds: e.Minutes() * 60, Description: ne.Description()}
		if err := tc.Update(ctx, e.WorklogID, nw, ticket != e.Ticket); err != nil {
			return worklog.DayView{}, http.StatusBadGateway, fmt.Errorf("Tempo rejected the change: %w", err)
		}
		// make sure Tempo really holds the change before the file says so
		got, err := tc.Get(ctx, e.WorklogID)
		if err != nil {
			return worklog.DayView{}, http.StatusBadGateway, fmt.Errorf("change sent, but it cannot be checked in Tempo: %w", err)
		}
		if got.IssueID != issueID || got.Description != nw.Description {
			return worklog.DayView{}, http.StatusBadGateway, errors.New("Tempo returned a different worklog than sent – check it in Tempo, the file is unchanged")
		}
		v, err := s.Store.UpdateLogged(date, e.Key(), ticket, desc)
		if err != nil {
			return v, http.StatusInternalServerError, fmt.Errorf("changed in Tempo, but not in the file: %w", err)
		}
		return v, 0, nil
	})
}

// unlogEntry deletes the worklog in Tempo and turns the entry back into an open block.
func (s *Server) unlogEntry(w http.ResponseWriter, r *http.Request) {
	s.withLogged(w, r, func(ctx context.Context, tc *tempo.Client, date time.Time, req loggedReq, e worklog.Entry) (worklog.DayView, int, error) {
		if err := tc.Delete(ctx, e.WorklogID); err != nil {
			return worklog.DayView{}, http.StatusBadGateway, fmt.Errorf("Tempo did not delete the worklog: %w", err)
		}
		v, err := s.Store.UnmarkLogged(date, e.Key())
		if err != nil {
			return v, http.StatusInternalServerError, fmt.Errorf("deleted in Tempo, but not in the file: %w", err)
		}
		return v, 0, nil
	})
}

// activity returns the day's activity per app and the suggested blocks. Tickets are only
// recognized with prefixes already used in the worklog, so "UTF-8" never becomes a ticket.
func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	date, err := parseDate(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err, nil)
		return
	}
	st := s.st()
	spans, err := st.Activity.Read(date.Format("2006-01-02"))
	if err != nil {
		fail(w, http.StatusInternalServerError, err, nil)
		return
	}
	known := map[string]bool{}
	if tickets, err := s.Store.Tickets(); err == nil {
		for _, t := range tickets {
			if i := strings.LastIndex(t, "-"); i > 0 {
				known[t[:i]] = true
			}
		}
	}
	probes := []activity.ProbeStatus{}
	if st.Collector != nil {
		probes = st.Collector.Status()
	}
	writeJSON(w, http.StatusOK, map[string]any{"probes": probes, "day": activity.Analyze(spans, known)})
}

// getSettings returns config.json with every secret masked.
func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	if s.Settings == nil {
		fail(w, http.StatusNotFound, errors.New("settings unavailable"), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": s.Settings.Path(), "config": s.Settings.Get().Masked(), "mask": config.Mask})
}

// putSettings saves the edited config (masked secrets keep their stored values) and applies it.
func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	if s.Settings == nil {
		fail(w, http.StatusNotFound, errors.New("settings unavailable"), nil)
		return
	}
	var sent config.Config
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sent); err != nil {
		fail(w, http.StatusBadRequest, err, nil)
		return
	}
	c := config.Merge(sent, s.Settings.Get())
	if err := c.Validate(); err != nil {
		fail(w, http.StatusUnprocessableEntity, err, nil)
		return
	}
	if err := s.Settings.Apply(c); err != nil {
		fail(w, http.StatusInternalServerError, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": s.Settings.Path(), "config": s.Settings.Get().Masked(), "mask": config.Mask})
}

func (s *Server) checkUpdate(w http.ResponseWriter, r *http.Request) {
	if s.Updates == nil {
		fail(w, http.StatusNotFound, errors.New("updates unavailable"), nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	in, err := s.Updates.Check(ctx, true)
	if err != nil {
		fail(w, http.StatusBadGateway, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, in)
}

// installUpdate downloads the release and restarts; the page reconnects to the new process.
func (s *Server) installUpdate(w http.ResponseWriter, r *http.Request) {
	if s.Updates == nil {
		fail(w, http.StatusNotFound, errors.New("updates unavailable"), nil)
		return
	}
	in := s.Updates.Info()
	if in.Dev || in.Latest == "" || in.Installing {
		fail(w, http.StatusConflict, errors.New("no update to install"), nil)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := s.Updates.Install(ctx); err != nil {
			log.Printf("update: %v", err)
		}
	}()
	in.Installing = true
	writeJSON(w, http.StatusAccepted, in)
}

// remoteDay reads the user's Tempo worklogs of a day as worklog entries.
func remoteDay(ctx context.Context, tc *tempo.Client, date time.Time) ([]worklog.Entry, error) {
	account, err := tc.AccountID(ctx)
	if err != nil {
		return nil, err
	}
	wls, err := tc.Existing(ctx, account, date)
	if err != nil {
		return nil, err
	}
	out := make([]worklog.Entry, 0, len(wls))
	for _, w := range wls {
		key, err := tc.IssueKey(ctx, w.IssueID)
		if err != nil {
			return nil, err
		}
		t, err := time.Parse("15:04:05", w.StartTime)
		if err != nil {
			t, _ = time.Parse("15:04", w.StartTime) // a worklog without a start time begins at 00:00
		}
		start := t.Hour()*60 + t.Minute()
		end := start + (w.Seconds+30)/60
		if end > 24*60 {
			end = 24 * 60
		}
		if end <= start {
			end = start + 1
		}
		desc := strings.Join(strings.Fields(w.Description), " ")
		if desc == "Continue task" {
			desc = ""
		}
		out = append(out, worklog.Entry{Start: start, End: end, Ticket: key, Desc: desc, Logged: true, WorklogID: w.ID})
	}
	return out, nil
}

// syncPlan compares the day in the file with Tempo; Tempo is the truth for logged worklogs.
func (s *Server) syncPlan(ctx context.Context, tc *tempo.Client, date time.Time) (worklog.DayView, []worklog.Change, error) {
	v, err := s.Store.GetDay(date)
	if err != nil {
		return v, nil, err
	}
	remote, err := remoteDay(ctx, tc, date)
	if err != nil {
		return v, nil, err
	}
	return v, worklog.PlanSync(v.Entries, remote), nil
}

// tempoPlan shows what "Sync from Tempo" would change in the day.
func (s *Server) tempoPlan(w http.ResponseWriter, r *http.Request) {
	date, err := parseDate(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err, nil)
		return
	}
	tc, tempoErr := s.st().Tempo, s.st().TempoErr
	if tc == nil {
		fail(w, http.StatusServiceUnavailable, errors.New("Tempo unavailable: "+tempoErr), nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	v, changes, err := s.syncPlan(ctx, tc, date)
	if err != nil {
		fail(w, http.StatusBadGateway, err, nil)
		return
	}
	if changes == nil {
		changes = []worklog.Change{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"hash": v.Hash, "changes": changes})
}

// tempoApply applies the accepted changes. The plan is made again from Tempo and the file, so the
// hash must still match the day the user looked at.
func (s *Server) tempoApply(w http.ResponseWriter, r *http.Request) {
	date, err := parseDate(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err, nil)
		return
	}
	tc, tempoErr := s.st().Tempo, s.st().TempoErr
	if tc == nil {
		fail(w, http.StatusServiceUnavailable, errors.New("Tempo unavailable: "+tempoErr), nil)
		return
	}
	var body struct {
		Hash   string   `json:"hash"`
		Accept []string `json:"accept"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, err, nil)
		return
	}
	if !s.logMu.TryLock() {
		fail(w, http.StatusConflict, errors.New("another Tempo operation is running"), nil)
		return
	}
	defer s.logMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	v, changes, err := s.syncPlan(ctx, tc, date)
	if err != nil {
		fail(w, http.StatusBadGateway, err, nil)
		return
	}
	if v.Hash != body.Hash {
		fail(w, http.StatusConflict, worklog.ErrConflict, &v)
		return
	}
	accept := map[string]bool{}
	for _, id := range body.Accept {
		accept[id] = true
	}
	entries, err := worklog.ApplySync(v.Entries, changes, accept)
	if err != nil {
		fail(w, http.StatusUnprocessableEntity, err, &v)
		return
	}
	day, err := s.Store.ReplaceDay(date, v.Hash, entries)
	switch {
	case errors.Is(err, worklog.ErrConflict):
		fail(w, http.StatusConflict, err, &day)
	case err != nil:
		fail(w, http.StatusInternalServerError, err, nil)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"day": day})
	}
}
