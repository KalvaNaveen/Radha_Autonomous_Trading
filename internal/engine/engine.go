// Package engine is the daemon: it sleeps until each trading morning, waits
// for the day's Kite login, loads the watchlist you dropped the evening before,
// runs the session, enforces the square-off, verifies the account is flat,
// and goes back to sleep. It runs indefinitely under systemd.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"

	"github.com/nkalva/kitealgo/internal/agent"
	"github.com/nkalva/kitealgo/internal/auth"
	"github.com/nkalva/kitealgo/internal/broker"
	"github.com/nkalva/kitealgo/internal/clock"
	"github.com/nkalva/kitealgo/internal/config"
	"github.com/nkalva/kitealgo/internal/history"
	"github.com/nkalva/kitealgo/internal/ordermanager"
	"github.com/nkalva/kitealgo/internal/radar"
	"github.com/nkalva/kitealgo/internal/risk"
	"github.com/nkalva/kitealgo/internal/ticker"
	"github.com/nkalva/kitealgo/internal/watchlist"
	"github.com/nkalva/kitealgo/pkg/models"
)

// Engine is the long-running daemon.
type Engine struct {
	cfg   config.Config
	log   *slog.Logger
	clk   clock.Clock
	sess  *clock.Session
	auth  *auth.Manager
	httpc *http.Client

	mu          sync.RWMutex
	status      func() any
	stage       string
	stageDetail string
	stageAt     time.Time
	lastErr     string
	activeDay   string
	retry       chan struct{}
}

// State is the engine lifecycle snapshot shown in the UI.
type State struct {
	Stage       string    `json:"stage"`
	Detail      string    `json:"detail"`
	Since       time.Time `json:"since"`
	LastError   string    `json:"last_error,omitempty"`
	Day         string    `json:"day,omitempty"`
	NextSession time.Time `json:"next_session"`
	TargetDay   string    `json:"target_day"`
	Session     any       `json:"session,omitempty"`
}

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

// State returns a snapshot for the UI.
func (e *Engine) State() State {
	e.mu.RLock()
	st := State{Stage: e.stage, Detail: e.stageDetail, Since: e.stageAt, LastError: e.lastErr, Day: e.activeDay,
		NextSession: e.nextSessionStart(), TargetDay: e.TargetDay().Format("2006-01-02")}
	f := e.status
	e.mu.RUnlock()
	if f != nil {
		st.Session = f()
	}
	return st
}

// TargetDay is the trading day the next watchlist is for: today until the
// square-off, otherwise the next trading day.
func (e *Engine) TargetDay() time.Time {
	now := e.clk.Now()
	if e.sess.IsTradingDay(now) && now.Before(e.sess.At(now, e.sess.SquareOff)) {
		return clock.Midnight(now)
	}
	return e.sess.NextTradingDay(now)
}

// Auth exposes the login manager (for the web UI).
func (e *Engine) Auth() *auth.Manager { return e.auth }

// Config returns the engine configuration.
func (e *Engine) Config() config.Config { return e.cfg }

// Session returns the trading calendar.
func (e *Engine) Session() *clock.Session { return e.sess }

// WatchlistChanged wakes a session that is waiting for today's watchlist.
func (e *Engine) WatchlistChanged() {
	e.mu.RLock()
	waiting := e.stage == "waiting_watchlist" || e.stage == "aborted"
	e.mu.RUnlock()
	if waiting {
		e.Retry()
	}
}

// Retry wakes the engine if it is sleeping after an aborted day, so today's
// session is attempted again (e.g. after fixing credentials or the watchlist).
func (e *Engine) Retry() {
	select {
	case e.retry <- struct{}{}:
	default:
	}
}

func (e *Engine) sleepOrRetry(ctx context.Context, t time.Time) (retried bool, err error) {
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
	e := &Engine{cfg: cfg, log: log, clk: clk, sess: sess, auth: am, httpc: httpc, retry: make(chan struct{}, 1),
		stage: "starting", stageAt: clk.Now()}
	am.SetStatusProvider(func() any {
		e.mu.RLock()
		f := e.status
		e.mu.RUnlock()
		if f == nil {
			return map[string]any{"state": "idle", "mode": cfg.Mode, "next_session": e.nextSessionStart().Format(time.RFC3339)}
		}
		return f()
	})
	return e, nil
}

func (e *Engine) setStatus(f func() any) {
	e.mu.Lock()
	e.status = f
	e.mu.Unlock()
}

func (e *Engine) nextSessionStart() time.Time {
	now := e.clk.Now()
	if e.sess.IsTradingDay(now) && now.Before(e.sess.At(now, e.sess.SquareOff)) {
		return e.sess.At(now, e.sess.PrepareAt)
	}
	return e.sess.At(e.sess.NextTradingDay(now), e.sess.PrepareAt)
}

func sleepUntil(ctx context.Context, t time.Time) error {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err()
	}
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-tm.C:
		return nil
	}
}

// Run loops over trading days until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) error {
	e.log.Info("engine up", "mode", e.cfg.Mode, "ui", e.cfg.Server.PublicURL, "bind_ip", e.cfg.Network.BindIP)

	for {
		now := e.clk.Now()
		if !e.sess.IsTradingDay(now) || !now.Before(e.sess.At(now, e.sess.SessionEnd)) {
			next := e.nextSessionStart()
			e.setStage("sleeping", "Market closed. Next session prepares at "+next.Format("Mon 02 Jan 15:04"))
			e.log.Info("sleeping until next session", "wake", next.Format(time.RFC1123))
			if _, err := e.sleepOrRetry(ctx, next); err != nil {
				return nil
			}
			continue
		}
		if prep := e.sess.At(now, e.sess.PrepareAt); now.Before(prep) {
			e.setStage("waiting_prepare", "Preparation starts at "+prep.Format("15:04")+". You can log in to Kite any time after 06:00.")
			e.log.Info("waiting for preparation time", "at", prep.Format("15:04:05"))
			if _, err := e.sleepOrRetry(ctx, prep); err != nil {
				return nil
			}
		}
		day := clock.Midnight(e.clk.Now())
		e.mu.Lock()
		e.activeDay = day.Format("2006-01-02")
		e.mu.Unlock()
		e.setErr(nil)
		err := e.runDay(ctx, day)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			e.setErr(err)
			e.setStage("aborted", "Today's session stopped: fix the problem shown and press Retry.")
			e.log.Error("trading day aborted", "day", day.Format("2006-01-02"), "err", err)
		} else {
			e.setStage("closed", "Session finished. Journal written.")
		}
		// Don't re-run a finished day; an aborted one can be retried from the UI.
		retried, serr := e.sleepOrRetry(ctx, e.sess.At(day, e.sess.SessionEnd))
		if serr != nil {
			return nil
		}
		if retried {
			e.log.Info("retry requested from UI")
		}
	}
}

// ---------------------------------------------------------------------------
// One trading day
// ---------------------------------------------------------------------------

type dayRun struct {
	e      *Engine
	day    time.Time
	log    *slog.Logger
	trader broker.Trader
	om     *ordermanager.OrderManager
	risk   *risk.Manager
	mux    *ticker.Multiplexer
	router *ordermanager.UpdateRouter
	agents []*agent.Agent
	tokens map[uint32]broker.Instrument

	killOnce sync.Once
}

func (e *Engine) runDay(parent context.Context, day time.Time) error {
	log := e.log.With("day", day.Format("2006-01-02"))

	var err error
	select { // discard a stale retry request
	case <-e.retry:
	default:
	}
	// 1. Today's Kite session (one browser login per day).
	e.setStage("waiting_login", "Log in to Kite to start today's session.")
	loginCtx, cancelLogin := context.WithDeadline(parent, e.sess.At(day, e.sess.SquareOff))
	defer cancelLogin()
	var kite *broker.Kite
	for {
		tok, err := e.auth.WaitForToken(loginCtx)
		if err != nil {
			return fmt.Errorf("no Kite login before square-off: %w", err)
		}
		kite = broker.NewKite(e.auth.APIKey(), tok.AccessToken, e.httpc, e.cfg.Orders.MarketProtection, e.cfg.Dev.APIRoot)
		prof, err := kite.Client().GetUserProfile()
		if err == nil {
			log.Info("kite session valid", "user", prof.UserID)
			break
		}
		var ke kiteconnect.Error
		if errors.As(err, &ke) && ke.ErrorType == kiteconnect.TokenError {
			e.setStage("waiting_login", "The saved Kite token was rejected — log in again.")
			log.Warn("stored token rejected — login again", "err", err)
			e.auth.Invalidate()
			continue
		}
		return fmt.Errorf("kite profile check: %w", err)
	}

	// 2. Watchlist (dropped the previous evening).
	e.setStage("loading_watchlist", "Reading today's watchlist.")
	//    If it is missing, keep checking every minute until the entry cutoff so
	//    a late drop still trades the rest of the day.
	var entries []watchlist.Entry
	for {
		var warns []string
		entries, warns, err = watchlist.Load(e.cfg.Paths.WatchlistDir, day)
		for _, w := range warns {
			log.Warn("watchlist", "note", w)
		}
		if err == nil {
			break
		}
		if !errors.Is(err, watchlist.ErrNoWatchlist) || !e.clk.Now().Before(e.sess.At(day, e.sess.EntryCutoff)) {
			return err
		}
		e.setStage("waiting_watchlist", "No watchlist for today yet — add symbols in the Watchlist panel.")
		log.Error("no watchlist for today yet — retrying every minute", "expected", watchlist.PathFor(e.cfg.Paths.WatchlistDir, day))
		if _, err := e.sleepOrRetry(parent, e.clk.Now().Add(time.Minute)); err != nil {
			return err
		}
	}

	// 3. Instruments + resolution.
	prepCtx, cancelPrep := context.WithTimeout(parent, 10*time.Minute)
	defer cancelPrep()
	e.setStage("loading_instruments", "Downloading the NSE instrument list.")
	nse, err := e.instruments(prepCtx, kite, day)
	if err != nil {
		return fmt.Errorf("instruments: %w", err)
	}
	resolved, rwarns := watchlist.Resolve(entries, nse)
	for _, w := range rwarns {
		log.Warn("watchlist", "note", w)
	}
	if len(resolved) == 0 {
		return errors.New("watchlist: no tradable symbols after resolution")
	}
	market, marketOK := watchlist.FindIndex(nse, e.cfg.Radar.MarketIndex)
	radarMode := radar.Mode(e.cfg.Radar.Mode)
	if !marketOK && radarMode != radar.ModeOff {
		log.Warn("market index not found — radar disabled", "index", e.cfg.Radar.MarketIndex)
		radarMode = radar.ModeOff
	}

	// 4. History: RVOL profiles + EMA seeds (cached per day).
	var histTokens []uint32
	indexSet := map[uint32]broker.Instrument{}
	if marketOK {
		indexSet[market.InstrumentToken] = market
	}
	for _, r := range resolved {
		histTokens = append(histTokens, r.Instrument.InstrumentToken)
		if r.Benchmark != nil {
			indexSet[r.Benchmark.InstrumentToken] = *r.Benchmark
		}
	}
	for t := range indexSet {
		histTokens = append(histTokens, t)
	}
	e.setStage("loading_history", fmt.Sprintf("Loading 10-day history for %d instruments.", len(histTokens)))
	log.Info("loading history", "instruments", len(histTokens))
	hist, err := history.NewLoader(kite, e.cfg.Paths.DataDir, e.cfg.Strategy.RVOLLookbackDays, log).
		Load(prepCtx, day, histTokens, e.cfg.Strategy.CandleInterval)
	if err != nil {
		return fmt.Errorf("history: %w", err)
	}

	// 5. Trader: live Kite or paper simulator on the live feed.
	mux := ticker.New(log)
	var trader broker.Trader = kite
	var paper *broker.Paper
	if e.cfg.Mode == "paper" {
		paper = broker.NewPaper(e.cfg.Risk.Capital, e.clk.Now)
		trader = paper
		mux.AddTap(paper.OnTick)
		log.Warn("PAPER MODE — orders are simulated, market data is live")
	}

	d := &dayRun{e: e, day: day, log: log, trader: trader, mux: mux, tokens: map[uint32]broker.Instrument{}}
	d.router = ordermanager.NewUpdateRouter(log, func(u models.OrderUpdate) {
		log.Warn("update for a tagged order with no agent today", "order", u.OrderID, "symbol", u.TradingSymbol, "status", u.Status)
	})
	if paper != nil {
		paper.SetUpdateHandler(d.router.Dispatch)
	} else {
		mux.OnOrderUpdate(d.router.Dispatch)
	}
	d.om = ordermanager.New(trader, e.cfg.Orders, log, nil)

	journal, err := risk.NewJournal(e.cfg.Paths.JournalDir, day)
	if err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	d.risk = risk.NewManager(e.cfg.Risk, e.cfg.Strategy.RoundTripCostPct, journal, log, func(reason string) {
		d.killAll(reason)
	})

	open := e.sess.OpenTime(day)
	var specs []radar.Spec
	for t, in := range indexSet {
		specs = append(specs, radar.Spec{Token: t, Name: in.TradingSymbol, Seed: hist[t].Seed5m})
	}
	rd := radar.New(radarMode, e.cfg.Radar.NeutralPct, market.InstrumentToken, specs,
		e.cfg.Strategy.FastEMA, e.cfg.Strategy.SlowEMA, e.cfg.Strategy.CandleInterval, e.cfg.Strategy.CandleGrace, open, log)

	for _, r := range resolved {
		in := r.Instrument
		var bench uint32
		if r.Benchmark != nil {
			bench = r.Benchmark.InstrumentToken
		}
		h := hist[in.InstrumentToken]
		a := agent.New(agent.Params{
			Instrument: in, Benchmark: bench, Day: day, Profile: h.Profile, Seed: h.Seed5m,
			Strategy: e.cfg.Strategy, Orders: e.cfg.Orders,
		}, agent.Deps{OM: d.om, Querier: trader, Risk: d.risk, Radar: rd, Session: e.sess, Clock: e.clk, Log: log},
			mux.Subscribe(in.InstrumentToken, e.cfg.Ticker.AgentBuffer))
		d.router.Register(in.InstrumentToken, a.Updates())
		d.agents = append(d.agents, a)
		d.tokens[in.InstrumentToken] = in
	}
	for t := range indexSet {
		mux.Subscribe(t, 64)
	}
	log.Info("session prepared", "agents", len(d.agents), "indices", len(indexSet), "radar", radarMode)

	return d.run(parent, kite, rd)
}

func (d *dayRun) run(parent context.Context, kite *broker.Kite, rd *radar.Radar) error {
	e := d.e
	// The day context is detached from the parent: on SIGTERM we still want to
	// finish flattening before tearing the order manager down.
	dayCtx, cancelDay := context.WithCancel(context.Background())
	defer cancelDay()

	var wg sync.WaitGroup
	start := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}
	tok, _ := e.auth.Current()
	start(func() { d.om.Run(dayCtx) })
	start(func() {
		d.mux.Run(dayCtx, e.auth.APIKey(), tok.AccessToken, e.cfg.Dev.TickerURL, e.cfg.Ticker.ReconnectMaxDelay)
	})
	start(func() {
		ordermanager.NewReconciler(d.trader, d.router, e.cfg.Orders.ReconcileInterval, d.log).Run(dayCtx)
	})
	start(func() { d.risk.RunMarginRefresh(dayCtx, d.trader) })
	start(func() {
		rd.Run(dayCtx, func(t uint32) <-chan models.Tick { return d.mux.Subscribe(t, 64) })
	})
	for _, a := range d.agents {
		a := a
		start(func() { a.Run(dayCtx) })
	}
	e.setStatus(d.statusFn)
	defer e.setStatus(nil)
	e.setStage("running", fmt.Sprintf("Session live with %d agents.", len(d.agents)))

	squareOff := e.sess.At(d.day, e.sess.SquareOff)
	sweepAt := e.sess.At(d.day, e.sess.SafetySweep)
	endAt := e.sess.At(d.day, e.sess.SessionEnd)
	if !e.clk.Now().Before(squareOff) {
		// Started late (crash/restart after square-off): flatten immediately.
		d.killAll("started after square-off")
	}

	watch := time.NewTicker(5 * time.Second)
	defer watch.Stop()
	sqTimer := time.NewTimer(time.Until(squareOff))
	swTimer := time.NewTimer(time.Until(sweepAt))
	endTimer := time.NewTimer(time.Until(endAt))
	defer sqTimer.Stop()
	defer swTimer.Stop()
	defer endTimer.Stop()
	staleWarned := false

loop:
	for {
		select {
		case <-parent.Done():
			d.log.Warn("shutdown requested — flattening before exit")
			d.killAll("engine shutdown")
			d.waitFlat(20 * time.Second)
			d.sweep(context.Background())
			break loop
		case <-sqTimer.C:
			d.log.Warn("SQUARE-OFF: kill switch firing", "at", squareOff.Format("15:04:05"))
			d.killAll("square-off " + squareOff.Format("15:04:05"))
		case <-swTimer.C:
			d.waitFlat(10 * time.Second)
			d.sweep(dayCtx)
		case <-endTimer.C:
			break loop
		case now := <-watch.C:
			ph := e.sess.PhaseAt(now)
			if ph == clock.PhaseWarmup || ph == clock.PhaseActive || ph == clock.PhaseExitOnly {
				if age := d.mux.LastTickAge(now); age > e.cfg.Ticker.StaleAfter && now.Sub(e.sess.OpenTime(now)) > time.Minute {
					if !staleWarned {
						d.log.Error("MARKET DATA STALE", "last_tick_age", age.Round(time.Second), "ws_connected", d.mux.Connected())
						staleWarned = true
					}
				} else {
					staleWarned = false
				}
			}
		}
	}
	cancelDay()
	wg.Wait()
	sum := d.risk.Summary()
	d.log.Info("session closed", "trades", sum.Trades, "realized", sum.Realized, "orders", d.om.Stats().Placed+d.om.Stats().Modified+d.om.Stats().Cancelled)
	return nil
}

// killAll blocks entries and tells every agent to flatten. Idempotent.
func (d *dayRun) killAll(reason string) {
	d.killOnce.Do(func() {
		d.log.Warn("KILL ALL", "reason", reason)
		d.om.BlockEntries()
		d.risk.Halt(reason)
		for _, a := range d.agents {
			a.Kill(reason)
		}
	})
}

func (d *dayRun) waitFlat(max time.Duration) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if d.risk.OpenCount() == 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	d.log.Error("positions still open after wait", "open", d.risk.OpenCount())
}

// sweep is the last line of defence: it asks the BROKER (not our state) for
// open MIS positions in our instruments and open orders carrying our tags,
// and flattens/cancels whatever it finds. Anything found here is a bug or a
// broker-side anomaly, so it logs at ERROR.
func (d *dayRun) sweep(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	reply := make(chan models.OrderResult, 128)
	pending := 0
	if ords, err := d.trader.Orders(ctx); err == nil {
		for _, o := range ords {
			if _, ours := models.TokenFromTag(o.Tag); ours && o.IsLive() {
				d.log.Error("SWEEP: cancelling live order", "order", o.OrderID, "symbol", o.TradingSymbol, "status", o.Status)
				_ = d.om.Submit(&models.OrderPayload{Action: models.ActionCancel, Priority: models.PriorityEmergency,
					Purpose: models.PurposeStopCancel, InstrumentToken: o.InstrumentToken, BrokerOrderID: o.OrderID, Reply: reply})
				pending++
			}
		}
	} else {
		d.log.Error("SWEEP: order book unavailable", "err", err)
	}
	for ; pending > 0; pending-- {
		select {
		case <-reply:
		case <-ctx.Done():
			pending = 0
		}
	}
	ps, err := d.trader.Positions(ctx)
	if err != nil {
		d.log.Error("SWEEP: positions unavailable — CHECK THE ACCOUNT MANUALLY", "err", err)
		return
	}
	for _, p := range ps {
		in, ours := d.tokens[p.InstrumentToken]
		if !ours || p.Product != "MIS" || p.NetQuantity == 0 {
			continue
		}
		txn := models.TxnSell
		qty := p.NetQuantity
		if qty < 0 {
			txn, qty = models.TxnBuy, -qty
		}
		d.log.Error("SWEEP: open MIS position found at broker — market exit", "symbol", p.TradingSymbol, "net", p.NetQuantity)
		_ = d.om.Submit(&models.OrderPayload{Action: models.ActionPlace, Priority: models.PriorityEmergency,
			Purpose: models.PurposeExitPlace, InstrumentToken: p.InstrumentToken, Reply: reply,
			Request: models.OrderRequest{InstrumentToken: p.InstrumentToken, Exchange: in.Exchange, TradingSymbol: in.TradingSymbol,
				TransactionType: txn, OrderType: models.OrderMarket, Validity: models.ValidityDay, Quantity: qty,
				MarketProtection: d.e.cfg.Orders.MarketProtection, Tag: models.TagForToken(p.InstrumentToken)}})
		pending++
	}
	for ; pending > 0; pending-- {
		select {
		case r := <-reply:
			if r.Err != nil {
				d.log.Error("SWEEP: exit failed — CHECK THE ACCOUNT MANUALLY", "err", r.Err)
			}
		case <-ctx.Done():
			return
		}
	}
	d.log.Info("sweep complete")
}

func (d *dayRun) statusFn() any {
	snaps := make([]models.AgentSnapshot, 0, len(d.agents))
	for _, a := range d.agents {
		snaps = append(snaps, a.Snapshot())
	}
	return map[string]any{
		"mode":   d.e.cfg.Mode,
		"day":    d.day.Format("2006-01-02"),
		"phase":  d.e.sess.PhaseAt(d.e.clk.Now()).String(),
		"risk":   d.risk.Summary(),
		"orders": d.om.Stats(),
		"ticker": map[string]any{"connected": d.mux.Connected(), "ticks": d.mux.Received(), "mailboxes": d.mux.Stats()},
		"agents": snaps,
	}
}

// instruments loads the NSE master once per day (cached on disk for restarts).
func (e *Engine) instruments(ctx context.Context, md broker.MarketData, day time.Time) ([]broker.Instrument, error) {
	path := filepath.Join(e.cfg.Paths.DataDir, "instruments", day.Format("2006-01-02")+"-NSE.json")
	if raw, err := os.ReadFile(path); err == nil {
		var ins []broker.Instrument
		if json.Unmarshal(raw, &ins) == nil && len(ins) > 0 {
			return ins, nil
		}
	}
	ins, err := md.Instruments(ctx, kiteconnect.ExchangeNSE)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		if raw, err := json.Marshal(ins); err == nil {
			_ = os.WriteFile(path, raw, 0o644)
		}
	}
	return ins, nil
}
