package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// Manager checks for releases in the background and installs one on request; the tray menu,
// the web UI and the CLI share it.
type Manager struct {
	Version    string
	StatusPath string
	Prerelease func() bool
	// Restart starts the new binary and stops this process (set by the tray).
	Restart func(exe string) error
	// Notify tells the user once about a new version found by a background check.
	Notify func(Info)

	mu         sync.Mutex
	st         Status
	loaded     bool
	installing bool
}

// Info is what the menu and the UI show.
type Info struct {
	Current    string    `json:"current"`
	Latest     string    `json:"latest,omitempty"` // only when newer than Current
	URL        string    `json:"url,omitempty"`
	Dev        bool      `json:"dev"` // not a release build: never replaced
	Installing bool      `json:"installing"`
	CheckedAt  time.Time `json:"checkedAt"`
	Error      string    `json:"error,omitempty"`
}

func (m *Manager) load() {
	if !m.loaded {
		m.st, m.loaded = LoadStatus(m.StatusPath), true
	}
}

func (m *Manager) Info() Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	in := Info{Current: m.Version, Installing: m.installing, CheckedAt: m.st.CheckedAt, Error: m.st.Error}
	if _, err := ParseVersion(m.Version); err != nil {
		in.Dev = true
		return in
	}
	if newer, _ := Newer(m.Version, m.st.Latest); newer {
		in.Latest, in.URL = m.st.Latest, m.st.URL
	}
	return in
}

// UpdatedFrom returns the version this process replaced, once, right after an update.
func (m *Manager) UpdatedFrom() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	from := m.st.UpdatedFrom
	if from != "" {
		m.st.UpdatedFrom = ""
		_ = SaveStatus(m.StatusPath, m.st)
	}
	if from == m.Version {
		return ""
	}
	return from
}

func (m *Manager) pre() bool { return m.Prerelease != nil && m.Prerelease() }

// Check asks GitHub for the latest release. A background check notifies once per version.
func (m *Manager) Check(ctx context.Context, manual bool) (Info, error) {
	rel, err := (&Client{Version: m.Version}).Latest(ctx, m.pre())
	m.mu.Lock()
	m.load()
	m.st.CheckedAt, m.st.Error = time.Now(), ""
	if err != nil {
		m.st.Error = err.Error()
	} else {
		m.st.Latest, m.st.URL = rel.Tag, rel.URL
	}
	newer, _ := Newer(m.Version, m.st.Latest)
	fresh := err == nil && newer && !manual && m.st.Notified != m.st.Latest
	if fresh {
		m.st.Notified = m.st.Latest
	}
	_ = SaveStatus(m.StatusPath, m.st)
	m.mu.Unlock()
	in := m.Info()
	if fresh && m.Notify != nil {
		m.Notify(in)
	}
	return in, err
}

// Loop checks shortly after start and then every interval (the last check survives restarts).
func (m *Manager) Loop(ctx context.Context, interval time.Duration, enabled func() bool) {
	t := time.NewTimer(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if enabled == nil || enabled() {
			if in := m.Info(); time.Since(in.CheckedAt) >= interval {
				c, cancel := context.WithTimeout(ctx, time.Minute)
				_, _ = m.Check(c, false)
				cancel()
			}
		}
		t.Reset(10 * time.Minute)
	}
}

// Install downloads the latest release, swaps the binary in place and restarts.
func (m *Manager) Install(ctx context.Context) error {
	m.mu.Lock()
	if m.installing {
		m.mu.Unlock()
		return errors.New("an update is already running")
	}
	m.installing = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.installing = false
		m.mu.Unlock()
	}()
	if _, err := ParseVersion(m.Version); err != nil {
		return fmt.Errorf("this is a development build (%s) — install a release: baca update --force", m.Version)
	}
	c := &Client{Version: m.Version}
	rel, err := c.Latest(ctx, m.pre())
	if err != nil {
		return err
	}
	if newer, err := Newer(m.Version, rel.Tag); err != nil || !newer {
		return fmt.Errorf("no version newer than %s", m.Version)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := c.Install(ctx, rel, exe); err != nil {
		return err
	}
	m.mu.Lock()
	m.st.UpdatedFrom = m.Version
	_ = SaveStatus(m.StatusPath, m.st)
	m.mu.Unlock()
	if m.Restart == nil {
		return nil
	}
	if err := m.Restart(exe); err != nil {
		return fmt.Errorf("installed %s, but the restart failed (%v) — start baca again", rel.Tag, err)
	}
	return nil
}
