package main

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nkalva/kitealgo/internal/backtest"
	"github.com/nkalva/kitealgo/internal/config"
	"github.com/nkalva/kitealgo/internal/swing"
)

// xrec is one combination of every Backtest-tab setting, with its results
// per calendar year.
type xrec struct {
	Hold     int // 0 = current rules (1% risk sizing, max positions, 2 new/day, NIFTY regime filter)
	Entry    string
	Days     int
	Ext      float64
	Market   string // off | nifty | category (hold mode only)
	IdxHA    bool   // index Heikin-Ashi must be green
	Research uint8  // bit per tag in swing.ResearchTags order; 31 = all
	Rank     string
	TopN     int
	Look     int

	End, CAGR, DD, PF, Win, Invested, H1, H2 float64
	Trades                                    int
	Years                                     []float64 // return % per calendar year (first/last partial)
}

func (r *xrec) researchList() string {
	if r.Research == 31 {
		return "all"
	}
	var s []string
	for k, t := range swing.ResearchTags {
		if r.Research&(1<<k) != 0 {
			s = append(s, t)
		}
	}
	return strings.Join(s, ",")
}

func (r *xrec) apply(c *config.Config) {
	c.Holdings.Enabled = r.Hold > 0
	if r.Hold > 0 {
		c.Holdings.Slots, c.Holdings.MarketCheck, c.Holdings.MarketHAGreen = r.Hold, r.Market, r.IdxHA
	}
	s := &c.Strategy
	s.EntryMode, s.TrendMaxDays, s.TrendMaxExtPct, s.ResearchAllow, s.ResearchRank = r.Entry, r.Days, r.Ext, r.researchList(), r.Rank
	c.Backtest.AutoUniverse = config.AutoUniverseConfig{Enabled: r.TopN > 0, Source: "all_nse", TopN: r.TopN, LookbackDays: r.Look}
}

func (r *xrec) label() string {
	h := "current rules"
	if r.Hold > 0 {
		h = fmt.Sprintf("hold %d", r.Hold)
	}
	e := r.Entry
	if r.Entry != "cross" {
		e = fmt.Sprintf("%s ≤%dd ≤%g%%", r.Entry, r.Days, r.Ext)
	}
	m := ""
	if r.Hold > 0 {
		m = " · mkt " + r.Market
		if r.Market != "off" && r.IdxHA {
			m += "+HA"
		}
	}
	u := ""
	if r.TopN > 0 {
		u = fmt.Sprintf(" · top%d/%dd", r.TopN, r.Look)
	}
	return fmt.Sprintf("%s · %s%s · research %s · rank %s%s", h, e, m, r.researchList(), r.Rank, u)
}

// exhaustive runs every combination of the Backtest-tab settings once over
// the whole window, streams every row (with each calendar year's return) to
// csvPath and writes a year-by-year analysis to reportPath.
func exhaustive(in backtest.Input, cfg config.Config, from, mid, to time.Time, csvPath, reportPath string, topNs, looks []int, sample int) {
	var combos []*xrec
	entries := []struct {
		e string
		d int
		x float64
	}{{"cross", 15, 8}}
	for _, e := range []string{"both", "trend"} {
		for _, d := range []int{10, 15, 30, 60} {
			for _, x := range []float64{5, 8, 12, 20} {
				entries = append(entries, struct {
					e string
					d int
					x float64
				}{e, d, x})
			}
		}
	}
	type mk struct {
		m  string
		ha bool
	}
	markets := []mk{{"off", false}, {"nifty", false}, {"nifty", true}, {"category", false}, {"category", true}}
	for _, tn := range topNs {
		lks := looks
		if tn == 0 {
			lks = []int{0} // a fixed stock list has no strength window
		}
		for _, lk := range lks {
			for _, h := range []int{0, 3, 5, 7, 10} {
				for _, en := range entries {
					ms := markets
					if h == 0 {
						ms = []mk{{"regime", false}}
					}
					for _, m := range ms {
						for rs := uint8(1); rs <= 31; rs++ {
							for _, rk := range []string{"rs", "research"} {
								r := &xrec{Hold: h, Entry: en.e, Days: en.d, Ext: en.x, Market: m.m, IdxHA: m.ha, Research: rs, Rank: rk}
								r.TopN, r.Look = tn, lk
								combos = append(combos, r)
							}
						}
					}
				}
			}
		}
	}
	if sample > 1 { // smoke test: every sample-th combination
		var s []*xrec
		for k := 0; k < len(combos); k += sample {
			s = append(s, combos[k])
		}
		combos = s
	}
	// Indicators and bar positions once for every run.
	pc := cfg
	if topNs[0] > 0 {
		pc.Backtest.AutoUniverse = config.AutoUniverseConfig{Enabled: true, Source: "all_nse", TopN: topNs[0], LookbackDays: looks[0]}
	}
	in.From, in.To, in.Config = from, to, pc
	in.Prepared = backtest.Prepare(in)
	nifty := backtest.Run(in) // any run: for the calendar and NIFTY per year
	years := make([]int, 0, len(nifty.Summary.Yearly))
	nYear := make([]float64, 0, len(nifty.Summary.Yearly))
	for _, y := range nifty.Summary.Yearly {
		years, nYear = append(years, y.Year), append(nYear, y.BenchmarkPct)
	}
	fmt.Printf("%d combinations, one %s→%s run each, %d workers (years %d–%d)\n", len(combos), from.Format("2006-01"), to.Format("2006-01"), runtime.NumCPU(), years[0], years[len(years)-1])

	f, err := os.Create(csvPath)
	if err != nil {
		fail(err)
	}
	defer f.Close()
	bw := bufio.NewWriterSize(f, 1<<20)
	w := csv.NewWriter(bw)
	head := []string{"portfolio", "entry", "trend_max_days", "trend_max_ext_pct", "market_check", "index_ha_green", "research", "rank", "top_n", "lookback",
		"end", "return_pct", "cagr_pct", "max_dd_pct", "profit_factor", "trades", "win_pct", "invested_pct",
		fmt.Sprintf("h1_%d_%d_pct", from.Year(), mid.Year()), fmt.Sprintf("h2_%d_%d_pct", mid.Year(), to.Year()), "years_beating_nifty"}
	for _, y := range years {
		head = append(head, fmt.Sprintf("y%d_pct", y))
	}
	_ = w.Write(head)
	nrow := []string{"NIFTY 50 (buy and hold)", "", "", "", "", "", "", "", "", "", "", f2s(nifty.Summary.BenchmarkReturnPc), f2s(nifty.Summary.BenchmarkCAGRPct), "", "", "", "", "", "", "", ""}
	for _, v := range nYear {
		nrow = append(nrow, f2s(v))
	}
	_ = w.Write(nrow)

	var mu sync.Mutex
	var done atomic.Int64
	start := time.Now()
	jobs := make(chan *xrec, 256)
	var wg sync.WaitGroup
	for k := 0; k < runtime.NumCPU(); k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range jobs {
				c := cfg
				r.apply(&c)
				rin := in
				rin.From, rin.To, rin.Config = from, to, c
				res := backtest.Run(rin)
				s := res.Summary
				r.End, r.CAGR, r.DD, r.PF, r.Win, r.Invested, r.Trades = s.EndEquity, s.CAGRPct, s.MaxDrawdownPct, s.ProfitFactor, s.WinRatePct, s.AvgInvestedPct, s.Trades
				r.Years = make([]float64, len(years))
				for _, y := range s.Yearly {
					for k, yy := range years {
						if yy == y.Year {
							r.Years[k] = y.ReturnPct
						}
					}
				}
				mEq := s.StartEquity
				for _, e := range res.Equity {
					if !e.Date.Before(mid) {
						mEq = e.Equity
						break
					}
				}
				r.H1, r.H2 = (mEq/s.StartEquity-1)*100, (s.EndEquity/mEq-1)*100
				row := []string{r.portfolio(), r.Entry, strconv.Itoa(r.Days), f2s(r.Ext), r.Market, strconv.FormatBool(r.IdxHA), r.researchList(), r.Rank,
					strconv.Itoa(r.TopN), strconv.Itoa(r.Look), f2s(r.End), f2s(s.TotalReturnPct), f2s(r.CAGR), f2s(r.DD), f2s(r.PF), strconv.Itoa(r.Trades),
					f2s(r.Win), f2s(r.Invested), f2s(r.H1), f2s(r.H2), strconv.Itoa(beats(r.Years, nYear))}
				for _, v := range r.Years {
					row = append(row, f2s(v))
				}
				mu.Lock()
				_ = w.Write(row)
				mu.Unlock()
				if n := done.Add(1); n%5000 == 0 {
					el := time.Since(start)
					fmt.Printf("  %d/%d  (%s elapsed, ~%s left)\n", n, len(combos), el.Round(time.Second), (el / time.Duration(n) * time.Duration(int64(len(combos))-n)).Round(time.Minute))
				}
			}
		}()
	}
	for _, r := range combos {
		jobs <- r
	}
	close(jobs)
	wg.Wait()
	w.Flush()
	_ = bw.Flush()
	fmt.Printf("done in %s → %s\n", time.Since(start).Round(time.Second), csvPath)
	yearReport(combos, years, nYear, reportPath)
}

func (r *xrec) portfolio() string {
	if r.Hold == 0 {
		return "current rules"
	}
	return "hold " + strconv.Itoa(r.Hold)
}

func f2s(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

// beats counts the calendar years in which the combination beat NIFTY.
func beats(ys, nifty []float64) int {
	n := 0
	for k := range ys {
		if ys[k] > nifty[k] {
			n++
		}
	}
	return n
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// yearReport writes the year-by-year analysis.
func yearReport(cs []*xrec, years []int, nifty []float64, path string) {
	var b strings.Builder
	p := func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }
	ny := len(years)
	p("%d combinations. Returns in %% per calendar year (%d and %d are partial years).\n\n", len(cs), years[0], years[ny-1])

	p("== Every year: NIFTY, the median combination, the best one, and how many beat NIFTY\n")
	p("%-6s %8s %9s %9s %9s %9s   best combination that year\n", "year", "NIFTY", "median", "top 10%", "best", "beat%")
	for k, y := range years {
		v := make([]float64, len(cs))
		bi := 0
		n := 0
		for i, c := range cs {
			v[i] = c.Years[k]
			if c.Years[k] > cs[bi].Years[k] {
				bi = i
			}
			if c.Years[k] > nifty[k] {
				n++
			}
		}
		sort.Float64s(v)
		p("%-6d %7.1f%% %8.1f%% %8.1f%% %8.1f%% %8.1f%%   %s\n", y, nifty[k], v[len(v)/2], v[len(v)*9/10], v[len(v)-1], 100*float64(n)/float64(len(cs)), cs[bi].label())
	}

	row := func(c *xrec) string {
		var s strings.Builder
		for _, v := range c.Years {
			fmt.Fprintf(&s, " %6.0f", v)
		}
		return s.String()
	}
	hdr := func() {
		p("%-4s %6s %6s %5s |", "beat", "end L", "CAGR", "DD")
		for _, y := range years {
			p(" %6d", y)
		}
		p(" | settings\n")
		p("%-4s %6s %6s %5s |", "", "", "", "")
		for _, v := range nifty {
			p(" %6.0f", v)
		}
		p(" | ← NIFTY\n")
	}
	list := func(title string, less func(a, b *xrec) bool, n int) {
		s := append([]*xrec(nil), cs...)
		sort.SliceStable(s, func(i, j int) bool { return less(s[i], s[j]) })
		p("\n== %s\n", title)
		hdr()
		for k := 0; k < n && k < len(s); k++ {
			c := s[k]
			p("%2d/%d %6.1f %5.1f%% %4.0f%% |%s | %s\n", beats(c.Years, nifty), ny, c.End/1e5, c.CAGR, c.DD, row(c), c.label())
		}
	}
	worst := func(c *xrec) float64 {
		m := math.Inf(1)
		for k := 1; k < ny-1; k++ { // full calendar years only
			m = math.Min(m, c.Years[k])
		}
		return m
	}
	list("Most years beating NIFTY (then highest CAGR)", func(a, b *xrec) bool {
		ba, bb := beats(a.Years, nifty), beats(b.Years, nifty)
		if ba != bb {
			return ba > bb
		}
		return a.CAGR > b.CAGR
	}, 25)
	list("Best worst full year (the most consistent)", func(a, b *xrec) bool { return worst(a) > worst(b) }, 25)
	list("Highest 10-year CAGR (may be luck — check the years)", func(a, b *xrec) bool { return a.CAGR > b.CAGR }, 25)
	list("Lowest drawdown with CAGR above NIFTY", func(a, b *xrec) bool {
		ga, gb := a.CAGR > 10.2, b.CAGR > 10.2
		if ga != gb {
			return ga
		}
		return a.DD < b.DD
	}, 15)

	// Per setting value: the median return each year across everything else.
	p("\n== Median return per year for each setting value (across all other settings)\n")
	dims := []struct {
		name string
		val  func(c *xrec) string
	}{
		{"portfolio", func(c *xrec) string { return c.portfolio() }},
		{"entry", func(c *xrec) string { return c.Entry }},
		{"cross max age", func(c *xrec) string {
			if c.Entry == "cross" {
				return ""
			}
			return strconv.Itoa(c.Days) + "d"
		}},
		{"max % above EMA20", func(c *xrec) string {
			if c.Entry == "cross" {
				return ""
			}
			return strconv.FormatFloat(c.Ext, 'g', -1, 64) + "%"
		}},
		{"market check", func(c *xrec) string {
			if c.Market == "off" || c.Market == "regime" {
				return c.Market
			}
			if c.IdxHA {
				return c.Market + "+HA"
			}
			return c.Market
		}},
		{"research tag in filter", nil},
		{"rank", func(c *xrec) string { return c.Rank }},
		{"stocks per month", func(c *xrec) string { return strconv.Itoa(c.TopN) }},
		{"strength window", func(c *xrec) string { return strconv.Itoa(c.Look) + "d" }},
	}
	for _, d := range dims {
		groups := map[string][]*xrec{}
		if d.val == nil { // research: does including each tag help?
			for k, t := range swing.ResearchTags {
				for _, c := range cs {
					key := "without " + t
					if c.Research&(1<<k) != 0 {
						key = "with " + t
					}
					groups[key] = append(groups[key], c)
				}
			}
		} else {
			for _, c := range cs {
				if v := d.val(c); v != "" {
					groups[v] = append(groups[v], c)
				}
			}
		}
		var keys []string
		for k := range groups {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		p("\n%s\n%-22s %8s %6s |", d.name, "", "CAGR", "beat")
		for _, y := range years {
			p(" %6d", y)
		}
		p("\n")
		for _, k := range keys {
			g := groups[k]
			cg, bt := make([]float64, len(g)), make([]float64, len(g))
			for i, c := range g {
				cg[i], bt[i] = c.CAGR, float64(beats(c.Years, nifty))
			}
			p("%-22s %7.1f%% %6.1f |", k, median(cg), median(bt))
			for y := range years {
				v := make([]float64, len(g))
				for i, c := range g {
					v[i] = c.Years[y]
				}
				p(" %6.1f", median(v))
			}
			p("\n")
		}
	}

	// Rolling choice: at the start of each year, pick the combination with the
	// best compounded return over the previous three full years, and hold it
	// for that year. This is what "use the best recent backtest" would earn.
	p("\n== Rolling choice: each year, use the combination that was best over the previous 3 years\n")
	p("%-6s %9s %9s   chosen combination\n", "year", "result", "NIFTY")
	tot, ntot := 1.0, 1.0
	for k := 4; k < ny; k++ { // needs 3 full years before (first year is partial)
		bi, bv := 0, math.Inf(-1)
		for i, c := range cs {
			v := (1 + c.Years[k-3]/100) * (1 + c.Years[k-2]/100) * (1 + c.Years[k-1]/100)
			if v > bv {
				bi, bv = i, v
			}
		}
		r := cs[bi].Years[k]
		tot *= 1 + r/100
		ntot *= 1 + nifty[k]/100
		p("%-6d %8.1f%% %8.1f%%   %s\n", years[k], r, nifty[k], cs[bi].label())
	}
	p("%-6s %8.1f%% %8.1f%%   (compounded over those years)\n", "total", (tot-1)*100, (ntot-1)*100)

	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		fail(err)
	}
	fmt.Print(b.String())
}
