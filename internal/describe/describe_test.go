package describe

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	blocks := []Block{{Key: "420-480-ABC-1"}, {Key: "480-500-ABC-2"}}
	out := "Here you go:\n[{\"key\":\"420-480-ABC-1\",\"desc\":\" Add export,\\n fix totals \"},{\"key\":\"nope\",\"desc\":\"x\"},{\"key\":\"480-500-ABC-2\",\"desc\":\"\"}]\nthanks"
	ps, err := Parse(out, blocks)
	if err != nil || len(ps) != 1 || ps[0].Desc != "Add export, fix totals" {
		t.Fatalf("%v %+v", err, ps)
	}
	if _, err := Parse("I cannot do that", blocks); err == nil {
		t.Fatal("no JSON accepted")
	}
	p := Render(Request{Day: "Tuesday 03.11.2026", Earlier: []string{"@Monday 02.11\n[07:00 - 08:00] - ABC-1 - Add login"}, Blocks: blocks})
	for _, want := range []string{DefaultStyle[:40], "Tuesday 03.11.2026", "Add login", "420-480-ABC-1", "Reply with only a JSON array"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q", want)
		}
	}
}

func TestPromptText(t *testing.T) {
	for in, want := range map[string]string{
		"  fix the  export\nplease ": "fix the export please",
		"<command-name>/review-queue</command-name><command-args>12</command-args>": "/review-queue 12",
		"<command-name>/clear</command-name>": "",
		"<system-reminder>x</system-reminder>": "",
		"Caveat: the messages below":            "",
	} {
		if got := promptText(in); got != want {
			t.Fatalf("%q: %q", in, got)
		}
	}
}

func TestCommits(t *testing.T) {
	repo := t.TempDir()
	run := func(env []string, args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	run(nil, "init", "-q", "-b", "feature/ABC-7")
	run(nil, "config", "user.name", "Dev")
	run(nil, "config", "user.email", "dev@example.com")
	day := time.Date(2026, 11, 3, 0, 0, 0, 0, time.Local)
	at := func(h int) []string {
		d := day.Add(time.Duration(h) * time.Hour).Format(time.RFC3339)
		return []string{"GIT_AUTHOR_DATE=" + d, "GIT_COMMITTER_DATE=" + d}
	}
	run(at(-20), "commit", "-q", "--allow-empty", "-m", "Yesterday")
	run(at(9), "commit", "-q", "--allow-empty", "-m", "Add export")
	run(append(at(10), "GIT_AUTHOR_NAME=Other", "GIT_AUTHOR_EMAIL=o@example.com"), "commit", "-q", "--allow-empty", "-m", "Not mine")
	cs := Commits(context.Background(), []string{repo}, day)
	if len(cs) != 1 || cs[0].Subject != "Add export" || cs[0].Branch != "feature/ABC-7" || cs[0].At.Hour() != 9 {
		t.Fatalf("%+v", cs)
	}
}

func TestPrompts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	day := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	cl := filepath.Join(home, ".claude", "projects", "-repo")
	os.MkdirAll(cl, 0o755)
	os.WriteFile(filepath.Join(cl, "s.jsonl"), []byte(strings.Join([]string{
		`{"type":"user","timestamp":"2026-11-03T09:00:00Z","cwd":"/w/app","gitBranch":"ABC-1","message":{"content":"add the export"}}`,
		`{"type":"user","timestamp":"2026-11-03T09:01:00Z","cwd":"/w/app","message":{"content":[{"type":"tool_result","content":"x"}]}}`,
		`{"type":"user","isMeta":true,"timestamp":"2026-11-03T09:02:00Z","cwd":"/w/app","message":{"content":"meta"}}`,
		`{"type":"user","timestamp":"2026-11-02T09:00:00Z","cwd":"/w/app","message":{"content":"yesterday"}}`,
		`{"type":"user","timestamp":"2026-11-03T09:03:00Z","cwd":"/tmp/x","message":{"content":"tooling"}}`,
	}, "\n")), 0o644)
	cx := filepath.Join(home, ".codex", "sessions", "2026", "11", "03")
	os.MkdirAll(cx, 0o755)
	os.WriteFile(filepath.Join(cx, "r.jsonl"), []byte(strings.Join([]string{
		`{"timestamp":"2026-11-03T10:00:00Z","type":"session_meta","payload":{"cwd":"/w/web"}}`,
		`{"timestamp":"2026-11-03T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>x</environment_context>"}]}}`,
		`{"timestamp":"2026-11-03T10:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"review the login MR"}]}}`,
	}, "\n")), 0o644)
	now := time.Now()
	os.Chtimes(filepath.Join(cl, "s.jsonl"), now, now)
	os.Chtimes(filepath.Join(cx, "r.jsonl"), now, now)
	ps := Prompts(day)
	var got []string
	for _, p := range ps {
		got = append(got, p.Agent+"|"+p.Cwd+"|"+p.Text)
	}
	if strings.Join(got, " ; ") != "Claude Code|/w/app|add the export ; Codex|/w/web|review the login MR" {
		t.Fatalf("%v", got)
	}
}
