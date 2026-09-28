package backtest

import (
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nkalva/kitealgo/internal/config"
	"github.com/nkalva/kitealgo/pkg/models"
)

// synth generates n weekday bars: a random walk with drift and volume noise.
func synth(seed int64, n int, start, drift, vol float64) []models.Bar {
	r := rand.New(rand.NewSource(seed))
	d := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	var out []models.Bar
	px := start
	for len(out) < n {
		d = d.AddDate(0, 0, 1)
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			continue
		}
		o := px * (1 + r.NormFloat64()*vol*0.3)
		c := o * (1 + drift + r.NormFloat64()*vol)
		h := math.Max(o, c) * (1 + math.Abs(r.NormFloat64())*vol*0.5)
		l := math.Min(o, c) * (1 - math.Abs(r.NormFloat64())*vol*0.5)
		v := 2e6 * (0.6 + r.Float64())
		out = append(out, models.Bar{Date: d, Open: o, High: h, Low: l, Close: c, Volume: v})
		px = c
	}
	return out
}

func TestBacktestInvariants(t *testing.T) {
	cfg := config.Defaults()
	cfg.Strategy.Setups, cfg.Strategy.FixedStop, cfg.Strategy.ExitOnEMACross = "breakout", false, false
	cfg.Strategy.ExitBelowEMAFast, cfg.Strategy.MaxHoldBars = true, 40
	var ins []Instrument
	for i := 0; i < 12; i++ {
		ins = append(ins, Instrument{Symbol: string(rune('A' + i)), Token: uint32(i + 1),
			Bars: synth(int64(i+7), 900, 300+50*float64(i), 0.0006, 0.018)})
	}
	idx := synth(99, 900, 15000, 0.0004, 0.009)
	in := Input{Instruments: ins, Index: idx, From: idx[120].Date, To: idx[len(idx)-1].Date, Config: cfg}
	res := Run(in)

	if res.Summary.Trades == 0 {
		t.Fatal("expected some trades on trending synthetic data")
	}
	for _, e := range res.Equity {
		if e.Cash < -1e-6 {
			t.Fatalf("cash went negative on %s: %.2f", e.Date.Format("2006-01-02"), e.Cash)
		}
		if e.Positions > cfg.Risk.MaxPositions {
			t.Fatalf("position cap exceeded: %d", e.Positions)
		}
	}
	open := map[string]time.Time{}
	for _, tr := range res.Trades {
		if tr.ExitDate.Before(tr.EntryDate) {
			t.Fatalf("exit before entry: %+v", tr)
		}
		if tr.Costs <= 0 {
			t.Fatalf("every trade pays charges: %+v", tr)
		}
		if last, ok := open[tr.Symbol]; ok && tr.EntryDate.Before(last) {
			t.Fatalf("overlapping positions in %s", tr.Symbol)
		}
		open[tr.Symbol] = tr.ExitDate
		// Risk per trade: a stop-out can lose ~1% of equity plus gap/slippage.
		if tr.RMultiple < -3 {
			t.Fatalf("loss far beyond 1R (gap handling?): %+v", tr)
		}
	}
	dir := t.TempDir()
	if err := WriteReport(dir, res); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "report.html")); err != nil {
		t.Fatal(err)
	}
	t.Logf("trades=%d win=%.1f%% cagr=%.1f%% maxdd=%.1f%% expectancy=%.2fR",
		res.Summary.Trades, res.Summary.WinRatePct, res.Summary.CAGRPct, res.Summary.MaxDrawdownPct, res.Summary.ExpectancyR)
}

// A position that gaps below its stop must be sold at the open, not the stop.
func TestGapThroughStopFillsAtOpen(t *testing.T) {
	cfg := config.Defaults()
	cfg.Strategy.Setups, cfg.Strategy.FixedStop, cfg.Strategy.ExitOnEMACross = "breakout", false, false
	cfg.Strategy.ExitBelowEMAFast, cfg.Strategy.MaxHoldBars = true, 40
	cfg.Market.RegimeFilter = false
	cfg.Strategy.MinTurnoverCr = 0
	n := 140
	d := time.Date(2021, 1, 4, 0, 0, 0, 0, time.UTC)
	var bars, idx []models.Bar
	for i := 0; len(bars) < n; i++ {
		day := d.AddDate(0, 0, i)
		if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			continue
		}
		c := 100 + 0.5*float64(len(bars))
		bars = append(bars, models.Bar{Date: day, Open: c - 0.2, High: c + 0.3, Low: c - 0.4, Close: c, Volume: 1e6})
		idx = append(idx, models.Bar{Date: day, Open: 1000, High: 1001, Low: 999, Close: 1000 + float64(len(idx)), Volume: 0})
	}
	k := n - 10
	bars[k].Close, bars[k].High, bars[k].Volume = bars[k-1].Close*1.03, bars[k-1].Close*1.035, 3e6 // breakout
	bars[k+1].Open, bars[k+1].High, bars[k+1].Low, bars[k+1].Close = bars[k].Close, bars[k].Close*1.01, bars[k].Close*0.995, bars[k].Close*1.005
	g := bars[k].Close * 0.85 // 15% gap down next day
	bars[k+2].Open, bars[k+2].High, bars[k+2].Low, bars[k+2].Close = g, g*1.01, g*0.99, g
	res := Run(Input{Instruments: []Instrument{{Symbol: "G", Token: 1, Bars: bars}}, Index: idx,
		From: idx[80].Date, To: idx[len(idx)-1].Date, Config: cfg})
	var found bool
	for _, tr := range res.Trades {
		if tr.Reason == "gapped below stop" {
			found = true
			if tr.ExitPrice > g {
				t.Fatalf("gap exit must fill at/below the open %.2f, got %.2f", g, tr.ExitPrice)
			}
		}
	}
	if !found {
		t.Fatalf("expected a gap-through-stop exit, trades: %+v", res.Trades)
	}
}
