// Package swing holds the pure swing-trading rules — indicator series, entry
// signals, stop management, position sizing and cost model. The backtester
// and the live engine call exactly the same functions, so what you backtest is
// what trades.
package swing

import (
	"fmt"
	"math"
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
	s.ATR = indicators.ATR(h, l, s.Close, p.ATRPeriod)
	s.VolAvg = indicators.SMA(v, p.VolumeAvgPeriod)
	s.PriorHigh = indicators.PriorHighest(h, p.BreakoutLookback)
	s.HAOpen, s.HAHigh, s.HALow, s.HAClose = indicators.HeikinAshi(o, h, l, s.Close)
	return s
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
	for _, x := range []int{p.BreakoutLookback + 1, p.ATRPeriod + 1, p.VolumeAvgPeriod + 1, p.RSLookback + 1} {
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
	case !(c > es && ef > es && es > s.EMASlow[i-p.SlopeLookback]):
		return models.Signal{}, false, "not in an uptrend (needs close and EMA20 above a rising EMA50)"
	case atr <= 0:
		return models.Signal{}, false, "ATR unavailable"
	}
	prevVolAvg := s.VolAvg[i-1]
	var kind models.SetupKind
	var why string
	allowBO, allowPB := p.Setups != "pullback", p.Setups != "breakout"
	if allowBO && c > s.PriorHigh[i] && b.Volume >= p.BreakoutVolRatio*prevVolAvg {
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
	rs := st.RelativeStrength(s, i, idx, j)
	return models.Signal{InstrumentToken: token, Symbol: sym, Date: b.Date, Setup: kind, Close: c, Stop: stop,
		ATR: atr, RS: rs, Score: rs, Reason: why}, true, why
}

// InitialStop is entry − stop_atr_mult × ATR, clamped to [min_stop_pct, max_stop_pct].
func (st *Strategy) InitialStop(entry, atr float64) float64 {
	d := st.P.StopATRMult * atr
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
	if gain >= p.BreakevenR*R {
		raise(pos.EntryPrice*(1+st.Costs.RoundTripFrac()), models.StageBreakeven)
	}
	if gain >= p.LockR*R {
		raise(pos.EntryPrice+R, models.StageLocked)
	}
	if pos.Stage != models.StageInitial && s.ATR[i] > 0 {
		if trail := pos.HighestClose - p.TrailATRMult*s.ATR[i]; trail > pos.Stop+1e-9 {
			pos.Stop = trail
			pos.Stage = models.StageTrailing
		}
	}
	switch {
	case pos.Stop >= c:
		return fmt.Sprintf("close %.2f at/below the stop %.2f", c, pos.Stop)
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
