// Package agent implements the per-symbol execution agent: one goroutine per
// stock that owns its candles, indicators, position and order state machine.
//
// Concurrency model: every mutable field below is owned by the agent's run
// goroutine and touched nowhere else, so the hot path needs no locks. The only
// shared state is the status snapshot (guarded by an RWMutex) and the
// channels. Results from the order manager, broker order updates, ticks,
// control messages and timers all arrive as events on that one goroutine,
// which makes the state machine deterministic.
//
// Position truth is the FILL LEDGER, not the state enum: the net quantity is
// always recomputed from the filled quantities of every order carrying this
// agent's tag. Any divergence (a fill we did not expect, a stop that fired, an
// ambiguous placement that actually went through) is reconciled against it.
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/nkalva/kitealgo/internal/broker"
	"github.com/nkalva/kitealgo/internal/clock"
	"github.com/nkalva/kitealgo/internal/config"
	"github.com/nkalva/kitealgo/internal/indicators"
	"github.com/nkalva/kitealgo/pkg/models"
)

// Submitter is the order manager's intake.
type Submitter interface {
	Submit(p *models.OrderPayload) error
}

// OrderQuerier reads order state (not rate-limited by the order limiter).
type OrderQuerier interface {
	OrderStatus(ctx context.Context, orderID string) (models.OrderUpdate, error)
}

// RiskGate is the subset of the risk manager the agent needs.
type RiskGate interface {
	Size(price, stopDistance float64) int
	Reserve(token uint32, notional float64) (bool, string)
	Release(token uint32)
	Adopt(token uint32, notional float64)
	UpdateUnrealized(token uint32, pnl float64)
	RecordTrade(token uint32, t models.TradeRecord)
	EstCosts(entryPx, exitPx float64, qty int) float64
}

// MarketFilter is the radar veto.
type MarketFilter interface {
	Allow(side models.Side, benchmark uint32) (bool, string)
}

// Deps are the agent's collaborators.
type Deps struct {
	OM      Submitter
	Querier OrderQuerier
	Risk    RiskGate
	Radar   MarketFilter // may be nil
	Session *clock.Session
	Clock   clock.Clock
	Log     *slog.Logger
}

// Params configure one agent.
type Params struct {
	Instrument broker.Instrument
	Benchmark  uint32
	Day        time.Time // trading day
	Profile    indicators.RVOLProfile
	Seed       []float64 // prior 5-minute closes, oldest first
	Strategy   config.StrategyConfig
	Orders     config.OrdersConfig
}

type ledgerEntry struct {
	purpose models.OrderPurpose
	known   bool // purpose assigned (we have the place result)
	txn     models.TransactionType
	status  string
	filled  int
	avg     float64
}

type statusCheck struct {
	why     models.OrderPurpose
	orderID string
	u       models.OrderUpdate
	err     error
}

type control struct {
	kill   bool
	reason string
}

// Agent is one symbol's execution worker.
type Agent struct {
	// immutable
	inst      broker.Instrument
	token     uint32
	tick      float64
	benchmark uint32
	p         Params
	d         Deps
	log       *slog.Logger
	strat     *Strategy
	open      time.Time

	// inbound
	ticks   <-chan models.Tick
	updates chan models.OrderUpdate
	results chan models.OrderResult
	checks  chan statusCheck
	ctrl    chan control

	// owned by run goroutine ------------------------------------------------
	state      models.AgentState
	candles    *indicators.CandleBuilder
	fast, slow *indicators.EMA
	vwap       indicators.VWAP
	ltp        float64
	bid, ask   float64
	cumVol     uint64
	rvol       float64
	lastTickAt time.Time

	ledger map[string]*ledgerEntry
	pos    *models.Position

	entrySide      models.Side
	entrySlowEMA   float64
	entryOrderID   string
	entryDeadline  time.Time
	entryCancelled bool

	stopOrderID    string
	stopInFlight   bool // a stop place/modify/cancel is outstanding
	stopInFlightPx float64
	desiredStop    float64
	stopReplacing  bool
	noMoreMods     bool
	flowCancelID   string // the order a flow-critical cancel targets (replace / exit re-drive)
	maxLock        models.LockStage

	exitOrderID     string
	exitReason      string
	exitSubmittedAt time.Time
	exitPending     bool // an exit REST call is outstanding
	flattenQueued   bool // flatten requested while a stop op was in flight

	killed      bool
	killReason  string
	tradesToday int
	realized    float64
	cooldown    int
	lastSignal  string
	entryTime   time.Time
	entryPx     float64

	snapMu sync.RWMutex
	snap   models.AgentSnapshot
	snapAt time.Time
	done   chan struct{}
}

// New creates an agent. ticks is the agent's mailbox from the multiplexer.
func New(p Params, d Deps, ticks <-chan models.Tick) *Agent {
	if d.Clock == nil {
		d.Clock = clock.System{}
	}
	a := &Agent{
		inst: p.Instrument, token: p.Instrument.InstrumentToken, tick: p.Instrument.TickSize,
		benchmark: p.Benchmark, p: p, d: d,
		log:     d.Log.With("symbol", p.Instrument.TradingSymbol),
		strat:   NewStrategy(p.Strategy),
		open:    d.Session.OpenTime(p.Day),
		ticks:   ticks,
		updates: make(chan models.OrderUpdate, 256),
		results: make(chan models.OrderResult, 64),
		checks:  make(chan statusCheck, 16),
		ctrl:    make(chan control, 4),
		ledger:  map[string]*ledgerEntry{},
		fast:    indicators.NewEMA(p.Strategy.FastEMA),
		slow:    indicators.NewEMA(p.Strategy.SlowEMA),
		done:    make(chan struct{}),
	}
	if a.tick <= 0 {
		a.tick = 0.05
	}
	a.candles = indicators.NewCandleBuilder(a.token, p.Strategy.CandleInterval, p.Strategy.CandleGrace, a.open)
	a.fast.Seed(p.Seed)
	a.slow.Seed(p.Seed)
	a.publish(true)
	return a
}

// Token returns the instrument token.
func (a *Agent) Token() uint32 { return a.token }

// Updates is the inbox the update router delivers broker order updates to.
func (a *Agent) Updates() chan<- models.OrderUpdate { return a.updates }

// Kill asks the agent to flatten and stop trading for the day. Non-blocking.
func (a *Agent) Kill(reason string) {
	select {
	case a.ctrl <- control{kill: true, reason: reason}:
	default:
	}
}

// Done is closed when Run returns.
func (a *Agent) Done() <-chan struct{} { return a.done }

// Snapshot returns a copy of the agent's public state.
func (a *Agent) Snapshot() models.AgentSnapshot {
	a.snapMu.RLock()
	defer a.snapMu.RUnlock()
	s := a.snap
	if s.Position != nil {
		p := *s.Position
		s.Position = &p
	}
	return s
}

// Run is the agent's event loop.
func (a *Agent) Run(ctx context.Context) {
	defer close(a.done)
	hk := time.NewTicker(500 * time.Millisecond)
	defer hk.Stop()
	a.log.Info("agent started", "token", a.token, "seeded_ema", a.slow.Ready(), "rvol_days", a.p.Profile.Days)
	for {
		select {
		case <-ctx.Done():
			a.log.Info("agent stopped", "state", a.state, "trades", a.tradesToday, "realized", round2(a.realized))
			a.publish(true)
			return
		case t := <-a.ticks:
			a.onTick(t)
		case u := <-a.updates:
			a.onUpdate(u)
		case r := <-a.results:
			a.onResult(r)
		case c := <-a.checks:
			a.onStatusCheck(c)
		case c := <-a.ctrl:
			if c.kill {
				a.onKill(c.reason)
			}
		case <-hk.C:
			a.housekeeping()
		}
	}
}

// ---------------------------------------------------------------------------
// Market data
// ---------------------------------------------------------------------------

func (a *Agent) onTick(t models.Tick) {
	if t.LastPrice <= 0 {
		return
	}
	a.ltp, a.cumVol, a.lastTickAt = t.LastPrice, t.VolumeTraded, t.MarketTime()
	if t.BestBid > 0 {
		a.bid = t.BestBid
	}
	if t.BestAsk > 0 {
		a.ask = t.BestAsk
	}
	if !t.MarketTime().Before(a.open) {
		a.vwap.Update(t.LastPrice, t.VolumeTraded, t.AverageTradePrice)
		a.rvol = a.p.Profile.RVOL(t.MarketTime().Sub(a.open), t.VolumeTraded)
	}
	for _, c := range a.candles.Update(t) {
		a.onCandle(c)
	}
	if a.pos != nil && (a.state == models.StateInPosition || a.state == models.StateTrailing) {
		a.ratchet()
		a.d.Risk.UpdateUnrealized(a.token, a.pos.UnrealizedPnL(a.ltp))
	}
	a.publish(false)
}

func (a *Agent) currentVWAP() float64 {
	return a.vwap.Value(a.p.Strategy.VWAPSource == "exchange")
}

func (a *Agent) onCandle(c models.Candle) {
	prevFast, prevSlow := a.fast.Value(), a.slow.Value()
	a.fast.Update(c.Close)
	a.slow.Update(c.Close)
	if a.cooldown > 0 {
		a.cooldown--
	}
	sig := a.strat.OnBar(BarInput{Candle: c, Fast: a.fast.Value(), Slow: a.slow.Value(),
		PrevFast: prevFast, PrevSlow: prevSlow, VWAP: a.currentVWAP(), RVOL: a.rvol})

	switch a.state {
	case models.StateFlat:
		if sig.Side == models.SideNone {
			return
		}
		a.lastSignal = sig.Reason
		a.tryEntry(sig, c)
	case models.StateTrailing:
		fast := a.fast.Value()
		if CloseAdverse(a.pos.Side, c.Close, fast) {
			a.flatten(fmt.Sprintf("5m close %.2f on adverse side of 10 EMA %.2f", c.Close, fast), models.PriorityStop)
			return
		}
		lock := a.strat.LockLevel(a.pos.Side, a.pos.EntryPrice, a.pos.Lock)
		a.setDesiredStop(TighterOf(a.pos.Side, lock, a.strat.TrailStop(a.pos.Side, fast)), false)
	}
}

// ---------------------------------------------------------------------------
// Entry
// ---------------------------------------------------------------------------

func (a *Agent) tryEntry(sig Signal, c models.Candle) {
	phase := a.d.Session.PhaseAt(c.End)
	if nowPhase := a.d.Session.PhaseAt(a.d.Clock.Now()); nowPhase < phase {
		phase = nowPhase // late bar processing never extends the window
	}
	skip := func(why string) {
		a.lastSignal = sig.Reason + " — skipped: " + why
		a.log.Info("signal skipped", "side", sig.Side, "why", why, "signal", sig.Reason)
	}
	switch {
	case a.killed:
		skip("killed")
		return
	case phase != clock.PhaseActive:
		skip("outside active window (" + phase.String() + ")")
		return
	case a.tradesToday >= a.p.Strategy.MaxTradesPerSymbol:
		skip("max trades per symbol reached")
		return
	case a.cooldown > 0:
		skip("cooldown")
		return
	}
	if a.d.Radar != nil {
		if ok, why := a.d.Radar.Allow(sig.Side, a.benchmark); !ok {
			skip(why)
			return
		}
	}
	ref := a.ltp
	stop := a.strat.InitialStop(sig.Side, ref, a.slow.Value())
	qty := a.d.Risk.Size(ref, math.Abs(ref-stop))
	if qty < 1 {
		skip("size is zero (stop distance too wide for risk budget)")
		return
	}
	if ok, why := a.d.Risk.Reserve(a.token, ref*float64(qty)); !ok {
		skip(why)
		return
	}
	// Marketable limit, IOC: fills at the touch, never rests, never chases
	// beyond 0.5% (SEBI market-protection requirement satisfied by the limit).
	buf := indicators.Pct(a.p.Orders.EntryLimitBufferPct)
	var px float64
	if sig.Side == models.SideLong {
		base := a.ask
		if base <= 0 {
			base = ref
		}
		px = indicators.RoundToTick(base*(1+buf), a.tick, +1)
	} else {
		base := a.bid
		if base <= 0 {
			base = ref
		}
		px = indicators.RoundToTick(base*(1-buf), a.tick, -1)
	}
	a.entrySide, a.entrySlowEMA = sig.Side, a.slow.Value()
	a.entryOrderID, a.entryCancelled = "", false
	a.entryDeadline = a.d.Clock.Now().Add(a.p.Strategy.EntryTimeout)
	a.setState(models.StatePendingEntry)
	a.log.Info("ENTRY signal", "side", sig.Side, "qty", qty, "limit", px, "ref", ref, "planned_stop", round2(stop), "why", sig.Reason)
	a.submit(models.ActionPlace, models.PriorityEntry, models.PurposeEntry, "", models.OrderRequest{
		TransactionType: sig.Side.EntryTxn(), OrderType: models.OrderLimit, Validity: models.ValidityIOC,
		Quantity: qty, Price: px,
	})
}

// ---------------------------------------------------------------------------
// Order plumbing
// ---------------------------------------------------------------------------

func (a *Agent) submit(action models.OrderAction, prio models.Priority, purpose models.OrderPurpose, orderID string, req models.OrderRequest) {
	a.submitN(action, prio, purpose, orderID, req, 0)
}

func (a *Agent) submitN(action models.OrderAction, prio models.Priority, purpose models.OrderPurpose, orderID string, req models.OrderRequest, attempts int) {
	req.InstrumentToken = a.token
	req.Exchange = a.inst.Exchange
	req.TradingSymbol = a.inst.TradingSymbol
	req.Tag = models.TagForToken(a.token)
	if req.Validity == "" {
		req.Validity = models.ValidityDay
	}
	if req.OrderType == models.OrderMarket || req.OrderType == models.OrderSLM {
		req.MarketProtection = a.p.Orders.MarketProtection
	}
	p := &models.OrderPayload{Action: action, Priority: prio, Purpose: purpose, InstrumentToken: a.token,
		BrokerOrderID: orderID, Request: req, Reply: a.results, MaxAttempts: attempts}
	if err := a.d.OM.Submit(p); err != nil {
		a.log.Error("submit failed", "err", err)
		a.results <- models.OrderResult{Action: action, Purpose: purpose, BrokerOrderID: orderID, Err: err}
	}
}

func (a *Agent) checkStatus(why models.OrderPurpose, orderID string) {
	if orderID == "" || a.d.Querier == nil {
		a.checks <- statusCheck{why: why, orderID: orderID, err: fmt.Errorf("no order to check")}
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		u, err := a.d.Querier.OrderStatus(ctx, orderID)
		a.checks <- statusCheck{why: why, orderID: orderID, u: u, err: err}
	}()
}

func (a *Agent) entry(id string) *ledgerEntry {
	e := a.ledger[id]
	if e == nil {
		e = &ledgerEntry{}
		a.ledger[id] = e
	}
	return e
}

func (a *Agent) onUpdate(u models.OrderUpdate) {
	e := a.entry(u.OrderID)
	e.status = u.Status
	e.txn = u.TransactionType
	if u.FilledQuantity >= e.filled {
		e.filled = u.FilledQuantity
		if u.AveragePrice > 0 {
			e.avg = u.AveragePrice
		}
	}
	if u.Status == models.StatusRejected {
		a.log.Warn("order rejected", "order", u.OrderID, "msg", u.StatusMessage)
	}
	a.reconcile()
}

func (a *Agent) netQty() int {
	n := 0
	for _, e := range a.ledger {
		if e.txn == models.TxnBuy {
			n += e.filled
		} else if e.txn == models.TxnSell {
			n -= e.filled
		}
	}
	return n
}

func (a *Agent) isLive(id string) bool {
	e, ok := a.ledger[id]
	if !ok {
		return id != "" // placed but no update seen yet: assume live
	}
	return e.status != models.StatusComplete && e.status != models.StatusCancelled && e.status != models.StatusRejected
}

func (a *Agent) onResult(r models.OrderResult) {
	switch r.Purpose {
	case models.PurposeEntry:
		if r.Err != nil {
			a.log.Warn("entry order failed", "err", r.Err)
			if a.state == models.StatePendingEntry && a.entryOrderID == "" {
				a.d.Risk.Release(a.token)
				a.setState(a.flatState())
			}
			return
		}
		a.entryOrderID = r.BrokerOrderID
		e := a.entry(r.BrokerOrderID)
		e.purpose, e.known, e.txn = models.PurposeEntry, true, a.entrySide.EntryTxn()
		a.reconcile()

	case models.PurposeEntryCancel:
		if r.Err != nil {
			a.log.Debug("entry cancel failed (usually already terminal)", "err", r.Err)
		}

	case models.PurposeStopPlace:
		a.stopInFlight = false
		if r.Err != nil {
			a.log.Error("STOP PLACEMENT FAILED after retry — position is naked, flattening", "err", r.Err)
			a.flatten("stop placement failed", models.PriorityEmergency)
			return
		}
		if a.pos == nil || a.state == models.StatePendingExit {
			// We started exiting while this stop was being placed: it is now a
			// liability (it could fill after our exit). Cancel it immediately.
			e := a.entry(r.BrokerOrderID)
			e.purpose, e.known = models.PurposeStopPlace, true
			if e.status == "" {
				e.status = models.StatusTriggerPending
			}
			a.log.Warn("stop landed after exit started — cancelling it", "order", r.BrokerOrderID)
			a.submit(models.ActionCancel, models.PriorityEmergency, models.PurposeStopCancel, r.BrokerOrderID, models.OrderRequest{})
			return
		}
		a.stopOrderID = r.BrokerOrderID
		e := a.entry(r.BrokerOrderID)
		e.purpose, e.known, e.txn = models.PurposeStopPlace, true, a.pos.Side.ExitTxn()
		a.pos.StopOrderID, a.pos.StopPrice, a.pos.StopModCount = r.BrokerOrderID, a.stopInFlightPx, 0
		a.stopReplacing = false
		a.log.Info("stop live", "order", r.BrokerOrderID, "trigger", a.pos.StopPrice)
		a.afterStopOp()

	case models.PurposeStopModify:
		a.stopInFlight = false
		if r.Err != nil {
			a.log.Warn("stop modify failed after retry — verifying the resting stop", "err", r.Err)
			a.stopInFlight = true // hold further stop ops until verified
			a.checkStatus(models.PurposeStopModify, a.stopOrderID)
			return
		}
		if a.pos == nil {
			return
		}
		a.pos.StopPrice = a.stopInFlightPx
		a.pos.StopModCount++
		a.log.Info("stop moved", "trigger", a.pos.StopPrice, "lock", a.pos.Lock, "mods", a.pos.StopModCount)
		a.afterStopOp()

	case models.PurposeStopCancel:
		if r.BrokerOrderID == "" || r.BrokerOrderID != a.flowCancelID {
			// Stray-order cleanup (or a cancel superseded by events): the ledger
			// update will reflect the outcome; nothing else depends on it.
			if r.Err != nil {
				a.log.Debug("stray cancel failed (usually already terminal)", "order", r.BrokerOrderID, "err", r.Err)
			}
			return
		}
		a.flowCancelID = ""
		a.stopInFlight = false
		if a.state == models.StatePendingExit {
			// Cancel-then-exit path (modify-to-market was not possible, or the
			// exit order did not fill in time).
			a.exitPending = false
			if r.Err != nil {
				a.checkStatus(models.PurposeStopCancel, r.BrokerOrderID)
				return
			}
			if r.BrokerOrderID == a.stopOrderID {
				a.stopOrderID = ""
			}
			a.placeMarketExit(models.PriorityEmergency)
			return
		}
		if a.pos == nil {
			return
		}
		// Cancel-and-replace (Kite allows 25 modifications per order).
		if r.Err != nil {
			a.log.Warn("stop cancel for replace failed — keeping existing stop, trailing frozen", "err", r.Err)
			a.stopReplacing, a.noMoreMods = false, true
			a.checkStatus(models.PurposeStopCancel, r.BrokerOrderID)
			a.afterStopOp()
			return
		}
		a.stopOrderID, a.pos.StopOrderID = "", ""
		if a.flattenQueued {
			a.flattenQueued, a.stopReplacing = false, false
			a.flatten(a.exitReason, models.PriorityEmergency)
			return
		}
		a.placeStop(a.desiredStop, models.PriorityEmergency) // naked until this lands

	case models.PurposeExitViaStop:
		a.exitPending = false
		if r.Err != nil {
			a.log.Warn("stop→market conversion failed — checking stop", "err", r.Err)
			a.checkStatus(models.PurposeExitViaStop, a.stopOrderID)
			return
		}
		a.exitOrderID = r.BrokerOrderID
		a.exitSubmittedAt = a.d.Clock.Now()

	case models.PurposeExitPlace:
		a.exitPending = false
		if r.Err != nil {
			a.log.Error("MARKET EXIT FAILED after retries — will retry in 1s", "err", r.Err)
			a.exitSubmittedAt = a.d.Clock.Now().Add(-4 * time.Second) // housekeeping re-drives after 5s total
			return
		}
		a.exitOrderID = r.BrokerOrderID
		e := a.entry(r.BrokerOrderID)
		e.purpose, e.known = models.PurposeExitPlace, true
		if e.txn == "" && a.pos != nil {
			e.txn = a.pos.Side.ExitTxn()
		}
		a.exitSubmittedAt = a.d.Clock.Now()
	}
	a.reconcile()
}

func (a *Agent) onStatusCheck(c statusCheck) {
	if c.err == nil {
		a.onUpdate(c.u) // fold into the ledger first
	}
	live := c.err == nil && c.u.IsLive()
	switch c.why {
	case models.PurposeStopModify:
		a.stopInFlight = false
		if a.pos == nil || a.netQty() == 0 {
			return
		}
		if live {
			a.log.Warn("stop verified live at previous level; will retry on next bar", "trigger", a.pos.StopPrice)
			a.afterStopOp()
			return
		}
		a.log.Error("stop not verifiable after failed modify — flattening", "err", c.err)
		a.flatten("stop modify failed; stop state unknown", models.PriorityEmergency)
	case models.PurposeExitViaStop, models.PurposeStopCancel:
		if a.netQty() == 0 {
			return // stop already filled — we are flat
		}
		if a.state != models.StatePendingExit {
			return // replace-path verification: nothing more to do
		}
		if live {
			a.flowCancelID = c.orderID
			a.stopInFlight, a.exitPending = true, true
			a.submit(models.ActionCancel, models.PriorityEmergency, models.PurposeStopCancel, c.orderID, models.OrderRequest{})
			return
		}
		if c.orderID == a.stopOrderID {
			a.stopOrderID = ""
		}
		a.placeMarketExit(models.PriorityEmergency)
	}
}

// ---------------------------------------------------------------------------
// Reconciliation — derive the state from the fill ledger
// ---------------------------------------------------------------------------

func (a *Agent) reconcile() {
	net := a.netQty()
	switch a.state {
	case models.StatePendingEntry:
		if a.entryOrderID == "" {
			if net != 0 {
				// Fill arrived before the place result — wait for it.
				return
			}
			return
		}
		e := a.ledger[a.entryOrderID]
		if e == nil || !(e.status == models.StatusComplete || e.status == models.StatusCancelled || e.status == models.StatusRejected) {
			return
		}
		if net == 0 {
			a.log.Info("entry not filled", "status", e.status)
			a.d.Risk.Release(a.token)
			a.setState(a.flatState())
			return
		}
		a.openPosition(net, e.avg)

	case models.StateInPosition, models.StateTrailing, models.StatePendingExit:
		if a.pos == nil {
			return
		}
		if net == 0 {
			a.closePosition()
			return
		}
		if abs(net) != a.pos.Quantity {
			a.log.Warn("position quantity changed at broker", "was", a.pos.Quantity, "now", abs(net))
			a.pos.Quantity = abs(net)
		}

	case models.StateFlat, models.StateHalted:
		if net != 0 {
			a.adoptOrphan(net)
			return
		}
		a.cancelStrays()
	}
}

func (a *Agent) openPosition(net int, avg float64) {
	side := models.SideLong
	if net < 0 {
		side = models.SideShort
	}
	if avg <= 0 {
		avg = a.ltp
	}
	a.pos = &models.Position{InstrumentToken: a.token, TradingSymbol: a.inst.TradingSymbol, Side: side,
		Quantity: abs(net), EntryPrice: avg, EntryTime: a.d.Clock.Now()}
	a.entryTime, a.entryPx, a.maxLock = a.pos.EntryTime, avg, models.LockNone
	a.stopOrderID, a.desiredStop, a.noMoreMods, a.stopReplacing = "", 0, false, false
	a.exitOrderID, a.exitReason = "", ""
	a.tradesToday++
	a.d.Risk.Adopt(a.token, avg*float64(abs(net)))
	a.setState(models.StateInPosition)

	stop := a.strat.InitialStop(side, avg, a.entrySlowEMA)
	a.log.Info("FILLED", "side", side, "qty", a.pos.Quantity, "avg", avg, "initial_stop", round2(stop))
	if a.killed {
		a.flatten(a.killReason, models.PriorityEmergency)
		return
	}
	a.placeStop(stop, models.PriorityEmergency)
}

func (a *Agent) adoptOrphan(net int) {
	var avg float64
	for _, e := range a.ledger {
		if e.filled > 0 && ((net > 0 && e.txn == models.TxnBuy) || (net < 0 && e.txn == models.TxnSell)) {
			avg = e.avg
		}
	}
	if avg == 0 {
		avg = a.ltp
	}
	side := models.SideLong
	if net < 0 {
		side = models.SideShort
	}
	a.log.Error("ORPHAN position detected from fills — adopting and flattening", "net", net)
	a.pos = &models.Position{InstrumentToken: a.token, TradingSymbol: a.inst.TradingSymbol, Side: side,
		Quantity: abs(net), EntryPrice: avg, EntryTime: a.d.Clock.Now()}
	a.entryTime, a.entryPx = a.pos.EntryTime, avg
	a.d.Risk.Adopt(a.token, avg*float64(abs(net)))
	a.stopOrderID = ""
	for id, e := range a.ledger {
		if e.known && e.purpose == models.PurposeStopPlace && a.isLive(id) {
			a.stopOrderID = id
		}
	}
	a.setState(models.StateInPosition)
	a.flatten("orphan fill", models.PriorityEmergency)
}

// cancelStrays cancels any of our orders still live while we are flat — a
// forgotten SL-M would open a fresh, unmanaged position when triggered.
func (a *Agent) cancelStrays() {
	for id, e := range a.ledger {
		if e.status == "" || !a.isLive(id) || e.status == "CANCEL PENDING" {
			continue
		}
		a.log.Warn("cancelling stray live order while flat", "order", id, "status", e.status)
		e.status = "CANCEL PENDING"
		a.submit(models.ActionCancel, models.PriorityEmergency, models.PurposeStopCancel, id, models.OrderRequest{})
	}
}

func (a *Agent) closePosition() {
	p := a.pos
	exitTxn := p.Side.ExitTxn()
	var qty int
	var val float64
	for id, e := range a.ledger {
		if e.txn == exitTxn && e.filled > 0 && id != a.entryOrderID {
			qty += e.filled
			val += float64(e.filled) * e.avg
		}
	}
	exitPx := a.ltp
	if qty > 0 {
		exitPx = val / float64(qty)
	}
	gross := p.Side.Sign() * (exitPx - p.EntryPrice) * float64(p.Quantity)
	costs := a.d.Risk.EstCosts(p.EntryPrice, exitPx, p.Quantity)
	reason := a.exitReason
	if reason == "" {
		reason = fmt.Sprintf("stop hit (%s @ %.2f)", p.Lock, p.StopPrice)
	}
	rec := models.TradeRecord{TradingSymbol: p.TradingSymbol, Side: p.Side, Quantity: p.Quantity,
		EntryTime: p.EntryTime, EntryPrice: p.EntryPrice, ExitTime: a.d.Clock.Now(), ExitPrice: exitPx,
		GrossPnL: gross, EstCosts: costs, NetPnL: gross - costs, ExitReason: reason, MaxLock: a.maxLock}
	a.realized += rec.NetPnL
	a.d.Risk.RecordTrade(a.token, rec)
	a.d.Risk.Release(a.token)

	// Clear per-trade ledger state so the next trade starts clean; keep only
	// orders that are still live (they will be cancelled as strays).
	for id, e := range a.ledger {
		if e.status != "" && !a.isLive(id) {
			delete(a.ledger, id)
		}
	}
	a.pos = nil
	a.stopOrderID, a.exitOrderID, a.entryOrderID = "", "", ""
	a.stopInFlight, a.exitPending, a.flattenQueued, a.stopReplacing = false, false, false, false
	a.flowCancelID = ""
	a.cooldown = a.p.Strategy.CooldownCandles
	a.setState(a.flatState())
	a.cancelStrays()
}

func (a *Agent) flatState() models.AgentState {
	if a.killed {
		return models.StateHalted
	}
	return models.StateFlat
}

// ---------------------------------------------------------------------------
// Stops
// ---------------------------------------------------------------------------

func (a *Agent) stopRound(side models.Side, px float64) float64 {
	// Long stops round UP (tighter), short stops round DOWN (tighter), so the
	// breakeven/profit-lock levels are always at least what they promise.
	if side == models.SideLong {
		return indicators.RoundToTick(px, a.tick, +1)
	}
	return indicators.RoundToTick(px, a.tick, -1)
}

// stopThroughMarket reports whether a stop at px would already be triggered.
func (a *Agent) stopThroughMarket(side models.Side, px float64) bool {
	if side == models.SideLong {
		return px >= a.ltp-a.tick
	}
	return px <= a.ltp+a.tick
}

func (a *Agent) placeStop(px float64, prio models.Priority) {
	side := a.pos.Side
	px = a.stopRound(side, px)
	if a.stopThroughMarket(side, px) {
		a.flatten("stop level already through market at placement", models.PriorityEmergency)
		return
	}
	a.stopInFlight, a.stopInFlightPx, a.desiredStop = true, px, px
	// Queued in the emergency lane (the position is unprotected until it lands)
	// but with the stop retry budget: one retry after 100ms, then flatten.
	a.submitN(models.ActionPlace, prio, models.PurposeStopPlace, "", models.OrderRequest{
		TransactionType: side.ExitTxn(), OrderType: models.OrderSLM, Quantity: a.pos.Quantity, TriggerPrice: px,
	}, 2)
}

// ratchet moves the stop through the breakeven / profit-lock stages on ticks.
func (a *Agent) ratchet() {
	gross := a.pos.GrossReturn(a.ltp)
	stage, target := a.strat.LockTarget(a.pos.Side, a.pos.EntryPrice, gross)
	if stage <= a.pos.Lock {
		return
	}
	a.pos.Lock = stage
	if stage > a.maxLock {
		a.maxLock = stage
	}
	a.log.Info("lock stage reached", "stage", stage, "gross_pct", round2(gross*100), "stop_target", round2(target))
	if a.state == models.StateInPosition {
		a.setState(models.StateTrailing)
	}
	a.setDesiredStop(target, true)
}

// setDesiredStop records a new target stop (only ever tighter) and applies it.
func (a *Agent) setDesiredStop(px float64, force bool) {
	if a.pos == nil || px <= 0 {
		return
	}
	side := a.pos.Side
	px = a.stopRound(side, px)
	cur := a.desiredStop
	if cur == 0 {
		cur = a.pos.StopPrice
	}
	step := a.pos.EntryPrice * indicators.Pct(a.p.Strategy.MinStopStepPct)
	if force {
		step = a.tick
	}
	if !Improves(side, px, cur, step) {
		return
	}
	if a.stopThroughMarket(side, px) {
		a.flatten(fmt.Sprintf("target stop %.2f already reached by price %.2f", px, a.ltp), models.PriorityStop)
		return
	}
	a.desiredStop = px
	a.applyStop()
}

func (a *Agent) applyStop() {
	if a.pos == nil || a.stopInFlight || a.stopOrderID == "" || a.state == models.StatePendingExit {
		return
	}
	if !Improves(a.pos.Side, a.desiredStop, a.pos.StopPrice, a.tick) {
		return
	}
	if a.noMoreMods {
		return
	}
	if a.pos.StopModCount >= a.p.Orders.MaxModsPerOrder {
		a.log.Info("modification budget exhausted — cancel-and-replace stop", "mods", a.pos.StopModCount)
		a.stopReplacing, a.stopInFlight = true, true
		a.flowCancelID = a.stopOrderID
		a.submit(models.ActionCancel, models.PriorityStop, models.PurposeStopCancel, a.stopOrderID, models.OrderRequest{})
		return
	}
	a.stopInFlight, a.stopInFlightPx = true, a.desiredStop
	a.submit(models.ActionModify, models.PriorityStop, models.PurposeStopModify, a.stopOrderID, models.OrderRequest{
		OrderType: models.OrderSLM, Quantity: a.pos.Quantity, TriggerPrice: a.desiredStop,
	})
}

func (a *Agent) afterStopOp() {
	if a.flattenQueued {
		a.flattenQueued = false
		a.flatten(a.exitReason, models.PriorityEmergency)
		return
	}
	a.applyStop()
}

// ---------------------------------------------------------------------------
// Exits
// ---------------------------------------------------------------------------

// flatten closes the position. Preferred path: convert the resting SL-M into a
// MARKET order with one MODIFY — atomic, so the stop and the exit can never
// both fill (which a cancel-then-place sequence cannot guarantee).
func (a *Agent) flatten(reason string, prio models.Priority) {
	switch a.state {
	case models.StatePendingEntry:
		a.exitReason = reason
		if a.entryOrderID != "" && a.isLive(a.entryOrderID) && !a.entryCancelled {
			a.entryCancelled = true
			a.submit(models.ActionCancel, models.PriorityEmergency, models.PurposeEntryCancel, a.entryOrderID, models.OrderRequest{})
		}
		return // reconcile() opens then immediately flattens any fill (killed flag)
	case models.StatePendingExit:
		if a.exitReason == "" {
			a.exitReason = reason
		}
		return
	case models.StateFlat, models.StateHalted:
		return
	}
	if a.pos == nil {
		return
	}
	a.exitReason = reason
	if a.stopInFlight {
		// A stop op is outstanding; exit as soon as it resolves.
		a.flattenQueued = true
		a.log.Warn("flatten queued behind in-flight stop op", "reason", reason)
		if a.stopOrderID == "" && !a.stopReplacing {
			// The in-flight op is the initial placement: don't wait — exit now.
			a.flattenQueued = false
		} else {
			return
		}
	}
	a.setState(models.StatePendingExit)
	a.log.Info("EXIT", "reason", reason, "qty", a.pos.Quantity, "ltp", a.ltp)
	if a.stopOrderID != "" && a.isLive(a.stopOrderID) {
		a.exitPending = true
		a.exitOrderID = a.stopOrderID
		a.exitSubmittedAt = a.d.Clock.Now()
		a.submit(models.ActionModify, prio, models.PurposeExitViaStop, a.stopOrderID, models.OrderRequest{
			OrderType: models.OrderMarket, Quantity: a.pos.Quantity,
		})
		return
	}
	a.placeMarketExit(prio)
}

func (a *Agent) placeMarketExit(prio models.Priority) {
	if a.pos == nil || a.exitPending {
		return
	}
	net := a.netQty()
	if net == 0 {
		a.reconcile()
		return
	}
	txn := models.TxnSell
	if net < 0 {
		txn = models.TxnBuy
	}
	a.exitPending = true
	a.exitSubmittedAt = a.d.Clock.Now()
	a.submit(models.ActionPlace, prio, models.PurposeExitPlace, "", models.OrderRequest{
		TransactionType: txn, OrderType: models.OrderMarket, Quantity: abs(net),
	})
}

func (a *Agent) onKill(reason string) {
	if a.killed {
		return
	}
	a.killed, a.killReason = true, reason
	a.log.Warn("KILL received", "reason", reason, "state", a.state)
	switch a.state {
	case models.StateFlat:
		a.setState(models.StateHalted)
		a.cancelStrays()
	case models.StatePendingEntry, models.StateInPosition, models.StateTrailing:
		a.flatten(reason, models.PriorityEmergency)
	}
}

// ---------------------------------------------------------------------------
// Housekeeping (timer driven)
// ---------------------------------------------------------------------------

func (a *Agent) housekeeping() {
	now := a.d.Clock.Now()
	for _, c := range a.candles.Flush(now) {
		a.onCandle(c)
	}
	switch a.state {
	case models.StatePendingEntry:
		if a.entryOrderID != "" && !a.entryCancelled && now.After(a.entryDeadline) && a.isLive(a.entryOrderID) {
			a.log.Warn("entry timed out — cancelling")
			a.entryCancelled = true
			a.submit(models.ActionCancel, models.PriorityStop, models.PurposeEntryCancel, a.entryOrderID, models.OrderRequest{})
		}
	case models.StateInPosition, models.StateTrailing:
		// Invariant: a live position always has a live (or in-flight) stop.
		if a.pos != nil && a.stopOrderID == "" && !a.stopInFlight {
			a.log.Error("INVARIANT: position without stop — flattening")
			a.flatten("position without stop", models.PriorityEmergency)
		} else if a.pos != nil && a.stopOrderID != "" && !a.stopInFlight && !a.isLive(a.stopOrderID) && a.netQty() != 0 {
			a.log.Error("INVARIANT: stop order no longer live but position open — flattening")
			a.stopOrderID = ""
			a.flatten("stop order died", models.PriorityEmergency)
		}
	case models.StatePendingExit:
		if a.exitPending {
			break
		}
		if a.exitSubmittedAt.IsZero() || now.Sub(a.exitSubmittedAt) > 5*time.Second {
			// Exit not confirmed: the MARKET order may have been converted to a
			// resting limit by market protection in a fast market. Re-drive it.
			if a.exitOrderID != "" && a.isLive(a.exitOrderID) {
				a.log.Warn("exit not filled within 5s — cancelling and re-placing", "order", a.exitOrderID)
				id := a.exitOrderID
				a.exitOrderID = ""
				if id == a.stopOrderID {
					a.stopOrderID = ""
				}
				a.stopInFlight, a.exitPending = true, true
				a.flowCancelID = id
				a.submit(models.ActionCancel, models.PriorityEmergency, models.PurposeStopCancel, id, models.OrderRequest{})
			} else if a.netQty() != 0 {
				a.placeMarketExit(models.PriorityEmergency)
			} else {
				a.reconcile()
			}
		}
	}
	a.publish(false)
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

func (a *Agent) setState(s models.AgentState) {
	if a.state != s {
		a.log.Debug("state", "from", a.state, "to", s)
		a.state = s
		a.publish(true)
	}
}

func (a *Agent) publish(force bool) {
	now := time.Now()
	if !force && now.Sub(a.snapAt) < 500*time.Millisecond {
		return
	}
	a.snapAt = now
	s := models.AgentSnapshot{TradingSymbol: a.inst.TradingSymbol, State: a.state.String(), LastPrice: a.ltp,
		VWAP: round2(a.currentVWAP()), EMA10: round2(a.fast.Value()), EMA20: round2(a.slow.Value()),
		RVOL: round2(a.rvol), Trades: a.tradesToday, RealizedPnL: round2(a.realized), LastSignal: a.lastSignal,
		UpdatedAt: now}
	if a.pos != nil {
		p := *a.pos
		s.Position = &p
	}
	a.snapMu.Lock()
	a.snap = s
	a.snapMu.Unlock()
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
