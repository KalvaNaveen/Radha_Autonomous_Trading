package backtest

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// WriteReport writes summary.json, result.json, trades.csv, equity.csv and a
// self-contained report.html into dir.
func WriteReport(dir string, r Result) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	js, _ := json.MarshalIndent(r.Summary, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "summary.json"), js, 0o644); err != nil {
		return err
	}
	full, _ := json.Marshal(r)
	if err := os.WriteFile(filepath.Join(dir, "result.json"), full, 0o644); err != nil {
		return err
	}
	if err := writeCSV(filepath.Join(dir, "trades.csv"), tradeRows(r)); err != nil {
		return err
	}
	eq := [][]string{{"date", "equity", "cash", "positions", "benchmark"}}
	for _, e := range r.Equity {
		eq = append(eq, []string{e.Date.Format("2006-01-02"), f2(e.Equity), f2(e.Cash), strconv.Itoa(e.Positions), f2(e.Benchmark)})
	}
	if err := writeCSV(filepath.Join(dir, "equity.csv"), eq); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(dir, "report.html"))
	if err != nil {
		return err
	}
	defer f.Close()
	return reportTmpl.Execute(f, buildView(r))
}

func f2(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

func writeCSV(path string, rows [][]string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.WriteAll(rows)
	return w.Error()
}

func tradeRows(r Result) [][]string {
	rows := [][]string{{"symbol", "setup", "qty", "entry_date", "entry_price", "exit_date", "exit_price", "gross", "costs", "net", "r", "days", "stage", "reason"}}
	for _, t := range r.Trades {
		rows = append(rows, []string{t.Symbol, string(t.Setup), strconv.Itoa(t.Quantity), t.EntryDate.Format("2006-01-02"),
			f2(t.EntryPrice), t.ExitDate.Format("2006-01-02"), f2(t.ExitPrice), f2(t.Gross), f2(t.Costs), f2(t.Net),
			f2(t.RMultiple), strconv.Itoa(t.BarsHeld), string(t.Stage), t.Reason})
	}
	return rows
}

type view struct {
	S       Summary
	Chart   template.HTML
	Trades  [][]string
	Skipped map[string]int
	Symbols int
}

// EquitySVG renders the equity curve against the benchmark as inline SVG.
func EquitySVG(r Result, w, h int) string {
	if len(r.Equity) < 2 {
		return ""
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, e := range r.Equity {
		lo = math.Min(lo, math.Min(e.Equity, e.Benchmark))
		hi = math.Max(hi, math.Max(e.Equity, e.Benchmark))
	}
	if hi <= lo {
		hi = lo + 1
	}
	pad := 36.0
	x := func(i int) float64 { return pad + float64(i)/float64(len(r.Equity)-1)*(float64(w)-pad-8) }
	y := func(v float64) float64 { return 8 + (hi-v)/(hi-lo)*(float64(h)-28) }
	var eq, bm strings.Builder
	for i, e := range r.Equity {
		sep := " L"
		if i == 0 {
			sep = "M"
		}
		fmt.Fprintf(&eq, "%s%.1f,%.1f", sep, x(i), y(e.Equity))
		fmt.Fprintf(&bm, "%s%.1f,%.1f", sep, x(i), y(e.Benchmark))
	}
	first, last := r.Equity[0].Date.Format("Jan 2006"), r.Equity[len(r.Equity)-1].Date.Format("Jan 2006")
	return fmt.Sprintf(`<svg viewBox="0 0 %d %d" width="100%%" role="img" aria-label="Equity curve">
<line x1="%.0f" y1="%.1f" x2="%d" y2="%.1f" class="grid"/>
<text x="2" y="14" class="ax">₹%s</text><text x="2" y="%d" class="ax">₹%s</text>
<text x="%.0f" y="%d" class="ax">%s</text><text x="%d" y="%d" class="ax" text-anchor="end">%s</text>
<path d="%s" class="bm"/><path d="%s" class="eq"/></svg>`,
		w, h, pad, y(r.Summary.StartEquity), w-8, y(r.Summary.StartEquity),
		kfmt(hi), h-22, kfmt(lo), pad, h-4, first, w-8, h-4, last, bm.String(), eq.String())
}

func kfmt(v float64) string {
	switch {
	case v >= 1e7:
		return fmt.Sprintf("%.2f Cr", v/1e7)
	case v >= 1e5:
		return fmt.Sprintf("%.2f L", v/1e5)
	default:
		return fmt.Sprintf("%.0f", v)
	}
}

func buildView(r Result) view {
	rows := tradeRows(r)
	return view{S: r.Summary, Chart: template.HTML(EquitySVG(r, 900, 280)), Trades: rows[1:], Skipped: r.Skipped, Symbols: r.Summary.Symbols}
}

var reportTmpl = template.Must(template.New("r").Funcs(template.FuncMap{
	"f2":  func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) },
	"f1":  func(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) },
	"d":   func(t interface{ Format(string) string }) string { return t.Format("02 Jan 2006") },
	"neg": func(s string) bool { return strings.HasPrefix(s, "-") },
}).Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Swing Backtest Report</title><style>
:root{--bg:#f6f7f9;--p:#fff;--ink:#15171c;--mu:#5d6472;--ln:#e3e6eb;--eq:#2f5bd3;--bm:#9aa2b1;--g:#11845b;--r:#c2372f}
@media (prefers-color-scheme:dark){:root{--bg:#0f1115;--p:#171a21;--ink:#e8eaee;--mu:#9aa2b1;--ln:#262b35;--eq:#6f93ff;--bm:#5d6472;--g:#3ecf8e;--r:#ff6b61}}
body{margin:0;background:var(--bg);color:var(--ink);font:14px/1.45 system-ui,Segoe UI,Roboto,sans-serif}
main{max-width:1100px;margin:0 auto;padding:20px 16px}h1{font-size:20px;margin:0 0 4px}.mu{color:var(--mu)}
.k{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:10px;margin:16px 0}
.c{background:var(--p);border:1px solid var(--ln);border-radius:10px;padding:10px 12px}.c b{display:block;font-size:20px;font-variant-numeric:tabular-nums}
.card{background:var(--p);border:1px solid var(--ln);border-radius:10px;padding:14px;margin:12px 0;overflow-x:auto}
svg .eq{fill:none;stroke:var(--eq);stroke-width:2}svg .bm{fill:none;stroke:var(--bm);stroke-width:1.5;stroke-dasharray:4 3}
svg .grid{stroke:var(--ln)}svg .ax{fill:var(--mu);font-size:11px}
table{border-collapse:collapse;width:100%;font-variant-numeric:tabular-nums;font-size:13px}th,td{padding:6px 8px;border-bottom:1px solid var(--ln);text-align:left;white-space:nowrap}
th{color:var(--mu);font-weight:600}.g{color:var(--g)}.r{color:var(--r)}.lg span{display:inline-block;width:18px;height:3px;vertical-align:middle;margin:0 6px 0 12px}
</style></head><body><main>
<h1>Swing backtest</h1><div class="mu">{{d .S.From}} – {{d .S.To}} · {{.Symbols}} symbols · long-only CNC · costs and slippage included</div>
<div class="k">
<div class="c">Start<b>₹{{f2 .S.StartEquity}}</b></div><div class="c">End<b>₹{{f2 .S.EndEquity}}</b></div>
<div class="c">Total return<b>{{f1 .S.TotalReturnPct}}%</b></div><div class="c">CAGR<b>{{f1 .S.CAGRPct}}%</b></div>
<div class="c">NIFTY CAGR<b>{{f1 .S.BenchmarkCAGRPct}}%</b></div><div class="c">Max drawdown<b class="r">−{{f1 .S.MaxDrawdownPct}}%</b></div>
<div class="c">Trades<b>{{.S.Trades}}</b></div><div class="c">Win rate<b>{{f1 .S.WinRatePct}}%</b></div>
<div class="c">Avg win / loss<b>{{f1 .S.AvgWinPct}}% / {{f1 .S.AvgLossPct}}%</b></div><div class="c">Profit factor<b>{{f2 .S.ProfitFactor}}</b></div>
<div class="c">Expectancy<b>{{f2 .S.ExpectancyR}} R</b></div><div class="c">Avg days held<b>{{f1 .S.AvgBarsHeld}}</b></div>
<div class="c">Time in market<b>{{f1 .S.ExposurePct}}%</b></div><div class="c">Total charges<b>₹{{f2 .S.TotalCosts}}</b></div>
</div>
<div class="card"><div class="lg mu"><span style="background:var(--eq)"></span>Strategy<span style="background:var(--bm)"></span>NIFTY 50 (rebased)</div>{{.Chart}}
<div class="mu">Max drawdown {{f1 .S.MaxDrawdownPct}}% from {{d .S.MaxDrawdownFrom}} to {{d .S.MaxDrawdownTo}}.</div></div>
<div class="card"><b>Signals not taken</b><table>{{range $k,$v := .Skipped}}<tr><td>{{$k}}</td><td>{{$v}}</td></tr>{{end}}</table></div>
<div class="card"><b>Trades</b><table><tr><th>Symbol</th><th>Setup</th><th>Qty</th><th>Entry</th><th>Price</th><th>Exit</th><th>Price</th><th>Net ₹</th><th>R</th><th>Days</th><th>Stage</th><th>Reason</th></tr>
{{range .Trades}}<tr><td>{{index . 0}}</td><td>{{index . 1}}</td><td>{{index . 2}}</td><td>{{index . 3}}</td><td>{{index . 4}}</td><td>{{index . 5}}</td><td>{{index . 6}}</td>
<td class="{{if neg (index . 9)}}r{{else}}g{{end}}">{{index . 9}}</td><td>{{index . 10}}</td><td>{{index . 11}}</td><td>{{index . 12}}</td><td>{{index . 13}}</td></tr>{{end}}</table></div>
<p class="mu">Past performance in a backtest does not guarantee future results. Fills assume the stated slippage; real gaps and liquidity can be worse.</p>
</main></body></html>`))
