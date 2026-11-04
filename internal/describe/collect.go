package describe

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Commit is one of the user's commits.
type Commit struct {
	At      time.Time
	Repo    string
	Branch  string
	Subject string
}

func git(ctx context.Context, repo string, args ...string) string {
	out, _ := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...).Output()
	return string(out)
}

// Commits lists the user's commits of the day in the repos, by author date (a rebase keeps it),
// each once even when it sits on several branches or clones.
func Commits(ctx context.Context, repos []string, day time.Time) []Commit {
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, day.Location())
	seen := map[string]bool{}
	var out []Commit
	for _, repo := range repos {
		name := strings.TrimSpace(git(ctx, repo, "config", "user.name"))
		email := strings.TrimSpace(git(ctx, repo, "config", "user.email"))
		if name == "" && email == "" {
			continue
		}
		args := []string{"log", "--all", "--source", "--no-merges", "--since=" + start.Format("2006-01-02 15:04:05"), "--format=%aI%x09%S%x09%s"}
		for _, a := range []string{name, email} {
			if a != "" {
				args = append(args, "--author="+regexp.QuoteMeta(a))
			}
		}
		for _, line := range strings.Split(git(ctx, repo, args...), "\n") {
			f := strings.SplitN(line, "\t", 3)
			if len(f) < 3 || seen[f[2]] {
				continue
			}
			at, err := time.Parse(time.RFC3339, f[0])
			if err != nil {
				continue
			}
			at = at.In(day.Location())
			if at.Before(start) || !at.Before(start.AddDate(0, 0, 1)) {
				continue
			}
			seen[f[2]] = true
			branch := regexp.MustCompile(`^refs/(heads|remotes/[^/]+)/`).ReplaceAllString(f[1], "")
			out = append(out, Commit{At: at, Repo: filepath.Base(repo), Branch: branch, Subject: f[2]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// Prompt is what the user asked a coding agent.
type Prompt struct {
	At     time.Time
	Agent  string
	Cwd    string
	Branch string
	Text   string
}

var (
	commandName = regexp.MustCompile(`<command-name>\s*(/?[\w:-]+)\s*</command-name>`)
	commandArgs = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
	bareCommand = regexp.MustCompile(`^/[\w:-]+$`)
)

// promptText keeps the words the user typed: no tool results, meta entries or bare commands.
func promptText(text string) string {
	text = strings.TrimSpace(text)
	if m := commandName.FindStringSubmatch(text); m != nil {
		args := ""
		if a := commandArgs.FindStringSubmatch(text); a != nil {
			args = strings.TrimSpace(a[1])
		}
		text = strings.TrimSpace(m[1] + " " + args)
	}
	if text == "" || strings.HasPrefix(text, "<") || strings.HasPrefix(text, "Caveat:") || bareCommand.MatchString(text) {
		return ""
	}
	text = strings.Join(strings.Fields(text), " ")
	if r := []rune(text); len(r) > 300 {
		text = string(r[:300]) + "…"
	}
	return text
}

func inDay(at, day time.Time) bool {
	y, m, d := at.In(day.Location()).Date()
	return y == day.Year() && m == day.Month() && d == day.Day()
}

func scanLines(path string, fn func([]byte)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	for sc.Scan() {
		fn(sc.Bytes())
	}
}

// Prompts lists the day's prompts from Claude Code (~/.claude/projects) and Codex
// (~/.codex/sessions). Sessions in a temp dir are tooling, not work.
func Prompts(day time.Time) []Prompt {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, day.Location())
	fresh := func(p string) bool { st, err := os.Stat(p); return err == nil && !st.ModTime().Before(start) }
	tmp := func(cwd string) bool { return strings.HasPrefix(cwd, "/tmp") || strings.HasPrefix(cwd, "/private/tmp") }
	var out []Prompt

	claude, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", "*.jsonl"))
	for _, p := range claude {
		if !fresh(p) {
			continue
		}
		scanLines(p, func(line []byte) {
			var e struct {
				Type      string    `json:"type"`
				IsMeta    bool      `json:"isMeta"`
				Compact   bool      `json:"isCompactSummary"`
				Timestamp time.Time `json:"timestamp"`
				Cwd       string    `json:"cwd"`
				Branch    string    `json:"gitBranch"`
				Message   struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &e) != nil || e.Type != "user" || e.IsMeta || e.Compact || !inDay(e.Timestamp, day) || tmp(e.Cwd) {
				return
			}
			var text string
			if json.Unmarshal(e.Message.Content, &text) != nil {
				var parts []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if json.Unmarshal(e.Message.Content, &parts) != nil {
					return
				}
				var texts []string
				for _, c := range parts {
					if c.Type == "tool_result" {
						return
					}
					if c.Type == "text" {
						texts = append(texts, c.Text)
					}
				}
				text = strings.Join(texts, "\n")
			}
			if t := promptText(text); t != "" {
				out = append(out, Prompt{At: e.Timestamp.In(day.Location()), Agent: "Claude Code", Cwd: e.Cwd, Branch: e.Branch, Text: t})
			}
		})
	}

	codex, _ := filepath.Glob(filepath.Join(home, ".codex", "sessions", "*", "*", "*", "*.jsonl"))
	for _, p := range codex {
		if !fresh(p) {
			continue
		}
		cwd := ""
		scanLines(p, func(line []byte) {
			var e struct {
				Timestamp time.Time `json:"timestamp"`
				Type      string    `json:"type"`
				Payload   struct {
					Type    string `json:"type"`
					Role    string `json:"role"`
					Cwd     string `json:"cwd"`
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &e) != nil {
				return
			}
			if e.Type == "session_meta" {
				cwd = e.Payload.Cwd
				return
			}
			if e.Type != "response_item" || e.Payload.Type != "message" || e.Payload.Role != "user" || !inDay(e.Timestamp, day) || tmp(cwd) {
				return
			}
			var texts []string
			for _, c := range e.Payload.Content {
				if c.Type == "input_text" {
					texts = append(texts, c.Text)
				}
			}
			if t := promptText(strings.Join(texts, "\n")); t != "" {
				out = append(out, Prompt{At: e.Timestamp.In(day.Location()), Agent: "Codex", Cwd: cwd, Text: t})
			}
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}
