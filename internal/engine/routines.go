package engine

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/nkalva/kitealgo/internal/broker"
	"github.com/nkalva/kitealgo/internal/clock"
	"github.com/nkalva/kitealgo/internal/data"
	"github.com/nkalva/kitealgo/internal/indicators"
	"github.com/nkalva/kitealgo/internal/swing"
	"github.com/nkalva/kitealgo/pkg/models"
)

const tick = 0.05

// historyDays of daily bars are loaded for indicators (EMA50 + RS60 + slack).
const historyDays = 420

func key(sym string) string { return "NSE:" + sym }

// gttRequest builds the stop GTT: SELL qty, trigger = stop, LIMIT a little
// below the trigger so a fast drop still fills.
func (e *Engine) gttRequest(p *models.Position, stop float64) models.OrderRequest {
	trig := indicators.RoundToTick(stop, tick, -1)
	lim := indicators.RoundToTick(trig*(1-indicators.Pct(e.cfg.Orders.GTTLimitBufferPct)), tick, -1)
	last := p.LastPrice
	if last <= 0 {
		last = p.LastClose
	}
	return models.OrderRequest{InstrumentToken: p.InstrumentToken, Exchange: "NSE", TradingSymbol: p.Symbol,
		TransactionType: models.TxnSell, OrderType: models.OrderLimit, Product: models.ProductCNC,
		Quantity: p.Quantity, TriggerPrice: trig, Price: lim, LastPrice: last}
}

func (e *Engine) quotes(ctx context.Context, d *dayCtx, syms []string) map[string]models.Quote {
	if len(syms) == 0 {
		return map[string]models.Quote{}
	}
	keys := make([]string, 0, len(syms))
	for _, s := range syms {
		keys = append(keys, key(s))
	}
	qs, err := d.kite.Quotes(ctx, keys)
	if err != nil {
		e.log.Warn("quote fetch failed", "err", err)
		return map[string]models.Quote{}
	}
	if e.paper != nil {
		for _, q := range qs {
			e.paper.OnQuote(q)
		}
	}
	return qs
}

// waitOrder polls until the order is terminal or the timeout passes.
func (e *Engine) waitOrder(ctx context.Context, d *dayCtx, id string, timeout time.Duration) (models.OrderUpdate, error) {
	deadline := time.Now().Add(timeout)
	for {
		u, err := d.trader.OrderStatus(ctx, id)
		if err == nil && u.IsTerminal() {
			return u, nil
		}
		if time.Now().After(deadline) {
			return u, fmt.Errorf("order %s not terminal after %s (status %q)", id, timeout, u.Status)
		}
		select {
		case <-ctx.Done():
			return u, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// ---------------------------------------------------------------------------
// Reconcile
// ---------------------------------------------------------------------------

// reconcile compares the book with the broker. Only quantities the engine
// bought are ever managed; any extra shares you hold yourself are ignored.
func (e *Engine) reconcile(ctx context.Context, d *dayCtx) error {
	hs, err := d.trader.Holdings(ctx)
	if err != nil {
		return err
	}
	held := map[string]int{}
	for _, h := range hs {
		held[h.TradingSymbol] += h.Quantity
	}
	gtts, _ := d.trader.GTTs(ctx)
	gttByID := map[string]models.GTTInfo{}
	for _, g := range gtts {
		gttByID[g.ID] = g
	}
	snap := e.store.Snapshot()
	for _, p := range snap.SortedPositions() {
		if held[p.Symbol] < p.Quantity {
			// Sold while the engine was not watching (GTT fired, or you sold manually).
			px := p.Stop
			if g, ok := gttByID[p.GTTID]; ok && g.Status == models.GTTTriggered && g.LimitPrice > 0 {
				px = math.Min(p.Stop, g.TriggerPrice)
			}
			if bp, ok := e.exitPriceFromBook(ctx, d, p); ok {
				px = bp
			}
			e.log.Warn("position no longer held at broker — booking the exit", "symbol", p.Symbol, "held", held[p.Symbol], "book", p.Quantity, "price", px)
			e.closePosition(p.Symbol, px, d.day, "sold at broker (stop GTT or manual)")
			continue
		}
		g, ok := gttByID[p.GTTID]
		if p.GTTID == "" || !ok || g.Status != models.GTTActive {
			e.log.Warn("stop GTT missing or inactive — re-arming", "symbol", p.Symbol, "gtt", p.GTTID)
			e.armStop(ctx, d, p.Symbol)
		}
	}
	return nil
}

// exitPriceFromBook finds today's untagged CNC sell for the symbol (a GTT
// order) and returns its average price.
func (e *Engine) exitPriceFromBook(ctx context.Context, d *dayCtx, p *models.Position) (float64, bool) {
	ords, err := d.trader.Orders(ctx)
	if err != nil {
		return 0, false
	}
	for i := len(ords) - 1; i >= 0; i-- {
		o := ords[i]
		if o.TradingSymbol == p.Symbol && o.TransactionType == models.TxnSell && o.Status == models.StatusComplete && o.AveragePrice > 0 {
			return o.AveragePrice, true
		}
	}
	return 0, false
}

// armStop places a GTT for a position that has none.
func (e *Engine) armStop(ctx context.Context, d *dayCtx, sym string) {
	var p models.Position
	e.store.Read(func(s *swing.State) {
		if x := s.Positions[sym]; x != nil {
			p = *x
		}
	})
	if p.Symbol == "" {
		return
	}
	req := e.gttRequest(&p, p.Stop)
	r := d.om.Do(ctx, &models.OrderPayload{Action: models.ActionGTTPlace, Priority: models.PriorityStop,
		InstrumentToken: p.InstrumentToken, Request: req, MaxAttempts: 2})
	if r.Err != nil {
		e.log.Error("could not place stop GTT — will retry every minute while the market is open", "symbol", sym, "err", r.Err)
		return
	}
	_ = e.store.Update(func(s *swing.State) {
		if x := s.Positions[sym]; x != nil {
			x.GTTID, x.GTTTrigger = r.BrokerOrderID, req.TriggerPrice
		}
	})
	e.log.Info("stop GTT armed", "symbol", sym, "trigger", req.TriggerPrice, "limit", req.Price, "gtt", r.BrokerOrderID)
}

// closePosition books a trade and frees cash.
func (e *Engine) closePosition(sym string, px float64, day time.Time, reason string) {
	var tr models.Trade
	_ = e.store.Update(func(s *swing.State) {
		p := s.Positions[sym]
		if p == nil {
			return
		}
		tr = swing.CloseTrade(p, px, day, reason, e.strat.Costs)
		s.Cash += px*float64(p.Quantity) - (tr.Costs - p.EntryCosts)
		s.Realized += tr.Net
		delete(s.Positions, sym)
		cd := day
		for i := 0; i < e.cfg.Strategy.CooldownBars; i++ {
			cd = e.sess.NextTradingDay(cd)
		}
		s.Cooldown[sym] = swing.DateKey(cd)
	})
	if tr.Symbol == "" {
		return
	}
	if err := e.journal.Write(tr); err != nil {
		e.log.Error("journal write failed", "err", err)
	}
	e.log.Info("TRADE CLOSED", "symbol", sym, "qty", tr.Quantity, "entry", round2(tr.EntryPrice), "exit", round2(px),
		"net", round2(tr.Net), "r", round2(tr.RMultiple), "days", tr.BarsHeld, "reason", reason)
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// sell exits a position at market: delete its GTT first so the stop and the
// exit cannot both fill, then sell and book the trade.
func (e *Engine) sell(ctx context.Context, d *dayCtx, sym, reason string, prio models.Priority) error {
	var p models.Position
	e.store.Read(func(s *swing.State) {
		if x := s.Positions[sym]; x != nil {
			p = *x
		}
	})
	if p.Symbol == "" {
		return nil
	}
	if p.GTTID != "" {
		r := d.om.Do(ctx, &models.OrderPayload{Action: models.ActionGTTDelete, Priority: prio,
			InstrumentToken: p.InstrumentToken, BrokerOrderID: p.GTTID})
		if r.Err != nil {
			// The GTT may have triggered already: check whether we still hold the shares.
			if hs, err := d.trader.Holdings(ctx); err == nil {
				q := 0
				for _, h := range hs {
					if h.TradingSymbol == sym {
						q += h.Quantity
					}
				}
				if q < p.Quantity {
					px, ok := e.exitPriceFromBook(ctx, d, &p)
					if !ok {
						px = p.Stop
					}
					e.closePosition(sym, px, d.day, "stop GTT filled")
					return nil
				}
			}
			e.log.Warn("GTT delete failed — selling anyway", "symbol", sym, "err", r.Err)
		}
	}
	r := d.om.Do(ctx, &models.OrderPayload{Action: models.ActionPlace, Priority: prio, InstrumentToken: p.InstrumentToken,
		Request: models.OrderRequest{InstrumentToken: p.InstrumentToken, Exchange: "NSE", TradingSymbol: sym,
			TransactionType: models.TxnSell, OrderType: models.OrderMarket, Product: models.ProductCNC,
			Validity: models.ValidityDay, Quantity: p.Quantity, MarketProtection: e.cfg.Orders.MarketProtection,
			Tag: models.TagForToken(p.InstrumentToken)}})
	if r.Err != nil {
		return fmt.Errorf("sell %s: %w", sym, r.Err)
	}
	u, err := e.waitOrder(ctx, d, r.BrokerOrderID, e.cfg.Orders.FillTimeout)
	if err != nil || u.Status != models.StatusComplete {
		return fmt.Errorf("sell %s not filled: %v (status %s) — check Kite", sym, err, u.Status)
	}
	e.closePosition(sym, u.AveragePrice, d.day, reason)
	return nil
}

// ---------------------------------------------------------------------------
// Morning
// ---------------------------------------------------------------------------

func (e *Engine) morning(ctx context.Context, d *dayCtx) error {
	snap := e.store.Snapshot()
	syms := []string{}
	for s := range snap.Positions {
		syms = append(syms, s)
	}
	if snap.PendingFor == swing.DateKey(d.day) {
		for _, sg := range snap.Pending {
			syms = append(syms, sg.Symbol)
		}
	}
	qs := e.quotes(ctx, d, syms)

	// 1. Exits: flagged last evening, or gapped through the stop.
	for _, p := range snap.SortedPositions() {
		q, ok := qs[key(p.Symbol)]
		switch {
		case p.PendingExit != "":
			e.log.Info("planned exit", "symbol", p.Symbol, "reason", p.PendingExit)
			if err := e.sell(ctx, d, p.Symbol, p.PendingExit, models.PriorityStop); err != nil {
				e.log.Error("planned exit failed", "err", err)
			}
		case ok && q.LastPrice > 0 && q.LastPrice <= p.Stop:
			e.log.Warn("GAPPED THROUGH STOP — selling at market", "symbol", p.Symbol, "ltp", q.LastPrice, "stop", p.Stop)
			if err := e.sell(ctx, d, p.Symbol, "gapped below stop", models.PriorityEmergency); err != nil {
				e.log.Error("gap exit failed", "err", err)
			}
		}
	}

	// 2. Entries.
	snap = e.store.Snapshot()
	if snap.PendingFor == swing.DateKey(d.day) {
		e.enter(ctx, d, snap, qs)
	}
	return e.store.Update(func(s *swing.State) {
		s.LastMorning = swing.DateKey(d.day)
		s.Pending = nil
	})
}

func (e *Engine) enter(ctx context.Context, d *dayCtx, snap swing.State, qs map[string]models.Quote) {
	if dd := e.cfg.Risk.DrawdownPausePct; dd > 0 && snap.Equity() < snap.PeakEquity*(1-indicators.Pct(dd)) {
		e.log.Warn("drawdown pause active — no new entries", "equity", round2(snap.Equity()), "peak", round2(snap.PeakEquity))
		return
	}
	brokerCash, err := d.trader.AvailableCash(ctx)
	if err != nil {
		e.log.Warn("cash check failed — skipping entries", "err", err)
		return
	}
	newToday := 0
	for _, sg := range snap.Pending {
		cur := e.store.Snapshot()
		if len(cur.Positions) >= e.cfg.Risk.MaxPositions || newToday >= e.cfg.Risk.MaxNewPerDay {
			break
		}
		if _, held := cur.Positions[sg.Symbol]; held {
			continue
		}
		q, ok := qs[key(sg.Symbol)]
		if !ok || q.LastPrice <= 0 {
			e.log.Warn("entry skipped: no quote", "symbol", sg.Symbol)
			continue
		}
		ask := q.BestAsk
		if ask <= 0 {
			ask = q.LastPrice
		}
		switch {
		case q.LastPrice > sg.Close*(1+indicators.Pct(e.cfg.Strategy.MaxGapUpPct)):
			e.log.Info("entry skipped: gapped up too far", "symbol", sg.Symbol, "ltp", q.LastPrice, "signal_close", sg.Close)
			continue
		case q.LastPrice <= sg.Stop:
			e.log.Info("entry skipped: opened below the stop", "symbol", sg.Symbol, "ltp", q.LastPrice, "stop", sg.Stop)
			continue
		case q.UpperCircuit > 0 && ask >= q.UpperCircuit*0.995:
			e.log.Info("entry skipped: at upper circuit", "symbol", sg.Symbol)
			continue
		}
		cash := math.Min(cur.Cash, brokerCash)
		qty := swing.Size(cur.Equity(), cash, ask, sg.Stop, e.cfg.Risk, e.strat.Costs)
		if qty < 1 {
			e.log.Info("entry skipped: size is zero (cash or risk budget)", "symbol", sg.Symbol)
			continue
		}
		limit := indicators.RoundToTick(ask*(1+indicators.Pct(e.cfg.Orders.EntryLimitBufferPct)), tick, 1)
		r := d.om.Do(ctx, &models.OrderPayload{Action: models.ActionPlace, Priority: models.PriorityEntry, InstrumentToken: sg.InstrumentToken,
			Request: models.OrderRequest{InstrumentToken: sg.InstrumentToken, Exchange: "NSE", TradingSymbol: sg.Symbol,
				TransactionType: models.TxnBuy, OrderType: models.OrderLimit, Product: models.ProductCNC,
				Validity: models.ValidityDay, Quantity: qty, Price: limit, Tag: models.TagForToken(sg.InstrumentToken)}})
		if r.Err != nil {
			e.log.Warn("entry order failed", "symbol", sg.Symbol, "err", r.Err)
			continue
		}
		u, werr := e.waitOrder(ctx, d, r.BrokerOrderID, e.cfg.Orders.FillTimeout)
		if werr != nil && !u.IsTerminal() {
			_ = d.om.Do(ctx, &models.OrderPayload{Action: models.ActionCancel, Priority: models.PriorityStop,
				InstrumentToken: sg.InstrumentToken, BrokerOrderID: r.BrokerOrderID})
			u, _ = e.waitOrder(ctx, d, r.BrokerOrderID, 10*time.Second)
		}
		if u.FilledQuantity < 1 {
			e.log.Info("entry not filled", "symbol", sg.Symbol, "status", u.Status)
			continue
		}
		fill := u.AveragePrice
		n := u.FilledQuantity
		bc := e.strat.Costs.Buy(fill * float64(n))
		_ = e.store.Update(func(s *swing.State) {
			s.Cash -= fill*float64(n) + bc
			s.Positions[sg.Symbol] = &models.Position{InstrumentToken: sg.InstrumentToken, Symbol: sg.Symbol, Quantity: n,
				EntryPrice: fill, EntryDate: d.day, Setup: sg.Setup, InitialStop: sg.Stop, Stop: sg.Stop,
				Stage: models.StageInitial, HighestClose: fill, LastPrice: fill, LastClose: fill, EntryCosts: bc}
		})
		brokerCash -= fill*float64(n) + bc
		newToday++
		e.log.Info("BOUGHT", "symbol", sg.Symbol, "qty", n, "price", round2(fill), "stop", round2(sg.Stop),
			"risk", round2((fill-sg.Stop)*float64(n)), "setup", sg.Setup)
		e.armStop(ctx, d, sg.Symbol)
	}
}

// ---------------------------------------------------------------------------
// Monitor
// ---------------------------------------------------------------------------

func (e *Engine) monitor(ctx context.Context, d *dayCtx, until time.Time) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		e.monitorOnce(ctx, d)
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if !now.Before(until) {
				e.monitorOnce(ctx, d)
				return
			}
		}
	}
}

func (e *Engine) monitorOnce(ctx context.Context, d *dayCtx) {
	snap := e.store.Snapshot()
	if len(snap.Positions) == 0 {
		return
	}
	syms := make([]string, 0, len(snap.Positions))
	for s := range snap.Positions {
		syms = append(syms, s)
	}
	qs := e.quotes(ctx, d, syms)
	_ = e.store.Update(func(s *swing.State) {
		for sym, p := range s.Positions {
			if q, ok := qs[key(sym)]; ok && q.LastPrice > 0 {
				p.LastPrice = q.LastPrice
			}
		}
	})
	hs, err := d.trader.Holdings(ctx)
	if err != nil {
		return
	}
	held := map[string]int{}
	for _, h := range hs {
		held[h.TradingSymbol] += h.Quantity
	}
	for _, p := range snap.SortedPositions() {
		if held[p.Symbol] < p.Quantity {
			px, ok := e.exitPriceFromBook(ctx, d, p)
			if !ok {
				px = p.Stop
			}
			e.log.Info("stop GTT filled", "symbol", p.Symbol, "price", px)
			e.closePosition(p.Symbol, px, d.day, "stop hit ("+string(p.Stage)+")")
			continue
		}
		if p.GTTID == "" {
			e.armStop(ctx, d, p.Symbol)
		}
		if q, ok := qs[key(p.Symbol)]; ok && p.GTTID == "" && q.LastPrice > 0 && q.LastPrice <= p.Stop {
			e.log.Error("price below stop and no GTT armed — selling at market", "symbol", p.Symbol)
			_ = e.sell(ctx, d, p.Symbol, "stop hit (no GTT)", models.PriorityEmergency)
		}
	}
}

// ---------------------------------------------------------------------------
// Evening
// ---------------------------------------------------------------------------

func (e *Engine) evening(ctx context.Context, d *dayCtx, day time.Time) error {
	syms, warns, err := data.LoadUniverse(e.cfg.Paths.UniverseFile)
	for _, w := range warns {
		e.log.Warn("universe", "note", w)
	}
	if err != nil {
		return fmt.Errorf("universe: %w", err)
	}
	nse, err := e.data.Instruments(ctx, clock.Midnight(e.clk.Now()))
	if err != nil {
		return fmt.Errorf("instruments: %w", err)
	}
	snap := e.store.Snapshot()
	for s := range snap.Positions { // always manage held stocks even if removed from the universe
		syms = append(syms, s)
	}
	ins, rwarns := data.Resolve(uniq(syms), nse)
	for _, w := range rwarns {
		e.log.Warn("universe", "note", w)
	}
	idxIn, ok := data.FindIndex(nse, e.cfg.Market.Index)
	if !ok {
		return fmt.Errorf("index %q not found in instruments", e.cfg.Market.Index)
	}
	from := day.AddDate(0, 0, -historyDays)
	e.setStage("evening", fmt.Sprintf("Loading daily candles for %d stocks + %s.", len(ins), e.cfg.Market.Index))
	idxBars, err := e.data.Daily(ctx, idxIn.InstrumentToken, from, day)
	if err != nil {
		return fmt.Errorf("index candles: %w", err)
	}
	idx := swing.NewSeries(idxBars, e.cfg.Strategy)
	j, ok := idx.IndexOn(day)
	if !ok {
		return fmt.Errorf("no %s candle for %s yet — is the day's data published?", e.cfg.Market.Index, swing.DateKey(day))
	}
	series := map[string]*swing.Series{}
	tokens := map[string]uint32{}
	for n, in := range ins {
		bars, err := e.data.Daily(ctx, in.InstrumentToken, from, day)
		if err != nil {
			e.log.Warn("candles unavailable", "symbol", in.TradingSymbol, "err", err)
			continue
		}
		series[in.TradingSymbol] = swing.NewSeries(bars, e.cfg.Strategy)
		tokens[in.TradingSymbol] = in.InstrumentToken
		if n%10 == 9 {
			e.setStage("evening", fmt.Sprintf("Loaded candles %d/%d.", n+1, len(ins)))
		}
	}

	// 1. Positions: paper stop replay, then ratchet stops and flag exits.
	for _, p := range snap.SortedPositions() {
		s := series[p.Symbol]
		if s == nil {
			continue
		}
		i, ok := s.IndexOn(day)
		if !ok {
			continue
		}
		if e.paper != nil && p.GTTID != "" {
			e.paper.ApplyDailyBar(p.InstrumentToken, s.Bars[i])
		}
	}
	if err := e.reconcile(ctx, d); err != nil {
		e.log.Warn("reconcile failed", "err", err)
	}
	snap = e.store.Snapshot()
	for _, p0 := range snap.SortedPositions() {
		s := series[p0.Symbol]
		if s == nil {
			continue
		}
		i, ok := s.IndexOn(day)
		if !ok {
			continue
		}
		p := *p0
		oldStop := p.Stop
		reason := e.strat.Manage(&p, s, i)
		p.LastPrice = s.Bars[i].Close
		p.PendingExit = reason
		_ = e.store.Update(func(st *swing.State) { st.Positions[p.Symbol] = &p })
		if reason != "" {
			e.log.Info("EXIT FLAGGED for the morning run", "symbol", p.Symbol, "reason", reason)
			continue
		}
		if p.Stop > oldStop+tick/2 && p.GTTID != "" {
			req := e.gttRequest(&p, p.Stop)
			r := d.om.Do(ctx, &models.OrderPayload{Action: models.ActionGTTModify, Priority: models.PriorityStop,
				InstrumentToken: p.InstrumentToken, BrokerOrderID: p.GTTID, Request: req, MaxAttempts: 2})
			if r.Err != nil {
				e.log.Warn("GTT modify failed — the previous stop stays active", "symbol", p.Symbol, "err", r.Err)
			} else {
				_ = e.store.Update(func(st *swing.State) {
					if x := st.Positions[p.Symbol]; x != nil {
						x.GTTTrigger = req.TriggerPrice
					}
				})
				e.log.Info("stop raised", "symbol", p.Symbol, "from", round2(oldStop), "to", req.TriggerPrice, "stage", p.Stage)
			}
		}
	}

	// 2. Regime and scan.
	regime := !e.cfg.Market.RegimeFilter || e.strat.RegimeOK(idx, j)
	regimeText := fmt.Sprintf("%s close %.2f vs EMA%d %.2f", e.cfg.Market.Index, idx.Close[j], e.cfg.Strategy.EMASlow, idx.EMASlow[j])
	var rows []swing.ScanRow
	var sigs []models.Signal
	snap = e.store.Snapshot()
	for _, in := range ins {
		sym := in.TradingSymbol
		s := series[sym]
		if s == nil {
			continue
		}
		i, ok := s.IndexOn(day)
		if !ok {
			rows = append(rows, swing.ScanRow{Symbol: sym, Reason: "no candle for today"})
			continue
		}
		row := swing.ScanRow{Symbol: sym, Close: s.Close[i], RS: round2(e.strat.RelativeStrength(s, i, idx, j))}
		switch {
		case snap.Positions[sym] != nil:
			row.Reason = "held"
		case snap.Cooldown[sym] > swing.DateKey(day):
			row.Reason = "cooldown until " + snap.Cooldown[sym]
		default:
			sig, ok, why := e.strat.Evaluate(sym, tokens[sym], s, i, idx, j)
			row.Reason = why
			if ok {
				row.Signal, row.Setup = true, string(sig.Setup)
				if regime {
					sigs = append(sigs, sig)
				} else {
					row.Reason += " — not taken: market regime is risk-off"
				}
			}
		}
		rows = append(rows, row)
	}
	sort.SliceStable(sigs, func(a, b int) bool { return sigs[a].Score > sigs[b].Score })
	sort.SliceStable(rows, func(a, b int) bool {
		if rows[a].Signal != rows[b].Signal {
			return rows[a].Signal
		}
		return rows[a].RS > rows[b].RS
	})
	next := e.sess.NextTradingDay(day)
	err = e.store.Update(func(st *swing.State) {
		st.Pending, st.PendingFor, st.Scanned = sigs, swing.DateKey(next), rows
		st.LastEvening = swing.DateKey(day)
		if regime {
			st.Regime = "RISK-ON: " + regimeText
		} else {
			st.Regime = "RISK-OFF: " + regimeText
		}
	})
	e.log.Info("evening run complete", "positions", len(snap.Positions), "candidates", len(sigs), "for", swing.DateKey(next), "regime", regime)
	return err
}

func uniq(v []string) []string {
	seen := map[string]bool{}
	out := v[:0:0]
	for _, s := range v {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

var _ broker.Trader = (*broker.Paper)(nil)
