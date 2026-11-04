package activity

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

const osName = "Linux"

// probe uses xdotool (window, title, class) and xprintidle (idle) on X11.
// Wayland compositors do not let other programs see the focused window.
func probe(ctx context.Context, wantTitle bool) (sample, error) {
	var s sample
	if os.Getenv("WAYLAND_DISPLAY") != "" && os.Getenv("DISPLAY") == "" {
		return s, errors.New("Wayland without XWayland: the active window cannot be read")
	}
	id, err := run(ctx, "xdotool", "getactivewindow")
	if err != nil {
		return s, err
	}
	if c, err := run(ctx, "xdotool", "getwindowclassname", id); err == nil {
		s.App = c
	}
	if wantTitle {
		if t, err := run(ctx, "xdotool", "getwindowname", id); err == nil {
			s.Title = t
		} else {
			s.TitleErr = err
		}
	}
	if out, err := run(ctx, "xprintidle"); err == nil {
		ms, _ := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
		s.Idle = time.Duration(ms) * time.Millisecond
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
