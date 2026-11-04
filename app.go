package main

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"baca/internal/activity"
	"baca/internal/config"
	"baca/internal/server"
	"baca/internal/tempo"
	"baca/internal/tray"
	"baca/internal/worklog"
)

// app owns config.json and rebuilds the server state (Tempo client, probes, working hours)
// whenever the settings change.
type app struct {
	path     string
	fileFlag string // -file wins over worklogFile
	collect  bool
	ctx      context.Context
	srv      *server.Server

	mu       sync.Mutex
	cfg      config.Config
	cancel   context.CancelFunc
	finished chan struct{}
}

func (a *app) Path() string { return a.path }

func (a *app) Get() config.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

func (a *app) Apply(c config.Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := config.Save(a.path, c); err != nil {
		return err
	}
	a.apply(c)
	log.Printf("settings saved: %s", a.path)
	return nil
}

func (a *app) stopCollector() {
	a.mu.Lock()
	cancel, done := a.cancel, a.finished
	a.cancel, a.finished = nil, nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (a *app) apply(c config.Config) {
	a.stopCollector()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg = c

	file := a.fileFlag
	if file == "" {
		file = c.WorklogFile
	}
	if file == "" {
		file = "~/.local/share/baca/worklog.txt"
	}
	a.srv.Store.SetPath(home(file))
	worklog.SetWeekdays(c.Weekdays)

	st := &server.State{Workday: c.Workday}
	act := c.Activity // Defaults fills the copy only, config.json stays as written
	tc, err := tempo.Prepare(tempo.Config{AccountEmail: c.AccountEmail, Tokens: c.Tokens, JiraBaseURL: c.JiraBaseURL, TempoBaseURL: c.TempoBaseURL})
	if err != nil {
		st.TempoErr = err.Error()
		log.Printf("Tempo disabled: %v", err)
	} else {
		st.Tempo = tempo.New(tc)
		act.JiraAuth = activity.JiraAuth{BaseURL: tc.JiraBaseURL, Email: tc.AccountEmail, Token: tc.Tokens.Jira}
	}
	act.Defaults(home(file))
	st.Activity = activity.Log{Dir: act.Dir}
	if probes := act.Probes(); a.collect && len(probes) > 0 {
		st.Collector = activity.NewCollector(st.Activity, probes...)
		ctx, cancel := context.WithCancel(a.ctx)
		done := make(chan struct{})
		go func() { st.Collector.Run(ctx); close(done) }()
		a.cancel, a.finished = cancel, done
		names := []string{}
		for _, p := range probes {
			names = append(names, p.Name())
		}
		log.Printf("activity: %s → %s", strings.Join(names, ", "), act.Dir)
	}
	a.srv.SetState(st)
}

// today is the current day for the tray icon.
func (a *app) today() tray.Today {
	_, _, target := a.Get().Workday.Minutes()
	t := tray.Today{Target: target}
	days, err := a.srv.Store.Days()
	if err != nil {
		return t
	}
	key := time.Now().Format("2006-01-02")
	for _, d := range days {
		if d.Date == key {
			t.Minutes, t.Entries, t.Logged = d.Minutes, d.Entries, d.Logged
		}
	}
	return t
}
