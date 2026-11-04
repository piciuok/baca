package activity

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const osName = "Linux"

var wmClass = regexp.MustCompile(`"([^"]*)"\s*$`)

// probe uses xdotool (window, title), xprop (WM_CLASS) and xprintidle (idle) on X11.
// On Wayland other programs cannot see the focused window or the input.
func probe(ctx context.Context, wantTitle bool, idleAfter time.Duration) (sample, error) {
	var s sample
	if os.Getenv("XDG_SESSION_TYPE") == "wayland" || (os.Getenv("WAYLAND_DISPLAY") != "" && os.Getenv("DISPLAY") == "") {
		return s, errors.New("Wayland session: other apps cannot read the active window or idle time (an X11 session can); the git, agent and remote probes still work")
	}
	if out, err := run(ctx, "xprintidle"); err == nil {
		ms, _ := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
		s.Idle = time.Duration(ms) * time.Millisecond
	}
	id, err := run(ctx, "xdotool", "getactivewindow")
	if err != nil {
		return s, err
	}
	// getwindowclassname is missing in older xdotool (Debian 12, Ubuntu 22.04/24.04): xprop has it
	if c, err := run(ctx, "xdotool", "getwindowclassname", id); err == nil && c != "" {
		s.App = c
	} else if out, err := run(ctx, "xprop", "-id", id, "WM_CLASS"); err == nil {
		if m := wmClass.FindStringSubmatch(out); m != nil {
			s.App = m[1]
		}
	}
	// without the app name the password-manager list cannot apply: no title then
	if wantTitle && s.App != "" && s.Idle < idleAfter {
		if t, err := run(ctx, "xdotool", "getwindowname", id); err == nil {
			s.Title = t
		} else {
			s.TitleErr = err
		}
	}
	return s, nil
}

// windows lists "app<TAB>title" of the windows whose WM_CLASS matches one of apps (wmctrl).
func windows(ctx context.Context, apps []string) ([][2]string, error) {
	out, err := run(ctx, "wmctrl", "-lx")
	if err != nil {
		return nil, err
	}
	var res [][2]string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		class := strings.ToLower(f[2])
		for _, a := range apps {
			if strings.Contains(class, strings.ToLower(strings.ReplaceAll(a, " ", ""))) {
				res = append(res, [2]string{a, strings.Join(f[4:], " ")})
			}
		}
	}
	return res, nil
}
