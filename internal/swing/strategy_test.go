package swing

import (
	"math"
	"testing"
	"time"

	"github.com/nkalva/kitealgo/internal/config"
	"github.com/nkalva/kitealgo/pkg/models"
)

// cfg returns the breakout/ratchet rule set most tests below were written for.
func cfg() config.Config {
	c := config.Defaults()
	c.Strategy.Setups, c.Strategy.FixedStop, c.Strategy.ExitOnEMACross = "breakout", false, false
	c.Strategy.ExitBelowEMAFast, c.Strategy.MaxHoldBars = true, 40
	return c
}

// bars builds daily bars from closes with a 1% range and constant volume.
func bars(closes []float64, vol float64) []models.Bar {
	d := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	out := make([]models.Bar, len(closes))
	for i, c := range closes {
		o := c
		if i > 0 {
			o = closes[i-1]
		}
		out[i] = models.Bar{Date: d.AddDate(0, 0, i), Open: o, High: math.Max(o, c) * 1.005, Low: math.Min(o, c) * 0.995, Close: c, Volume: vol}
	}
	return out
}

func uptrend(n int, start, step float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = start + step*float64(i)
	}
	return out
}

func TestBreakoutSignal(t *testing.T) {
	c := cfg()
	st := NewStrategy(c.Strategy, c.Costs)
	cl := uptrend(120, 500, 1) // steady uptrend
	b := bars(cl, 1e6)
	last := len(b) - 1
	b[last].Close = b[last-1].Close * 1.03 // jump above the 20-day high
	b[last].High = b[last].Close * 1.002
	b[last].Volume = 2e6 // 2× average
	s := NewSeries(b, c.Strategy)
	sig, ok, why := st.Evaluate("X", 1, s, last, nil, 0)
	if !ok || sig.Setup != models.SetupBreakout {
		t.Fatalf("expected breakout, got ok=%v %s", ok, why)
	}
	stopPct := (sig.Close - sig.Stop) / sig.Close * 100
	if stopPct < c.Strategy.MinStopPct-1e-9 || stopPct > c.Strategy.MaxStopPct+1e-9 {
		t.Fatalf("stop %.2f%% outside clamp", stopPct)
	}
}

func TestNoSignalInDowntrend(t *testing.T) {
	c := cfg()
	st := NewStrategy(c.Strategy, c.Costs)
	b := bars(uptrend(120, 700, -1), 1e6)
	s := NewSeries(b, c.Strategy)
	if _, ok, _ := st.Evaluate("X", 1, s, len(b)-1, nil, 0); ok {
		t.Fatal("no longs in a downtrend")
	}
}

func TestLiquidityFilter(t *testing.T) {
	c := cfg()
	st := NewStrategy(c.Strategy, c.Costs)
	b := bars(uptrend(120, 500, 1), 1000) // ₹6 lakh/day turnover
	s := NewSeries(b, c.Strategy)
	if _, ok, why := st.Evaluate("X", 1, s, len(b)-1, nil, 0); ok {
		t.Fatalf("illiquid stock must be rejected (%s)", why)
	}
}

func TestManageRatchet(t *testing.T) {
	c := cfg()
	st := NewStrategy(c.Strategy, c.Costs)
	b := bars(uptrend(120, 500, 1), 1e6)
	s := NewSeries(b, c.Strategy)
	pos := &models.Position{EntryPrice: 100, InitialStop: 95, Stop: 95, Stage: models.StageInitial, HighestClose: 100, Quantity: 10}
	// Fake bars: reuse the series structure but control close/ATR.
	s.Bars[100].Close, s.ATR[100], s.EMAFast[100] = 105.5, 3, 100 // +1.1R
	if r := st.Manage(pos, s, 100); r != "" || pos.Stage != models.StageBreakeven || pos.Stop <= 100 {
		t.Fatalf("breakeven: %+v %s", pos, r)
	}
	s.Bars[101].Close, s.ATR[101], s.EMAFast[101] = 110.5, 1, 104 // +2.1R → lock at +1R, trail 110.5-3=107.5
	st.Manage(pos, s, 101)
	if pos.Stop != 107.5 || pos.Stage != models.StageTrailing {
		t.Fatalf("lock/trail: stop %.2f stage %s", pos.Stop, pos.Stage)
	}
	s.Bars[102].Close, s.ATR[102], s.EMAFast[102] = 108, 1, 108.5 // close below EMA20 → exit
	if r := st.Manage(pos, s, 102); r == "" {
		t.Fatal("close below EMA20 after breakeven must exit")
	}
	if pos.Stop != 107.5 {
		t.Fatal("stop must never move down")
	}
}

func TestSizing(t *testing.T) {
	c := cfg()
	k := Costs{C: c.Costs}
	// ₹1L equity, 1% risk = ₹1,000; stop ₹20 away → 50; cap 20% = ₹20,000/₹500 = 40 → 40.
	if q := Size(100000, 100000, 500, 480, c.Risk, k); q != 40 {
		t.Fatalf("want 40 (cap), got %d", q)
	}
	// Wider stop: ₹1,000 / ₹50 = 20 → 20.
	if q := Size(100000, 100000, 500, 450, c.Risk, k); q != 20 {
		t.Fatalf("want 20 (risk), got %d", q)
	}
	// Cash-limited.
	if q := Size(100000, 5000, 500, 480, c.Risk, k); q != 9 {
		t.Fatalf("want 9 (cash), got %d", q)
	}
}

func TestDeliveryCosts(t *testing.T) {
	k := Costs{C: cfg().Costs}
	buy := k.Buy(50000)
	sell := k.Sell(50000)
	// STT 50 + stamp 7.5 + exch 1.535 + SEBI 0.05 + GST ~0.29 ≈ 59.37 ; sell ≈ 51.87 + 15.34
	if math.Abs(buy-59.37) > 0.05 || math.Abs(sell-67.21) > 0.05 {
		t.Fatalf("costs buy %.2f sell %.2f", buy, sell)
	}
}

func TestHeikinAshiEntryFilter(t *testing.T) {
	c := cfg()
	c.Strategy.HAEntry = "green"
	st := NewStrategy(c.Strategy, c.Costs)
	b := bars(uptrend(120, 500, 1), 1e6)
	last := len(b) - 1
	// A breakout close, but the candle itself opened high and sold off hard
	// after a string of red days → the HA candle stays red.
	for k := last - 4; k < last; k++ {
		b[k].Open, b[k].Close = b[k].Close*1.02, b[k].Close*0.99
	}
	b[last].Open, b[last].High, b[last].Low = 540, 650, 480
	b[last].Close = b[last-1].Close * 1.03
	b[last].Volume = 2e6
	s := NewSeries(b, c.Strategy)
	if s.HAGreen(last) {
		t.Skip("fixture did not produce a red HA candle")
	}
	if _, ok, why := st.Evaluate("X", 1, s, last, nil, 0); ok {
		t.Fatalf("red Heikin-Ashi candle must block the entry (%s)", why)
	}
	c.Strategy.HAEntry = "off"
	if _, ok, why := NewStrategy(c.Strategy, c.Costs).Evaluate("X", 1, s, last, nil, 0); !ok {
		t.Fatalf("without the filter the breakout should be taken: %s", why)
	}
}

func TestHeikinAshiExit(t *testing.T) {
	c := cfg()
	c.Strategy.ExitBelowEMAFast = false
	c.Strategy.HAExit, c.Strategy.HAExitBars = "red", 2
	st := NewStrategy(c.Strategy, c.Costs)
	cl := uptrend(120, 500, 1)
	for k := 116; k < 120; k++ { // four falling days at the end
		cl[k] = cl[k-1] - 2
	}
	s := NewSeries(bars(cl, 1e6), c.Strategy)
	if !s.HARed(118) || !s.HARed(119) {
		t.Fatal("fixture: expected red HA candles")
	}
	pos := &models.Position{EntryPrice: 400, InitialStop: 380, Stop: 401, Stage: models.StageBreakeven, HighestClose: 620, Quantity: 1}
	if r := st.Manage(pos, s, 119); r == "" {
		t.Fatal("2 red Heikin-Ashi candles after breakeven must exit")
	}
	pos = &models.Position{EntryPrice: 600, InitialStop: 560, Stop: 560, Stage: models.StageInitial, HighestClose: 600, Quantity: 1}
	if r := st.Manage(pos, s, 119); r != "" {
		t.Fatalf("before breakeven the HA exit is off by default, got %q", r)
	}
}

func TestEMACrossSupertrendEntryAndExit(t *testing.T) {
	c := config.Defaults() // ema_cross is the default rule set
	st := NewStrategy(c.Strategy, c.Costs)
	cl := uptrend(80, 600, -2)                     // falling: EMA10 < EMA20, Supertrend red
	cl = append(cl, uptrend(60, cl[79]+3, 4)...)   // strong rise: cross up, Supertrend green
	cl = append(cl, uptrend(40, cl[139]-5, -5)...) // fall again: cross down
	s := NewSeries(bars(cl, 1e6), c.Strategy)
	entries := 0
	first := -1
	for i := st.Warmup(); i < 140; i++ {
		if sig, ok, _ := st.Evaluate("X", 1, s, i, nil, 0); ok {
			entries++
			if first < 0 {
				first = i
			}
			if sig.Setup != models.SetupEMACross || !(s.CrossFast[i] > s.CrossSlow[i] && s.STDir[i] == 1) {
				t.Fatalf("bad signal at %d: %+v", i, sig)
			}
		}
	}
	if entries != 1 {
		t.Fatalf("want exactly one entry on the day both conditions turn true, got %d", entries)
	}
	pos := &models.Position{EntryPrice: cl[first], InitialStop: cl[first] * 0.9, Stop: cl[first] * 0.9, Stage: models.StageInitial, HighestClose: cl[first], Quantity: 1}
	exit := -1
	for i := first + 1; i < len(cl); i++ {
		if r := st.Manage(pos, s, i); r != "" {
			exit = i
			break
		}
	}
	if exit < 140 || s.CrossFast[exit] >= s.CrossSlow[exit] {
		t.Fatalf("exit must come from EMA10 crossing below EMA20 in the final fall, got bar %d", exit)
	}
	if pos.Stop != cl[first]*0.9 {
		t.Fatalf("fixed_stop: the stop must not move (got %.2f)", pos.Stop)
	}
}
