// Package swing holds the pure swing-trading rules — indicator series, entry
// signals, stop management, position sizing and cost model. The backtester
// and the live engine call exactly the same functions, so what you backtest is
// what trades.
package swing

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/nkalva/kitealgo/internal/config"
	"github.com/nkalva/kitealgo/internal/indicators"
	"github.com/nkalva/kitealgo/pkg/models"
)

// Series is one instrument's daily bars with precomputed indicators.
type Series struct {
	Bars      []models.Bar
	Close     []float64
	EMAFast   []float64
	EMASlow   []float64
	ATR       []float64
	VolAvg    []float64 // average volume of the N bars ending at i
	PriorHigh []float64 // highest high of the N bars before i
	HAOpen    []float64 // Heikin-Ashi candles (signals only)
	HAHigh    []float64
	HALow     []float64
	HAClose   []float64
	CrossFast []float64 // EMA(ema_cross_fast), e.g. EMA10
	CrossSlow []float64 // EMA(ema_cross_slow), e.g. EMA20
	EMA200    []float64 // long-term trend (research tags)
	research  sync.Map  // bar → researchHit: tags depend only on the bars (and RS vs the same index), so studies share them
	STDir     []int8    // Supertrend direction: +1 green, −1 red
	STLine    []float64 // Supertrend line
	byDate    map[string]int
}

// NewSeries computes indicators over bars (oldest first).
func NewSeries(bars []models.Bar, p config.StrategyConfig) *Series {
	n := len(bars)
	s := &Series{Bars: bars, Close: make([]float64, n), byDate: make(map[string]int, n)}
	o, h, l, v := make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n)
	for i, b := range bars {
		s.Close[i], o[i], h[i], l[i], v[i] = b.Close, b.Open, b.High, b.Low, b.Volume
		s.byDate[DateKey(b.Date)] = i
	}
	s.EMAFast = indicators.EMA(s.Close, p.EMAFast)
	s.EMASlow = indicators.EMA(s.Close, p.EMASlow)
	s.EMA200 = indicators.EMA(s.Close, 200)
	s.ATR = indicators.ATR(h, l, s.Close, p.ATRPeriod)
	s.VolAvg = indicators.SMA(v, p.VolumeAvgPeriod)
	s.PriorHigh = indicators.PriorHighest(h, p.BreakoutLookback)
	s.HAOpen, s.HAHigh, s.HALow, s.HAClose = indicators.HeikinAshi(o, h, l, s.Close)
	cf, cs, sp, sm := crossParams(p)
	if p.CrossSource == "ha" { // EMA cross and Supertrend on Heikin-Ashi candles (signals only)
		s.CrossFast = indicators.EMA(s.HAClose, cf)
		s.CrossSlow = indicators.EMA(s.HAClose, cs)
		s.STDir, s.STLine = indicators.Supertrend(s.HAHigh, s.HALow, s.HAClose, sp, sm)
	} else {
		s.CrossFast = indicators.EMA(s.Close, cf)
		s.CrossSlow = indicators.EMA(s.Close, cs)
		s.STDir, s.STLine = indicators.Supertrend(h, l, s.Close, sp, sm)
	}
	return s
}

// crossParams returns the ema_cross settings (defaults for an older config).
func crossParams(p config.StrategyConfig) (fast, slow, stPeriod int, stMult float64) {
	fast, slow, stPeriod, stMult = p.CrossFast, p.CrossSlow, p.SupertrendPeriod, p.SupertrendMult
	if fast <= 0 {
		fast = 10
	}
	if slow <= fast {
		slow = 20
	}
	if stPeriod <= 0 {
		stPeriod = 10
	}
	if stMult <= 0 {
		stMult = 3
	}
	return
}

// crossUp reports whether the ema_cross condition holds at bar i:
// EMA(fast) above EMA(slow) and Supertrend green.
func (s *Series) crossUp(i int) bool {
	return s.CrossFast[i] > s.CrossSlow[i] && s.STDir[i] == 1
}

// HAGreen reports whether Heikin-Ashi candle i closed above its open.
func (s *Series) HAGreen(i int) bool { return s.HAClose[i] > s.HAOpen[i] }

// HARed reports whether Heikin-Ashi candle i closed below its open.
func (s *Series) HARed(i int) bool { return s.HAClose[i] < s.HAOpen[i] }

// haWick returns the lower and upper wick of HA candle i as a % of its range.
func (s *Series) haWick(i int) (lower, upper float64) {
	rng := s.HAHigh[i] - s.HALow[i]
	if rng <= 0 {
		return 0, 0
	}
	lo := math.Min(s.HAOpen[i], s.HAClose[i])
	hi := math.Max(s.HAOpen[i], s.HAClose[i])
	return (lo - s.HALow[i]) / rng * 100, (s.HAHigh[i] - hi) / rng * 100
}

// DateKey formats a bar date as YYYY-MM-DD.
func DateKey(t time.Time) string { return t.Format("2006-01-02") }

// IndexOn returns the bar index for a date.
func (s *Series) IndexOn(t time.Time) (int, bool) {
	i, ok := s.byDate[DateKey(t)]
	return i, ok
}

// Len returns the number of bars.
func (s *Series) Len() int { return len(s.Bars) }

// Strategy evaluates entries and manages positions.
type Strategy struct {
	P     config.StrategyConfig
	Costs Costs
}

// NewStrategy creates a strategy.
func NewStrategy(p config.StrategyConfig, c config.CostsConfig) *Strategy {
	return &Strategy{P: p, Costs: Costs{C: c}}
}

// Warmup is the number of bars needed before a signal can be evaluated.
func (st *Strategy) Warmup() int {
	p := st.P
	w := p.EMASlow + p.SlopeLookback
	_, cs, sp, _ := crossParams(p)
	for _, x := range []int{p.BreakoutLookback + 1, p.ATRPeriod + 1, p.VolumeAvgPeriod + 1, p.RSLookback + 1, cs + 1, sp + 2} {
		if x > w {
			w = x
		}
	}
	return w
}

// RegimeOK reports whether the index allows new longs at bar i: close above
// the slow EMA and the slow EMA not falling.
func (st *Strategy) RegimeOK(idx *Series, i int) bool {
	if idx == nil || i < st.P.EMASlow+st.P.SlopeLookback {
		return false
	}
	ok := idx.Close[i] > idx.EMASlow[i] && idx.EMASlow[i] >= idx.EMASlow[i-st.P.SlopeLookback]
	if ok && st.P.RegimeMode == "strict" {
		ok = idx.Close[i] > idx.EMAFast[i] && idx.EMAFast[i] > idx.EMASlow[i]
	}
	return ok
}

// RelativeStrength is the stock's RSLookback-bar % return minus the index's.
func (st *Strategy) RelativeStrength(s *Series, i int, idx *Series, j int) float64 {
	n := st.P.RSLookback
	if i < n || s.Close[i-n] <= 0 {
		return 0
	}
	sr := (s.Close[i]/s.Close[i-n] - 1) * 100
	if idx == nil || j < n || idx.Close[j-n] <= 0 {
		return sr
	}
	return sr - (idx.Close[j]/idx.Close[j-n]-1)*100
}

// Evaluate checks the entry rules on completed bar i. idx/j is the index
// series and its bar for the same date (idx may be nil). It returns the
// signal, or ok=false with the reason the stock was not a candidate.
//
// Trend filter (both setups): close > EMA50, EMA20 > EMA50, EMA50 rising,
// price ≥ min_price, average turnover ≥ min_turnover_cr.
//
//	BREAKOUT: close > prior 20-day high AND volume ≥ 1.5 × average.
//	PULLBACK: the low touched the 20 EMA (within 1%) in the last 3 bars, and
//	          today closed above the 20 EMA and above yesterday's high on
//	          ≥ average volume — the dip was bought.
func (st *Strategy) Evaluate(sym string, token uint32, s *Series, i int, idx *Series, j int) (models.Signal, bool, string) {
	p := st.P
	if i < st.Warmup() || i >= s.Len() {
		return models.Signal{}, false, "not enough history"
	}
	b := s.Bars[i]
	c := b.Close
	ef, es, atr := s.EMAFast[i], s.EMASlow[i], s.ATR[i]
	switch {
	case c < p.MinPrice:
		return models.Signal{}, false, fmt.Sprintf("price %.2f < %.0f", c, p.MinPrice)
	case c*s.VolAvg[i] < p.MinTurnoverCr*1e7:
		return models.Signal{}, false, fmt.Sprintf("turnover ₹%.1f cr < ₹%.0f cr", c*s.VolAvg[i]/1e7, p.MinTurnoverCr)
	case (p.Setups != "ema_cross" || p.CrossTrendFilter) && !(c > es && ef > es && es > s.EMASlow[i-p.SlopeLookback]):
		return models.Signal{}, false, "not in an uptrend (needs close and EMA20 above a rising EMA50)"
	case atr <= 0:
		return models.Signal{}, false, "ATR unavailable"
	}
	prevVolAvg := s.VolAvg[i-1]
	var kind models.SetupKind
	var why string
	allowBO, allowPB := p.Setups != "pullback" && p.Setups != "ema_cross", p.Setups != "breakout" && p.Setups != "ema_cross"
	if p.Setups == "ema_cross" {
		cf, cs, _, _ := crossParams(p)
		fresh := s.crossUp(i) && !s.crossUp(i-1)
		switch {
		case fresh && p.EntryMode != "trend":
			kind = models.SetupEMACross
			why = fmt.Sprintf("EMA%d %.2f crossed above EMA%d %.2f with Supertrend green (line %.2f)", cf, s.CrossFast[i], cs, s.CrossSlow[i], s.STLine[i])
		case s.crossUp(i) && (p.EntryMode == "trend" || p.EntryMode == "both"):
			age := 0 // sessions since the EMA/Supertrend condition turned true
			for k := i; k > 0 && s.crossUp(k-1); k-- {
				age++
			}
			ext := (c/s.CrossSlow[i] - 1) * 100
			switch {
			case age > p.TrendMaxDays:
				return models.Signal{}, false, fmt.Sprintf("in an EMA%d/%d uptrend, but the cross is %d days old (max %d)", cf, cs, age, p.TrendMaxDays)
			case ext > p.TrendMaxExtPct:
				return models.Signal{}, false, fmt.Sprintf("in an EMA%d/%d uptrend, but %.1f%% above EMA%d (max %.0f%%) — too stretched", cf, cs, ext, cs, p.TrendMaxExtPct)
			}
			kind = models.SetupTrend
			why = fmt.Sprintf("EMA%d above EMA%d with Supertrend green for %d days; close %.1f%% above EMA%d", cf, cs, age, ext, cs)
		case s.crossUp(i):
			return models.Signal{}, false, "EMA cross and Supertrend already bullish — entry only on the day it turns"
		default:
			return models.Signal{}, false, fmt.Sprintf("no entry: needs EMA%d above EMA%d and Supertrend green", cf, cs)
		}
	} else if allowBO && c > s.PriorHigh[i] && b.Volume >= p.BreakoutVolRatio*prevVolAvg {
		kind = models.SetupBreakout
		why = fmt.Sprintf("close %.2f > %d-day high %.2f on %.1f× volume", c, p.BreakoutLookback, s.PriorHigh[i], b.Volume/prevVolAvg)
	} else if allowPB {
		touched := false
		for k := i - p.PullbackLookback + 1; k <= i; k++ {
			if s.Bars[k].Low <= s.EMAFast[k]*(1+indicators.Pct(p.PullbackTolPct)) {
				touched = true
				break
			}
		}
		if touched && c > ef && c > s.Bars[i-1].High && b.Volume >= p.PullbackVolRatio*prevVolAvg {
			kind = models.SetupPullback
			why = fmt.Sprintf("pullback to EMA20 %.2f bought: close %.2f > prior high %.2f", ef, c, s.Bars[i-1].High)
		}
	}
	if kind == "" {
		return models.Signal{}, false, "uptrend, but no breakout or pullback setup today"
	}
	switch p.HAEntry {
	case "green", "strong":
		if !s.HAGreen(i) {
			return models.Signal{}, false, string(kind) + " setup, but the Heikin-Ashi candle is not green"
		}
		if lw, _ := s.haWick(i); p.HAEntry == "strong" && lw > st.wickPct() {
			return models.Signal{}, false, fmt.Sprintf("%s setup, but the Heikin-Ashi candle has a %.0f%% lower wick (not a strong candle)", kind, lw)
		}
		why += "; Heikin-Ashi confirms"
	}
	stop := st.InitialStop(c, atr)
	switch {
	case p.StopMode == "supertrend" && s.STDir[i] == 1 && s.STLine[i] > 0 && s.STLine[i] < c:
		stop = st.clampStop(c, s.STLine[i])
	case p.StopMode == "swing_low":
		n := p.SwingLowBars
		if n <= 0 {
			n = 10
		}
		lo := b.Low
		for k := i - n + 1; k <= i && k >= 0; k++ {
			lo = math.Min(lo, s.Bars[k].Low)
		}
		stop = st.clampStop(c, lo*0.995) // just below the recent swing low
	}
	rs := st.RelativeStrength(s, i, idx, j)
	tag, notes := s.researchAt(i, rs)
	if !researchAllowed(p.ResearchAllow, notes) {
		return models.Signal{}, false, fmt.Sprintf("%s setup, but research %s is not in research_allow (%s) — %s", kind, tag, p.ResearchAllow, strings.Join(notes, "; "))
	}
	why += " · research " + tag
	return models.Signal{InstrumentToken: token, Symbol: sym, Date: b.Date, Setup: kind, Close: c, Stop: stop,
		ATR: atr, RS: rs, Score: rs, Reason: why, Research: tag, ResearchNote: strings.Join(notes, "; ")}, true, why
}

// InitialStop is entry − stop_atr_mult × ATR, clamped to [min_stop_pct, max_stop_pct].
func (st *Strategy) InitialStop(entry, atr float64) float64 {
	d := st.P.StopATRMult * atr
	d = math.Max(d, entry*indicators.Pct(st.P.MinStopPct))
	d = math.Min(d, entry*indicators.Pct(st.P.MaxStopPct))
	return entry - d
}

// trail applies breakeven_at_r and trail_mode (end of day, stop only rises).
func (st *Strategy) trail(pos *models.Position, s *Series, i int, R float64, raise func(float64, models.StopStage)) {
	p := st.P
	if pos.PartialDone || (p.BreakevenAtR > 0 && pos.HighestClose >= pos.EntryPrice+p.BreakevenAtR*R) {
		raise(pos.EntryPrice*(1+st.Costs.RoundTripFrac()), models.StageBreakeven)
	}
	if p.TrailMode == "" || p.TrailMode == "off" || pos.HighestClose < pos.EntryPrice+p.TrailStartR*R {
		return
	}
	switch p.TrailMode {
	case "atr":
		if s.ATR[i] > 0 {
			raise(pos.HighestClose-p.TrailATRMult*s.ATR[i], models.StageTrailing)
		}
	case "supertrend":
		if s.STDir[i] == 1 && s.STLine[i] > 0 {
			raise(s.STLine[i], models.StageTrailing)
		}
	}
}

// Target returns the profit-target price for a position (0 = none).
func (st *Strategy) Target(pos *models.Position) float64 {
	if st.P.TargetR <= 0 || pos.PartialDone {
		return 0
	}
	return pos.EntryPrice + st.P.TargetR*pos.RiskPerShare()
}

// clampStop keeps a stop between min_stop_pct and max_stop_pct below entry.
func (st *Strategy) clampStop(entry, stop float64) float64 {
	d := entry - stop
	d = math.Max(d, entry*indicators.Pct(st.P.MinStopPct))
	d = math.Min(d, entry*indicators.Pct(st.P.MaxStopPct))
	return entry - d
}

// Manage applies the end-of-day rules for bar i to an open position and
// returns a non-empty reason if it should be sold at the next morning run.
// The stop only ever moves up.
//
//	+1R  → stop to entry + round-trip costs          (BREAKEVEN)
//	+2R  → stop to entry + 1R                         (LOCKED)
//	after breakeven: stop ≥ highest close − 3 × ATR   (TRAILING)
//	after breakeven: close < EMA20 → exit next open
//	max_hold_bars without reaching +time_stop_min_r → exit (time stop)
func (st *Strategy) Manage(pos *models.Position, s *Series, i int) string {
	p := st.P
	b := s.Bars[i]
	c := b.Close
	pos.BarsHeld++
	pos.LastClose = c
	if c > pos.HighestClose {
		pos.HighestClose = c
	}
	R := pos.RiskPerShare()
	if R <= 0 {
		R = pos.EntryPrice * indicators.Pct(p.MinStopPct)
	}
	raise := func(level float64, stage models.StopStage) {
		if level > pos.Stop+1e-9 {
			pos.Stop = level
			pos.Stage = stage
		} else if stageRank(stage) > stageRank(pos.Stage) {
			pos.Stage = stage
		}
	}
	gain := c - pos.EntryPrice
	if p.FixedStop {
		gain = -1 // the initial stop is the only stop: skip breakeven, lock and trailing
	}
	if gain >= p.BreakevenR*R {
		raise(pos.EntryPrice*(1+st.Costs.RoundTripFrac()), models.StageBreakeven)
	}
	if gain >= p.LockR*R {
		raise(pos.EntryPrice+R, models.StageLocked)
	}
	if p.FixedStop {
		st.trail(pos, s, i, R, raise)
	}
	if !p.FixedStop && pos.Stage != models.StageInitial && s.ATR[i] > 0 {
		if trail := pos.HighestClose - p.TrailATRMult*s.ATR[i]; trail > pos.Stop+1e-9 {
			pos.Stop = trail
			pos.Stage = models.StageTrailing
		}
	}
	switch {
	case pos.Stop >= c:
		return fmt.Sprintf("close %.2f at/below the stop %.2f", c, pos.Stop)
	case p.ExitOnEMACross && s.CrossFast[i] < s.CrossSlow[i]:
		cf, cs, _, _ := crossParams(p)
		return fmt.Sprintf("EMA%d %.2f crossed below EMA%d %.2f", cf, s.CrossFast[i], cs, s.CrossSlow[i])
	case p.ExitBelowEMAFast && pos.Stage != models.StageInitial && c < s.EMAFast[i]:
		return fmt.Sprintf("close %.2f below EMA%d %.2f after breakeven", c, p.EMAFast, s.EMAFast[i])
	case (p.HAExitAlways || pos.Stage != models.StageInitial) && st.haExit(s, i) != "":
		return st.haExit(s, i)
	case p.MaxHoldBars > 0 && pos.BarsHeld >= p.MaxHoldBars && pos.HighestClose < pos.EntryPrice+p.TimeStopMinR*R:
		return fmt.Sprintf("time stop: %d days without reaching +%.1fR", pos.BarsHeld, p.TimeStopMinR)
	}
	return ""
}

func (st *Strategy) wickPct() float64 {
	if st.P.HAWickPct <= 0 {
		return 10
	}
	return st.P.HAWickPct
}

// haExit returns a reason when the Heikin-Ashi trend has turned down at bar i.
func (st *Strategy) haExit(s *Series, i int) string {
	switch st.P.HAExit {
	case "red":
		n := st.P.HAExitBars
		if n < 1 {
			n = 2
		}
		if i+1 < n {
			return ""
		}
		for k := i - n + 1; k <= i; k++ {
			if !s.HARed(k) {
				return ""
			}
		}
		return fmt.Sprintf("Heikin-Ashi turned down: %d red candles in a row", n)
	case "strong_red":
		if _, uw := s.haWick(i); s.HARed(i) && uw <= st.wickPct() {
			return "Heikin-Ashi strong red candle (no upper wick)"
		}
	}
	return ""
}

func stageRank(s models.StopStage) int {
	switch s {
	case models.StageBreakeven:
		return 1
	case models.StageLocked:
		return 2
	case models.StageTrailing:
		return 3
	}
	return 0
}

// Size returns the quantity for an entry: the smallest of the risk budget
// (equity × risk% ÷ per-share risk), the position cap (equity × max% ÷ price)
// and the cash available (including buy costs).
func Size(equity, cash, entry, stop float64, r config.RiskConfig, costs Costs) int {
	if entry <= 0 || stop >= entry {
		return 0
	}
	qRisk := math.Floor(equity * indicators.Pct(r.RiskPerTradePct) / (entry - stop))
	qCap := math.Floor(equity * indicators.Pct(r.MaxPositionPct) / entry)
	perShare := entry * (1 + costs.BuyFrac())
	qCash := math.Floor(cash / perShare)
	q := math.Min(qRisk, math.Min(qCap, qCash))
	if q < 1 {
		return 0
	}
	return int(q)
}

// ---------------------------------------------------------------------------
// Costs — Zerodha equity delivery
// ---------------------------------------------------------------------------

// Costs computes statutory and broker charges for delivery trades.
type Costs struct{ C config.CostsConfig }

func (k Costs) exchangeFrac() float64 { return indicators.Pct(k.C.ExchangePct) + k.C.SEBIPerCrore/1e7 }

// BuyFrac is the fraction of the buy value paid as charges.
func (k Costs) BuyFrac() float64 {
	brok := indicators.Pct(k.C.BrokeragePct)
	gst := indicators.Pct(k.C.GSTPct) * (brok + k.exchangeFrac())
	return brok + indicators.Pct(k.C.STTPct) + indicators.Pct(k.C.StampBuyPct) + k.exchangeFrac() + gst
}

// SellFrac is the fraction of the sell value paid as charges (excluding DP).
func (k Costs) SellFrac() float64 {
	brok := indicators.Pct(k.C.BrokeragePct)
	gst := indicators.Pct(k.C.GSTPct) * (brok + k.exchangeFrac())
	return brok + indicators.Pct(k.C.STTPct) + k.exchangeFrac() + gst
}

// Buy returns ₹ charges on a buy of the given value.
func (k Costs) Buy(value float64) float64 { return value * k.BuyFrac() }

// Sell returns ₹ charges on a sell of the given value (including the DP charge).
func (k Costs) Sell(value float64) float64 { return value*k.SellFrac() + k.C.DPPerSell }

// RoundTripFrac is buy + sell charges as a fraction (DP excluded) — used to
// place the breakeven stop so that a stop-out there is actually flat.
func (k Costs) RoundTripFrac() float64 { return k.BuyFrac() + k.SellFrac() }
