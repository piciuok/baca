// Package tray is the menu bar / system tray icon: today's total next to the clock, a ring
// that fills towards the daily target, and a menu to open the UI, update and quit.
package tray

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"time"

	"fyne.io/systray"

	"baca/internal/platform"
	"baca/internal/update"
)

// Today is the state of the current day for the icon.
type Today struct {
	Minutes, Target int
	Entries, Logged int
}

type Tray struct {
	Page    func() string // a fresh one-time address of the UI
	Updates *update.Manager
	Today   func() Today
	OnExit  func()
}

func dur(m int) string { return fmt.Sprintf("%dh %02dm", m/60, m%60) }

// Run shows the icon and blocks until Quit (must run on the main goroutine on macOS).
func (t *Tray) Run() { systray.Run(t.onReady, t.onExit) }

func Quit() { systray.Quit() }

func (t *Tray) onExit() {
	if t.OnExit != nil {
		t.OnExit()
	}
}

func (t *Tray) onReady() {
	systray.SetTooltip("baca")
	head := systray.AddMenuItem("Today …", "")
	head.Disable()
	open := systray.AddMenuItem("Open baca", "Day timeline and worklog in the browser")
	settings := systray.AddMenuItem("Settings…", "Working hours, accounts, probes")
	systray.AddSeparator()
	upd := systray.AddMenuItem("⬆ Update", "")
	upd.Hide()
	check := systray.AddMenuItem("Check for updates", "")
	auto := systray.AddMenuItemCheckbox("Start at login", "", platform.AutostartEnabled())
	systray.AddSeparator()
	ver := systray.AddMenuItem("", "")
	ver.Disable()
	quit := systray.AddMenuItem("Quit", "")

	refresh := func() {
		d := t.Today()
		frac := 0.0
		if d.Target > 0 {
			frac = float64(d.Minutes) / float64(d.Target)
		}
		done := d.Entries > 0 && d.Logged == d.Entries && d.Minutes >= d.Target
		if runtime.GOOS == "darwin" {
			systray.SetTemplateIcon(Icon(frac, done, true), Icon(frac, done, false))
			systray.SetTitle(fmt.Sprintf("%dh%02d", d.Minutes/60, d.Minutes%60))
		} else {
			systray.SetIcon(Icon(frac, done, false))
		}
		line := fmt.Sprintf("Today %s / %s", dur(d.Minutes), dur(d.Target))
		if d.Entries > 0 {
			line += fmt.Sprintf(" · logged %d/%d", d.Logged, d.Entries)
		}
		head.SetTitle(line)
		systray.SetTooltip("baca — " + line)

		in := t.Updates.Info()
		ver.SetTitle("Version " + in.Current)
		switch {
		case in.Installing:
			upd.SetTitle("⬆ Installing " + in.Latest + "…")
			upd.Disable()
			upd.Show()
		case in.Latest != "":
			upd.SetTitle("⬆ Update to " + in.Latest)
			upd.Enable()
			upd.Show()
		default:
			upd.Hide()
		}
		if platform.AutostartEnabled() {
			auto.Check()
		} else {
			auto.Uncheck()
		}
	}
	refresh()

	go func() {
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				refresh()
			case <-open.ClickedCh:
				_ = platform.Open(t.Page())
			case <-settings.ClickedCh:
				_ = platform.Open(t.Page() + "#settings")
			case <-check.ClickedCh:
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
					defer cancel()
					in, err := t.Updates.Check(ctx, true)
					refresh()
					switch {
					case err != nil:
						platform.Info("baca", "Update check failed: "+err.Error())
					case in.Dev:
						platform.Info("baca", "This is a development build ("+in.Current+"), it does not update itself.\n\nTo switch to releases: baca update --force")
					case in.Latest != "":
						platform.Info("baca", "Version "+in.Latest+" is available (you have "+in.Current+").\n\nChoose “⬆ Update” in the menu.")
					default:
						platform.Info("baca", "You are on the latest version ("+in.Current+").")
					}
				}()
			case <-upd.ClickedCh:
				go func() {
					refresh()
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
					defer cancel()
					if err := t.Updates.Install(ctx); err != nil {
						platform.Notify(platform.Notification{Title: "baca: update failed", Message: err.Error()})
					}
					refresh()
				}()
			case <-auto.ClickedCh:
				exe, _ := os.Executable()
				var err error
				if platform.AutostartEnabled() {
					err = platform.AutostartUninstall()
				} else {
					err = platform.AutostartInstall(exe)
				}
				if err != nil {
					go platform.Info("baca", err.Error()) // a dialog must not stall the menu
				}
				refresh()
			case <-quit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}
