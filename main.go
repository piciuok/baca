// baca serves the day bar UI on localhost and keeps the worklog in a plain text file.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"baca/internal/activity"
	"baca/internal/config"
	"baca/internal/server"
	"baca/internal/tempo"
	"baca/internal/worklog"
	"baca/web"
)

func home(p string) string {
	if strings.HasPrefix(p, "~/") {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, p[2:])
	}
	return p
}

// defaultConfig is config.json next to the binary, or in the working directory under go run.
func defaultConfig() string {
	if exe, err := os.Executable(); err == nil && !strings.Contains(exe, "go-build") {
		return filepath.Join(filepath.Dir(exe), "config.json")
	}
	return "config.json"
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	addr := flag.String("addr", env("BACA_ADDR", "127.0.0.1:7788"), "server address (localhost only)")
	cfgPath := flag.String("config", env("BACA_CONFIG", defaultConfig()), "config.json (settings, tokens)")
	file := flag.String("file", os.Getenv("BACA_FILE"), "worklog file (overrides worklogFile from the config)")
	open := flag.Bool("open", false, "open the UI in the browser")
	collect := flag.Bool("collect", true, "collect activity (probes from the settings)")
	flag.Parse()

	host, _, err := net.SplitHostPort(*addr)
	if err != nil || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
		log.Fatalf("-addr must be on localhost, got %q", *addr)
	}
	cfg, err := config.Load(home(*cfgPath))
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a := &app{path: home(*cfgPath), fileFlag: *file, collect: *collect, ctx: ctx,
		srv: &server.Server{Store: worklog.NewStore(""), Web: web.FS}}
	a.srv.Settings = a
	a.apply(cfg)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	url := "http://" + ln.Addr().String()
	log.Printf("baca: %s  file: %s", url, a.srv.Store.FilePath())
	if *open {
		go openBrowser(url)
	}
	srv := &http.Server{Handler: a.srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	a.stopCollector() // the open activity span is written before exit
}

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

func openBrowser(url string) {
	cmd := "xdg-open"
	if runtime.GOOS == "darwin" {
		cmd = "open"
	}
	if err := exec.Command(cmd, url).Start(); err != nil {
		fmt.Fprintln(os.Stderr, "cannot open the browser:", err)
	}
}
