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

	"baca/internal/config"
	"baca/internal/tempo"
	"baca/internal/worklog"
)

// fakeAtlassian is Jira + Tempo in one test server.
type fakeAtlassian struct {
	mu       sync.Mutex
	created  []map[string]any
	existing []map[string]any
	failKey  string
	updates  []map[string]any
	deleted  []string
	stored   map[string]map[string]any // tempoWorklogId -> last PUT body
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
		if k, ok := map[string]string{"101": "ABC-1", "102": "ABC-2"}[key]; ok { // by numeric id
			io.WriteString(w, `{"id":"`+key+`","key":"`+k+`"}`)
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
	case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/4/worklogs/"):
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.updates = append(f.updates, body)
		if f.stored == nil {
			f.stored = map[string]map[string]any{}
		}
		f.stored[strings.TrimPrefix(r.URL.Path, "/4/worklogs/")] = body
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/4/worklogs/"):
		b := f.stored[strings.TrimPrefix(r.URL.Path, "/4/worklogs/")]
		issue := b["issueId"]
		if issue == nil {
			issue = 101
		}
		json.NewEncoder(w).Encode(map[string]any{"issue": map[string]any{"id": issue}, "description": b["description"]})
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/4/worklogs/"):
		f.deleted = append(f.deleted, strings.TrimPrefix(r.URL.Path, "/4/worklogs/"))
		w.WriteHeader(http.StatusNoContent)
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
	s := &Server{Store: worklog.NewStore(path), Web: fstest.MapFS{"index.html": {Data: []byte("<html>")}}, Token: "tok"}
	s.SetState(&State{Tempo: tempo.New(cfg)})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, fake, path
}

func call(t *testing.T, srv *httptest.Server, method, path, body string, out any) int {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("X-Baca-Token", "tok")
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
	if string(b) != "@Tuesday 03.11\n[07:00 - 08:00] - ABC-1 - a\n" {
		t.Fatalf("file: %q", b)
	}

	// no token (reads too), a wrong token, a token in the API's query and foreign hosts are refused
	for _, c := range []struct{ method, path, token string }{
		{"PUT", "/api/days/2026-11-03", ""}, {"GET", "/api/days", ""}, {"GET", "/api/days", "nope"},
		{"GET", "/api/days?t=tok", ""}, {"GET", "/", ""}, {"GET", "/?t=nope", ""},
	} {
		req, _ := http.NewRequest(c.method, srv.URL+c.path, strings.NewReader(body))
		if c.token != "" {
			req.Header.Set("X-Baca-Token", c.token)
		}
		if res, _ := http.DefaultClient.Do(req); res.StatusCode != http.StatusForbidden {
			t.Fatalf("%s %s %q: %d", c.method, c.path, c.token, res.StatusCode)
		}
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/days", nil)
	req.Header.Set("X-Baca-Token", "tok")
	req.Host = "evil.example.com"
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign host: %d", res.StatusCode)
	}
	res, _ := http.Get(srv.URL + "/?t=tok")
	if res.StatusCode != 200 || res.Header.Get("X-Frame-Options") != "DENY" || res.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("page: %d %v", res.StatusCode, res.Header)
	}
	var days []worklog.DaySummary
	call(t, srv, "GET", "/api/days", "", &days)
	if len(days) != 1 || days[0].Status != "open" {
		t.Fatalf("days: %+v", days)
	}
}

func TestUpdateAndUnlog(t *testing.T) {
	srv, fake, path := setup(t, "@Wtorek 03.11\n*[07:00 - 07:30] - ABC-1 - Review the import screen, fix the date parsing #~55~#\n[07:30 - 08:00] - ABC-2 - Work\n")
	var day worklog.DayView
	call(t, srv, "GET", "/api/days/2026-11-03", "", &day)
	key := day.Entries[0].Key()

	if code := call(t, srv, "POST", "/api/days/2026-11-03/logged/update", `{"hash":"stale","key":"`+key+`","ticket":"ABC-1","desc":"x"}`, nil); code != http.StatusConflict {
		t.Fatalf("stale hash: %d", code)
	}
	if code := call(t, srv, "POST", "/api/days/2026-11-03/logged/update", `{"hash":"`+day.Hash+`","key":"`+day.Entries[1].Key()+`","ticket":"ABC-2","desc":"x"}`, nil); code != http.StatusNotFound {
		t.Fatalf("open entry accepted: %d", code)
	}
	var out struct {
		Day worklog.DayView `json:"day"`
	}
	body := `{"hash":"` + day.Hash + `","key":"` + key + `","ticket":"abc-1","desc":"Review the import screen,\n fix the date parsing, open PR"}`
	if code := call(t, srv, "POST", "/api/days/2026-11-03/logged/update", body, &out); code != 200 {
		t.Fatalf("update: %d", code)
	}
	if len(fake.updates) != 1 || fake.updates[0]["description"] != "Review the import screen, fix the date parsing, open PR" ||
		fake.updates[0]["issueId"] != nil || fake.updates[0]["startTime"] != "07:00:00" || fake.updates[0]["timeSpentSeconds"] != float64(1800) {
		t.Fatalf("PUT body = %+v", fake.updates)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "*[07:00 - 07:30] - ABC-1 - Review the import screen, fix the date parsing, open PR #~55~#\n") {
		t.Fatalf("file:\n%s", b)
	}

	// moving to another ticket sends issueId
	body = `{"hash":"` + out.Day.Hash + `","key":"` + key + `","ticket":"ABC-2","desc":"Work"}`
	if code := call(t, srv, "POST", "/api/days/2026-11-03/logged/update", body, &out); code != 200 || fake.updates[1]["issueId"] != float64(102) {
		t.Fatalf("move: %d %+v", code, fake.updates)
	}

	// delete in Tempo turns the entry back into an open block
	body = `{"hash":"` + out.Day.Hash + `","key":"` + out.Day.Entries[0].Key() + `"}`
	if code := call(t, srv, "POST", "/api/days/2026-11-03/logged/delete", body, &out); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	b, _ = os.ReadFile(path)
	if strings.Join(fake.deleted, ",") != "55" || string(b) != "@Wtorek 03.11\n[07:00 - 07:30] - ABC-2 - Work\n[07:30 - 08:00] - ABC-2 - Work\n" {
		t.Fatalf("deleted %v file:\n%s", fake.deleted, b)
	}
}

type fakeSettings struct {
	cfg     config.Config
	applied int
}

func (f *fakeSettings) Get() config.Config { return f.cfg }
func (f *fakeSettings) Path() string       { return "/x/config.json" }
func (f *fakeSettings) Apply(c config.Config) error {
	f.cfg = c
	f.applied++
	return nil
}

func TestSettings(t *testing.T) {
	fs := &fakeSettings{}
	fs.cfg.Tokens.Jira = "jira-secret"
	fs.cfg.Activity.Slack.Token = "xoxp-secret"
	s := &Server{Store: worklog.NewStore(filepath.Join(t.TempDir(), "w.txt")), Web: fstest.MapFS{"index.html": {Data: []byte("x")}}, Settings: fs, Token: "tok"}
	srv2 := httptest.NewServer(s.Handler())
	defer srv2.Close()

	req, _ := http.NewRequest("GET", srv2.URL+"/api/settings", nil)
	req.Header.Set("X-Baca-Token", "tok")
	res, _ := http.DefaultClient.Do(req)
	b, _ := io.ReadAll(res.Body)
	if strings.Contains(string(b), "secret") || !strings.Contains(string(b), config.Mask) {
		t.Fatalf("leak or no mask: %s", b)
	}
	var got struct {
		Config config.Config `json:"config"`
	}
	json.Unmarshal(b, &got)
	got.Config.Workday.Start, got.Config.Activity.Slack.Token = "07:00", ""
	body, _ := json.Marshal(got.Config)
	if code := call(t, srv2, "PUT", "/api/settings", string(body), nil); code != 200 {
		t.Fatalf("put %d", code)
	}
	if fs.cfg.Tokens.Jira != "jira-secret" || fs.cfg.Activity.Slack.Token != "" || fs.cfg.Workday.Start != "07:00" || fs.applied != 1 {
		t.Fatalf("%+v", fs.cfg)
	}
	got.Config.Workday.Start = "7"
	body, _ = json.Marshal(got.Config)
	if code := call(t, srv2, "PUT", "/api/settings", string(body), nil); code != http.StatusUnprocessableEntity || fs.applied != 1 {
		t.Fatalf("bad hours accepted: %d", code)
	}
	if code := call(t, srv2, "PUT", "/api/settings", `{"nope":1}`, nil); code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", code)
	}
}

func TestTempoSync(t *testing.T) {
	srv, fake, path := setup(t, "@Tuesday 03.11\n*[07:00 - 07:30] - ABC-1 - Review #~9001~#\n[07:30 - 09:00] - ABC-2 - Work\n*[09:00 - 09:15] - ABC-1 - Gone #~9002~#\n")
	fake.existing = []map[string]any{
		{"tempoWorklogId": 9001, "issue": map[string]any{"id": 101}, "startDate": "2026-11-03", "startTime": "07:00:00", "timeSpentSeconds": 2700, "description": "Review, more"},
		{"tempoWorklogId": 777, "issue": map[string]any{"id": 102}, "startDate": "2026-11-03", "startTime": "08:00:00", "timeSpentSeconds": 1800, "description": "Continue task"},
	}
	var plan struct {
		Hash    string           `json:"hash"`
		Changes []worklog.Change `json:"changes"`
	}
	if code := call(t, srv, "GET", "/api/days/2026-11-03/tempo", "", &plan); code != 200 {
		t.Fatalf("plan %d", code)
	}
	var got []string
	for _, c := range plan.Changes {
		got = append(got, c.ID+":"+c.Kind)
	}
	if strings.Join(got, " ") != "9001:changed 777:new 9002:deleted" {
		t.Fatalf("plan %v", got)
	}
	if code := call(t, srv, "POST", "/api/days/2026-11-03/tempo", `{"hash":"stale","accept":["9001"]}`, nil); code != http.StatusConflict {
		t.Fatalf("stale hash: %d", code)
	}
	var out struct {
		Day worklog.DayView `json:"day"`
	}
	if code := call(t, srv, "POST", "/api/days/2026-11-03/tempo", `{"hash":"`+plan.Hash+`","accept":["9001","777","9002"]}`, &out); code != 200 {
		t.Fatalf("apply %d", code)
	}
	b, _ := os.ReadFile(path)
	want := "@Tuesday 03.11\n*[07:00 - 07:45] - ABC-1 - Review, more #~9001~#\n[07:45 - 08:00] - ABC-2 - Work\n*[08:00 - 08:30] - ABC-2 - Continue task #~777~#\n[08:30 - 09:00] - ABC-2 - Work\n[09:00 - 09:15] - ABC-1 - Gone\n"
	if string(b) != want {
		t.Fatalf("file:\n%s\nwant:\n%s", b, want)
	}
	if len(fake.created) != 0 || len(fake.updates) != 0 {
		t.Fatal("sync must not write to Tempo")
	}
}
