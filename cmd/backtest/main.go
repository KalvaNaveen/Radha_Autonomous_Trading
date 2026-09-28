// Command backtest replays the swing strategy over history and writes a report.
//
//	backtest -years 5                       download from Kite (uses today's login saved by the engine)
//	backtest -csv ./history -years 5        offline: one CSV per symbol + NIFTY50.csv
//
// CSV format (header optional): date,open,high,low,close,volume  (date YYYY-MM-DD).
// Output: report.html, summary.json, trades.csv, equity.csv in -out.
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nkalva/kitealgo/internal/auth"
	"github.com/nkalva/kitealgo/internal/backtest"
	"github.com/nkalva/kitealgo/internal/broker"
	"github.com/nkalva/kitealgo/internal/clock"
	"github.com/nkalva/kitealgo/internal/config"
	"github.com/nkalva/kitealgo/internal/data"
	"github.com/nkalva/kitealgo/pkg/models"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "engine config (strategy, risk and costs are read from it)")
	years := flag.Int("years", 5, "years to simulate")
	csvDir := flag.String("csv", "", "offline mode: directory of <SYMBOL>.csv files plus the index file")
	indexFile := flag.String("index-file", "NIFTY50.csv", "offline mode: index CSV file name inside -csv")
	out := flag.String("out", "backtest-report", "output directory")
	flag.Parse()

	if _, err := config.WriteDefault(*cfgPath); err != nil {
		fail("config: %v", err)
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fail("config: %v", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	// Last completed session (today's candle is final only after the close).
	to := clock.Midnight(time.Now())
	if time.Now().In(clock.IST).Hour() < 16 {
		to = to.AddDate(0, 0, -1)
	}
	from := to.AddDate(-*years, 0, 0)

	var in backtest.Input
	if *csvDir != "" {
		in, err = loadCSV(*csvDir, *indexFile)
	} else {
		in, err = loadKite(cfg, from.AddDate(0, 0, -420), to, log)
	}
	if err != nil {
		fail("%v", err)
	}
	in.From, in.To, in.Config = from, to, cfg
	if len(in.Index) > 0 && in.Index[len(in.Index)-1].Date.Before(to) {
		in.To = in.Index[len(in.Index)-1].Date
	}
	fmt.Printf("Simulating %d symbols, %s → %s …\n", len(in.Instruments), in.From.Format("2006-01-02"), in.To.Format("2006-01-02"))
	res := backtest.Run(in)
	if err := backtest.WriteReport(*out, res); err != nil {
		fail("report: %v", err)
	}
	s := res.Summary
	fmt.Printf(`
  Period          %s → %s
  Start / end     ₹%.0f → ₹%.0f
  Total return    %.1f%%   (CAGR %.1f%%, NIFTY CAGR %.1f%%)
  Max drawdown    %.1f%%
  Trades          %d   win rate %.1f%%   avg win %.1f%% / avg loss %.1f%%
  Profit factor   %.2f   expectancy %.2f R   avg hold %.1f days
  Charges paid    ₹%.0f
  Report          %s
`, s.From.Format("2006-01-02"), s.To.Format("2006-01-02"), s.StartEquity, s.EndEquity, s.TotalReturnPct, s.CAGRPct,
		s.BenchmarkCAGRPct, s.MaxDrawdownPct, s.Trades, s.WinRatePct, s.AvgWinPct, s.AvgLossPct, s.ProfitFactor,
		s.ExpectancyR, s.AvgBarsHeld, s.TotalCosts, filepath.Join(*out, "report.html"))
}

func fail(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "backtest: "+f+"\n", a...)
	os.Exit(1)
}

func loadKite(cfg config.Config, from, to time.Time, log *slog.Logger) (backtest.Input, error) {
	httpc, err := broker.NewStaticIPClient(cfg.Network.BindIP, cfg.Network.RequestTimeout)
	if err != nil {
		return backtest.Input{}, err
	}
	am := auth.NewManager(cfg.Kite.APIKey, cfg.Kite.APISecret, cfg.Paths.DataDir, cfg.Server.PublicURL, httpc, clock.System{}, log)
	tok, ok := am.Current()
	if !ok {
		return backtest.Input{}, fmt.Errorf("no valid Kite token today — start the engine and log in from the control panel first (or use -csv)")
	}
	kite := broker.NewKite(am.APIKey(), tok.AccessToken, httpc, cfg.Orders.MarketProtection, cfg.Dev.APIRoot)
	ds := data.NewStore(cfg.Paths.DataDir, kite, log)
	ctx := context.Background()
	syms, _, err := data.LoadUniverse(cfg.Paths.UniverseFile)
	if err != nil {
		return backtest.Input{}, fmt.Errorf("universe: %w", err)
	}
	nse, err := ds.Instruments(ctx, clock.Midnight(time.Now()))
	if err != nil {
		return backtest.Input{}, err
	}
	ins, warns := data.Resolve(syms, nse)
	for _, w := range warns {
		fmt.Println("  warning:", w)
	}
	idxIn, ok := data.FindIndex(nse, cfg.Market.Index)
	if !ok {
		return backtest.Input{}, fmt.Errorf("index %q not found", cfg.Market.Index)
	}
	var in backtest.Input
	if in.Index, err = ds.Daily(ctx, idxIn.InstrumentToken, from, to); err != nil {
		return in, err
	}
	for n, x := range ins {
		fmt.Printf("\r  downloading %d/%d %-12s", n+1, len(ins), x.TradingSymbol)
		bars, err := ds.Daily(ctx, x.InstrumentToken, from, to)
		if err != nil {
			fmt.Println("\n  skipped", x.TradingSymbol, err)
			continue
		}
		in.Instruments = append(in.Instruments, backtest.Instrument{Symbol: x.TradingSymbol, Token: x.InstrumentToken, Bars: bars})
	}
	fmt.Println()
	return in, nil
}

func loadCSV(dir, indexFile string) (backtest.Input, error) {
	var in backtest.Input
	files, _ := filepath.Glob(filepath.Join(dir, "*.csv"))
	for n, f := range files {
		bars, err := readBars(f)
		if err != nil {
			return in, fmt.Errorf("%s: %w", f, err)
		}
		name := strings.TrimSuffix(filepath.Base(f), ".csv")
		if filepath.Base(f) == indexFile {
			in.Index = bars
			continue
		}
		in.Instruments = append(in.Instruments, backtest.Instrument{Symbol: strings.ToUpper(name), Token: uint32(n + 1), Bars: bars})
	}
	if len(in.Index) == 0 {
		return in, fmt.Errorf("index file %s not found in %s", indexFile, dir)
	}
	return in, nil
}

func readBars(path string) ([]models.Bar, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	var out []models.Bar
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(rec) < 6 {
			continue
		}
		d, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(rec[0])[:min(10, len(strings.TrimSpace(rec[0])))], clock.IST)
		if err != nil {
			continue // header
		}
		v := make([]float64, 5)
		for i := range v {
			v[i], _ = strconv.ParseFloat(strings.TrimSpace(rec[i+1]), 64)
		}
		out = append(out, models.Bar{Date: d, Open: v[0], High: v[1], Low: v[2], Close: v[3], Volume: v[4]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	return out, nil
}
