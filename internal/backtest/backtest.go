// Package backtest replays the swing strategy over historical daily candles
// using the same rules the live engine runs (internal/swing).
//
// Day loop (for every index trading day d):
//
//  1. OPEN   — sell positions flagged for exit last evening at d's open;
//     buy last evening's candidates at d's open (skipping gaps above
//     max_gap_up_pct or below the stop), best relative strength first.
//  2. INTRADAY — a position whose stop is hit sells at the stop, or at the
//     open if the stock gapped below it (gaps are not forgiven).
//  3. CLOSE  — ratchet stops, flag exits, mark to market, scan for tomorrow.
//
// Fills include slippage; every buy and sell pays Zerodha delivery charges.
package backtest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/nkalva/kitealgo/internal/config"
	"github.com/nkalva/kitealgo/internal/data"
	"github.com/nkalva/kitealgo/internal/indicators"
	"github.com/nkalva/kitealgo/internal/swing"
	"github.com/nkalva/kitealgo/pkg/models"
)

// Instrument is one symbol's history.
type Instrument struct {
	Symbol string
	Token  uint32
	Bars   []models.Bar
}

// Input configures a run.
type Input struct {
	Instruments []Instrument
	Index       []models.Bar // market index (for the calendar, regime filter and RS)
	From, To    time.Time    // simulation window (history before From is used for warmup)
	Config      config.Config
	// Holdings mode, market_check category: extra index histories by name
	// (holdings.midcap_index / smallcap_index) and each symbol's category
	// (large | mid | small; unlisted symbols count as large).
	Indices map[string][]models.Bar
	Caps    map[string]string
	// Prepared, when set, supplies precomputed indicator series (see Prepare).
	Prepared *Prepared
}

// EquityPoint is one day of the equity curve.
type EquityPoint struct {
	Date      time.Time `json:"date"`
	Equity    float64   `json:"equity"`
	Cash      float64   `json:"cash"`
	Positions int       `json:"positions"`
	Benchmark float64   `json:"benchmark"` // index rebased to starting capital
}

// Summary holds headline statistics.
type Summary struct {
	From, To          time.Time
	StartEquity       float64 `json:"start_equity"`
	EndEquity         float64 `json:"end_equity"`
	TotalReturnPct    float64 `json:"total_return_pct"`
	CAGRPct           float64 `json:"cagr_pct"`
	MaxDrawdownPct    float64 `json:"max_drawdown_pct"`
	MaxDrawdownFrom   time.Time
	MaxDrawdownTo     time.Time
	BenchmarkReturnPc float64 `json:"benchmark_return_pct"`
	BenchmarkCAGRPct  float64 `json:"benchmark_cagr_pct"`
	Trades            int     `json:"trades"`
	Wins              int     `json:"wins"`
	WinRatePct        float64 `json:"win_rate_pct"`
	AvgWinPct         float64 `json:"avg_win_pct"`
	AvgLossPct        float64 `json:"avg_loss_pct"`
	ProfitFactor      float64 `json:"profit_factor"`
	ExpectancyR       float64 `json:"expectancy_r"`
	AvgBarsHeld       float64 `json:"avg_days_held"`
	ExposurePct       float64 `json:"exposure_pct"`
	TotalCosts        float64 `json:"total_costs"`
	BestTradePct      float64 `json:"best_trade_pct"`
	WorstTradePct     float64 `json:"worst_trade_pct"`
	SignalsSkipped    int     `json:"signals_skipped"`
	Symbols           int     `json:"symbols"`
	NetProfit         float64 `json:"net_profit"`       // end − start equity, ₹
	AvgInvestedPct    float64 `json:"avg_invested_pct"` // average share of equity in stocks (the rest is idle cash)
	Yearly            []Year  `json:"yearly"`           // calendar-year returns (compounded into the total)
	ByResearch        []Group `json:"by_research"`      // results per research tag at entry
}

// Pool is the auto universe chosen on one date (strongest first).
type Pool struct {
	Date     time.Time `json:"date"`
	Eligible int       `json:"eligible"` // stocks passing price and liquidity that day
	Symbols  []string  `json:"symbols"`
}

// pickPool ranks every stock with a bar on d that passes min_price and
// min_turnover_cr by its return over lookback sessions, using only data up
// to d, and keeps the top n.
func pickPool(insts []*inst, d time.Time, p config.StrategyConfig, n, lookback int) Pool {
	type cand struct {
		sym string
		ret float64
	}
	var cs []cand
	for _, x := range insts {
		i, ok := x.s.IndexOn(d)
		if !ok || i < lookback || x.s.Close[i-lookback] <= 0 {
			continue
		}
		c := x.s.Close[i]
		if c < p.MinPrice || c*x.s.VolAvg[i] < p.MinTurnoverCr*1e7 {
			continue
		}
		cs = append(cs, cand{x.Symbol, c/x.s.Close[i-lookback] - 1})
	}
	sort.Slice(cs, func(a, b int) bool { return cs[a].ret > cs[b].ret })
	pl := Pool{Date: d, Eligible: len(cs)}
	for k := 0; k < n && k < len(cs); k++ {
		pl.Symbols = append(pl.Symbols, cs[k].sym)
	}
	return pl
}

// everLiquid reports whether the stock ever passes min_price and the
// average-turnover filter (volume_avg_period sessions) from `from` onwards.
func everLiquid(bars []models.Bar, p config.StrategyConfig, from time.Time) bool {
	n := p.VolumeAvgPeriod
	if n < 1 {
		n = 20
	}
	var vol float64
	for i, b := range bars {
		vol += b.Volume
		if i >= n {
			vol -= bars[i-n].Volume
		}
		if i >= n-1 && !b.Date.Before(from) && b.Close >= p.MinPrice && b.Close*vol/float64(n) >= p.MinTurnoverCr*1e7 {
			return true
		}
	}
	return false
}

// Group is the trade statistics of one research tag.
type Group struct {
	Key    string  `json:"key"`
	Trades int     `json:"trades"`
	Wins   int     `json:"wins"`
	Net    float64 `json:"net"`
}

// Year is one calendar year of the equity curve.
type Year struct {
	Year         int     `json:"year"`
	StartEquity  float64 `json:"start_equity"`
	EndEquity    float64 `json:"end_equity"`
	ReturnPct    float64 `json:"return_pct"`
	BenchmarkPct float64 `json:"benchmark_pct"`
	Trades       int     `json:"trades"` // trades closed in the year
	Net          float64 `json:"net"`    // ₹ gained in the year (incl. open positions)
}

// Result is the full output of a run.
type Result struct {
	Summary  Summary        `json:"summary"`
	Trades   []models.Trade `json:"trades"`
	Equity   []EquityPoint  `json:"equity"`
	Config   config.Config  `json:"-"`
	Skipped  map[string]int `json:"skipped_reasons"`
	Notes    []string       `json:"notes,omitempty"`
	Pools    []Pool         `json:"pools,omitempty"` // auto universe: the stocks scanned each month
	MTFCosts float64        `json:"mtf_costs"`  // interest + MTF brokerage + pledge fees
	Rules    string         `json:"rules"`      // human summary of the rules used
	RulesID  string         `json:"rules_hash"` // RulesHash of the config used
}

// RulesHash fingerprints the settings that change backtest results
// (strategy, risk, costs, market), so the UI can flag a stale result.
func RulesHash(c config.Config) string {
	raw, _ := json.Marshal(struct {
		S config.StrategyConfig
		R config.RiskConfig
		C config.CostsConfig
		M config.MarketConfig
		F config.MTFConfig
		H config.HoldingsConfig
		A config.AutoUniverseConfig
	}{c.Strategy, c.Risk, c.Costs, c.Market, c.MTF, c.Holdings, c.Backtest.AutoUniverse})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// DescribeRules is a one-line summary of the entry, stop and exit rules.
func DescribeRules(c config.Config) string {
	s := c.Strategy
	var parts []string
	switch s.Setups {
	case "ema_cross":
		f, sl := s.CrossFast, s.CrossSlow
		if f <= 0 {
			f = 10
		}
		if sl <= f {
			sl = 20
		}
		parts = append(parts, fmt.Sprintf("entry EMA%d/%d cross + Supertrend(%d, %g)", f, sl, s.SupertrendPeriod, s.SupertrendMult))
		switch s.EntryMode {
		case "trend":
			parts = append(parts, fmt.Sprintf("in trend (cross ≤ %dd old, ≤ %g%% above EMA%d)", s.TrendMaxDays, s.TrendMaxExtPct, sl))
		case "both":
			parts = append(parts, fmt.Sprintf("fresh cross first, then in trend (cross ≤ %dd old, ≤ %g%% above EMA%d)", s.TrendMaxDays, s.TrendMaxExtPct, sl))
		}
	case "", "both":
		parts = append(parts, "entry breakout + pullback")
	default:
		parts = append(parts, "entry "+s.Setups)
	}
	if s.HAEntry != "" && s.HAEntry != "off" {
		parts = append(parts, "HA "+s.HAEntry+" entry")
	}
	switch s.StopMode {
	case "supertrend":
		parts = append(parts, "stop Supertrend line")
	case "swing_low":
		parts = append(parts, fmt.Sprintf("stop %d-day swing low", s.SwingLowBars))
	default:
		parts = append(parts, fmt.Sprintf("stop %g×ATR", s.StopATRMult))
	}
	if !s.FixedStop {
		parts = append(parts, "ratchet +1R/+2R/trail")
	}
	if s.BreakevenAtR > 0 {
		parts = append(parts, fmt.Sprintf("breakeven +%gR", s.BreakevenAtR))
	}
	if s.TrailMode != "" && s.TrailMode != "off" {
		parts = append(parts, "trail "+s.TrailMode)
	}
	if s.TargetR > 0 {
		parts = append(parts, fmt.Sprintf("target +%gR (%g%%)", s.TargetR, s.PartialPct))
	}
	var exits []string
	if s.ExitOnEMACross {
		exits = append(exits, "EMA cross down")
	}
	if s.ExitBelowEMAFast {
		exits = append(exits, "close < EMA20")
	}
	if s.HAExit != "" && s.HAExit != "off" {
		exits = append(exits, "HA "+s.HAExit)
	}
	if s.MaxHoldBars > 0 {
		exits = append(exits, fmt.Sprintf("time stop %dd", s.MaxHoldBars))
	}
	if len(exits) > 0 {
		parts = append(parts, "exit "+strings.Join(exits, ", "))
	}
	if a := c.Backtest.AutoUniverse; a.Enabled {
		src := "all NSE shares"
		if a.Source == "file" {
			src = a.File
		}
		parts = append(parts, fmt.Sprintf("stocks: each month the %d strongest (%d-day return) from %s", a.TopN, a.LookbackDays, src))
	}
	if s.ResearchAllow != "" && !strings.EqualFold(s.ResearchAllow, "all") {
		parts = append(parts, "research only "+s.ResearchAllow)
	}
	if s.ResearchRank == "research" {
		parts = append(parts, "ranked by research tag")
	}
	if h := c.Holdings; h.Enabled {
		mc := "no market check"
		switch h.MarketCheck {
		case "nifty":
			mc = "market check " + c.Market.Index
		case "category":
			mc = "market check by category (" + c.Market.Index + " / " + h.MidcapIndex + " / " + h.SmallcapIndex + ")"
		}
		if h.MarketCheck != "off" && h.MarketCheck != "" && h.MarketHAGreen {
			mc += " + index HA green"
		}
		parts = append(parts, fmt.Sprintf("hold %d stocks, equal split, empty slots refilled daily", c.SlotCount()), mc)
	} else {
		parts = append(parts, fmt.Sprintf("risk %g%%/trade, max %d positions", c.Risk.RiskPerTradePct, c.Risk.MaxPositions))
	}
	if c.MTF.Enabled && c.MTF.Leverage > 1 {
		parts = append(parts, fmt.Sprintf("MTF %g× at %g%%/day", c.MTF.Leverage, c.MTF.InterestPctPerDay))
	}
	return strings.Join(parts, " · ")
}

type inst struct {
	Instrument
	s *swing.Series
}

// Prepared holds the indicator series of a run's instruments. Studies that
// run thousands of variants on the same history build it once with Prepare
// and share it (read-only) between runs, as long as the variants differ only
// in settings that do not change the indicators (EMA/ATR/Supertrend periods,
// cross_source, min_price/min_turnover_cr with the auto universe).
type Prepared struct {
	idx   *swing.Series
	insts []*inst
}

// Prepare computes the indicator series for in (with in.Config's strategy).
func Prepare(in Input) *Prepared {
	cfg := in.Config
	p := &Prepared{idx: swing.NewSeries(in.Index, cfg.Strategy)}
	for _, ins := range in.Instruments {
		if len(ins.Bars) == 0 {
			continue
		}
		if cfg.Backtest.AutoUniverse.Enabled && !everLiquid(ins.Bars, cfg.Strategy, in.From) {
			continue // never tradable in the window: skip the indicator work (thousands of small caps)
		}
		p.insts = append(p.insts, &inst{Instrument: ins, s: swing.NewSeries(ins.Bars, cfg.Strategy)})
	}
	return p
}

// Run executes the backtest.
func Run(in Input) Result {
	cfg := in.Config
	st := swing.NewStrategy(cfg.Strategy, cfg.Costs)
	res := Result{Config: cfg, Skipped: map[string]int{}, Rules: DescribeRules(cfg), RulesID: RulesHash(cfg)}
	slip := indicators.Pct(cfg.Costs.SlippagePct)
	p := in.Prepared
	if p == nil {
		p = Prepare(in)
	}
	idx, insts := p.idx, p.insts
	bySym := make(map[string]*inst, len(insts))
	for _, x := range insts {
		bySym[x.Symbol] = x
	}
	find := func(sym string) *inst { return bySym[sym] }

	// Holdings mode: N equal slots, refilled every morning without a daily cap.
	hold := cfg.Holdings.Enabled
	maxPos, maxNew := cfg.Risk.MaxPositions, cfg.Risk.MaxNewPerDay
	slots := cfg.SlotCount()
	if hold {
		maxPos, maxNew = slots, slots
	}
	// Market check per stock: its category's index (NIFTY 50 when the
	// category's index history is missing).
	market := map[string]*swing.Series{data.CapLarge: idx, data.CapMid: idx, data.CapSmall: idx}
	marketName := map[string]string{data.CapLarge: cfg.Market.Index, data.CapMid: cfg.Market.Index, data.CapSmall: cfg.Market.Index}
	mc := cfg.Holdings.MarketCheck
	if hold && mc == "category" {
		for c, name := range map[string]string{data.CapMid: cfg.Holdings.MidcapIndex, data.CapSmall: cfg.Holdings.SmallcapIndex} {
			if bars := in.Indices[name]; len(bars) > 0 {
				market[c], marketName[c] = swing.NewSeries(bars, cfg.Strategy), name
			} else if name != "" {
				res.Notes = append(res.Notes, fmt.Sprintf("no history for %s — %s-cap stocks were checked against %s", name, c, cfg.Market.Index))
			}
		}
	}
	capOf := func(sym string) string {
		if c, ok := in.Caps[sym]; ok {
			return c
		}
		return data.CapLarge
	}
	marketOK := func(ser *swing.Series, d time.Time) bool {
		j, ok := ser.IndexOn(d)
		if !ok {
			return false
		}
		return st.RegimeOK(ser, j) && (!cfg.Holdings.MarketHAGreen || ser.HAGreen(j))
	}

	auto := cfg.Backtest.AutoUniverse
	var pool map[string]bool // nil: scan every instrument
	poolMonth := -1

	cash := cfg.Risk.Capital
	peak := cash
	pauseUntil := 0 // index bar before which entries are paused (drawdown breaker)
	positions := map[string]*models.Position{}
	cooldown := map[string]int{} // symbol → index-bar number when re-entry is allowed
	var pending []models.Signal
	var benchBase float64
	exposedDays, days := 0, 0

	// MTF: borrowed amount and extra MTF charges (interest, brokerage,
	// pledge) per open position.
	mtf := cfg.MTF
	lev := 1.0
	if mtf.Enabled && mtf.Leverage > 1 {
		lev = mtf.Leverage
	}
	gst := 1 + indicators.Pct(cfg.Costs.GSTPct)
	mtfBrokerage := func(v float64) float64 { return math.Min(v*indicators.Pct(mtf.BrokeragePct), mtf.BrokerageMax) * gst }
	debt := map[string]float64{}
	extra := map[string]float64{}
	totalDebt := func() float64 {
		var t float64
		for _, v := range debt {
			t += v
		}
		return t
	}
	equityNow := func() float64 { return cash + markValue(positions) - totalDebt() }
	var prevDay time.Time

	sell := func(p *models.Position, px float64, d time.Time, reason string, k int) {
		t := swing.CloseTrade(p, px, d, reason, st.Costs)
		proceeds := px*float64(p.Quantity) - (t.Costs - p.EntryCosts)
		if b, ok := debt[p.Symbol]; ok {
			fee := mtfBrokerage(px*float64(p.Quantity)) + mtf.UnpledgeFee*gst
			proceeds -= b + fee
			extra[p.Symbol] += fee
			t.Costs += extra[p.Symbol]
			t.Net -= extra[p.Symbol]
			if risk := p.RiskPerShare() * float64(p.Quantity); risk > 0 {
				t.RMultiple = t.Net / risk
			}
			res.MTFCosts += extra[p.Symbol]
			delete(debt, p.Symbol)
			delete(extra, p.Symbol)
		}
		cash += proceeds
		res.Trades = append(res.Trades, t)
		delete(positions, p.Symbol)
		cooldown[p.Symbol] = k + cfg.Strategy.CooldownBars
	}

	for k, ib := range in.Index {
		d := ib.Date
		inWindow := !d.Before(in.From) && !d.After(in.To)
		if !inWindow {
			if d.After(in.To) {
				break
			}
			continue
		}
		if benchBase == 0 {
			benchBase = ib.Close
		}
		days++
		// MTF interest for the calendar days since the previous session.
		if !prevDay.IsZero() && len(debt) > 0 {
			nd := math.Round(d.Sub(prevDay).Hours() / 24)
			for sym, b := range debt {
				in := b * indicators.Pct(mtf.InterestPctPerDay) * nd
				cash -= in
				extra[sym] += in
			}
		}
		prevDay = d

		// 1. OPEN — planned exits, then entries.
		for _, p := range sortedPositions(positions) {
			if p.PendingExit == "" {
				continue
			}
			x := find(p.Symbol)
			if i, ok := x.s.IndexOn(d); ok {
				sell(p, x.s.Bars[i].Open*(1-slip), d, p.PendingExit, k)
			}
		}
		equityPrev := equityNow()
		newToday := 0
		for _, sig := range pending {
			if len(positions) >= maxPos || newToday >= maxNew {
				res.Skipped["no free slot"]++
				continue
			}
			if _, held := positions[sig.Symbol]; held {
				continue
			}
			x := find(sig.Symbol)
			i, ok := x.s.IndexOn(d)
			if !ok {
				res.Skipped["no bar next day"]++
				continue
			}
			o := x.s.Bars[i].Open
			if o > sig.Close*(1+indicators.Pct(cfg.Strategy.MaxGapUpPct)) {
				res.Skipped["gapped up too far"]++
				continue
			}
			if o <= sig.Stop {
				res.Skipped["gapped below stop"]++
				continue
			}
			fill := o * (1 + slip)
			qty := swing.Size(equityPrev, cash*lev, fill, sig.Stop, cfg.Risk, st.Costs)
			if hold { // equal split: equity ÷ slots per stock (compounds as equity grows)
				target := equityPrev / float64(slots)
				qty = int(math.Floor(math.Min(target, cash*lev) / (fill * (1 + st.Costs.BuyFrac()))))
				if float64(qty)*fill < target/4 {
					qty = 0 // not enough cash left for a meaningful position
				}
			}
			var own, fee float64
			for ; qty >= 1; qty-- { // own share of the cost must fit the cash
				v := fill * float64(qty)
				own, fee = v, 0
				if lev > 1 {
					bc := st.Costs.Buy(v)
					if mtf.BorrowOnlyShortfall && v+bc <= cash {
						own, fee = v, 0 // enough cash: a normal CNC buy
					} else {
						fee = mtfBrokerage(v) + mtf.PledgeFee*gst
						own = v / lev
						if mtf.BorrowOnlyShortfall {
							own = math.Max(own, math.Min(v, cash-bc-fee))
						}
					}
				}
				if own+st.Costs.Buy(v)+fee <= cash {
					break
				}
			}
			if qty < 1 {
				res.Skipped["size zero (cash/risk)"]++
				continue
			}
			bc := st.Costs.Buy(fill * float64(qty))
			cash -= own + bc + fee
			if lev > 1 && fill*float64(qty)-own > 0.01 {
				debt[sig.Symbol] = fill*float64(qty) - own
				extra[sig.Symbol] = fee
			}
			positions[sig.Symbol] = &models.Position{InstrumentToken: x.Token, Symbol: x.Symbol, Quantity: qty,
				EntryPrice: fill, EntryDate: d, Setup: sig.Setup, InitialStop: sig.Stop, Stop: sig.Stop,
				Stage: models.StageInitial, HighestClose: fill, EntryCosts: bc, Research: sig.Research}
			newToday++
		}
		pending = nil

		// 2. INTRADAY — stops.
		for _, p := range sortedPositions(positions) {
			x := find(p.Symbol)
			i, ok := x.s.IndexOn(d)
			if !ok {
				continue
			}
			b := x.s.Bars[i]
			switch {
			case b.Open <= p.Stop && !p.EntryDate.Equal(d):
				sell(p, b.Open*(1-slip), d, "gapped below stop", k)
			case b.Low <= p.Stop:
				sell(p, p.Stop*(1-slip), d, "stop hit ("+string(p.Stage)+")", k)
			case st.Target(p) > 0 && b.High >= st.Target(p):
				// Profit target (a limit sell): filled at the target, or at the
				// open if the stock gapped above it. Same-bar stop-and-target is
				// resolved as the stop above (conservative).
				px := math.Max(b.Open, st.Target(p)) * (1 - slip)
				part := int(math.Floor(float64(p.Quantity) * cfg.Strategy.PartialPct / 100))
				if cfg.Strategy.PartialPct <= 0 || cfg.Strategy.PartialPct >= 100 || part < 1 || p.Quantity-part < 1 {
					sell(p, px, d, fmt.Sprintf("target +%.1fR hit", cfg.Strategy.TargetR), k)
					break
				}
				booked := *p
				booked.Quantity = part
				booked.EntryCosts = p.EntryCosts * float64(part) / float64(p.Quantity)
				t := swing.CloseTrade(&booked, px, d, fmt.Sprintf("partial %.0f%% at target +%.1fR", cfg.Strategy.PartialPct, cfg.Strategy.TargetR), st.Costs)
				cash += px*float64(part) - (t.Costs - booked.EntryCosts)
				if b, ok := debt[p.Symbol]; ok { // repay the loan share of the part sold
					share := b * float64(part) / float64(p.Quantity)
					fee := mtfBrokerage(px*float64(part)) + mtf.UnpledgeFee*gst
					cash -= share + fee
					debt[p.Symbol] -= share
					t.Costs += fee
					t.Net -= fee
					res.MTFCosts += fee
				}
				res.Trades = append(res.Trades, t)
				p.Quantity -= part
				p.EntryCosts -= booked.EntryCosts
				p.PartialDone = true
				if be := p.EntryPrice * (1 + st.Costs.RoundTripFrac()); be > p.Stop {
					p.Stop, p.Stage = be, models.StageBreakeven // the rest can no longer lose
				}
			}
		}

		// 3. CLOSE — manage, mark, scan.
		for _, p := range sortedPositions(positions) {
			x := find(p.Symbol)
			i, ok := x.s.IndexOn(d)
			if !ok {
				continue
			}
			p.LastPrice = x.s.Bars[i].Close
			if reason := st.Manage(p, x.s, i); reason != "" {
				p.PendingExit = reason
			}
		}
		equity := equityNow()
		if equity > peak {
			peak = equity
		}
		if len(positions) > 0 {
			exposedDays++
		}
		res.Equity = append(res.Equity, EquityPoint{Date: d, Equity: equity, Cash: cash, Positions: len(positions),
			Benchmark: cfg.Risk.Capital * ib.Close / benchBase})

		j, _ := idx.IndexOn(d)
		regime := !cfg.Market.RegimeFilter || st.RegimeOK(idx, j)
		if hold { // the market check is per stock (below)
			regime = true
		}
		paused := false
		if cfg.Risk.DrawdownPausePct > 0 {
			switch {
			case pauseUntil > 0 && k < pauseUntil:
				paused = true
			case pauseUntil > 0:
				pauseUntil, peak = 0, equity // pause over: reset the peak and resume
			case equity < peak*(1-indicators.Pct(cfg.Risk.DrawdownPausePct)):
				pauseUntil, paused = k+cfg.Risk.PauseDays(), true
				res.Skipped["drawdown pauses"]++
			}
		}
		if auto.Enabled && int(d.Month()) != poolMonth { // first session of the month: re-pick the stocks to scan
			pl := pickPool(insts, d, cfg.Strategy, auto.TopN, auto.LookbackDays)
			pool, poolMonth = map[string]bool{}, int(d.Month())
			for _, s := range pl.Symbols {
				pool[s] = true
			}
			res.Pools = append(res.Pools, pl)
		}
		if regime && !paused {
			for _, x := range insts {
				if _, held := positions[x.Symbol]; held || cooldown[x.Symbol] > k {
					continue
				}
				if pool != nil && !pool[x.Symbol] {
					continue
				}
				i, ok := x.s.IndexOn(d)
				if !ok {
					continue
				}
				sig, ok, _ := st.Evaluate(x.Symbol, x.Token, x.s, i, idx, j)
				if !ok {
					continue
				}
				if hold && mc != "off" && mc != "" {
					c := data.CapLarge
					if mc == "category" {
						c = capOf(x.Symbol)
					}
					if !marketOK(market[c], d) {
						res.Skipped["market check: "+marketName[c]+" weak"]++
						continue
					}
				}
				pending = append(pending, sig)
			}
			swing.SortSignals(pending, cfg.Strategy)
		}
	}

	// Close whatever is still open at the last close (for complete statistics).
	if n := len(res.Equity); n > 0 {
		last := res.Equity[n-1].Date
		for _, p := range sortedPositions(positions) {
			sell(p, p.LastPrice*(1-slip), last, "end of backtest", len(in.Index))
		}
		res.Equity[n-1].Equity = cash
	}
	res.Summary = summarise(res, cfg, exposedDays, days, len(insts))
	return res
}


func sortedPositions(m map[string]*models.Position) []*models.Position {
	out := make([]*models.Position, 0, len(m))
	for _, p := range m {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}

func markValue(m map[string]*models.Position) float64 {
	var v float64
	for _, p := range m {
		px := p.LastPrice
		if px == 0 {
			px = p.EntryPrice
		}
		v += px * float64(p.Quantity)
	}
	return v
}

func summarise(r Result, cfg config.Config, exposed, days, symbols int) Summary {
	s := Summary{StartEquity: cfg.Risk.Capital, Symbols: symbols}
	if len(r.Equity) == 0 {
		return s
	}
	s.From, s.To = r.Equity[0].Date, r.Equity[len(r.Equity)-1].Date
	s.EndEquity = r.Equity[len(r.Equity)-1].Equity
	s.TotalReturnPct = (s.EndEquity/s.StartEquity - 1) * 100
	years := s.To.Sub(s.From).Hours() / 24 / 365.25
	if years > 0 && s.EndEquity > 0 {
		s.CAGRPct = (math.Pow(s.EndEquity/s.StartEquity, 1/years) - 1) * 100
	}
	b0, b1 := r.Equity[0].Benchmark, r.Equity[len(r.Equity)-1].Benchmark
	if b0 > 0 {
		s.BenchmarkReturnPc = (b1/cfg.Risk.Capital - 1) * 100
		if years > 0 {
			s.BenchmarkCAGRPct = (math.Pow(b1/cfg.Risk.Capital, 1/years) - 1) * 100
		}
	}
	peak, peakAt := r.Equity[0].Equity, r.Equity[0].Date
	for _, e := range r.Equity {
		if e.Equity > peak {
			peak, peakAt = e.Equity, e.Date
		}
		if dd := (1 - e.Equity/peak) * 100; dd > s.MaxDrawdownPct {
			s.MaxDrawdownPct, s.MaxDrawdownFrom, s.MaxDrawdownTo = dd, peakAt, e.Date
		}
	}
	var grossWin, grossLoss, sumR, sumBars, sumWinPct, sumLossPct float64
	s.WorstTradePct = math.Inf(1)
	s.BestTradePct = math.Inf(-1)
	for _, t := range r.Trades {
		s.Trades++
		pct := t.Net / (t.EntryPrice * float64(t.Quantity)) * 100
		if pct > s.BestTradePct {
			s.BestTradePct = pct
		}
		if pct < s.WorstTradePct {
			s.WorstTradePct = pct
		}
		if t.Net > 0 {
			s.Wins++
			grossWin += t.Net
			sumWinPct += pct
		} else {
			grossLoss += -t.Net
			sumLossPct += pct
		}
		sumR += t.RMultiple
		sumBars += float64(t.BarsHeld)
		s.TotalCosts += t.Costs
	}
	if s.Trades > 0 {
		s.WinRatePct = float64(s.Wins) / float64(s.Trades) * 100
		s.ExpectancyR = sumR / float64(s.Trades)
		s.AvgBarsHeld = sumBars / float64(s.Trades)
		if s.Wins > 0 {
			s.AvgWinPct = sumWinPct / float64(s.Wins)
		}
		if l := s.Trades - s.Wins; l > 0 {
			s.AvgLossPct = sumLossPct / float64(l)
		}
	} else {
		s.BestTradePct, s.WorstTradePct = 0, 0
	}
	if grossLoss > 0 {
		s.ProfitFactor = grossWin / grossLoss
	}
	if days > 0 {
		s.ExposurePct = float64(exposed) / float64(days) * 100
	}
	for _, n := range r.Skipped {
		s.SignalsSkipped += n
	}
	s.NetProfit = s.EndEquity - s.StartEquity
	var inv float64
	for _, e := range r.Equity {
		if e.Equity > 0 {
			inv += 1 - e.Cash/e.Equity
		}
	}
	s.AvgInvestedPct = inv / float64(len(r.Equity)) * 100
	s.Yearly = yearly(r, s.StartEquity)
	by := map[string]*Group{}
	for _, t := range r.Trades {
		k := t.Research
		if k == "" {
			k = "—"
		}
		g := by[k]
		if g == nil {
			g = &Group{Key: k}
			by[k] = g
		}
		g.Trades++
		g.Net += t.Net
		if t.Net > 0 {
			g.Wins++
		}
	}
	for _, k := range append(append([]string{}, swing.ResearchTags...), "—") {
		if g := by[k]; g != nil {
			s.ByResearch = append(s.ByResearch, *g)
		}
	}
	return s
}

// yearly splits the equity curve into calendar years; each year starts from
// the previous year's closing equity, so the yearly returns compound into the
// total return.
func yearly(r Result, start float64) []Year {
	var out []Year
	prevEq, prevBm := start, start
	for k, e := range r.Equity {
		y := e.Date.Year()
		if len(out) == 0 || out[len(out)-1].Year != y {
			if n := len(out); n > 0 {
				prevEq, prevBm = out[n-1].EndEquity, r.Equity[k-1].Benchmark
			}
			out = append(out, Year{Year: y, StartEquity: prevEq})
		}
		cur := &out[len(out)-1]
		cur.EndEquity = e.Equity
		cur.Net = cur.EndEquity - cur.StartEquity
		if cur.StartEquity > 0 {
			cur.ReturnPct = (cur.EndEquity/cur.StartEquity - 1) * 100
		}
		if prevBm > 0 {
			cur.BenchmarkPct = (e.Benchmark/prevBm - 1) * 100
		}
	}
	for _, t := range r.Trades {
		for k := range out {
			if out[k].Year == t.ExitDate.Year() {
				out[k].Trades++
			}
		}
	}
	return out
}
