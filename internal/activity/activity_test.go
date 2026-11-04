package activity

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTickets(t *testing.T) {
	known := map[string]bool{"ABC": true, "XY": true}
	if got := Tickets("[ABC-17] Fix import – UTF-8 – XY-3 – QQ-1", false, known); !reflect.DeepEqual(got, []string{"ABC-17", "XY-3"}) {
		t.Fatal(got)
	}
	for branch, want := range map[string]string{"feature/ABC-717-login": "ABC-717", "abc717": "ABC-717", "fix/xy-03-x": "XY-3", "main": "", "release-2026": ""} {
		got := strings.Join(Tickets(branch, true, known), ",")
		if got != want {
			t.Fatalf("%s: %q", branch, got)
		}
	}
	if got := Tickets("SHA-256 and ABC-1", false, nil); !reflect.DeepEqual(got, []string{"ABC-1"}) {
		t.Fatal(got)
	}
}

func TestLogMergesPieces(t *testing.T) {
	l := Log{Dir: t.TempDir()}
	l.Append(
		Span{Date: "2026-11-04", From: "07:00:00", To: "07:01:00", Kind: "window", App: "Editor"},
		Span{Date: "2026-11-04", From: "07:01:00", To: "07:02:00", Kind: "window", App: "Editor"},
		Span{Date: "2026-11-04", From: "07:02:00", To: "07:03:00", Kind: "window", App: "Browser"},
		Span{Date: "2026-11-05", From: "07:00:00", To: "07:01:00", Kind: "window", App: "Editor"},
	)
	os.WriteFile(filepath.Join(l.Dir, "x"), nil, 0o600)
	f, _ := os.OpenFile(filepath.Join(l.Dir, "2026-11-04.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"from":"07:03`) // torn line
	f.Close()
	got, err := l.Read("2026-11-04")
	if err != nil || len(got) != 2 || got[0].To != "07:02:00" || got[1].App != "Browser" {
		t.Fatalf("%v %+v", err, got)
	}
	if st, _ := os.Stat(filepath.Join(l.Dir, "2026-11-04.jsonl")); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
}

func TestAnalyze(t *testing.T) {
	known := map[string]bool{"ABC": true}
	spans := []Span{
		{From: "07:00:00", To: "07:20:00", Kind: "window", App: "Browser", Title: "[ABC-1] Login – Jira"},
		{From: "07:20:00", To: "07:40:00", Kind: "window", App: "Editor", Title: "app – main.go"},
		{From: "07:20:00", To: "07:40:00", Kind: "repo", Repo: "app", Branch: "feature/ABC-2-export"},
		{From: "07:40:00", To: "07:50:00", Kind: "window", App: "Editor", Title: "app – x.go"}, // no evidence: continues ABC-2
		{From: "07:50:00", To: "08:30:00", Kind: "idle", App: "Editor"},
		{From: "08:30:00", To: "08:31:00", Kind: "window", App: "Chat"}, // too short for a slot
	}
	d := Analyze(spans, known)
	var got []string
	for _, s := range d.Suggestions {
		got = append(got, s.Ticket+"@"+hm(s.Start)+"-"+hm(s.End))
	}
	if strings.Join(got, " ") != "ABC-1@07:00-07:20 ABC-2@07:20-07:50" {
		t.Fatalf("suggestions %v", got)
	}
	if d.Suggestions[1].Sources[0] != "changes in app (feature/ABC-2-export)" || d.Suggestions[1].Apps[0].Name != "Editor" {
		t.Fatalf("%+v", d.Suggestions[1])
	}
	if len(d.Apps) != 4 || !d.Apps[2].Idle {
		t.Fatalf("apps %+v", d.Apps)
	}
}

func hm(m int) string { return time.Date(0, 1, 1, m/60, m%60, 0, 0, time.UTC).Format("15:04") }

// fake clock + sampler: window, no input for 5 minutes, then back
func TestSystemProbe(t *testing.T) {
	p := NewSystemProbe(SystemConfig{Interval: 5, IdleAfter: 180}, GitConfig{})
	c := NewCollector(Log{Dir: t.TempDir()}, p)
	now := time.Date(2026, 11, 4, 9, 0, 0, 0, time.Local)
	c.Now = func() time.Time { return now }
	var idle time.Duration
	app, title := "Editor", "a.go"
	p.sample = func(ctx context.Context, wantTitle bool) (sample, error) {
		return sample{App: app, Title: title, Idle: idle}, nil
	}
	step := func(n int) {
		for range n {
			c.tick(context.Background(), p)
			now = now.Add(5 * time.Second)
			if idle > 0 {
				idle += 5 * time.Second
			}
		}
	}
	step(24) // 09:00-09:02 working
	idle = 5 * time.Second
	step(60) // 09:02-09:07 no input; idle from 09:05 (180s), reaching back to 09:02
	idle = 0
	title = "b.go"
	step(12) // 09:07-09:08 b.go
	c.write(p, p.Flush(now))
	spans, _ := c.Log.Read("2026-11-04")
	var got []string
	for _, s := range spans {
		got = append(got, s.Probe+":"+s.Kind+":"+s.Title+"@"+s.From+"-"+s.To)
	}
	want := "system:window:a.go@09:00:00-09:01:55 system:idle:@09:01:55-09:07:00 system:window:b.go@09:07:00-09:08:00"
	if strings.Join(got, " ") != want {
		t.Fatalf("got  %v\nwant %s", got, want)
	}
	if st := c.Status(); len(st) != 1 || st[0].Info != osName+" · window titles: yes" {
		t.Fatalf("status %+v", st)
	}
}

func TestGitProbe(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "app")
	os.MkdirAll(filepath.Join(repo, ".git", "logs"), 0o755)
	os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/feature/ABC-7-x\n"), 0o644)
	os.MkdirAll(filepath.Join(root, "notrepo"), 0o755)
	// a worktree: .git is a file pointing at the git dir
	wt := filepath.Join(root, "app-abc8")
	os.MkdirAll(filepath.Join(repo, ".git", "worktrees", "w"), 0o755)
	os.WriteFile(filepath.Join(repo, ".git", "worktrees", "w", "HEAD"), []byte("ref: refs/heads/abc8\n"), 0o644)
	os.MkdirAll(wt, 0o755)
	os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.Join(repo, ".git", "worktrees", "w")+"\n"), 0o644)

	p := NewGitProbe(GitConfig{Repos: []string{filepath.Join(root, "*")}})
	now := time.Date(2026, 11, 4, 9, 0, 0, 0, time.Local)
	if out, err := p.Tick(context.Background(), now); err != nil || len(out) != 0 || p.Info() != "2 repo" {
		t.Fatalf("first scan: %v %v %s", out, err, p.Info())
	}
	later := time.Now().Add(time.Second)
	os.WriteFile(filepath.Join(repo, ".git", "index"), []byte("x"), 0o644)
	os.Chtimes(filepath.Join(repo, ".git", "index"), later, later)
	out, _ := p.Tick(context.Background(), now.Add(time.Minute))
	if len(out) != 1 || out[0].Repo != "app" || out[0].Branch != "feature/ABC-7-x" || out[0].From != "09:00:00" || out[0].To != "09:01:00" {
		t.Fatalf("%+v", out)
	}
	if _, b := GitInfo(wt); b != "abc8" {
		t.Fatalf("worktree branch %q", b)
	}
}

func TestCodexProbe(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repos", "app")
	os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/ABC-9-fix\n"), 0o644)
	p := NewCodexProbe(GitConfig{Repos: []string{filepath.Join(root, "repos", "*")}})
	p.root = filepath.Join(root, "sessions")
	now := time.Date(2026, 11, 4, 9, 0, 0, 0, time.Local)
	dir := filepath.Join(p.root, "2026", "11", "04")
	os.MkdirAll(dir, 0o755)
	if _, err := p.Tick(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	meta := `{"type":"session_meta","payload":{"cwd":"` + filepath.Join(repo, "sub") + `","base_instructions":{"text":"` + strings.Repeat("x", 100000) + `"}}}` + "\n"
	os.WriteFile(filepath.Join(dir, "rollout-1.jsonl"), []byte(meta), 0o644)
	later := time.Now().Add(time.Second)
	os.Chtimes(filepath.Join(dir, "rollout-1.jsonl"), later, later)
	p.last = later.Add(-time.Minute) // the file is newer than the previous scan
	out, _ := p.Tick(context.Background(), now.Add(time.Minute))
	if len(out) != 1 || out[0].Kind != "agent" || out[0].App != "Codex" || out[0].Repo != "app" || out[0].Branch != "ABC-9-fix" {
		t.Fatalf("%+v", out)
	}
	if out2, _ := p.Tick(context.Background(), now.Add(2*time.Minute)); len(out2) != 0 {
		t.Fatalf("unchanged session reported again: %+v", out2)
	}
}

func TestContainsWord(t *testing.T) {
	for _, c := range []struct {
		s, w string
		want bool
	}{{"app – src/a.go", "app", true}, {"a.go — app", "app", true}, {"app-admin – x", "app", false}, {"my-app – x", "app", false}, {"app-admin – x", "app-admin", true}} {
		if containsWord(c.s, c.w) != c.want {
			t.Fatalf("%q in %q", c.w, c.s)
		}
	}
}

func TestSystemProbeIDEAndMeeting(t *testing.T) {
	root := t.TempDir()
	for _, r := range []string{"app", "app-admin"} {
		os.MkdirAll(filepath.Join(root, r, ".git"), 0o755)
		os.WriteFile(filepath.Join(root, r, ".git", "HEAD"), []byte("ref: refs/heads/feature/ABC-"+map[string]string{"app": "1", "app-admin": "2"}[r]+"\n"), 0o644)
	}
	p := NewSystemProbe(SystemConfig{Interval: 5, IdleAfter: 180, Meetings: map[string]string{"Slack": `(?i)huddle`}}, GitConfig{Repos: []string{filepath.Join(root, "*")}})
	c := NewCollector(Log{Dir: t.TempDir()}, p)
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.Local)
	c.Now = func() time.Time { return now }
	title, idle, huddle := "app-admin – src/Export.php", time.Duration(0), false
	p.sample = func(context.Context, bool) (sample, error) {
		return sample{App: "PhpStorm", Title: title, Idle: idle}, nil
	}
	p.windows = func(context.Context, []string) ([][2]string, error) {
		if huddle {
			return [][2]string{{"Slack", "Huddle: #team"}}, nil
		}
		return [][2]string{{"Slack", "general (Channel) - Acme - Slack"}}, nil
	}
	step := func(n int) {
		for range n {
			c.tick(context.Background(), p)
			now = now.Add(5 * time.Second)
			if idle > 0 {
				idle += 5 * time.Second
			}
		}
	}
	step(120) // 10:00-10:10 in the IDE on app-admin
	huddle, idle = true, 5*time.Second
	step(240) // 10:10-10:30 on a call, no input
	huddle, idle = false, 0
	step(60)
	c.write(p, p.Flush(now))
	spans, _ := c.Log.Read("2026-11-04")
	d := Analyze(spans, map[string]bool{"ABC": true})
	var got []string
	for _, s := range d.Suggestions {
		got = append(got, s.Ticket+"|"+s.Meeting+"@"+hm(s.Start)+"-"+hm(s.End))
	}
	want := "ABC-2|@10:00-10:10 |Slack · Huddle: #team@10:10-10:30 ABC-2|@10:30-10:35"
	if strings.Join(got, " ") != want {
		t.Fatalf("got  %v\nwant %s", got, want)
	}
	if !strings.HasPrefix(d.Suggestions[0].Sources[0], "PhpStorm in app-admin (feature/ABC-2)") {
		t.Fatalf("sources %v", d.Suggestions[0].Sources)
	}
}

func TestGitLabProbe(t *testing.T) {
	events := `[
	 {"project_id":1,"action_name":"pushed to","created_at":"2026-11-04T08:30:00Z","push_data":{"ref":"feature/ABC-5-x","commit_title":"Fix totals"}},
	 {"project_id":1,"action_name":"commented on","target_type":"DiffNote","target_title":"ABC-6 Export","created_at":"2026-11-04T09:00:00Z","note":{"noteable_type":"MergeRequest"}},
	 {"project_id":2,"action_name":"approved","target_type":"MergeRequest","target_iid":7,"target_title":"ABC-7 Login","created_at":"2026-11-03T15:00:00Z"},
	 {"project_id":2,"action_name":"approved","target_type":"MergeRequest","target_iid":1,"target_title":"old","created_at":"2026-11-01T15:00:00Z"}
	]`
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("PRIVATE-TOKEN"))
		switch r.URL.Path {
		case "/api/v4/events":
			if r.URL.Query().Get("after") != "2026-11-02" || r.URL.Query().Get("before") != "2026-11-05" {
				t.Errorf("query %s", r.URL.RawQuery)
			}
			io.WriteString(w, events)
		case "/api/v4/projects/1":
			io.WriteString(w, `{"path":"app"}`)
		case "/api/v4/projects/2":
			io.WriteString(w, `{"path":"web"}`)
		default:
			http.Error(w, "nf", 404)
		}
	}))
	defer srv.Close()
	p := NewGitLabProbe(GitLabConfig{Instances: []GitLabInstance{{BaseURL: srv.URL, Token: "tok"}}})
	c := NewCollector(Log{Dir: t.TempDir()}, p)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC).In(time.Local)
	c.Now = func() time.Time { return now }
	c.tick(context.Background(), p)
	c.tick(context.Background(), p) // a second poll replaces, never duplicates
	today, _ := c.Log.Read(dayOf(now))
	yesterday, _ := c.Log.Read(dayOf(now.AddDate(0, 0, -1)))
	if len(today) != 2 || len(yesterday) != 1 || auth[0] != "tok" {
		t.Fatalf("today %+v\nyesterday %+v", today, yesterday)
	}
	push := today[0]
	if push.Kind != "remote" || push.App != "GitLab" || push.Repo != "app" || push.Branch != "feature/ABC-5-x" ||
		push.To != clock(time.Date(2026, 11, 4, 8, 30, 0, 0, time.UTC).In(time.Local)) || push.Probe != "gitlab" {
		t.Fatalf("push %+v", push)
	}
	if today[1].Title != "commented on MR ABC-6 Export" || yesterday[0].Title != "approved MR !7 ABC-7 Login" {
		t.Fatalf("titles %q %q", today[1].Title, yesterday[0].Title)
	}
	d := Analyze(today, map[string]bool{"ABC": true})
	if len(d.Suggestions) != 2 || d.Suggestions[0].Ticket != "ABC-5" || d.Suggestions[1].Ticket != "ABC-6" {
		t.Fatalf("suggestions %+v", d.Suggestions)
	}
}

func TestJiraProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/rest/api/3/myself":
			io.WriteString(w, `{"accountId":"me"}`)
		case r.URL.Path == "/rest/api/3/search/jql":
			b, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(b), `updatedBy(\"me\", \"2026/11/03\", \"2026/11/05\")`) {
				t.Errorf("jql %s", b)
			}
			io.WriteString(w, `{"issues":[{"key":"ABC-3","fields":{"summary":"Export"}}]}`)
		case r.URL.Path == "/rest/api/3/issue/ABC-3/changelog":
			io.WriteString(w, `{"values":[
			 {"author":{"accountId":"me"},"created":"2026-11-04T09:00:00.000+0000","items":[{"field":"status","fromString":"To Do","toString":"In Progress"}]},
			 {"author":{"accountId":"me"},"created":"2026-11-04T10:00:00.000+0000","items":[{"field":"timespent"},{"field":"WorklogId"}]},
			 {"author":{"accountId":"other"},"created":"2026-11-04T11:00:00.000+0000","items":[{"field":"status"}]}]}`)
		case r.URL.Path == "/rest/api/3/issue/ABC-3/comment":
			io.WriteString(w, `{"comments":[{"author":{"accountId":"me"},"created":"2026-11-04T12:00:00.000+0000"},{"author":{"accountId":"other"},"created":"2026-11-04T12:30:00.000+0000"}]}`)
		default:
			http.Error(w, "nf", 404)
		}
	}))
	defer srv.Close()
	p := NewJiraProbe(JiraAuth{BaseURL: srv.URL, Email: "a@example.com", Token: "t"})
	now := time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC).In(time.Local)
	out, err := p.Tick(context.Background(), now)
	if err != nil || len(out) != 2 {
		t.Fatalf("%v %+v", err, out)
	}
	if out[0].Title != "ABC-3 Export: status To Do → In Progress" || out[1].Title != "ABC-3 Export: comment" || out[0].App != "Jira" {
		t.Fatalf("%+v", out)
	}
	if out[1].To != clock(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC).In(time.Local)) {
		t.Fatalf("comment time %s", out[1].To)
	}
}

func TestGitHubProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/dev/events" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "nf", 404)
			return
		}
		io.WriteString(w, `[
		 {"type":"PullRequestReviewEvent","repo":{"name":"acme/web"},"created_at":"2026-11-04T10:00:00Z","payload":{"action":"created","review":{"state":"APPROVED"},"pull_request":{"number":5,"title":"ABC-5 Login","head":{"ref":"feature/ABC-5"}}}},
		 {"type":"PushEvent","repo":{"name":"acme/app"},"created_at":"2026-11-04T09:00:00Z","payload":{"ref":"refs/heads/abc6-fix","commits":[{"message":"first"},{"message":"Fix totals\n\nbody"}]}},
		 {"type":"WatchEvent","repo":{"name":"acme/app"},"created_at":"2026-11-04T08:00:00Z","payload":{}},
		 {"type":"PushEvent","repo":{"name":"acme/app"},"created_at":"2026-11-01T09:00:00Z","payload":{"ref":"refs/heads/old"}}
		]`)
	}))
	defer srv.Close()
	p := NewGitHubProbe(GitHubConfig{User: "dev", Token: "tok", BaseURL: srv.URL})
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC).In(time.Local)
	out, err := p.Tick(context.Background(), now)
	if err != nil || len(out) != 2 {
		t.Fatalf("%v %+v", err, out)
	}
	if out[0].Title != "approved PR #5 ABC-5 Login" || out[0].Repo != "web" || out[0].Branch != "feature/ABC-5" ||
		out[1].Title != "pushed to abc6-fix: Fix totals" || out[1].Branch != "abc6-fix" {
		t.Fatalf("%+v", out)
	}
	d := Analyze(out, map[string]bool{"ABC": true})
	if len(d.Suggestions) != 2 || d.Suggestions[0].Ticket != "ABC-6" || d.Suggestions[1].Ticket != "ABC-5" {
		t.Fatalf("%+v", d.Suggestions)
	}
}
