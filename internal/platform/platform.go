// Package platform is the OS glue for the tray app: start at login, relaunch after an update,
// notifications, dialogs and opening URLs (macOS: launchd/osascript, Linux: XDG/notify-send).
package platform

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const launchLabel = "pl.piciuok.baca"

// Notification is a fire-and-forget message; Open (URL) is opened on click where supported.
type Notification struct {
	Title   string
	Message string
	Open    string
}

// StateDir holds logs and update status (~/.local/state/baca).
func StateDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "baca")
}

// start runs a command in the background and reaps it, so no zombie is left behind.
func start(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

func has(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// relaunchScript waits for the old process ($1) to exit, so the new one can take the port,
// then starts the new binary ($0).
const relaunchScript = `while kill -0 "$1" 2>/dev/null; do sleep 0.2; done; exec "$0" tray --foreground`

// spawnTray starts a detached `exe tray` that waits for this process to exit. A fresh
// process (not exec in place) is what gets the tray icon registered again.
func spawnTray(exe string) error {
	cmd := exec.Command("/bin/sh", "-c", relaunchScript, exe, strconv.Itoa(os.Getpid()))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// the new tray logs where the first one did
	if out, errf, err := logFiles(); err == nil {
		defer out.Close()
		defer errf.Close()
		cmd.Stdout, cmd.Stderr = out, errf
	}
	return cmd.Start()
}

func logFiles() (*os.File, *os.File, error) {
	dir := StateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	open := func(name string) (*os.File, error) {
		return os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	}
	out, err := open("tray.out.log")
	if err != nil {
		return nil, nil, err
	}
	errf, err := open("tray.err.log")
	if err != nil {
		out.Close()
		return nil, nil, err
	}
	return out, errf, nil
}

// detachTray starts `exe tray --foreground` in its own session with output appended to the
// logs in StateDir, and does not wait for it.
func detachTray(exe string) error {
	out, errf, err := logFiles()
	if err != nil {
		return err
	}
	defer out.Close()
	defer errf.Close()
	cmd := exec.Command(exe, "tray", "--foreground")
	cmd.Stdout, cmd.Stderr = out, errf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func xmlEsc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// Token returns the UI token from StateDir/token, creating it (mode 600) on first use. It is
// stable across restarts, so a bookmarked page keeps working, and only this user can read it.
func Token() (string, error) {
	dir := StateDir()
	path := filepath.Join(dir, "token")
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) >= 32 {
		return strings.TrimSpace(string(b)), nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, os.Chmod(path, 0o600)
}
