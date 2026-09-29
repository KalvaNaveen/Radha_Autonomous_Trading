package main

import (
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nkalva/kitealgo/internal/backtest"
	"github.com/nkalva/kitealgo/internal/config"
)

// combo is one point of the full holdings-mode grid.
type combo struct {
	Slots    int
	Entry    string
	Days     int
	Ext      float64
	Research string
	Rank     string
	StopATR  float64
	HA       string
	Market   string
	TopN     int // auto universe: stocks scanned each month (0 = off)
	Look     int // auto universe: strength window, sessions

	Full, H1, H2 backtest.Summary
	Robust       float64 // median CAGR of this combo and its one-step neighbours
	Neighbours   int
}

func (c *combo) key() string {
	return fmt.Sprintf("%d|%s|%d|%g|%s|%s|%g|%s|%s|%d|%d", c.Slots, c.Entry, c.Days, c.Ext, c.Research, c.Rank, c.StopATR, c.HA, c.Market, c.TopN, c.Look)
}

func (c *combo) apply(cfg *config.Config) {
	cfg.Holdings.Enabled, cfg.Holdings.Slots, cfg.Holdings.MarketCheck = true, c.Slots, c.Market
	s := &cfg.Strategy
	s.EntryMode, s.TrendMaxDays, s.TrendMaxExtPct = c.Entry, c.Days, c.Ext
	s.ResearchAllow, s.ResearchRank, s.StopATRMult, s.HAEntry = c.Research, c.Rank, c.StopATR, c.HA
	if c.TopN > 0 {
		cfg.Backtest.AutoUniverse = config.AutoUniverseConfig{Enabled: true, Source: "all_nse", TopN: c.TopN, LookbackDays: c.Look}
	}
}

var (
	gSlots    = []int{3, 5, 7, 10}
	gEntry    = []string{"cross", "both", "trend"}
	gDays     = []int{10, 15, 30}
	gExt      = []float64{5, 8, 12}
	gResearch = []string{"all", "RESULTS,TURNAROUND,NEW_HIGH,MOMENTUM", "RESULTS,NEW_HIGH,MOMENTUM", "RESULTS,NEW_HIGH", "NEW_HIGH,MOMENTUM", "RESULTS,TURNAROUND"}
	gRank     = []string{"rs", "research"}
	gStop     = []float64{2.5, 3, 4}
	gHA       = []string{"green", "off"}
	gTopN     = []int{0} // 0: keep the stock list as loaded
	gLook     = []int{0}
)

var researchName = map[string]string{
	"all": "all", "RESULTS,TURNAROUND,NEW_HIGH,MOMENTUM": "all but NONE", "RESULTS,NEW_HIGH,MOMENTUM": "results+high+momentum",
	"RESULTS,NEW_HIGH": "results+high", "NEW_HIGH,MOMENTUM": "high+momentum", "RESULTS,TURNAROUND": "results+turnaround",
	"TURNAROUND,NEW_HIGH,MOMENTUM,NONE": "all but RESULTS", "TURNAROUND,MOMENTUM": "turnaround+momentum",
}

// fullGrid runs every combination (index gate off) over the whole window and
// each half, scores robustness, writes every row to csvPath and prints the
// leaders. The winner is then re-run with the NIFTY index gate for comparison.
func fullGrid(in backtest.Input, cfg config.Config, from, mid, to time.Time, csvPath string) {
	var cs []*combo
	for _, sl := range gSlots {
		for _, en := range gEntry {
			days, exts := gDays, gExt
			if en == "cross" { // trend limits do not apply
				days, exts = []int{15}, []float64{8}
			}
			for _, d := range days {
				for _, x := range exts {
					for _, r := range gResearch {
						for _, rk := range gRank {
							for _, stp := range gStop {
								for _, ha := range gHA {
									for _, tn := range gTopN {
										for _, lb := range gLook {
											cs = append(cs, &combo{Slots: sl, Entry: en, Days: d, Ext: x, Research: r, Rank: rk, StopATR: stp, HA: ha, Market: "off", TopN: tn, Look: lb})
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
	// Indicators do not depend on the varied settings: compute them once.
	pc := cfg
	if gTopN[0] > 0 {
		pc.Backtest.AutoUniverse = config.AutoUniverseConfig{Enabled: true, Source: "all_nse", TopN: gTopN[0], LookbackDays: gLook[0]}
	}
	in.From, in.To, in.Config = from, to, pc
	t0 := time.Now()
	in.Prepared = backtest.Prepare(in)
	fmt.Printf("indicators ready for %d-stock pool in %s\n", len(in.Instruments), time.Since(t0).Round(time.Second))
	fmt.Printf("%d combinations × 3 windows on %d workers …\n", len(cs), runtime.NumCPU())
	var done atomic.Int64
	start := time.Now()
	jobs := make(chan *combo)
	var wg sync.WaitGroup
	for w := 0; w < runtime.NumCPU(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range jobs {
				cc := cfg
				c.apply(&cc)
				if err := cc.Validate(); err != nil {
					fail(err)
				}
				c.Full, c.H1, c.H2 = run(in, cc, from, to), run(in, cc, from, mid), run(in, cc, mid, to)
				if n := done.Add(1); n%200 == 0 {
					el := time.Since(start)
					fmt.Printf("  %d/%d  (%s elapsed, ~%s left)\n", n, len(cs), el.Round(time.Second), (el / time.Duration(n) * time.Duration(int64(len(cs))-n)).Round(time.Second))
				}
			}
		}()
	}
	for _, c := range cs {
		jobs <- c
	}
	close(jobs)
	wg.Wait()

	// Robustness: median full-period CAGR over the combo and every combo that
	// differs from it in exactly one setting by one step.
	by := map[string]*combo{}
	for _, c := range cs {
		by[c.key()] = c
	}
	for _, c := range cs {
		vals := []float64{c.Full.CAGRPct}
		for _, n := range neighbours(c) {
			if x, ok := by[n.key()]; ok {
				vals = append(vals, x.Full.CAGRPct)
			}
		}
		sort.Float64s(vals)
		c.Robust, c.Neighbours = vals[len(vals)/2], len(vals)-1
	}
	writeCombos(csvPath, cs)

	print := func(title string, list []*combo, n int) {
		fmt.Printf("\n== %s\n", title)
		fmt.Printf("%-4s %-5s %-4s %-4s %-22s %-8s %-4s %-5s | %11s %7s %6s %6s %5s %4s | %7s %7s | %7s\n", "hold", "entry", "days", "ext", "research", "rank", "stop", "HA",
			"end ₹", "ret%", "CAGR%", "maxDD%", "PF", "trd", "H1 ret%", "H2 ret%", "robust")
		for k, c := range list {
			if k >= n {
				break
			}
			f := c.Full
			fmt.Printf("%-4d %-5s %-4d %-4g %-22s %-8s %-4g %-5s | %11.0f %7.1f %6.1f %6.1f %5.2f %4d | %7.1f %7.1f | %7.1f\n", c.Slots, c.Entry, c.Days, c.Ext, researchName[c.Research], c.Rank, c.StopATR, c.HA,
				f.EndEquity, f.TotalReturnPct, f.CAGRPct, f.MaxDrawdownPct, f.ProfitFactor, f.Trades, c.H1.TotalReturnPct, c.H2.TotalReturnPct, c.Robust)
		}
	}
	sorted := func(less func(a, b *combo) bool, keep func(c *combo) bool) []*combo {
		var out []*combo
		for _, c := range cs {
			if keep == nil || keep(c) {
				out = append(out, c)
			}
		}
		sort.SliceStable(out, func(i, j int) bool { return less(out[i], out[j]) })
		return out
	}
	solid := func(c *combo) bool { return c.H1.TotalReturnPct > 0 && c.H2.TotalReturnPct > 0 && c.Full.Trades >= 40 }
	raw := sorted(func(a, b *combo) bool { return a.Full.TotalReturnPct > b.Full.TotalReturnPct }, nil)
	print("Highest 5-year return (raw — may be luck)", raw, 15)
	both := sorted(func(a, b *combo) bool {
		return math.Min(a.H1.TotalReturnPct, a.H2.TotalReturnPct) > math.Min(b.H1.TotalReturnPct, b.H2.TotalReturnPct)
	}, solid)
	print("Best worse-half (strong in BOTH 2021–24 and 2024–26)", both, 15)
	rob := sorted(func(a, b *combo) bool { return a.Robust > b.Robust }, solid)
	print("Most robust (median CAGR of the combo and its neighbours; both halves positive, ≥40 trades)", rob, 15)

	// Walk-forward: pick the best on the first half only, then see the second half.
	wf := sorted(func(a, b *combo) bool { return a.H1.TotalReturnPct > b.H1.TotalReturnPct }, nil)
	fmt.Printf("\n== Walk-forward: the 10 best on 2021–24 alone, and what they did in 2024–26 (unseen)\n")
	for k := 0; k < 10 && k < len(wf); k++ {
		c := wf[k]
		fmt.Printf("  %-70s H1 %7.1f%% → H2 %7.1f%%\n", c.label(), c.H1.TotalReturnPct, c.H2.TotalReturnPct)
	}
	var h2 []float64
	for _, c := range cs {
		h2 = append(h2, c.H2.TotalReturnPct)
	}
	sort.Float64s(h2)
	fmt.Printf("  (median H2 across all %d combos: %.1f%%)\n", len(cs), h2[len(h2)/2])

	// Averages per setting value: which choices help across everything else.
	fmt.Printf("\n== Average across all combos, per setting value (median full-period CAGR | median worse-half return)\n")
	dims := []struct {
		name string
		val  func(c *combo) string
	}{
		{"hold", func(c *combo) string { return strconv.Itoa(c.Slots) }},
		{"entry", func(c *combo) string { return c.Entry }},
		{"days", func(c *combo) string { return strconv.Itoa(c.Days) }},
		{"ext", func(c *combo) string { return strconv.FormatFloat(c.Ext, 'g', -1, 64) }},
		{"research", func(c *combo) string { return researchName[c.Research] }},
		{"rank", func(c *combo) string { return c.Rank }},
		{"stop", func(c *combo) string { return strconv.FormatFloat(c.StopATR, 'g', -1, 64) }},
		{"HA", func(c *combo) string { return c.HA }},
		{"topN", func(c *combo) string { return strconv.Itoa(c.TopN) }},
		{"lookback", func(c *combo) string { return strconv.Itoa(c.Look) }},
	}
	for _, d := range dims {
		groups := map[string][][2]float64{}
		for _, c := range cs {
			if d.name == "days" || d.name == "ext" {
				if c.Entry == "cross" {
					continue
				}
			}
			v := d.val(c)
			groups[v] = append(groups[v], [2]float64{c.Full.CAGRPct, math.Min(c.H1.TotalReturnPct, c.H2.TotalReturnPct)})
		}
		var ks []string
		for k := range groups {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		fmt.Printf("  %-9s", d.name)
		for _, k := range ks {
			g := groups[k]
			a, b := make([]float64, len(g)), make([]float64, len(g))
			for i, x := range g {
				a[i], b[i] = x[0], x[1]
			}
			sort.Float64s(a)
			sort.Float64s(b)
			fmt.Printf("  %s: %.1f%% | %.1f%%", k, a[len(a)/2], b[len(b)/2])
		}
		fmt.Println()
	}

	if len(rob) > 0 {
		w := *rob[0]
		fmt.Printf("\n== Recommended (most robust): %s\n", w.label())
		for _, m := range []string{"off", "nifty"} {
			w.Market = m
			cc := cfg
			w.apply(&cc)
			in.From, in.To, in.Config = from, to, cc
			r := backtest.Run(in)
			f := r.Summary
			fmt.Printf("  index gate %-5s: ₹%.0f → ₹%.0f  %+.1f%%  CAGR %.1f%%  maxDD %.1f%%  PF %.2f  trades %d  invested %.0f%%\n",
				m, f.StartEquity, f.EndEquity, f.TotalReturnPct, f.CAGRPct, f.MaxDrawdownPct, f.ProfitFactor, f.Trades, f.AvgInvestedPct)
			if m == "off" {
				for _, y := range f.Yearly {
					fmt.Printf("      %d  ₹%10.0f → ₹%10.0f  %6.1f%%  (NIFTY %5.1f%%)  %d trades\n", y.Year, y.StartEquity, y.EndEquity, y.ReturnPct, y.BenchmarkPct, y.Trades)
				}
				for _, g := range f.ByResearch {
					fmt.Printf("      %-11s %3d trades  win %3.0f%%  net ₹%10.0f\n", g.Key, g.Trades, 100*float64(g.Wins)/float64(g.Trades), g.Net)
				}
				var nets []float64
				var tot float64
				for _, t := range r.Trades {
					nets = append(nets, t.Net)
					tot += t.Net
				}
				sort.Sort(sort.Reverse(sort.Float64Slice(nets)))
				var top float64
				for k := 0; k < 5 && k < len(nets); k++ {
					top += nets[k]
				}
				fmt.Printf("      top 5 trades ₹%.0f of ₹%.0f net (%.0f%%)\n", top, tot, 100*top/tot)
			}
		}
	}
}

func (c *combo) label() string {
	s := fmt.Sprintf("hold %d · %s · ≤%dd · ≤%g%% · research %s · rank %s · stop %g×ATR · HA %s",
		c.Slots, c.Entry, c.Days, c.Ext, researchName[c.Research], c.Rank, c.StopATR, c.HA)
	if c.TopN > 0 {
		s += fmt.Sprintf(" · top %d by %d-day strength", c.TopN, c.Look)
	}
	return s
}

// neighbours are the combos one step away in a single ordered setting.
func neighbours(c *combo) []*combo {
	var out []*combo
	stepI := func(list []int, v int, set func(x *combo, v int)) {
		for k, x := range list {
			if x == v {
				for _, d := range []int{-1, 1} {
					if k+d >= 0 && k+d < len(list) {
						n := *c
						set(&n, list[k+d])
						out = append(out, &n)
					}
				}
			}
		}
	}
	stepF := func(list []float64, v float64, set func(x *combo, v float64)) {
		for k, x := range list {
			if x == v {
				for _, d := range []int{-1, 1} {
					if k+d >= 0 && k+d < len(list) {
						n := *c
						set(&n, list[k+d])
						out = append(out, &n)
					}
				}
			}
		}
	}
	stepI(gSlots, c.Slots, func(x *combo, v int) { x.Slots = v })
	stepF(gStop, c.StopATR, func(x *combo, v float64) { x.StopATR = v })
	stepI(gTopN, c.TopN, func(x *combo, v int) { x.TopN = v })
	stepI(gLook, c.Look, func(x *combo, v int) { x.Look = v })
	if c.Entry != "cross" {
		stepI(gDays, c.Days, func(x *combo, v int) { x.Days = v })
		stepF(gExt, c.Ext, func(x *combo, v float64) { x.Ext = v })
	}
	for _, rk := range gRank {
		if rk != c.Rank {
			n := *c
			n.Rank = rk
			out = append(out, &n)
		}
	}
	return out
}

func writeCombos(path string, cs []*combo) {
	if path == "" {
		return
	}
	f, err := os.Create(path)
	if err != nil {
		fail(err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"hold", "entry", "trend_max_days", "trend_max_ext_pct", "research", "rank", "stop_atr", "ha_entry", "index_gate",
		"start", "end", "return_pct", "cagr_pct", "max_dd_pct", "profit_factor", "trades", "win_pct", "invested_pct",
		"h1_return_pct", "h2_return_pct", "worse_half_pct", "robust_cagr_pct", "top_n", "lookback"})
	f2 := func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }
	for _, c := range cs {
		s := c.Full
		_ = w.Write([]string{strconv.Itoa(c.Slots), c.Entry, strconv.Itoa(c.Days), f2(c.Ext), researchName[c.Research], c.Rank, f2(c.StopATR), c.HA, c.Market,
			f2(s.StartEquity), f2(s.EndEquity), f2(s.TotalReturnPct), f2(s.CAGRPct), f2(s.MaxDrawdownPct), f2(s.ProfitFactor), strconv.Itoa(s.Trades),
			f2(s.WinRatePct), f2(s.AvgInvestedPct), f2(c.H1.TotalReturnPct), f2(c.H2.TotalReturnPct),
			f2(math.Min(c.H1.TotalReturnPct, c.H2.TotalReturnPct)), f2(c.Robust), strconv.Itoa(c.TopN), strconv.Itoa(c.Look)})
	}
	w.Flush()
}
