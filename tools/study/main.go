// Command study compares strategy variants (Heikin-Ashi filters) on cached
// daily candles, over the full window and each half separately.
//
//	study -candles data/candles -symbols symbols.json -config config.yaml
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nkalva/kitealgo/internal/backtest"
	"github.com/nkalva/kitealgo/internal/config"
	"github.com/nkalva/kitealgo/internal/data"
	"github.com/nkalva/kitealgo/pkg/models"
)

type variant struct {
	name string
	set  func(*config.StrategyConfig)
}

func main() {
	dir := flag.String("candles", "data/candles", "candle cache directory (<token>.json)")
	symf := flag.String("symbols", "symbols.json", "token → symbol map")
	cfgPath := flag.String("config", "", "config file (default: built-in defaults)")
	years := flag.Int("years", 5, "years")
	grid := flag.String("grid", "ha", "ha: Heikin-Ashi variants · entry: entry-side variants on top of the HA exit")
	detail := flag.String("detail", "", "print a trade breakdown for the variant with this exact name")
	uniPath := flag.String("universe", "universe.csv", "only these symbols (missing file: every cached stock)")
	capsPath := flag.String("caps", "caps.csv", "market-cap categories for the holdings market check")
	capital := flag.Float64("capital", 0, "starting capital (0: backtest.capital from the config)")
	outDir := flag.String("out", "", "holdings grid: write the full report of the -detail variant here")
	flag.Parse()

	cfg := config.Defaults()
	if *cfgPath != "" {
		var err error
		if cfg, err = config.Load(*cfgPath); err != nil {
			fail(err)
		}
	}
	if *capital > 0 {
		cfg.Backtest.Capital = *capital
	}
	if *grid == "holdings" { // the backtester's capital (the other grids keep risk.capital for comparability)
		cfg = cfg.ForBacktest()
	}
	syms := map[string]string{}
	if raw, err := os.ReadFile(*symf); err == nil {
		_ = json.Unmarshal(raw, &syms)
	} else { // token → symbol from the cached instrument master
		for _, x := range data.NewStore(filepath.Dir(*dir), nil, nil).LatestInstruments() {
			if x.Segment == "INDICES" || (x.Segment == "NSE" && x.InstrumentType == "EQ") {
				syms[strconv.FormatUint(uint64(x.InstrumentToken), 10)] = x.TradingSymbol
			}
		}
	}
	if len(syms) == 0 {
		fail(fmt.Errorf("no symbol map: pass -symbols or keep data/instruments"))
	}
	universe := map[string]bool{}
	if u, _, err := data.LoadUniverse(*uniPath); err == nil {
		for _, s := range u {
			universe[s] = true
		}
	}
	var in backtest.Input
	in.Indices = map[string][]models.Bar{}
	in.Caps, _, _ = data.LoadCaps(*capsPath)
	files, _ := filepath.Glob(filepath.Join(*dir, "*.json"))
	for _, f := range files {
		tok := strings.TrimSuffix(filepath.Base(f), ".json")
		var cf struct {
			Bars []models.Bar `json:"bars"`
		}
		b, _ := os.ReadFile(f)
		if json.Unmarshal(b, &cf) != nil || len(cf.Bars) == 0 {
			continue
		}
		name := syms[tok]
		if name == "NIFTY 50" {
			in.Index = cf.Bars
			continue
		}
		if strings.HasPrefix(name, "NIFTY") {
			in.Indices[name] = cf.Bars
			continue
		}
		if name == "" || (len(universe) > 0 && !universe[name]) {
			continue
		}
		t, _ := strconv.ParseUint(tok, 10, 32)
		in.Instruments = append(in.Instruments, backtest.Instrument{Symbol: name, Token: uint32(t), Bars: cf.Bars})
	}
	sort.Slice(in.Instruments, func(a, b int) bool { return in.Instruments[a].Symbol < in.Instruments[b].Symbol })
	to := in.Index[len(in.Index)-1].Date
	from := to.AddDate(-*years, 0, 0)
	mid := from.Add(to.Sub(from) / 2)
	fmt.Printf("%d stocks, %s → %s (halves split at %s)\n\n", len(in.Instruments), from.Format("2006-01-02"), to.Format("2006-01-02"), mid.Format("2006-01-02"))

	vs := []variant{{"baseline (current rules)", func(*config.StrategyConfig) {}}}
	for _, e := range []string{"off", "green", "strong"} {
		for _, x := range []struct {
			n      string
			mode   string
			bars   int
			keep   bool // keep the EMA20 exit as well
			always bool
		}{
			{"", "off", 0, true, false},
			{"red1 +ema", "red", 1, true, false}, {"red2 +ema", "red", 2, true, false},
			{"red1 (no ema)", "red", 1, false, false}, {"red2 (no ema)", "red", 2, false, false}, {"red3 (no ema)", "red", 3, false, false},
			{"strongred (no ema)", "strong_red", 0, false, false}, {"strongred +ema", "strong_red", 0, true, false},
			{"red2 always (no ema)", "red", 2, false, true}, {"strongred always +ema", "strong_red", 0, true, true},
		} {
			if e == "off" && x.mode == "off" {
				continue
			}
			e, x := e, x
			name := "entry " + e
			if x.mode != "off" {
				name += " · exit " + x.n
			}
			vs = append(vs, variant{name, func(s *config.StrategyConfig) {
				s.HAEntry, s.HAExit, s.HAExitAlways, s.ExitBelowEMAFast = e, x.mode, x.always, x.keep
				if x.bars > 0 {
					s.HAExitBars = x.bars
				}
			}})
		}
	}
	if *grid == "entry" {
		vs = vs[:1]
		for _, ha := range []bool{false, true} {
			for _, set := range []string{"both", "breakout"} {
				for _, rg := range []string{"basic", "strict"} {
					for _, sm := range []float64{2, 3} {
						ha, set, rg, sm := ha, set, rg, sm
						name := fmt.Sprintf("%s · %s · regime %s · stop %.0f×ATR", map[bool]string{false: "no HA", true: "HA green+red2"}[ha], set, rg, sm)
						vs = append(vs, variant{name, func(s *config.StrategyConfig) {
							s.Setups, s.RegimeMode, s.StopATRMult = set, rg, sm
							if ha {
								s.HAEntry, s.HAExit, s.HAExitBars = "green", "red", 2
							}
						}})
					}
				}
			}
		}
	}
	if *grid == "cross" {
		vs = []variant{{"defaults in this build", func(*config.StrategyConfig) {}}}
		for _, tf := range []bool{false, true} {
			for _, sm := range []string{"atr", "supertrend"} {
				for _, rat := range []bool{false, true} {
					for _, ts := range []bool{false, true} {
						tf, sm, rat, ts := tf, sm, rat, ts
						name := fmt.Sprintf("cross · trend50 %s · stop %s · ratchet %s · time %s", onoff(tf), sm, onoff(rat), onoff(ts))
						vs = append(vs, variant{name, func(s *config.StrategyConfig) {
							s.Setups, s.ExitOnEMACross, s.CrossTrendFilter, s.StopMode = "ema_cross", true, tf, sm
							s.HAEntry, s.HAExit, s.ExitBelowEMAFast = "off", "off", false
							if !rat {
								s.BreakevenR, s.LockR = 1e6, 2e6 // stop stays at the initial level
							}
							if !ts {
								s.MaxHoldBars = 0
							}
						}})
					}
				}
			}
		}
	}
	if *grid == "exits" {
		vs = []variant{{"current: fixed 3xATR stop, exit on cross", func(*config.StrategyConfig) {}}}
		add := func(name string, f func(s *config.StrategyConfig)) { vs = append(vs, variant{name, f}) }
		for _, m := range []float64{2, 2.5, 4} {
			m := m
			add(fmt.Sprintf("A stop %.1fxATR", m), func(s *config.StrategyConfig) { s.StopATRMult = m })
		}
		add("A stop supertrend line", func(s *config.StrategyConfig) { s.StopMode = "supertrend" })
		for _, n := range []int{10, 20} {
			n := n
			add(fmt.Sprintf("A stop swing low %d", n), func(s *config.StrategyConfig) { s.StopMode, s.SwingLowBars = "swing_low", n })
		}
		for _, r := range []float64{1, 1.5, 2} {
			r := r
			add(fmt.Sprintf("B breakeven at +%.1fR", r), func(s *config.StrategyConfig) { s.BreakevenAtR = r })
		}
		for _, m := range []float64{3, 4, 5} {
			for _, r := range []float64{1, 2} {
				m, r := m, r
				add(fmt.Sprintf("C trail %.0fxATR from +%.0fR", m, r), func(s *config.StrategyConfig) { s.TrailMode, s.TrailATRMult, s.TrailStartR = "atr", m, r })
			}
		}
		for _, r := range []float64{0, 1, 2} {
			r := r
			add(fmt.Sprintf("D trail supertrend from +%.0fR", r), func(s *config.StrategyConfig) { s.TrailMode, s.TrailStartR = "supertrend", r })
		}
		for _, r := range []float64{3, 5} {
			r := r
			add(fmt.Sprintf("E target +%.0fR (all)", r), func(s *config.StrategyConfig) { s.TargetR, s.PartialPct = r, 100 })
		}
		for _, r := range []float64{2, 3} {
			r := r
			add(fmt.Sprintf("E book 50%% at +%.0fR, rest to cross", r), func(s *config.StrategyConfig) { s.TargetR, s.PartialPct = r, 50 })
		}
		add("E book 50% at +2R, rest supertrend trail", func(s *config.StrategyConfig) { s.TargetR, s.PartialPct, s.TrailMode = 2, 50, "supertrend" })
		add("F no cross exit, supertrend trail only", func(s *config.StrategyConfig) { s.ExitOnEMACross, s.TrailMode = false, "supertrend" })
		add("F no cross exit, 4xATR trail only", func(s *config.StrategyConfig) { s.ExitOnEMACross, s.TrailMode, s.TrailATRMult = false, "atr", 4 })
	}
	if *grid == "combo" {
		vs = []variant{{"current: fixed 3xATR stop, exit on cross", func(*config.StrategyConfig) {}}}
		add := func(name string, f func(s *config.StrategyConfig)) { vs = append(vs, variant{name, f}) }
		for _, t := range []float64{4, 5, 6, 8} {
			t := t
			add(fmt.Sprintf("target +%.0fR", t), func(s *config.StrategyConfig) { s.TargetR, s.PartialPct = t, 100 })
		}
		for _, be := range []float64{1.25, 1.5, 1.75} {
			be := be
			add(fmt.Sprintf("breakeven +%.2fR", be), func(s *config.StrategyConfig) { s.BreakevenAtR = be })
		}
		add("2.5xATR + BE 1.5R", func(s *config.StrategyConfig) { s.StopATRMult, s.BreakevenAtR = 2.5, 1.5 })
		add("BE 1.5R + target 5R", func(s *config.StrategyConfig) { s.BreakevenAtR, s.TargetR, s.PartialPct = 1.5, 5, 100 })
		add("2.5xATR + target 5R", func(s *config.StrategyConfig) { s.StopATRMult, s.TargetR, s.PartialPct = 2.5, 5, 100 })
		add("2.5xATR + BE 1.5R + target 5R", func(s *config.StrategyConfig) {
			s.StopATRMult, s.BreakevenAtR, s.TargetR, s.PartialPct = 2.5, 1.5, 5, 100
		})
		add("2.0xATR + BE 1.5R", func(s *config.StrategyConfig) { s.StopATRMult, s.BreakevenAtR = 2, 1.5 })
	}
	if *grid == "ha2" {
		vs = []variant{{"defaults in this build", func(*config.StrategyConfig) {}}}
		add := func(name string, f func(s *config.StrategyConfig)) { vs = append(vs, variant{name, f}) }
		add("1 entry: HA candle green", func(s *config.StrategyConfig) { s.HAEntry = "green" })
		add("1 entry: HA green, no lower wick", func(s *config.StrategyConfig) { s.HAEntry = "strong" })
		add("2 EMA + Supertrend on HA candles", func(s *config.StrategyConfig) { s.CrossSource = "ha" })
		add("2 HA EMA/ST + HA green entry", func(s *config.StrategyConfig) { s.CrossSource, s.HAEntry = "ha", "green" })
		add("3 + exit 2 red HA (after breakeven)", func(s *config.StrategyConfig) { s.HAExit, s.HAExitBars = "red", 2 })
		add("3 + exit 3 red HA (after breakeven)", func(s *config.StrategyConfig) { s.HAExit, s.HAExitBars = "red", 3 })
		add("3 + exit strong red HA (after breakeven)", func(s *config.StrategyConfig) { s.HAExit = "strong_red" })
		add("3 + exit 3 red HA (any time)", func(s *config.StrategyConfig) { s.HAExit, s.HAExitBars, s.HAExitAlways = "red", 3, true })
		add("1+3 HA green entry + 3 red exit", func(s *config.StrategyConfig) { s.HAEntry, s.HAExit, s.HAExitBars = "green", "red", 3 })
		add("2+3 HA EMA/ST + 3 red exit", func(s *config.StrategyConfig) { s.CrossSource, s.HAExit, s.HAExitBars = "ha", "red", 3 })
	}
	if *grid == "holdings" {
		type hv struct {
			name string
			f    func(c *config.Config)
		}
		on := func(c *config.Config, slots int, entry, market string) {
			c.Holdings.Enabled, c.Holdings.Slots, c.Holdings.MarketCheck = true, slots, market
			c.Strategy.EntryMode = entry
		}
		hvs := []hv{
			{"current rules (1% risk sizing, 5 max, 2 new/day)", func(*config.Config) {}},
			{"hold 5 · fresh cross only · NIFTY check", func(c *config.Config) { on(c, 5, "cross", "nifty") }},
			{"hold 5 · cross+trend · NIFTY check", func(c *config.Config) { on(c, 5, "both", "nifty") }},
			{"hold 5 · cross+trend · category check", func(c *config.Config) { on(c, 5, "both", "category") }},
			{"hold 5 · cross+trend · category, no index HA", func(c *config.Config) {
				on(c, 5, "both", "category")
				c.Holdings.MarketHAGreen = false
			}},
			{"hold 5 · cross+trend · no market check", func(c *config.Config) { on(c, 5, "both", "off") }},
			{"hold 5 · trend only · category check", func(c *config.Config) { on(c, 5, "trend", "category") }},
			{"hold 3 · cross+trend · category check", func(c *config.Config) { on(c, 3, "both", "category") }},
			{"hold 7 · cross+trend · category check", func(c *config.Config) { on(c, 7, "both", "category") }},
			{"hold 10 · cross+trend · category check", func(c *config.Config) { on(c, 10, "both", "category") }},
			{"hold 5 · cross+trend ≤10d · category", func(c *config.Config) { on(c, 5, "both", "category"); c.Strategy.TrendMaxDays = 10 }},
			{"hold 5 · cross+trend ≤30d · category", func(c *config.Config) { on(c, 5, "both", "category"); c.Strategy.TrendMaxDays = 30 }},
			{"hold 5 · cross+trend ≤5% ext · category", func(c *config.Config) { on(c, 5, "both", "category"); c.Strategy.TrendMaxExtPct = 5 }},
			{"hold 5 · cross+trend ≤12% ext · category", func(c *config.Config) { on(c, 5, "both", "category"); c.Strategy.TrendMaxExtPct = 12 }},
		}
		fmt.Printf("capital ₹%.0f · indices cached: %d (%s)\n\n", cfg.Risk.Capital, len(in.Indices), strings.Join(keys(in.Indices), ", "))
		fmt.Printf("%-48s | %12s %8s %6s %6s %5s %4s %5s %6s | %7s %7s\n", "variant", "end ₹", "return%", "CAGR%", "maxDD%", "PF", "trd", "win%", "invst%", "H1 ret%", "H2 ret%")
		fmt.Println(strings.Repeat("-", 136))
		for _, v := range hvs {
			c := cfg
			v.f(&c)
			if err := c.Validate(); err != nil {
				fail(err)
			}
			in.From, in.To, in.Config = from, to, c
			r := backtest.Run(in)
			f := r.Summary
			h1, h2 := run(in, c, from, mid), run(in, c, mid, to)
			fmt.Printf("%-48s | %12.0f %8.1f %6.1f %6.1f %5.2f %4d %5.1f %6.1f | %7.1f %7.1f\n", v.name, f.EndEquity, f.TotalReturnPct, f.CAGRPct,
				f.MaxDrawdownPct, f.ProfitFactor, f.Trades, f.WinRatePct, f.AvgInvestedPct, h1.TotalReturnPct, h2.TotalReturnPct)
			if *detail == v.name {
				for _, y := range f.Yearly {
					fmt.Printf("    %d  ₹%10.0f → ₹%10.0f  %6.1f%%  (NIFTY %5.1f%%)  %d trades\n", y.Year, y.StartEquity, y.EndEquity, y.ReturnPct, y.BenchmarkPct, y.Trades)
				}
				for _, n := range r.Notes {
					fmt.Println("    note:", n)
				}
				fmt.Printf("    skipped: %v\n", r.Skipped)
				if *outDir != "" {
					if err := backtest.WriteReport(*outDir, r); err != nil {
						fail(err)
					}
					fmt.Println("    report:", filepath.Join(*outDir, "report.html"))
				}
			}
		}
		b := run(in, cfg, from, to)
		fmt.Printf("\nNIFTY buy-and-hold over the full window: %.1f%%\n", b.BenchmarkReturnPc)
		return
	}
	if *grid == "mtf" {
		type mv struct {
			name string
			f    func(c *config.Config)
		}
		on := func(c *config.Config, lev, capPct, risk float64, pos int) {
			c.MTF.Enabled, c.MTF.Leverage, c.MTF.BorrowOnlyShortfall = true, lev, false
			c.Risk.MaxPositionPct, c.Risk.RiskPerTradePct, c.Risk.MaxPositions = capPct, risk, pos
		}
		mvs := []mv{
			{"CNC (current: no borrowing)", func(*config.Config) {}},
			{"MTF 2x on every buy, same sizing", func(c *config.Config) { on(c, 2, 20, 1, 5) }},
			{"MTF 2x only for the shortfall, 8 positions of 20%", func(c *config.Config) { on(c, 2, 20, 1, 8); c.MTF.BorrowOnlyShortfall = true }},
			{"MTF 2x only for the shortfall, 10 positions of 20%", func(c *config.Config) { on(c, 2, 20, 1, 10); c.MTF.BorrowOnlyShortfall = true }},
			{"MTF 2x only for the shortfall, 5 x 30%, risk 1.5%", func(c *config.Config) { on(c, 2, 30, 1.5, 5); c.MTF.BorrowOnlyShortfall = true }},
			{"CNC, 5 x 30%, risk 1.5% (same sizing, no loan)", func(c *config.Config) { c.Risk.MaxPositionPct, c.Risk.RiskPerTradePct = 30, 1.5 }},
			{"MTF shortfall, 5 x 25%, risk 1.25%", func(c *config.Config) { on(c, 2, 25, 1.25, 5); c.MTF.BorrowOnlyShortfall = true }},
			{"MTF shortfall, 5 x 35%, risk 1.75%", func(c *config.Config) { on(c, 2, 35, 1.75, 5); c.MTF.BorrowOnlyShortfall = true }},
			{"MTF shortfall, 5 x 30%, risk 1.5%, interest 0.05%/day", func(c *config.Config) {
				on(c, 2, 30, 1.5, 5)
				c.MTF.BorrowOnlyShortfall, c.MTF.InterestPctPerDay = true, 0.05
			}},
			{"MTF 2x, 10 positions of 20%", func(c *config.Config) { on(c, 2, 20, 1, 10) }},
			{"MTF 2x, 5 positions of 40%, risk 2%", func(c *config.Config) { on(c, 2, 40, 2, 5) }},
			{"MTF 1.5x, 5 positions of 30%, risk 1.5%", func(c *config.Config) { on(c, 1.5, 30, 1.5, 5) }},
			{"MTF 3x, 5 positions of 60%, risk 3%", func(c *config.Config) { on(c, 3, 60, 3, 5) }},
			{"MTF 2x, 5 x 40%, risk 2% — if interest were 0", func(c *config.Config) { on(c, 2, 40, 2, 5); c.MTF.InterestPctPerDay = 0 }},
		}
		fmt.Printf("%-52s | %8s %6s %5s %5s | %9s %9s | %7s %7s | %6s\n", "variant", "return%", "maxDD%", "PF", "trd", "charges₹", "MTF₹", "H1 ret%", "H2 ret%", "ret/DD")
		fmt.Println(strings.Repeat("-", 124))
		for _, v := range mvs {
			c := cfg
			v.f(&c)
			if err := c.Validate(); err != nil {
				fail(err)
			}
			in.From, in.To, in.Config = from, to, c
			r := backtest.Run(in)
			f := r.Summary
			h1, h2 := run(in, c, from, mid), run(in, c, mid, to)
			fmt.Printf("%-52s | %8.1f %6.1f %5.2f %5d | %9.0f %9.0f | %7.1f %7.1f | %6.2f\n", v.name, f.TotalReturnPct, f.MaxDrawdownPct, f.ProfitFactor, f.Trades, f.TotalCosts, r.MTFCosts, h1.TotalReturnPct, h2.TotalReturnPct, f.TotalReturnPct/f.MaxDrawdownPct)
		}
		return
	}
	if *grid == "portfolio" {
		vs = []variant{{"current (1% risk, 5 pos, 20% cap, 2 new/day, regime on)", func(*config.StrategyConfig) {}}}
		// Risk/market settings live outside StrategyConfig; carry them via closures on cfg copies.
		type pv struct {
			name string
			f    func(c *config.Config)
		}
		pvs2 := []pv{}
		for _, n := range []int{6, 7, 8, 10} {
			for _, rg := range []string{"basic", "strict"} {
				n, rg := n, rg
				capPct := math.Round(100.0 / float64(n) * 1.2)
				pvs2 = append(pvs2, pv{fmt.Sprintf("%d pos, %.0f%% cap, regime %s", n, capPct, rg), func(c *config.Config) {
					c.Risk.MaxPositions, c.Risk.MaxPositionPct, c.Strategy.RegimeMode = n, capPct, rg
				}})
			}
		}
		pvs := []pv{
			{"regime filter OFF", func(c *config.Config) { c.Market.RegimeFilter = false }},
			{"regime strict", func(c *config.Config) { c.Strategy.RegimeMode = "strict" }},
			{"risk 0.75%", func(c *config.Config) { c.Risk.RiskPerTradePct = 0.75 }},
			{"risk 1.5%", func(c *config.Config) { c.Risk.RiskPerTradePct = 1.5 }},
			{"risk 2%", func(c *config.Config) { c.Risk.RiskPerTradePct = 2 }},
			{"8 positions, 15% cap", func(c *config.Config) { c.Risk.MaxPositions, c.Risk.MaxPositionPct = 8, 15 }},
			{"10 positions, 12% cap", func(c *config.Config) { c.Risk.MaxPositions, c.Risk.MaxPositionPct = 10, 12 }},
			{"8 pos, 15% cap, risk 1.5%", func(c *config.Config) {
				c.Risk.MaxPositions, c.Risk.MaxPositionPct, c.Risk.RiskPerTradePct = 8, 15, 1.5
			}},
			{"5 pos, 25% cap", func(c *config.Config) { c.Risk.MaxPositionPct = 25 }},
			{"3 new per day", func(c *config.Config) { c.Risk.MaxNewPerDay = 3 }},
			{"cooldown 0", func(c *config.Config) { c.Strategy.CooldownBars = 0 }},
			{"cooldown 10", func(c *config.Config) { c.Strategy.CooldownBars = 10 }},
			{"max gap-up 1%", func(c *config.Config) { c.Strategy.MaxGapUpPct = 1 }},
			{"max gap-up 4%", func(c *config.Config) { c.Strategy.MaxGapUpPct = 4 }},
			{"RS lookback 20", func(c *config.Config) { c.Strategy.RSLookback = 20 }},
			{"RS lookback 120", func(c *config.Config) { c.Strategy.RSLookback = 120 }},
			{"min turnover 50 cr", func(c *config.Config) { c.Strategy.MinTurnoverCr = 50 }},
		}
		fmt.Printf("%-62s | %8s %6s %5s %5s %6s %6s | %7s %5s | %7s %5s | %5s\n", "variant", "return%", "maxDD%", "PF", "trd", "win%", "expo%", "H1 ret%", "H1 PF", "H2 ret%", "H2 PF", "ret/DD")
		fmt.Println(strings.Repeat("-", 150))
		if *detail == "combo" {
			pvs = pvs2
		}
		all := append([]pv{{vs[0].name, func(*config.Config) {}}}, pvs...)
		for _, v := range all {
			c := cfg
			v.f(&c)
			if err := c.Validate(); err != nil {
				fail(err)
			}
			f, h1, h2 := run(in, c, from, to), run(in, c, from, mid), run(in, c, mid, to)
			fmt.Printf("%-62s | %8.1f %6.1f %5.2f %5d %6.1f %6.1f | %7.1f %5.2f | %7.1f %5.2f | %5.2f\n", v.name,
				f.TotalReturnPct, f.MaxDrawdownPct, f.ProfitFactor, f.Trades, f.WinRatePct, f.ExposurePct, h1.TotalReturnPct, h1.ProfitFactor, h2.TotalReturnPct, h2.ProfitFactor, f.TotalReturnPct/f.MaxDrawdownPct)
		}
		return
	}
	fmt.Printf("%-62s | %8s %6s %5s %5s %6s %6s | %7s %5s | %7s %5s\n", "variant", "return%", "maxDD%", "PF", "trd", "win%", "avgW/L", "H1 ret%", "H1 PF", "H2 ret%", "H2 PF")
	fmt.Println(strings.Repeat("-", 142))
	for _, v := range vs {
		c := cfg
		v.set(&c.Strategy)
		if err := c.Validate(); err != nil {
			fail(err)
		}
		f := run(in, c, from, to)
		h1 := run(in, c, from, mid)
		h2 := run(in, c, mid, to)
		fmt.Printf("%-62s | %8.1f %6.1f %5.2f %5d %6.1f %6.2f | %7.1f %5.2f | %7.1f %5.2f\n", v.name,
			f.TotalReturnPct, f.MaxDrawdownPct, f.ProfitFactor, f.Trades, f.WinRatePct, ratio(f), h1.TotalReturnPct, h1.ProfitFactor, h2.TotalReturnPct, h2.ProfitFactor)
	}
	b := run(in, cfg, from, to)
	fmt.Printf("\nNIFTY buy-and-hold over the full window: %.1f%%\n", b.BenchmarkReturnPc)
	if *detail != "" {
		for _, v := range vs {
			if v.name == *detail {
				c := cfg
				v.set(&c.Strategy)
				for _, w := range [][2]time.Time{{from, mid}, {mid, to}} {
					in.From, in.To, in.Config = w[0], w[1], c
					breakdown(v.name+" "+w[0].Format("2006-01")+"→"+w[1].Format("2006-01"), backtest.Run(in))
				}
			}
		}
	}
}

func run(in backtest.Input, c config.Config, from, to time.Time) backtest.Summary {
	in.From, in.To, in.Config = from, to, c
	return backtest.Run(in).Summary
}

func breakdown(title string, r backtest.Result) {
	type agg struct {
		n, w int
		net  float64
		r    float64
	}
	by := map[string]*agg{}
	add := func(k string, t models.Trade) {
		a := by[k]
		if a == nil {
			a = &agg{}
			by[k] = a
		}
		a.n++
		a.net += t.Net
		a.r += t.RMultiple
		if t.Net > 0 {
			a.w++
		}
	}
	for _, t := range r.Trades {
		add("setup "+string(t.Setup), t)
		reason := t.Reason
		for _, p := range []string{"stop hit", "gapped below", "close", "Heikin", "time stop", "end of"} {
			if strings.HasPrefix(reason, p) {
				reason = p
			}
		}
		if strings.Contains(t.Reason, "below EMA") {
			reason = "close < EMA20"
		}
		add("exit  "+reason+" ["+string(t.Stage)+"]", t)
	}
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("\n== %s  (net ₹%.0f, %d trades, costs ₹%.0f)\n", title, r.Summary.EndEquity-r.Summary.StartEquity, r.Summary.Trades, r.Summary.TotalCosts)
	for _, k := range keys {
		a := by[k]
		fmt.Printf("  %-44s n=%3d win%%=%5.1f net ₹%8.0f avgR %5.2f\n", k, a.n, float64(a.w)/float64(a.n)*100, a.net, a.r/float64(a.n))
	}
	fmt.Printf("  skipped: %v\n", r.Skipped)
}

func onoff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func ratio(s backtest.Summary) float64 {
	if s.AvgLossPct == 0 {
		return 0
	}
	return -s.AvgWinPct / s.AvgLossPct
}

func keys(m map[string][]models.Bar) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func fail(err error) { fmt.Fprintln(os.Stderr, "study:", err); os.Exit(1) }
