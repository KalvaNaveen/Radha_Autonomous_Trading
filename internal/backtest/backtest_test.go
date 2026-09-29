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

// Holdings mode: never more than N positions, each bought with about
// equity ÷ N, and the yearly returns compound into the total return.
func TestHoldingsMode(t *testing.T) {
	cfg := config.Defaults()
	cfg.Holdings.Enabled, cfg.Holdings.Slots, cfg.Holdings.MarketCheck = true, 4, "category"
	cfg.Strategy.EntryMode = "both"
	cfg.Risk.Capital = 500000
	var ins []Instrument
	caps := map[string]string{}
	for i := 0; i < 12; i++ {
		sym := string(rune('A' + i))
		ins = append(ins, Instrument{Symbol: sym, Token: uint32(i + 1), Bars: synth(int64(i+7), 900, 300+50*float64(i), 0.0006, 0.018)})
		caps[sym] = []string{"large", "mid", "small"}[i%3]
	}
	idx := synth(99, 900, 15000, 0.0004, 0.009)
	mid := synth(98, 900, 30000, 0.0005, 0.011)
	in := Input{Instruments: ins, Index: idx, From: idx[120].Date, To: idx[len(idx)-1].Date, Config: cfg,
		Indices: map[string][]models.Bar{cfg.Holdings.MidcapIndex: mid}, Caps: caps}
	res := Run(in)
	if res.Summary.Trades == 0 {
		t.Fatal("expected trades")
	}
	if len(res.Notes) != 1 { // the smallcap index is missing → falls back to NIFTY 50, with a note
		t.Fatalf("want one fallback note, got %v", res.Notes)
	}
	eqOn := map[string]float64{}
	for _, e := range res.Equity {
		if e.Cash < -1e-6 || e.Positions > 4 {
			t.Fatalf("%s: cash %.2f positions %d", e.Date.Format("2006-01-02"), e.Cash, e.Positions)
		}
		eqOn[e.Date.Format("2006-01-02")] = e.Equity
	}
	for _, tr := range res.Trades {
		if tr.Reason == "end of backtest" {
			continue
		}
		v := tr.EntryPrice * float64(tr.Quantity)
		if v > 500000*3 { // sanity: no position near the whole account
			t.Fatalf("oversized position: %+v", tr)
		}
	}
	prod := 1.0
	for _, y := range res.Summary.Yearly {
		prod *= 1 + y.ReturnPct/100
	}
	if got := (prod - 1) * 100; math.Abs(got-res.Summary.TotalReturnPct) > 0.01 {
		t.Fatalf("yearly returns must compound to the total: %.4f vs %.4f", got, res.Summary.TotalReturnPct)
	}
	if res.Summary.NetProfit != res.Summary.EndEquity-res.Summary.StartEquity {
		t.Fatal("net profit")
	}
}

// Auto universe: each month only the top-N by strength (point in time) are
// scanned, and every trade is in a stock that was in that month's pool.
func TestAutoUniverse(t *testing.T) {
	cfg := config.Defaults()
	cfg.Holdings.Enabled, cfg.Strategy.EntryMode = true, "both"
	cfg.Backtest.AutoUniverse = config.AutoUniverseConfig{Enabled: true, Source: "all_nse", TopN: 4, LookbackDays: 60}
	var ins []Instrument
	for i := 0; i < 16; i++ {
		ins = append(ins, Instrument{Symbol: string(rune('A' + i)), Token: uint32(i + 1),
			Bars: synth(int64(i+11), 900, 300+20*float64(i), 0.0002*float64(i%5), 0.018)})
	}
	idx := synth(99, 900, 15000, 0.0004, 0.009)
	res := Run(Input{Instruments: ins, Index: idx, From: idx[120].Date, To: idx[len(idx)-1].Date, Config: cfg})
	if len(res.Pools) < 30 || res.Summary.Trades == 0 {
		t.Fatalf("pools %d trades %d", len(res.Pools), res.Summary.Trades)
	}
	inPool := func(sym string, d time.Time) bool {
		var cur []string
		for _, p := range res.Pools {
			if p.Date.After(d) {
				break
			}
			cur = p.Symbols
		}
		for _, s := range cur {
			if s == sym {
				return true
			}
		}
		return false
	}
	for _, p := range res.Pools {
		if len(p.Symbols) > 4 {
			t.Fatalf("pool larger than top_n: %v", p.Symbols)
		}
	}
	for _, tr := range res.Trades {
		// The signal formed the evening before the entry, on a pool day.
		if !inPool(tr.Symbol, tr.EntryDate.AddDate(0, 0, -1)) && !inPool(tr.Symbol, tr.EntryDate.AddDate(0, 0, -3)) {
			t.Fatalf("%s bought on %s but not in that month's pool", tr.Symbol, tr.EntryDate.Format("2006-01-02"))
		}
	}
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

// With MTF at 1× nothing changes; at 2× the engine borrows, pays interest
// and fees, and every rupee of debt is repaid by the end.
func TestMTFAccounting(t *testing.T) {
	in := mtfInput()
	base := Run(in)
	in.Config.MTF.Enabled, in.Config.MTF.Leverage = true, 1
	if one := Run(in); one.Summary.EndEquity != base.Summary.EndEquity {
		t.Fatalf("1× MTF must equal CNC: %.2f vs %.2f", one.Summary.EndEquity, base.Summary.EndEquity)
	}
	in.Config.MTF.Leverage = 2
	in.Config.Risk.MaxPositionPct = 40
	lev := Run(in)
	if lev.Summary.Trades == 0 || lev.MTFCosts <= 0 {
		t.Fatalf("2× MTF must trade and pay MTF costs: trades %d costs %.2f", lev.Summary.Trades, lev.MTFCosts)
	}
	in.Config.MTF.InterestPctPerDay, in.Config.MTF.BrokeragePct, in.Config.MTF.PledgeFee, in.Config.MTF.UnpledgeFee = 0, 0, 0, 0
	free := Run(in)
	if free.Summary.EndEquity <= lev.Summary.EndEquity {
		t.Fatalf("removing MTF costs must raise the result: %.2f vs %.2f", free.Summary.EndEquity, lev.Summary.EndEquity)
	}
}

func mtfInput() Input {
	cfg := config.Defaults()
	cfg.Strategy.Setups, cfg.Strategy.FixedStop, cfg.Strategy.ExitOnEMACross = "breakout", false, false
	cfg.Strategy.ExitBelowEMAFast, cfg.Strategy.MaxHoldBars, cfg.Strategy.HAEntry = true, 40, "off"
	var ins []Instrument
	for i := 0; i < 12; i++ {
		ins = append(ins, Instrument{Symbol: string(rune('A' + i)), Token: uint32(i + 1),
			Bars: synth(int64(i+7), 900, 300+50*float64(i), 0.0006, 0.018)})
	}
	idx := synth(99, 900, 15000, 0.0004, 0.009)
	return Input{Instruments: ins, Index: idx, From: idx[120].Date, To: idx[len(idx)-1].Date, Config: cfg}
}
