// Command study compares strategy variants (Heikin-Ashi filters) on cached
// daily candles, over the full window and each half separately.
//
//	study -candles data/candles -symbols symbols.json -config config.yaml
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nkalva/kitealgo/internal/backtest"
	"github.com/nkalva/kitealgo/internal/config"
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
	flag.Parse()

	cfg := config.Defaults()
	if *cfgPath != "" {
		var err error
		if cfg, err = config.Load(*cfgPath); err != nil {
			fail(err)
		}
	}
	var syms map[string]string
	raw, err := os.ReadFile(*symf)
	if err != nil {
		fail(err)
	}
	_ = json.Unmarshal(raw, &syms)
	var in backtest.Input
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

func fail(err error) { fmt.Fprintln(os.Stderr, "study:", err); os.Exit(1) }
