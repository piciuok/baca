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
