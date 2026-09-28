// Package engine is the swing daemon. Every trading day it:
//
//	08:45  PREPARE   wait for the Kite login, reconcile the book with Kite
//	                 holdings and GTTs (catch up a missed evening if needed)
//	09:20  MORNING   sell positions flagged last evening, sell anything that
//	                 gapped through its stop, buy last evening's candidates
//	market MONITOR   poll quotes every minute: mark to market, detect GTT stop
//	                 fills, re-arm any missing stop
//	15:50  EVENING   today's candle is final: ratchet stops (modify GTTs),
//	                 flag exits, scan the universe for tomorrow
//
// State lives in data/portfolio.json, so the engine can be stopped and
// restarted at any time; positions are protected by GTTs at the broker.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"

	"github.com/nkalva/kitealgo/internal/auth"
	"github.com/nkalva/kitealgo/internal/broker"
	"github.com/nkalva/kitealgo/internal/clock"
	"github.com/nkalva/kitealgo/internal/config"
	"github.com/nkalva/kitealgo/internal/data"
	"github.com/nkalva/kitealgo/internal/ordermanager"
	"github.com/nkalva/kitealgo/internal/swing"
)

// Engine is the long-running daemon.
type Engine struct {
	cfg     config.Config
	log     *slog.Logger
	clk     clock.Clock
	sess    *clock.Session
	auth    *auth.Manager
	httpc   *http.Client
	store   *swing.Store
	journal *swing.Journal
	strat   *swing.Strategy
	data    *data.Store
	paper   *broker.Paper // paper mode only; persists for the process lifetime

	mu          sync.RWMutex
	stage       string
	stageDetail string
	stageAt     time.Time
	lastErr     string
	retry       chan struct{}
	day         *dayCtx
	bt          BacktestStatus
}

// New wires the daemon.
func New(cfg config.Config, log *slog.Logger) (*Engine, error) {
	sess, err := clock.NewSession(cfg.Session, cfg.Holidays)
	if err != nil {
		return nil, err
	}
	httpc, err := broker.NewStaticIPClient(cfg.Network.BindIP, cfg.Network.RequestTimeout)
	if err != nil {
		return nil, err
	}
	clk := clock.System{}
	am := auth.NewManager(cfg.Kite.APIKey, cfg.Kite.APISecret, cfg.Paths.DataDir, cfg.Server.PublicURL, httpc, clk, log.With("component", "auth"))
	am.SetRoots(cfg.Dev.APIRoot, cfg.Dev.LoginRoot)
	if cfg.Kite.AccessToken != "" {
		am.Pin(cfg.Kite.AccessToken)
	}
	st, err := swing.OpenStore(filepath.Join(cfg.Paths.DataDir, "portfolio.json"), cfg.Risk.Capital)
	if err != nil {
		return nil, fmt.Errorf("portfolio state: %w", err)
	}
	if _, err := os.Stat(cfg.Paths.UniverseFile); errors.Is(err, os.ErrNotExist) {
		_ = os.WriteFile(cfg.Paths.UniverseFile, []byte(data.DefaultUniverse), 0o644)
	}
	e := &Engine{cfg: cfg, log: log, clk: clk, sess: sess, auth: am, httpc: httpc, store: st,
		journal: swing.NewJournal(cfg.Paths.JournalDir), strat: swing.NewStrategy(cfg.Strategy, cfg.Costs),
		data: data.NewStore(cfg.Paths.DataDir, nil, log), retry: make(chan struct{}, 1), stage: "starting", stageAt: clk.Now()}
	am.SetOnLogin(e.Retry) // wake an aborted/sleeping day as soon as you log in
	if cfg.Mode == "paper" {
		e.paper = broker.NewPaper(0, clk.Now)
		e.paper.SlippagePct = cfg.Costs.SlippagePct
		e.restorePaper()
	}
	return e, nil
}

// restorePaper rebuilds the simulated account from the persisted book.
func (e *Engine) restorePaper() {
	s := e.store.Snapshot()
	e.paper.SetCash(s.Cash)
	for _, p := range s.Positions {
		e.paper.Seed(p.InstrumentToken, p.Symbol, p.Quantity, p.EntryPrice)
		if p.GTTID != "" {
			e.paper.RestoreGTT(p.GTTID, e.gttRequest(p, p.Stop))
		}
	}
}

// Accessors for the web UI.
func (e *Engine) Auth() *auth.Manager     { return e.auth }
func (e *Engine) Config() config.Config   { return e.cfg }
func (e *Engine) Session() *clock.Session { return e.sess }
func (e *Engine) Store() *swing.Store     { return e.store }
func (e *Engine) Journal() *swing.Journal { return e.journal }
func (e *Engine) Data() *data.Store       { return e.data }
func (e *Engine) Clock() clock.Clock      { return e.clk }

func (e *Engine) setStage(stage, detail string) {
	e.mu.Lock()
	e.stage, e.stageDetail, e.stageAt = stage, detail, e.clk.Now()
	e.mu.Unlock()
}

func (e *Engine) setErr(err error) {
	e.mu.Lock()
	if err == nil {
		e.lastErr = ""
	} else {
		e.lastErr = err.Error()
	}
	e.mu.Unlock()
}

// State is the lifecycle snapshot for the UI.
type State struct {
	Stage     string    `json:"stage"`
	Detail    string    `json:"detail"`
	Since     time.Time `json:"since"`
	LastError string    `json:"last_error,omitempty"`
	Next      []Event   `json:"next"`
	Connected bool      `json:"connected"` // a Kite session is active today
}

// Event is an upcoming scheduled step.
type Event struct {
	Name string    `json:"name"`
	At   time.Time `json:"at"`
}

// State returns a snapshot.
func (e *Engine) State() State {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return State{Stage: e.stage, Detail: e.stageDetail, Since: e.stageAt, LastError: e.lastErr,
		Next: e.upcoming(), Connected: e.day != nil}
}

func (e *Engine) upcoming() []Event {
	now := e.clk.Now()
	day := clock.Midnight(now)
	if !e.sess.IsTradingDay(now) || !now.Before(e.sess.At(now, e.sess.EveningRun)) {
		day = e.sess.NextTradingDay(now)
	}
	var out []Event
	for _, ev := range []Event{{"Prepare & login check", e.sess.At(day, e.sess.PrepareAt)},
		{"Morning run (exits, entries)", e.sess.At(day, e.sess.MorningRun)},
		{"Evening run (stops, scan)", e.sess.At(day, e.sess.EveningRun)}} {
		if ev.At.After(now) {
			out = append(out, ev)
		}
	}
	return out
}

// Retry wakes the engine from an aborted day.
func (e *Engine) Retry() {
	select {
	case e.retry <- struct{}{}:
	default:
	}
}

func (e *Engine) sleepUntil(ctx context.Context, t time.Time) (retried bool, err error) {
	d := time.Until(t)
	if d <= 0 {
		return false, ctx.Err()
	}
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-tm.C:
		return false, nil
	case <-e.retry:
		return true, nil
	}
}

// Run loops over trading days until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) error {
	e.log.Info("swing engine up", "mode", e.cfg.Mode, "ui", e.cfg.Server.PublicURL)
	for {
		now := e.clk.Now()
		today := clock.Midnight(now)
		var lastEvening string
		e.store.Read(func(s *swing.State) { lastEvening = s.LastEvening })
		doneToday := lastEvening == swing.DateKey(today)
		if !e.sess.IsTradingDay(now) || (doneToday && !now.Before(e.sess.At(now, e.sess.EveningRun))) {
			next := e.sess.At(e.sess.NextTradingDay(now), e.sess.PrepareAt)
			if e.sess.IsTradingDay(now) && now.Before(e.sess.At(now, e.sess.PrepareAt)) {
				next = e.sess.At(now, e.sess.PrepareAt)
			}
			e.setStage("sleeping", "Next session prepares at "+next.Format("Mon 02 Jan 15:04"))
			if _, err := e.sleepUntil(ctx, next); err != nil {
				return nil
			}
			continue
		}
		if prep := e.sess.At(now, e.sess.PrepareAt); now.Before(prep) {
			e.setStage("sleeping", "Preparation starts at "+prep.Format("15:04")+". You can log in to Kite any time after 06:00.")
			if _, err := e.sleepUntil(ctx, prep); err != nil {
				return nil
			}
		}
		e.setErr(nil)
		err := e.runDay(ctx, today)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			e.setErr(err)
			e.setStage("aborted", "Today's run stopped: fix the problem shown and press Retry.")
			e.log.Error("trading day aborted", "err", err)
			retried, serr := e.sleepUntil(ctx, e.sess.At(e.sess.NextTradingDay(today), e.sess.PrepareAt))
			if serr != nil {
				return nil
			}
			if retried {
				e.log.Info("retry requested from UI")
			}
		}
	}
}

// dayCtx is everything bound to today's Kite session.
type dayCtx struct {
	day    time.Time
	kite   *broker.Kite
	trader broker.Trader
	om     *ordermanager.OrderManager
	stop   context.CancelFunc
}

func (e *Engine) runDay(parent context.Context, today time.Time) error {
	// 1. Login.
	e.setStage("waiting_login", "Log in to Kite to start today's run (tokens expire daily at 06:00).")
	// Wait for a login until shortly before the next session prepares: a
	// login late in the evening still runs today's evening scan.
	loginCtx, cancel := context.WithDeadline(parent, e.sess.At(e.sess.NextTradingDay(today), e.sess.PrepareAt).Add(-time.Minute))
	defer cancel()
	var kite *broker.Kite
	for {
		tok, err := e.auth.WaitForToken(loginCtx)
		if err != nil {
			return fmt.Errorf("no Kite login today: %w", err)
		}
		kite = broker.NewKite(e.auth.APIKey(), tok.AccessToken, e.httpc, e.cfg.Orders.MarketProtection, e.cfg.Dev.APIRoot)
		prof, err := kite.Client().GetUserProfile()
		if err == nil {
			e.log.Info("kite session valid", "user", prof.UserID)
			break
		}
		var ke kiteconnect.Error
		if errors.As(err, &ke) && ke.ErrorType == kiteconnect.TokenError {
			e.log.Warn("stored token rejected — log in again", "err", err)
			e.auth.Invalidate()
			continue
		}
		return fmt.Errorf("kite profile check: %w", err)
	}
	e.data.SetMarketData(kite)

	// 2. Order plumbing for the day.
	var trader broker.Trader = kite
	if e.paper != nil {
		trader = e.paper
	}
	dctx, stop := context.WithCancel(context.Background())
	defer stop()
	om := ordermanager.New(trader, e.cfg.Orders, e.log, nil)
	go om.Run(dctx)
	d := &dayCtx{day: today, kite: kite, trader: trader, om: om, stop: stop}
	e.mu.Lock()
	e.day = d
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.day = nil
		e.mu.Unlock()
	}()

	// 3. Prepare: reconcile, catch up a missed evening.
	e.setStage("preparing", "Reconciling positions with Kite.")
	if err := e.reconcile(parent, d); err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	var lastEvening, lastMorning string
	e.store.Read(func(s *swing.State) { lastEvening, lastMorning = s.LastEvening, s.LastMorning })
	prev := e.sess.PrevTradingDay(today)
	if lastEvening < swing.DateKey(prev) && e.clk.Now().Before(e.sess.At(today, e.sess.MorningRun).Add(5*time.Hour)) {
		e.log.Warn("previous evening run was missed — catching up now", "for", swing.DateKey(prev))
		e.setStage("evening", "Catching up the missed evening run for "+prev.Format("02 Jan"))
		if err := e.evening(parent, d, prev); err != nil {
			return fmt.Errorf("catch-up evening: %w", err)
		}
	}

	// 4. Morning run.
	if lastMorning != swing.DateKey(today) && e.clk.Now().Before(e.sess.At(today, e.sess.MarketClose)) {
		if at := e.sess.At(today, e.sess.MorningRun); e.clk.Now().Before(at) {
			e.setStage("waiting_morning", "Morning run at "+at.Format("15:04"))
			if _, err := e.sleepUntil(parent, at); err != nil {
				return err
			}
		}
		e.setStage("morning", "Checking gaps, selling flagged positions, buying candidates.")
		if err := e.morning(parent, d); err != nil {
			return fmt.Errorf("morning run: %w", err)
		}
	}

	// 5. Monitor until the close.
	if e.clk.Now().Before(e.sess.At(today, e.sess.MarketClose)) {
		e.setStage("monitoring", "Market open: stops rest at the broker as GTTs; prices refresh every minute.")
		e.monitor(parent, d, e.sess.At(today, e.sess.MarketClose))
		if parent.Err() != nil {
			return parent.Err()
		}
	}

	// 6. Evening run.
	if at := e.sess.At(today, e.sess.EveningRun); e.clk.Now().Before(at) {
		e.setStage("waiting_evening", "Evening run at "+at.Format("15:04")+" (after today's candle is final).")
		if _, err := e.sleepUntil(parent, at); err != nil {
			return err
		}
	}
	e.store.Read(func(s *swing.State) { lastEvening = s.LastEvening })
	if lastEvening != swing.DateKey(today) {
		e.setStage("evening", "Updating stops and scanning the universe.")
		if err := e.evening(parent, d, today); err != nil {
			return fmt.Errorf("evening run: %w", err)
		}
	}
	e.setStage("closed", "Day complete. Tomorrow's candidates are queued.")
	return nil
}
