// Command engine runs the intraday trading daemon.
//
//	engine -config config.yaml           run (normally under systemd)
//	engine -config config.yaml -check    validate config + watchlist, then exit
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nkalva/kitealgo/internal/clock"
	"github.com/nkalva/kitealgo/internal/config"
	"github.com/nkalva/kitealgo/internal/engine"
	"github.com/nkalva/kitealgo/internal/watchlist"
)

var version = "dev"

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config file")
	check := flag.Bool("check", false, "validate config and the next watchlist, then exit")
	debug := flag.Bool("debug", false, "debug logging")
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:\n"+err.Error())
		os.Exit(2)
	}

	log, closeLog, err := newLogger(cfg.Paths.LogFile, *debug)
	if err != nil {
		fmt.Fprintln(os.Stderr, "log setup:", err)
		os.Exit(2)
	}
	defer closeLog()

	if *check {
		os.Exit(runCheck(cfg))
	}

	for _, dir := range []string{cfg.Paths.DataDir, cfg.Paths.JournalDir, cfg.Paths.WatchlistDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Error("cannot create directory", "dir", dir, "err", err)
			os.Exit(1)
		}
	}

	eng, err := engine.New(cfg, log)
	if err != nil {
		log.Error("engine init failed", "err", err)
		os.Exit(1)
	}

	// First SIGINT/SIGTERM: graceful — flatten open positions, sweep, exit.
	// Second signal: immediate exit (broker-side stops remain in place).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop() // restore default handling so a second signal kills the process
		log.Warn("signal received — graceful shutdown (send again to force)")
		time.Sleep(60 * time.Second)
		log.Error("graceful shutdown timed out — forcing exit")
		os.Exit(1)
	}()

	if err := eng.Run(ctx); err != nil {
		log.Error("engine stopped with error", "err", err)
		os.Exit(1)
	}
	log.Info("engine stopped cleanly")
}

func newLogger(file string, debug bool) (*slog.Logger, func(), error) {
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
	return slog.New(h), closer, nil
}

func runCheck(cfg config.Config) int {
	sess, err := clock.NewSession(cfg.Session, cfg.Holidays)
	if err != nil {
		fmt.Println("session:", err)
		return 1
	}
	now := clock.System{}.Now()
	day := clock.Midnight(now)
	if !sess.IsTradingDay(now) || !now.Before(sess.At(now, sess.SquareOff)) {
		day = sess.NextTradingDay(now)
	}
	fmt.Printf("config OK (mode=%s, bind_ip=%q)\n", cfg.Mode, cfg.Network.BindIP)
	fmt.Printf("next trading day: %s — expecting %s\n", day.Format("Mon 2006-01-02"), watchlist.PathFor(cfg.Paths.WatchlistDir, day))
	entries, warns, err := watchlist.Load(cfg.Paths.WatchlistDir, day)
	for _, w := range warns {
		fmt.Println("  warning:", w)
	}
	if err != nil {
		fmt.Println("watchlist:", err)
		return 1
	}
	fmt.Printf("watchlist OK: %d symbols\n", len(entries))
	for _, e := range entries {
		if e.Benchmark != "" {
			fmt.Printf("  %-14s benchmark=%s\n", e.Symbol, e.Benchmark)
		} else {
			fmt.Printf("  %s\n", e.Symbol)
		}
	}
	return 0
}
