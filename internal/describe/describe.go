// Package describe writes worklog descriptions with a coding agent (Claude Code or Codex). baca
// gathers what happened during each block - commits, agent prompts, window titles, service events
// - and the agent only turns it into text: it gets no tools, no files and no session is kept.
package describe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Config is the "describe" section of config.json.
type Config struct {
	Agent string `json:"agent"` // claude (default) | codex
	Model string `json:"model"` // empty: the CLI's default
	Style string `json:"style"` // how descriptions read; empty: DefaultStyle
}

// DefaultStyle is how the descriptions read unless the settings say otherwise.
const DefaultStyle = `Write each description as comma-separated short phrases that start with a verb (Prepare ..., fix ..., add ..., verify ..., resolve ...), with ";" between bigger groups. Say what changed for the product or the team, not how: no file names, commit hashes, test counts or time estimates. Usually 3-12 phrases; merge small items. Write in the language the earlier days use for that ticket's project (English when unsure).
The first block of a ticket in the day carries the description; later blocks of the same ticket that day say only "Continue task". When the ticket already appears in the earlier days, start its first description with "Continue task, ".
A block with little evidence gets a short, honest description built from the ticket and what there is; never invent work.`

// Evidence is what the probes and git saw during a block.
type Evidence struct {
	Commits []string `json:"commits,omitempty"` // "09:12 repo (branch): subject"
	Prompts []string `json:"prompts,omitempty"` // "09:20 repo: what the user asked an agent"
	Windows []string `json:"windows,omitempty"` // "12m App · title"
	Events  []string `json:"events,omitempty"`  // "10:05 GitLab app: approved MR ..."
}

// Block is one worklog block to describe.
type Block struct {
	Key              string   `json:"key"`
	Ticket           string   `json:"ticket"`
	From             string   `json:"from"`
	To               string   `json:"to"`
	Current          string   `json:"current,omitempty"`
	FirstOfTicket    bool     `json:"firstOfTicketToday"`
	TicketSeenBefore bool     `json:"ticketSeenBefore"`
	Evidence         Evidence `json:"evidence"`
}

// Request is everything the agent gets.
type Request struct {
	Day     string   // "Tuesday 03.11.2026"
	Earlier []string // the file's last days, as text, newest first: style examples and continuity
	Blocks  []Block
	Style   string
}

// Proposal is the agent's description for a block.
type Proposal struct {
	Key  string `json:"key"`
	Desc string `json:"desc"`
}

// Render writes the request out as the prompt for the agent.
func Render(r Request) string {
	style := strings.TrimSpace(r.Style)
	if style == "" {
		style = DefaultStyle
	}
	blocks, _ := json.MarshalIndent(r.Blocks, "", "  ")
	var b strings.Builder
	b.WriteString("You write worklog descriptions for time logged on tickets. Answer with JSON only.\n\n")
	b.WriteString("Style:\n" + style + "\n\n")
	b.WriteString("Day: " + r.Day + "\n\n")
	if len(r.Earlier) > 0 {
		b.WriteString("Earlier days from the user's worklog (newest first) - follow their style and naming:\n")
		b.WriteString(strings.Join(r.Earlier, "\n\n") + "\n\n")
	}
	b.WriteString("Blocks to describe, in order, with what happened during each (commits, prompts the user gave coding agents, window titles with minutes, events in GitLab/GitHub/Jira, meetings). \"current\" is the description the block has now - keep what is right in it.\n")
	b.Write(blocks)
	b.WriteString("\n\nReply with only a JSON array, one object per block in the same order: [{\"key\": \"<key>\", \"desc\": \"<description>\"}]. No other text, no code fence.\n")
	return b.String()
}

// Parse reads the agent's reply: the JSON array, wherever it sits in the text.
func Parse(out string, blocks []Block) ([]Proposal, error) {
	i, j := strings.Index(out, "["), strings.LastIndex(out, "]")
	if i < 0 || j < i {
		return nil, fmt.Errorf("the agent did not answer with JSON: %s", clip(out))
	}
	var ps []Proposal
	if err := json.Unmarshal([]byte(out[i:j+1]), &ps); err != nil {
		return nil, fmt.Errorf("the agent's JSON: %v: %s", err, clip(out))
	}
	want := map[string]bool{}
	for _, b := range blocks {
		want[b.Key] = true
	}
	var res []Proposal
	for _, p := range ps {
		p.Desc = strings.Join(strings.Fields(p.Desc), " ")
		if want[p.Key] && p.Desc != "" {
			res = append(res, p)
			delete(want, p.Key)
		}
	}
	if len(res) == 0 {
		return nil, errors.New("the agent described none of the blocks")
	}
	return res, nil
}

func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 300 {
		return string(r[:300]) + "…"
	}
	return s
}

// Runner runs an agent on a prompt and returns its reply.
type Runner func(ctx context.Context, prompt string) (string, error)

// AgentRunner runs the configured CLI with no tools and no kept session, in a scratch dir.
func AgentRunner(cfg Config) Runner {
	return func(ctx context.Context, prompt string) (string, error) {
		dir, err := os.MkdirTemp("", "baca-describe-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(dir)
		var cmd *exec.Cmd
		last := filepath.Join(dir, "reply.txt")
		switch cfg.Agent {
		case "codex":
			args := []string{"exec", "--sandbox", "read-only", "--ephemeral", "--skip-git-repo-check", "--color", "never", "-o", last}
			if cfg.Model != "" {
				args = append(args, "--model", cfg.Model)
			}
			cmd = exec.CommandContext(ctx, "codex", append(args, "-")...)
		case "", "claude":
			args := []string{"-p", "--tools", "", "--max-turns", "1", "--output-format", "text",
				"--no-session-persistence", "--strict-mcp-config"}
			if cfg.Model != "" {
				args = append(args, "--model", cfg.Model)
			}
			cmd = exec.CommandContext(ctx, "claude", args...)
		default:
			return "", fmt.Errorf("unknown agent %q (claude or codex)", cfg.Agent)
		}
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(prompt)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			if errors.Is(err, exec.ErrNotFound) {
				return "", fmt.Errorf("%s is not installed or not in PATH", cmd.Path)
			}
			return "", fmt.Errorf("%s: %v: %s", filepath.Base(cmd.Path), err, clip(stderr.String()+" "+string(out)))
		}
		if cfg.Agent == "codex" {
			if b, err := os.ReadFile(last); err == nil {
				return string(b), nil
			}
		}
		return string(out), nil
	}
}

// Timeout is how long one run may take.
const Timeout = 4 * time.Minute
