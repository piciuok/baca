package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"baca/internal/tempo"
	"baca/internal/worklog"
)

// fakeAtlassian is Jira + Tempo in one test server.
type fakeAtlassian struct {
	mu       sync.Mutex
	created  []map[string]any
	existing []map[string]any
	failKey  string
}

func (f *fakeAtlassian) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/rest/api/3/myself":
		io.WriteString(w, `{"accountId":"acc-1"}`)
	case strings.HasPrefix(r.URL.Path, "/rest/api/3/issue/"):
		key := strings.TrimPrefix(r.URL.Path, "/rest/api/3/issue/")
		if key == f.failKey {
			http.Error(w, `{"errorMessages":["Issue does not exist"]}`, 404)
			return
		}
		io.WriteString(w, `{"id":"`+map[string]string{"ABC-1": "101", "ABC-2": "102"}[key]+`","key":"`+key+`"}`)
	case r.Method == "GET" && r.URL.Path == "/4/worklogs/user/acc-1":
		json.NewEncoder(w).Encode(map[string]any{"results": f.existing})
	case r.Method == "POST" && r.URL.Path == "/4/worklogs":
		if r.Header.Get("Authorization") != "Bearer tempo-token" {
			http.Error(w, "auth", 401)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.created = append(f.created, body)
		json.NewEncoder(w).Encode(map[string]any{"tempoWorklogId": 9000 + len(f.created)})
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, 500)
	}
}

func setup(t *testing.T, content string) (*httptest.Server, *fakeAtlassian, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worklog.txt")
	if content != "" {
		os.WriteFile(path, []byte(content), 0o644)
	}
	fake := &fakeAtlassian{}
	atl := httptest.NewServer(fake)
	t.Cleanup(atl.Close)
	var cfg tempo.Config
	cfg.AccountEmail, cfg.Tokens.Jira, cfg.Tokens.Tempo = "me@example.com", "jira-token", "tempo-token"
	cfg.JiraBaseURL, cfg.TempoBaseURL = atl.URL, atl.URL
	s := &Server{Store: worklog.NewStore(path), Tempo: tempo.New(cfg), Web: fstest.MapFS{"index.html": {Data: []byte("<html>")}}}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, fake, path
}

func call(t *testing.T, srv *httptest.Server, method, path, body string, out any) int {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if method != "GET" {
		req.Header.Set("X-Baca", "1")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if out != nil {
		json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode
}

func TestLogDay(t *testing.T) {
	srv, fake, path := setup(t, "@Wtorek 03.11\n[07:00 - 07:30] - ABC-1 - CR\n[07:30 - 08:00] - ABC-2 - Work\n[08:00 - 08:10] - ??? - nothing\n[08:10 - 08:20] - ABC-404 - missing\n")
	// ABC-2 07:30 30m is already in Tempo: it must only be marked
	fake.existing = []map[string]any{{"tempoWorklogId": 777, "issue": map[string]any{"id": 102}, "startDate": "2026-11-03", "startTime": "07:30:00", "timeSpentSeconds": 1800}}
	fake.failKey = "ABC-404"

	var day worklog.DayView
	call(t, srv, "GET", "/api/days/2026-11-03", "", &day)
	if call(t, srv, "POST", "/api/days/2026-11-03/log", `{"hash":"stale"}`, nil) != http.StatusConflict {
		t.Fatal("stale hash accepted")
	}
	var out struct {
		Results []LogResult     `json:"results"`
		Day     worklog.DayView `json:"day"`
	}
	if code := call(t, srv, "POST", "/api/days/2026-11-03/log", `{"hash":"`+day.Hash+`"}`, &out); code != 200 {
		t.Fatalf("status %d", code)
	}
	got := []string{}
	for _, r := range out.Results {
		got = append(got, r.Entry.Ticket+":"+r.Status+":"+r.WorklogID)
	}
	if strings.Join(got, " ") != "ABC-1:logged:9001 ABC-2:existing:777 :skipped: ABC-404:error:" {
		t.Fatalf("results = %v", got)
	}
	if len(fake.created) != 1 || fake.created[0]["issueId"] != float64(101) || fake.created[0]["startTime"] != "07:00:00" ||
		fake.created[0]["timeSpentSeconds"] != float64(1800) || fake.created[0]["authorAccountId"] != "acc-1" {
		t.Fatalf("created = %+v", fake.created)
	}
	b, _ := os.ReadFile(path)
	want := "@Wtorek 03.11\n*[07:00 - 07:30] - ABC-1 - CR #~9001~#\n*[07:30 - 08:00] - ABC-2 - Work #~777~#\n[08:00 - 08:10] - ??? - nothing\n[08:10 - 08:20] - ABC-404 - missing\n"
	if string(b) != want {
		t.Fatalf("file:\n%s", b)
	}
	// a second run sends nothing already logged
	call(t, srv, "POST", "/api/days/2026-11-03/log", `{"hash":"`+out.Day.Hash+`"}`, &out)
	if len(fake.created) != 1 {
		t.Fatalf("logged twice: %d", len(fake.created))
	}
}

func TestPutDayAndGuard(t *testing.T) {
	srv, _, path := setup(t, "")
	body := `{"base":"","entries":[{"start":420,"end":480,"ticket":"ABC-1","desc":"a"}]}`
	var day worklog.DayView
	if code := call(t, srv, "PUT", "/api/days/2026-11-03", body, &day); code != 200 || !day.Exists || day.Hash == "" {
		t.Fatalf("put: %d %+v", code, day)
	}
	if code := call(t, srv, "PUT", "/api/days/2026-11-03", body, nil); code != http.StatusConflict {
		t.Fatalf("stale put: %d", code)
	}
	bad := `{"base":"` + day.Hash + `","entries":[{"start":420,"end":480,"ticket":"ABC-1"},{"start":470,"end":490,"ticket":"ABC-2"}]}`
	if code := call(t, srv, "PUT", "/api/days/2026-11-03", bad, nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("overlap: %d", code)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "@Wtorek 03.11\n[07:00 - 08:00] - ABC-1 - a\n" {
		t.Fatalf("file: %q", b)
	}

	// writes without the header and foreign hosts are refused
	req, _ := http.NewRequest("PUT", srv.URL+"/api/days/2026-11-03", strings.NewReader(body))
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != http.StatusForbidden {
		t.Fatalf("no header: %d", res.StatusCode)
	}
	req, _ = http.NewRequest("GET", srv.URL+"/api/days", nil)
	req.Host = "evil.example.com"
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign host: %d", res.StatusCode)
	}
	var days []worklog.DaySummary
	call(t, srv, "GET", "/api/days", "", &days)
	if len(days) != 1 || days[0].Status != "open" {
		t.Fatalf("days: %+v", days)
	}
}
