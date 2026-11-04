// baca shows the working day as a timeline, keeps the worklog in a plain text file,
// suggests blocks from activity probes and logs hours to Tempo. It lives in the menu bar /
// system tray and serves its UI on localhost.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"baca/internal/config"
	"baca/internal/platform"
	"baca/internal/server"
	"baca/internal/tray"
	"baca/internal/update"
	"baca/internal/worklog"
	"baca/web"
)

const usage = `baca — day timeline, worklog and Tempo logging (macOS, Linux)

Usage:
  baca [tray] [--foreground]   menu bar / tray icon with the UI server; from a terminal
                                     it starts in the background and returns
  baca serve [-open]           UI server only, no icon (development)
  baca open                    open the UI (with its token) in the browser
  baca install | uninstall     copies the binary to ~/.local/bin and enables start at login
  baca update [--check] [--pre] [--force]
                                     update to the latest GitHub release
  baca version

Common flags (tray, serve): -addr, -config, -file, -collect=false
`

var version = "dev"

func main() {
	args := os.Args[1:]
	cmd := "tray"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "tray":
		err = cmdRun(args, true)
	case "serve":
		err = cmdRun(args, false)
	case "open":
		err = cmdOpen(args)
	case "install":
		err = cmdInstall()
	case "uninstall":
		err = platform.AutostartUninstall()
		if err == nil {
			fmt.Println("Start at login disabled. The binary, config and data are kept.")
		}
	case "update":
		err = cmdUpdate(args)
	case "version", "--version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "baca:", err)
		os.Exit(1)
	}
}

func home(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, strings.TrimPrefix(p, "~"))
	}
	return p
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// configPath: -config / BACA_CONFIG, else ~/.config/baca/config.json, else an older
// config.json next to the binary or in the working directory, else the new default.
func configPath(flagVal string) string {
	if flagVal != "" {
		return home(flagVal)
	}
	if v := os.Getenv("BACA_CONFIG"); v != "" {
		return home(v)
	}
	def := home("~/.config/baca/config.json")
	if exists(def) {
		return def
	}
	for _, p := range legacyConfigs() {
		if exists(p) {
			return p
		}
	}
	return def
}

func legacyConfigs() []string {
	var out []string
	if exe, err := os.Executable(); err == nil && !strings.Contains(exe, "go-build") {
		out = append(out, filepath.Join(filepath.Dir(exe), "config.json"))
	}
	if wd, err := os.Getwd(); err == nil {
		out = append(out, filepath.Join(wd, "config.json"))
	}
	return out
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func interactive() bool {
	st, err := os.Stdin.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func updateManager() *update.Manager {
	return &update.Manager{Version: version, StatusPath: filepath.Join(platform.StateDir(), "update.json")}
}

// cmdRun serves the UI and runs the probes; withTray adds the menu bar icon.
func cmdRun(args []string, withTray bool) error {
	name := "serve"
	if withTray {
		name = "tray"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	addr := fs.String("addr", env("BACA_ADDR", "127.0.0.1:7788"), "server address (localhost only)")
	cfgFlag := fs.String("config", "", "config.json (default ~/.config/baca/config.json)")
	file := fs.String("file", os.Getenv("BACA_FILE"), "worklog file (overrides the settings)")
	collect := fs.Bool("collect", true, "collect activity (probes from the settings)")
	open := fs.Bool("open", false, "open the UI in the browser")
	foreground := fs.Bool("foreground", false, "tray: stay attached to the terminal")
	fs.Parse(args)

	if withTray && !*foreground && interactive() {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if err := platform.StartTray(exe); err != nil {
			return err
		}
		fmt.Println("baca runs in the background (menu bar icon). Logs:", platform.StateDir())
		return nil
	}

	host, _, err := net.SplitHostPort(*addr)
	if err != nil || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
		return fmt.Errorf("-addr must be on localhost, got %q", *addr)
	}
	token, err := platform.Token()
	if err != nil {
		return err
	}
	url := "http://" + *addr
	page := url + "/?t=" + token
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		if withTray && errors.Is(err, syscall.EADDRINUSE) {
			// already running: show it instead of starting a second copy
			_ = platform.Open(page)
			return nil
		}
		return err
	}
	path := configPath(*cfgFlag)
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	upd := updateManager()
	upd.Prerelease = func() bool { return false }
	a := &app{path: path, fileFlag: *file, collect: *collect, ctx: ctx,
		srv: &server.Server{Store: worklog.NewStore(""), Web: web.FS, Updates: upd, Version: version, Token: token}}
	a.srv.Settings = a
	a.apply(cfg)
	upd.Prerelease = func() bool { return a.Get().Updates.Prerelease }

	log.Printf("baca %s: %s  file: %s  config: %s", version, url, a.srv.Store.FilePath(), path)
	if *open {
		_ = platform.Open(page)
	}
	srv := &http.Server{Handler: a.srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	go upd.Loop(ctx, 6*time.Hour, func() bool { return a.Get().Updates.CheckOn() })

	shutdown := func() {
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
		a.stopCollector() // the open activity span is written before exit
	}

	if !withTray {
		select {
		case <-ctx.Done():
		case err := <-serveErr:
			if err != nil && err != http.ErrServerClosed {
				return err
			}
		}
		shutdown()
		return nil
	}

	upd.Notify = func(in update.Info) {
		platform.Notify(platform.Notification{Title: "baca " + in.Latest + " is available",
			Message: "You have " + in.Current + ". Choose “⬆ Update” in the menu.", Open: in.URL})
	}
	upd.Restart = func(exe string) error {
		platform.Notify(platform.Notification{Title: "baca updated", Message: "Restarting…"})
		quit, err := platform.Relaunch(exe)
		if err == nil && quit {
			tray.Quit()
		}
		return err
	}
	if from := upd.UpdatedFrom(); from != "" {
		go platform.Notify(platform.Notification{Title: "baca " + version, Message: "Updated from " + from + "."})
	}
	go func() {
		select {
		case <-ctx.Done():
		case <-serveErr:
		}
		tray.Quit()
	}()
	t := &tray.Tray{URL: page, Updates: upd, Today: a.today}
	t.Run() // blocks until Quit
	shutdown()
	return nil
}

func cmdUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	check := fs.Bool("check", false, "only check whether a newer version exists")
	pre := fs.Bool("pre", false, "also consider pre-releases")
	force := fs.Bool("force", false, "install even over a development build")
	fs.Parse(args)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c := &update.Client{Version: version}
	rel, err := c.Latest(ctx, *pre)
	if err != nil {
		return fmt.Errorf("checking %s: %w", update.Repo, err)
	}
	newer, err := update.Newer(version, rel.Tag)
	switch {
	case errors.Is(err, update.ErrDevBuild):
		fmt.Printf("This is a development build (%s); the latest release is %s.\n", version, rel.Tag)
		if *check {
			return nil
		}
		if !*force {
			return errors.New("not replacing a development build — use --force")
		}
	case err != nil:
		return err
	case !newer:
		fmt.Printf("You are on the latest version (%s).\n", version)
		return nil
	default:
		fmt.Printf("%s is available (you have %s): %s\n", rel.Tag, version, rel.URL)
		if *check {
			return nil
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	fmt.Printf("Downloading %s…\n", update.AssetName(rel.Tag, runtime.GOOS, runtime.GOARCH))
	if err := c.Install(ctx, rel, exe); err != nil {
		return err
	}
	fmt.Printf("Installed %s at %s (checksum verified). Restart the icon (Quit → baca).\n", rel.Tag, exe)
	return nil
}

// cmdInstall copies the running binary to ~/.local/bin, moves an older config.json to
// ~/.config/baca and enables start at login.
func cmdInstall() error {
	src, err := os.Executable()
	if err != nil {
		return err
	}
	dst := home("~/.local/bin/baca")
	if src != dst {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if err := update.Replace(dst, b); err != nil {
			return err
		}
	}
	newCfg := home("~/.config/baca/config.json")
	if !exists(newCfg) {
		for _, p := range legacyConfigs() {
			if !exists(p) {
				continue
			}
			if err := copyFile(p, newCfg, 0o600); err != nil {
				return err
			}
			fmt.Println("Config copied:", p, "→", newCfg)
			break
		}
	}
	if err := platform.AutostartInstall(dst); err != nil {
		return err
	}
	fmt.Println("Installed", dst, "— start at login enabled.")
	if runtime.GOOS == "darwin" {
		fmt.Println("Window titles: grant the Accessibility permission to", dst, "(System Settings → Privacy & Security → Accessibility).")
	}
	fmt.Println("Start it now:", dst)
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// cmdOpen opens the running UI with its token.
func cmdOpen(args []string) error {
	fs := flag.NewFlagSet("open", flag.ExitOnError)
	addr := fs.String("addr", env("BACA_ADDR", "127.0.0.1:7788"), "server address")
	fs.Parse(args)
	token, err := platform.Token()
	if err != nil {
		return err
	}
	return platform.Open("http://" + *addr + "/?t=" + token)
}
