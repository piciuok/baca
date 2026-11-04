// PATH for apps started by launchd or an XDG autostart entry.
package platform

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// EnsurePath appends the usual user tool locations to PATH. Apps started from
// launchd, the Dock or an XDG autostart entry get a minimal PATH that misses
// Homebrew and ~/.local/bin, where gh, glab, git, claude and codex live.
func EnsurePath() {
	home, _ := os.UserHomeDir()
	extra := []string{
		"/opt/homebrew/bin", "/usr/local/bin",
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, ".claude", "local"),
		filepath.Join(home, "go", "bin"),
		"/home/linuxbrew/.linuxbrew/bin",
		"/snap/bin",
		"/usr/bin", "/bin", "/usr/sbin", "/sbin",
	}
	cur := filepath.SplitList(os.Getenv("PATH"))
	have := map[string]bool{}
	for _, p := range cur {
		have[p] = true
	}
	for _, p := range extra {
		if !have[p] {
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				cur = append(cur, p)
				have[p] = true
			}
		}
	}
	os.Setenv("PATH", strings.Join(cur, string(os.PathListSeparator)))
}

// UseLoginShellPath puts the PATH of the user's login shell first, so the
// tray (started by launchd or XDG autostart) runs the same gh, glab, claude
// and codex as the terminal — not, say, an old Homebrew copy that the
// shell's PATH shadows. It costs one shell start, so only the tray calls it.
func UseLoginShellPath() {
	sh := os.Getenv("SHELL")
	if sh == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const mark = "__BACA_PATH__"
	cmd := exec.CommandContext(ctx, sh, "-lic", `printf '\n`+mark+`%s\n' "$PATH"`)
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		return
	}
	var shellPath string
	for _, l := range strings.Split(string(out), "\n") {
		if p, ok := strings.CutPrefix(l, mark); ok {
			shellPath = strings.TrimSpace(p)
		}
	}
	if shellPath == "" {
		return
	}
	var merged []string
	have := map[string]bool{}
	for _, p := range append(filepath.SplitList(shellPath), filepath.SplitList(os.Getenv("PATH"))...) {
		if p != "" && !have[p] {
			have[p] = true
			merged = append(merged, p)
		}
	}
	os.Setenv("PATH", strings.Join(merged, string(os.PathListSeparator)))
}
