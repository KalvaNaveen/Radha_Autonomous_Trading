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
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/nkalva/kitealgo/internal/config"
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
}

// Result is the full output of a run.
type Result struct {
	Summary Summary        `json:"summary"`
	Trades  []models.Trade `json:"trades"`
	Equity  []EquityPoint  `json:"equity"`
	Config  config.Config  `json:"-"`
	Skipped map[string]int `json:"skipped_reasons"`
}

type inst struct {
	Instrument
	s *swing.Series
}

// Run executes the backtest.
func Run(in Input) Result {
	cfg := in.Config
	st := swing.NewStrategy(cfg.Strategy, cfg.Costs)
	slip := indicators.Pct(cfg.Costs.SlippagePct)
	idx := swing.NewSeries(in.Index, cfg.Strategy)

	insts := make([]*inst, 0, len(in.Instruments))
	for _, ins := range in.Instruments {
		if len(ins.Bars) == 0 {
			continue
		}
		insts = append(insts, &inst{Instrument: ins, s: swing.NewSeries(ins.Bars, cfg.Strategy)})
	}

	cash := cfg.Risk.Capital
	peak := cash
	pauseUntil := 0 // index bar before which entries are paused (drawdown breaker)
	positions := map[string]*models.Position{}
	cooldown := map[string]int{} // symbol → index-bar number when re-entry is allowed
	var pending []models.Signal
	res := Result{Config: cfg, Skipped: map[string]int{}}
	var benchBase float64
	exposedDays, days := 0, 0

	sell := func(p *models.Position, px float64, d time.Time, reason string, k int) {
		t := swing.CloseTrade(p, px, d, reason, st.Costs)
		cash += px*float64(p.Quantity) - (t.Costs - p.EntryCosts)
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

		// 1. OPEN — planned exits, then entries.
		for _, p := range sortedPositions(positions) {
			if p.PendingExit == "" {
				continue
			}
			x := find(insts, p.Symbol)
			if i, ok := x.s.IndexOn(d); ok {
				sell(p, x.s.Bars[i].Open*(1-slip), d, p.PendingExit, k)
			}
		}
		equityPrev := cash + markValue(positions)
		newToday := 0
		for _, sig := range pending {
			if len(positions) >= cfg.Risk.MaxPositions || newToday >= cfg.Risk.MaxNewPerDay {
				res.Skipped["no free slot"]++
				continue
			}
			if _, held := positions[sig.Symbol]; held {
				continue
			}
			x := find(insts, sig.Symbol)
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
			qty := swing.Size(equityPrev, cash, fill, sig.Stop, cfg.Risk, st.Costs)
			if qty < 1 {
				res.Skipped["size zero (cash/risk)"]++
				continue
			}
			bc := st.Costs.Buy(fill * float64(qty))
			cash -= fill*float64(qty) + bc
			positions[sig.Symbol] = &models.Position{InstrumentToken: x.Token, Symbol: x.Symbol, Quantity: qty,
				EntryPrice: fill, EntryDate: d, Setup: sig.Setup, InitialStop: sig.Stop, Stop: sig.Stop,
				Stage: models.StageInitial, HighestClose: fill, EntryCosts: bc}
			newToday++
		}
		pending = nil

		// 2. INTRADAY — stops.
		for _, p := range sortedPositions(positions) {
			x := find(insts, p.Symbol)
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
			x := find(insts, p.Symbol)
			i, ok := x.s.IndexOn(d)
			if !ok {
				continue
			}
			p.LastPrice = x.s.Bars[i].Close
			if reason := st.Manage(p, x.s, i); reason != "" {
				p.PendingExit = reason
			}
		}
		equity := cash + markValue(positions)
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
		if regime && !paused {
			for _, x := range insts {
				if _, held := positions[x.Symbol]; held || cooldown[x.Symbol] > k {
					continue
				}
				i, ok := x.s.IndexOn(d)
				if !ok {
					continue
				}
				if sig, ok, _ := st.Evaluate(x.Symbol, x.Token, x.s, i, idx, j); ok {
					pending = append(pending, sig)
				}
			}
			sort.SliceStable(pending, func(a, b int) bool { return pending[a].Score > pending[b].Score })
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

func find(insts []*inst, sym string) *inst {
	for _, x := range insts {
		if x.Symbol == sym {
			return x
		}
	}
	return nil
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
	return s
}
