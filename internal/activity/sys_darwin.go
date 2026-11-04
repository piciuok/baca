package activity

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const osName = "macOS"

var (
	lsName = regexp.MustCompile(`^"(.*)" ASN:`)
	idleRE = regexp.MustCompile(`"HIDIdleTime" = (\d+)`)
)

const titleScript = `tell application "System Events" to get name of front window of (first application process whose frontmost is true)`

// probe reads the front app with lsappinfo (no permission needed), the idle time from
// IOHIDSystem and, when wantTitle, the front window title through System Events, which
// needs the Accessibility permission for the app that started baca.
func probe(ctx context.Context, wantTitle bool) (sample, error) {
	var s sample
	asn, err := run(ctx, "lsappinfo", "front")
	if err != nil {
		return s, err
	}
	info, err := run(ctx, "lsappinfo", "info", "-only", "name", asn)
	if err != nil {
		return s, err
	}
	if m := lsName.FindStringSubmatch(info); m != nil {
		s.App = m[1]
	}
	if out, err := run(ctx, "ioreg", "-c", "IOHIDSystem", "-d", "4"); err == nil {
		if m := idleRE.FindStringSubmatch(out); m != nil {
			ns, _ := strconv.ParseInt(m[1], 10, 64)
			s.Idle = time.Duration(ns)
		}
	}
	if wantTitle && s.App != "" {
		t, err := run(ctx, "osascript", "-e", titleScript)
		var pe *probeError
		switch {
		case err == nil:
			s.Title = t
		case errors.As(err, &pe) && (strings.Contains(pe.msg, "-1719") || strings.Contains(pe.msg, "-25211") || strings.Contains(strings.ToLower(pe.msg), "assistive")):
			s.TitleErr = errors.New("no Accessibility permission (System Settings → Privacy & Security → Accessibility) for the app baca runs from")
		default:
			// windowless apps (Finder desktop, menu-bar apps) have no front window - not an error
		}
	}
	return s, nil
}
