package platform

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func Open(target string) error { return start(exec.Command("/usr/bin/open", target)) }

// Info shows a message the user asked for as a dialog (notifications may be muted).
func Info(title, msg string) {
	_ = exec.Command("/usr/bin/osascript", "-e", fmt.Sprintf(`display dialog %s with title %s buttons {"OK"} default button "OK" with icon note giving up after 60`,
		asq(trunc(msg, 900)), asq(title))).Run()
}

func Notify(n Notification) {
	if has("terminal-notifier") {
		args := []string{"-title", n.Title, "-message", trunc(n.Message, 240), "-group", "baca-" + n.Title}
		if n.Open != "" {
			args = append(args, "-open", n.Open)
		}
		_ = start(exec.Command("terminal-notifier", args...))
		return
	}
	_ = start(exec.Command("/usr/bin/osascript", "-e", fmt.Sprintf("display notification %s with title %s", asq(trunc(n.Message, 240)), asq(n.Title))))
}

func asq(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// Relaunch starts the tray anew from exe (after an update). Under the LaunchAgent launchd
// restarts the job and stops this process itself (quit=false); otherwise a fresh process
// waits for this one, which must quit (quit=true).
func Relaunch(exe string) (quit bool, err error) {
	if os.Getenv("XPC_SERVICE_NAME") == launchLabel {
		return false, start(exec.Command("/bin/launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchLabel)))
	}
	return true, spawnTray(exe)
}

// StartTray starts the tray in the background: through launchd when the LaunchAgent runs
// this binary (so updates and logout work as at login), otherwise detached.
func StartTray(exe string) error {
	if b, err := os.ReadFile(plistPath()); err == nil && strings.Contains(string(b), "<string>"+xmlEsc(exe)+"</string>") {
		domain := fmt.Sprintf("gui/%d", os.Getuid())
		if exec.Command("/bin/launchctl", "kickstart", domain+"/"+launchLabel).Run() == nil {
			return nil
		}
		if exec.Command("/bin/launchctl", "bootstrap", domain, plistPath()).Run() == nil {
			return nil
		}
	}
	return detachTray(exe)
}

func plistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchLabel+".plist")
}

func AutostartEnabled() bool {
	_, err := os.Stat(plistPath())
	return err == nil
}

// AutostartInstall registers exe as a LaunchAgent in the GUI session (needed for the menu bar
// item and the Accessibility permission). It does not start a second copy now.
func AutostartInstall(exe string) error {
	logDir := StateDir()
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return err
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array><string>%s</string><string>tray</string><string>--foreground</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>LimitLoadToSessionType</key><string>Aqua</string>
  <key>ProcessType</key><string>Interactive</string>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, launchLabel, xmlEsc(exe), xmlEsc(filepath.Join(logDir, "tray.out.log")), xmlEsc(filepath.Join(logDir, "tray.err.log")))
	if err := os.MkdirAll(filepath.Dir(plistPath()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(plistPath(), []byte(plist), 0o644)
}

// AutostartUninstall removes the LaunchAgent; a running tray keeps running.
func AutostartUninstall() error {
	err := os.Remove(plistPath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// TrayHost: the macOS menu bar always takes the icon.
func TrayHost() error { return nil }
