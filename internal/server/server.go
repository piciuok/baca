// Package server serves the bar UI and its JSON API on localhost.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"regexp"
	"sync"
	"time"

	"baca/internal/tempo"
	"baca/internal/worklog"
)

type Server struct {
	Store    *worklog.Store
	Tempo    *tempo.Client // nil when the Tempo config is missing
	TempoErr string        // why Tempo is not available
	Web      fs.FS         // must hold index.html

	logMu sync.Mutex
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
	mux.HandleFunc("GET /api/tickets", s.tickets)
	return guard(mux)
}

// guard keeps other web pages away from the API: the Host must be a loopback name (no DNS
// rebinding) and every write needs a custom header, which a cross-site request cannot send
// without a CORS preflight that this server never allows.
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "localhost" && host != "127.0.0.1" && host != "::1" {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-Baca") != "1" {
			http.Error(w, "missing X-Baca header", http.StatusForbidden)
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
	cfg := map[string]any{"file": s.Store.Path, "tempo": s.Tempo != nil, "tempoError": s.TempoErr}
	if s.Tempo != nil {
		cfg["jiraBase"] = s.Tempo.JiraBase()
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
	if s.Tempo == nil {
		fail(w, http.StatusServiceUnavailable, errors.New("Tempo unavailable: "+s.TempoErr), nil)
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

	account, err := s.Tempo.AccountID(ctx)
	if err != nil {
		fail(w, http.StatusBadGateway, err, &v)
		return
	}
	existing, err := s.Tempo.Existing(ctx, account, date)
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
		results = append(results, s.logEntry(ctx, date, account, &existing, res))
	}
	day, _ := s.Store.GetDay(date)
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "day": day})
}

func (s *Server) logEntry(ctx context.Context, date time.Time, account string, existing *[]tempo.Worklog, res LogResult) LogResult {
	e := res.Entry
	if !ticketRE.MatchString(e.Ticket) {
		res.Status, res.Error = "skipped", "no valid ticket"
		return res
	}
	issueID, err := s.Tempo.IssueID(ctx, e.Ticket)
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
		id, err := s.Tempo.Create(ctx, tempo.NewWorklog{AccountID: account, IssueID: issueID, Start: start, Seconds: secs, Description: e.Description()})
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
