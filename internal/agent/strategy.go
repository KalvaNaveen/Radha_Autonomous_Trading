package agent

import (
	"fmt"
	"math"

	"github.com/nkalva/kitealgo/internal/config"
	"github.com/nkalva/kitealgo/internal/indicators"
	"github.com/nkalva/kitealgo/pkg/models"
)

// Strategy holds the pure, side-effect-free signal and stop mathematics. It is
// separated from the agent so every rule can be unit-tested without channels.
type Strategy struct {
	p config.StrategyConfig

	armed     models.Side // side of the most recent EMA crossover still eligible
	armedBars int         // closed bars since the crossover bar (0 = crossover bar)
}

// NewStrategy creates a strategy with the given parameters.
func NewStrategy(p config.StrategyConfig) *Strategy { return &Strategy{p: p} }

// BarInput is everything the entry rule needs at a 5-minute close.
type BarInput struct {
	Candle             models.Candle
	Fast, Slow         float64 // EMA values after this bar
	PrevFast, PrevSlow float64 // EMA values before this bar
	VWAP               float64
	RVOL               float64
}

// Signal is the result of evaluating one bar.
type Signal struct {
	Side   models.Side
	Reason string // why it fired, or why it was rejected (for the status page)
}

// OnBar advances the crossover tracker and evaluates the entry rule.
//
// Long (short is the mirror image):
//  1. 10 EMA crossed above 20 EMA on this bar or within the last
//     PullbackCandles-1 bars, and is still above it;
//  2. the bar pulled back into the EMA band: low <= upper band × (1 + tolerance);
//  3. the bar closed back above the 10 EMA (the pullback held);
//  4. close > VWAP × (1 + 0.20%)  — out of the VWAP "magnet" zone;
//  5. RVOL >= 1.5 — the move has participation.
func (s *Strategy) OnBar(in BarInput) Signal {
	if in.Fast == 0 || in.Slow == 0 || in.PrevFast == 0 || in.PrevSlow == 0 {
		return Signal{Reason: "ema warming up"}
	}
	switch {
	case in.PrevFast <= in.PrevSlow && in.Fast > in.Slow:
		s.armed, s.armedBars = models.SideLong, 0
	case in.PrevFast >= in.PrevSlow && in.Fast < in.Slow:
		s.armed, s.armedBars = models.SideShort, 0
	case s.armed != models.SideNone:
		s.armedBars++
	}
	if s.armed == models.SideLong && in.Fast <= in.Slow || s.armed == models.SideShort && in.Fast >= in.Slow {
		s.armed = models.SideNone // alignment lost
	}
	if s.armed != models.SideNone && s.armedBars >= max(1, s.p.PullbackCandles) {
		s.armed = models.SideNone // window expired
	}
	if s.armed == models.SideNone {
		return Signal{Reason: "no fresh crossover"}
	}

	c := in.Candle
	tol := indicators.Pct(s.p.BandTolerancePct)
	buf := indicators.Pct(s.p.VWAPBufferPct)
	upper, lower := math.Max(in.Fast, in.Slow), math.Min(in.Fast, in.Slow)

	if s.armed == models.SideLong {
		switch {
		case in.VWAP <= 0:
			return Signal{Reason: "long: vwap unavailable"}
		case c.Close <= in.VWAP*(1+buf):
			return Signal{Reason: fmt.Sprintf("long: close %.2f not above VWAP+%.2f%% (%.2f)", c.Close, s.p.VWAPBufferPct, in.VWAP*(1+buf))}
		case in.RVOL < s.p.MinRVOL:
			return Signal{Reason: fmt.Sprintf("long: RVOL %.2f < %.2f", in.RVOL, s.p.MinRVOL)}
		case c.Low > upper*(1+tol):
			return Signal{Reason: "long: no pullback to EMA band"}
		case c.Close <= in.Fast || c.Close < lower:
			return Signal{Reason: "long: pullback did not hold above 10 EMA"}
		}
		s.armed = models.SideNone
		return Signal{Side: models.SideLong, Reason: fmt.Sprintf("long: cross+pullback, close %.2f vwap %.2f rvol %.2f", c.Close, in.VWAP, in.RVOL)}
	}

	switch {
	case in.VWAP <= 0:
		return Signal{Reason: "short: vwap unavailable"}
	case c.Close >= in.VWAP*(1-buf):
		return Signal{Reason: fmt.Sprintf("short: close %.2f not below VWAP-%.2f%% (%.2f)", c.Close, s.p.VWAPBufferPct, in.VWAP*(1-buf))}
	case in.RVOL < s.p.MinRVOL:
		return Signal{Reason: fmt.Sprintf("short: RVOL %.2f < %.2f", in.RVOL, s.p.MinRVOL)}
	case c.High < lower*(1-tol):
		return Signal{Reason: "short: no pullback to EMA band"}
	case c.Close >= in.Fast || c.Close > upper:
		return Signal{Reason: "short: pullback did not hold below 10 EMA"}
	}
	s.armed = models.SideNone
	return Signal{Side: models.SideShort, Reason: fmt.Sprintf("short: cross+pullback, close %.2f vwap %.2f rvol %.2f", c.Close, in.VWAP, in.RVOL)}
}

// InitialStop returns the hard stop for a fill at entry: the 20 EMA, but no
// further than MaxStopPct and no nearer than MinStopPct (a stop inside the
// noise gets hit by the bid-ask bounce). If the 20 EMA is on the wrong side of
// the fill (price gapped through it), the maximum distance is used.
func (s *Strategy) InitialStop(side models.Side, entry, slowEMA float64) float64 {
	maxD := entry * indicators.Pct(s.p.MaxStopPct)
	minD := entry * indicators.Pct(s.p.MinStopPct)
	d := side.Sign() * (entry - slowEMA)
	if d <= 0 {
		d = maxD
	}
	d = math.Min(math.Max(d, minD), maxD)
	return entry - side.Sign()*d
}

// LockTarget returns the ratchet stage reached at `gross` return and the stop
// level it guarantees:
//
//	gross >= +0.40%  → BREAKEVEN:   stop = entry ± 0.15%        (all friction covered, ~0 net)
//	gross >= +0.65%  → PROFIT_LOCK: stop = entry ± (0.25+0.15)% (+0.25% net locked)
func (s *Strategy) LockTarget(side models.Side, entry, gross float64) (models.LockStage, float64) {
	sign := side.Sign()
	switch {
	case gross >= indicators.Pct(s.p.ProfitLockTriggerPct)-1e-12:
		return models.LockProfit, entry * (1 + sign*indicators.Pct(s.p.MinNetProfitPct+s.p.RoundTripCostPct))
	case gross >= indicators.Pct(s.p.BreakevenTriggerPct)-1e-12:
		return models.LockBreakeven, entry * (1 + sign*indicators.Pct(s.p.RoundTripCostPct))
	}
	return models.LockNone, 0
}

// LockLevel returns the stop guaranteed by an already-reached stage.
func (s *Strategy) LockLevel(side models.Side, entry float64, stage models.LockStage) float64 {
	sign := side.Sign()
	switch stage {
	case models.LockProfit:
		return entry * (1 + sign*indicators.Pct(s.p.MinNetProfitPct+s.p.RoundTripCostPct))
	case models.LockBreakeven:
		return entry * (1 + sign*indicators.Pct(s.p.RoundTripCostPct))
	}
	return 0
}

// TrailStop is the resting stop pegged to the 10 EMA with a noise buffer.
func (s *Strategy) TrailStop(side models.Side, fastEMA float64) float64 {
	return fastEMA * (1 - side.Sign()*indicators.Pct(s.p.TrailBufferPct))
}

// TighterOf returns whichever stop is closer to price for this side.
func TighterOf(side models.Side, a, b float64) float64 {
	if a == 0 {
		return b
	}
	if b == 0 {
		return a
	}
	if side == models.SideLong {
		return math.Max(a, b)
	}
	return math.Min(a, b)
}

// Improves reports whether candidate is tighter than current by at least step.
func Improves(side models.Side, candidate, current, step float64) bool {
	return side.Sign()*(candidate-current) >= step-1e-9
}

// CloseAdverse reports whether a bar closed on the wrong side of the 10 EMA.
func CloseAdverse(side models.Side, close, fastEMA float64) bool {
	if side == models.SideLong {
		return close < fastEMA
	}
	return close > fastEMA
}
