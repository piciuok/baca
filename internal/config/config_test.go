package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"baca/internal/activity"
)

func TestMaskAndMerge(t *testing.T) {
	var c Config
	c.Tokens.Jira, c.Tokens.Tempo = "jira-secret", ""
	c.Activity.GitLab.Instances = []activity.GitLabInstance{{BaseURL: "https://gitlab.example.com", Token: "gl-secret"}}
	c.Activity.GitHub.Accounts = []activity.GitHubAccount{{User: "dev", Token: "gh-secret"}, {User: "other"}}
	c.Activity.Slack.Token = "xoxp-secret"
	c.Activity.Calendar.URLs = []string{"https://cal.example.com/a.ics", "https://cal.example.com/b.ics"}

	m := c.Masked()
	b := strings.Join([]string{m.Tokens.Jira, m.Activity.GitLab.Instances[0].Token, m.Activity.GitHub.Accounts[0].Token, m.Activity.Slack.Token, strings.Join(m.Activity.Calendar.URLs, ",")}, "|")
	if strings.Contains(b, "secret") || strings.Contains(b, "cal.example") || m.Tokens.Tempo != "" || m.Activity.GitHub.Accounts[1].Token != "" {
		t.Fatalf("leak: %s", b)
	}
	if c.Tokens.Jira != "jira-secret" || c.Activity.GitHub.Accounts[0].Token != "gh-secret" {
		t.Fatal("Masked changed the original")
	}

	// the UI drops the first calendar, reorders GitHub accounts, sets a new Tempo token
	m.Tokens.Tempo = " new-tempo "
	m.Activity.GitHub.Accounts = []activity.GitHubAccount{m.Activity.GitHub.Accounts[1], m.Activity.GitHub.Accounts[0]}
	m.Activity.Calendar.URLs = []string{m.Activity.Calendar.URLs[1], "https://cal.example.com/c.ics"}
	got := Merge(m, c)
	if got.Tokens.Jira != "jira-secret" || got.Tokens.Tempo != "new-tempo" || got.Activity.GitLab.Instances[0].Token != "gl-secret" ||
		got.Activity.GitHub.Accounts[1].Token != "gh-secret" || got.Activity.GitHub.Accounts[0].Token != "" || got.Activity.Slack.Token != "xoxp-secret" ||
		strings.Join(got.Activity.Calendar.URLs, ",") != "https://cal.example.com/b.ics,https://cal.example.com/c.ics" {
		t.Fatalf("%+v", got)
	}
}

func TestSaveLoadAndWorkday(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	var c Config
	c.Workday = Workday{Start: "07:30", End: "17:00", TargetHours: 7.5}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	back, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s, e, tg := back.Workday.Minutes(); s != 450 || e != 1020 || tg != 450 {
		t.Fatal(s, e, tg)
	}
	if s, e, tg := (Workday{}).Minutes(); s != 360 || e != 1080 || tg != 480 {
		t.Fatal("defaults", s, e, tg)
	}
	for _, w := range []Workday{{Start: "7:30"}, {Start: "17:00", End: "17:30"}, {TargetHours: 25}} {
		if (Config{Workday: w}).Validate() == nil {
			t.Fatalf("accepted %+v", w)
		}
	}
}
