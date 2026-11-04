package platform

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/godbus/dbus/v5"
)

func Open(target string) error { return start(exec.Command("xdg-open", target)) }

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
	_ = start(exec.Command("notify-send", "--app-name=baca", "--", n.Title, trunc(n.Message, 300)))
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

// TrayHost tells whether the desktop can show a tray icon: the StatusNotifier watcher must be on
// the session bus. Vanilla GNOME has none without the AppIndicator extension.
func TrayHost() error {
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		return errors.New("no desktop session bus (DBUS_SESSION_BUS_ADDRESS): run baca serve instead of the tray")
	}
	conn, err := dbus.SessionBus()
	if err != nil {
		return fmt.Errorf("session bus: %w", err)
	}
	var has bool
	if err := conn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, "org.kde.StatusNotifierWatcher").Store(&has); err != nil || !has {
		return errors.New("this desktop shows no tray icons (on GNOME install the AppIndicator extension); baca runs anyway: baca open")
	}
	return nil
}
