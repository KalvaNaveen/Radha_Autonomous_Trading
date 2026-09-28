// Command engine runs the swing trading daemon and its local control panel.
//
//	engine                                 run with ./config.yaml (created on first run)
//	engine -config path/to/config.yaml     run with a specific config
//	engine -check                          validate config + universe, then exit
//
// Open http://127.0.0.1:8080 for the control panel (it opens automatically).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/nkalva/kitealgo/internal/clock"
	"github.com/nkalva/kitealgo/internal/config"
	"github.com/nkalva/kitealgo/internal/data"
	"github.com/nkalva/kitealgo/internal/engine"
	"github.com/nkalva/kitealgo/internal/web"
)

var version = "dev"

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config file (created with paper-mode defaults if missing)")
	check := flag.Bool("check", false, "validate config and the universe, then exit")
	debug := flag.Bool("debug", false, "debug logging")
	noBrowser := flag.Bool("no-browser", false, "do not open the control panel in a browser")
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	created, err := config.WriteDefault(*cfgPath)
	if err != nil {
		fatal("cannot create default config: %v", err)
	}
	if created {
		fmt.Printf("Created %s with paper-mode defaults.\n", *cfgPath)
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal("config error in %s:\n%v", *cfgPath, err)
	}

	ring := web.NewLogRing(2000)
	log, closeLog, err := newLogger(cfg.Paths.LogFile, *debug, ring)
	if err != nil {
		fatal("log setup: %v", err)
	}
	defer closeLog()

	if *check {
		os.Exit(runCheck(cfg))
	}

	for _, dir := range []string{cfg.Paths.DataDir, cfg.Paths.JournalDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal("cannot create directory %s: %v", dir, err)
		}
	}

	eng, err := engine.New(cfg, log)
	if err != nil {
		fatal("engine init failed: %v", err)
	}

	// Fail fast (with a clear message) if the control-panel port is taken.
	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		fatal("cannot listen on %s (%v).\nAnother copy of the engine may already be running, or another program uses this port.\nClose it, or change server.listen and server.public_url in %s.", cfg.Server.Listen, err, *cfgPath)
	}
	_ = ln.Close()

	// First Ctrl+C / SIGTERM: graceful — flatten open positions, sweep, exit.
	// Second signal: immediate exit (broker-side stops remain in place).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop()
		log.Warn("shutdown requested — closing positions before exit (press Ctrl+C again to force)")
		time.Sleep(60 * time.Second)
		log.Error("graceful shutdown timed out — forcing exit")
		os.Exit(1)
	}()

	srv := web.New(eng, ring, log, version)
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Serve(ctx, cfg.Server.Listen) }()

	fmt.Printf("\n  Radha control panel:  %s\n  Mode: %s   (Ctrl+C to stop)\n\n", cfg.Server.PublicURL, cfg.Mode)
	if !*noBrowser {
		go func() {
			time.Sleep(700 * time.Millisecond)
			openBrowser(cfg.Server.PublicURL)
		}()
	}

	engErr := make(chan error, 1)
	go func() { engErr <- eng.Run(ctx) }()
	select {
	case err := <-srvErr:
		if err != nil {
			log.Error("control panel stopped", "err", err)
			os.Exit(1)
		}
	case err := <-engErr:
		if err != nil {
			log.Error("engine stopped with error", "err", err)
			os.Exit(1)
		}
	}
	log.Info("engine stopped cleanly")
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "\nERROR: "+format+"\n\n", a...)
	if runtime.GOOS == "windows" {
		// Keep a double-clicked console window open long enough to read.
		fmt.Fprintln(os.Stderr, "Press Enter to close.")
		_, _ = fmt.Scanln()
	}
	os.Exit(2)
}

func openBrowser(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	case "darwin":
		cmd = exec.Command("open", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	_ = cmd.Start()
}

func newLogger(file string, debug bool, ring *web.LogRing) (*slog.Logger, func(), error) {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	var w io.Writer = os.Stdout
	closer := func() {}
	if file != "" {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return nil, nil, err
		}
		f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, nil, err
		}
		w = io.MultiWriter(os.Stdout, f)
		closer = func() { _ = f.Close() }
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level, ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			a.Value = slog.StringValue(a.Value.Time().In(clock.IST).Format("2006-01-02T15:04:05.000Z07:00"))
		}
		return a
	}})
	return slog.New(ring.Handler(h, slog.LevelInfo)), closer, nil
}

func runCheck(cfg config.Config) int {
	if _, err := clock.NewSession(cfg.Session, cfg.Holidays); err != nil {
		fmt.Println("session:", err)
		return 1
	}
	fmt.Printf("config OK (mode=%s, bind_ip=%q)\n", cfg.Mode, cfg.Network.BindIP)
	syms, warns, err := data.LoadUniverse(cfg.Paths.UniverseFile)
	for _, w := range warns {
		fmt.Println("  warning:", w)
	}
	if err != nil {
		fmt.Println("universe:", err)
		return 1
	}
	fmt.Printf("universe OK: %d symbols\n", len(syms))
	return 0
}
