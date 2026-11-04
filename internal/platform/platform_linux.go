package platform

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func Open(target string) error { return exec.Command("xdg-open", target).Start() }

func Info(title, msg string) {
	switch {
	case has("zenity"):
		_ = exec.Command("zenity", "--info", "--title", title, "--text", trunc(msg, 900), "--timeout", "60").Run()
	case has("kdialog"):
		_ = exec.Command("kdialog", "--title", title, "--msgbox", trunc(msg, 900)).Run()
	default:
		Notify(Notification{Title: title, Message: msg})
	}
}

func Notify(n Notification) {
	if !has("notify-send") {
		return
	}
	_ = exec.Command("notify-send", "--app-name=baca", "--", n.Title, trunc(n.Message, 300)).Start()
}

// Relaunch starts the tray anew from exe: a fresh process waits for this one, which must quit.
func Relaunch(exe string) (quit bool, err error) {
	return true, spawnTray(exe)
}

func StartTray(exe string) error { return detachTray(exe) }

func desktopPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "autostart", "baca.desktop")
}

func AutostartEnabled() bool {
	_, err := os.Stat(desktopPath())
	return err == nil
}

// AutostartInstall writes an XDG autostart entry (GNOME, KDE, XFCE, Cinnamon).
func AutostartInstall(exe string) error {
	entry := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=baca
Comment=Day timeline and worklog
Exec=%s tray
Icon=appointment-soon
Terminal=false
X-GNOME-Autostart-enabled=true
X-GNOME-Autostart-Delay=10
`, desktopQuote(exe))
	if err := os.MkdirAll(filepath.Dir(desktopPath()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(desktopPath(), []byte(entry), 0o644)
}

func AutostartUninstall() error {
	err := os.Remove(desktopPath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func desktopQuote(s string) string {
	if !strings.ContainsAny(s, " \"'\\$`") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\\\`, `"`, `\"`, "`", "\\`", "$", `\$`).Replace(s) + `"`
}
