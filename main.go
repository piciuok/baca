// baca serves the day bar UI on localhost and keeps the worklog in a plain text file.
package main

import (
	"context"
	"encoding/json"
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
	"syscall"
	"time"

	"baca/internal/activity"
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
	cfgPath := flag.String("config", env("BACA_CONFIG", defaultConfig()), "config.json (worklog file, Jira/Tempo tokens)")
	file := flag.String("file", os.Getenv("BACA_FILE"), "worklog file (overrides worklogFile from the config)")
	open := flag.Bool("open", false, "open the UI in the browser")
	collect := flag.Bool("collect", true, "collect activity (probes from the activity section of the config)")
	flag.Parse()

	var local struct {
		WorklogFile string          `json:"worklogFile"`
		Activity    activity.Config `json:"activity"`
	}
	if b, err := os.ReadFile(home(*cfgPath)); err == nil {
		if err := json.Unmarshal(b, &local); err != nil {
			log.Fatalf("%s: %v", *cfgPath, err)
		}
	}
	if *file == "" {
		*file = local.WorklogFile
	}
	if *file == "" {
		*file = "~/.local/share/baca/worklog.txt"
	}

	host, _, err := net.SplitHostPort(*addr)
	if err != nil || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
		log.Fatalf("-addr must be on localhost, got %q", *addr)
	}

	s := &server.Server{Store: worklog.NewStore(home(*file)), Web: web.FS}
	if cfg, err := tempo.LoadConfig(home(*cfgPath)); err != nil {
		s.TempoErr = err.Error()
		log.Printf("Tempo disabled: %v", err)
	} else {
		s.Tempo = tempo.New(cfg)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	local.Activity.Defaults(s.Store.Path)
	s.Activity = activity.Log{Dir: local.Activity.Dir}
	collectorDone := make(chan struct{})
	if probes := local.Activity.Probes(); *collect && len(probes) > 0 {
		s.Collector = activity.NewCollector(s.Activity, probes...)
		go func() { s.Collector.Run(ctx); close(collectorDone) }()
		names := []string{}
		for _, p := range probes {
			names = append(names, p.Name())
		}
		log.Printf("activity: %s → %s", strings.Join(names, ", "), local.Activity.Dir)
	} else {
		close(collectorDone)
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	url := "http://" + ln.Addr().String()
	log.Printf("baca: %s  file: %s", url, s.Store.Path)
	if *open {
		go openBrowser(url)
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	<-collectorDone // the open activity span is written before exit
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
