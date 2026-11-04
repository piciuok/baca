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
func probe(ctx context.Context, wantTitle bool, idleAfter time.Duration) (sample, error) {
	var s sample
	// idle first: while nobody types, the title is not recorded, so osascript is not run
	if out, err := run(ctx, "ioreg", "-c", "IOHIDSystem", "-r", "-k", "HIDIdleTime", "-d", "1"); err == nil {
		if m := idleRE.FindStringSubmatch(out); m != nil {
			ns, _ := strconv.ParseInt(m[1], 10, 64)
			s.Idle = time.Duration(ns)
		}
	}
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
	if wantTitle && s.App != "" && s.Idle < idleAfter {
		t, err := run(ctx, "osascript", "-e", titleScript)
		var pe *probeError
		switch {
		case err == nil:
			s.Title = t
		case errors.As(err, &pe) && (strings.Contains(pe.msg, "-1719") || strings.Contains(pe.msg, "-25211") || strings.Contains(strings.ToLower(pe.msg), "assistive")):
			s.TitleErr = errors.New("no Accessibility permission (System Settings → Privacy & Security → Accessibility) for the app baca runs from")
		case errors.As(err, &pe) && (strings.Contains(pe.msg, "-1743") || strings.Contains(strings.ToLower(pe.msg), "not authorized")):
			s.TitleErr = errors.New("no permission to ask System Events (System Settings → Privacy & Security → Automation → System Events) for the app baca runs from")
		default:
			// windowless apps (Finder desktop, menu-bar apps) have no front window - not an error
		}
	}
	return s, nil
}

// windows lists "app<TAB>title" of every window of the given running apps (needs Accessibility).
func windows(ctx context.Context, apps []string) ([][2]string, error) {
	if len(apps) == 0 {
		return nil, nil
	}
	quoted := make([]string, len(apps))
	for i, a := range apps {
		quoted[i] = `"` + strings.ReplaceAll(a, `"`, ``) + `"`
	}
	script := `set AppleScript's text item delimiters to linefeed
tell application "System Events"
set out to {}
repeat with pn in {` + strings.Join(quoted, ", ") + `}
if exists process pn then
try
repeat with w in (get name of every window of process pn)
set end of out to (pn as text) & tab & w
end repeat
end try
end if
end repeat
end tell
return out as text`
	out, err := run(ctx, "osascript", "-e", script)
	if err != nil {
		return nil, err
	}
	return splitWindows(out), nil
}
