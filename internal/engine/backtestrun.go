package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nkalva/kitealgo/internal/backtest"
	"github.com/nkalva/kitealgo/internal/broker"
	"github.com/nkalva/kitealgo/internal/clock"
	"github.com/nkalva/kitealgo/internal/config"
	"github.com/nkalva/kitealgo/internal/data"
	"github.com/nkalva/kitealgo/pkg/models"
)

// BacktestStatus is the UI-visible state of the backtest runner.
type BacktestStatus struct {
	Running  bool      `json:"running"`
	Progress string    `json:"progress"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	Error    string    `json:"error,omitempty"`
	Years    int       `json:"years"`
}

// BacktestState returns the runner status.
func (e *Engine) BacktestState() BacktestStatus {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.bt
}

func (e *Engine) btDir() string { return filepath.Join(e.cfg.Paths.DataDir, "backtest", "latest") }

// LatestBacktest returns the last result, if any (as raw JSON for the UI).
func (e *Engine) LatestBacktest() ([]byte, error) {
	return os.ReadFile(filepath.Join(e.btDir(), "result.json"))
}

// ResetBacktest deletes the last backtest result. With clearCandles it also
// deletes the downloaded daily candles (and the instrument lists), so the
// next backtest or evening run downloads fresh history from Kite.
func (e *Engine) ResetBacktest(clearCandles bool) error {
	e.mu.Lock()
	if e.bt.Running {
		e.mu.Unlock()
		return errors.New("a backtest is running — wait for it to finish")
	}
	e.bt = BacktestStatus{}
	e.mu.Unlock()
	if err := os.RemoveAll(e.btDir()); err != nil {
		return fmt.Errorf("cannot delete the backtest result: %w", err)
	}
	if clearCandles {
		for _, d := range []string{"candles", "instruments"} {
			if err := os.RemoveAll(filepath.Join(e.cfg.Paths.DataDir, d)); err != nil {
				return fmt.Errorf("cannot delete %s: %w", d, err)
			}
		}
	}
	e.log.Info("backtest reset from UI", "cleared_candles", clearCandles)
	return nil
}

// BacktestSettings are the backtest-only choices made in the control panel
// (Backtest tab). They override config.yaml for backtests; the live and
// paper engine never read them.
type BacktestSettings struct {
	Capital        float64 `json:"capital"`
	Holdings       bool    `json:"holdings"`          // hold N stocks, equal split, refill daily
	Slots          int     `json:"slots"`             // N
	EntryMode      string  `json:"entry_mode"`        // cross | trend | both
	TrendMaxDays   int     `json:"trend_max_days"`    // trending entry: cross at most this old…
	TrendMaxExtPct float64 `json:"trend_max_ext_pct"` // …and close at most this % above EMA20
	MarketCheck    string  `json:"market_check"`      // off | nifty | category
	MarketHAGreen  bool    `json:"market_ha_green"`   // the index's Heikin-Ashi candle must be green
	ResearchAllow  string  `json:"research_allow"`    // all | comma list of research tags
	ResearchRank   string  `json:"research_rank"`     // rs | research
}

func (e *Engine) btSettingsPath() string {
	return filepath.Join(e.cfg.Paths.DataDir, "backtest", "settings.json")
}

// DefaultBacktestSettings mirrors config.yaml.
func (e *Engine) DefaultBacktestSettings() BacktestSettings {
	c := e.cfg.ForBacktest()
	return BacktestSettings{Capital: c.Risk.Capital, Holdings: c.Holdings.Enabled, Slots: c.SlotCount(),
		EntryMode: orDefault(c.Strategy.EntryMode, "cross"), TrendMaxDays: c.Strategy.TrendMaxDays, TrendMaxExtPct: c.Strategy.TrendMaxExtPct,
		MarketCheck: orDefault(c.Holdings.MarketCheck, "off"), MarketHAGreen: c.Holdings.MarketHAGreen,
		ResearchAllow: orDefault(c.Strategy.ResearchAllow, "all"), ResearchRank: orDefault(c.Strategy.ResearchRank, "rs")}
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// BacktestSettingsNow returns the saved settings (config.yaml defaults if none).
func (e *Engine) BacktestSettingsNow() BacktestSettings {
	s := e.DefaultBacktestSettings()
	if raw, err := os.ReadFile(e.btSettingsPath()); err == nil {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

// SaveBacktestSettings validates and stores the settings for the next runs.
func (e *Engine) SaveBacktestSettings(s BacktestSettings) error {
	if _, err := e.backtestConfig(s); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(e.btSettingsPath()), 0o755); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(e.btSettingsPath(), raw, 0o644)
}

// backtestConfig is config.yaml with the backtest settings applied.
func (e *Engine) backtestConfig(s BacktestSettings) (config.Config, error) {
	c := e.cfg.ForBacktest()
	if s.Capital < 10000 || s.Capital > 1e10 {
		return c, errors.New("capital must be between ₹10,000 and ₹1,000 crore")
	}
	c.Risk.Capital = s.Capital
	c.Holdings.Enabled, c.Holdings.Slots, c.Holdings.MarketCheck, c.Holdings.MarketHAGreen = s.Holdings, s.Slots, s.MarketCheck, s.MarketHAGreen
	c.Strategy.EntryMode, c.Strategy.TrendMaxDays, c.Strategy.TrendMaxExtPct = s.EntryMode, s.TrendMaxDays, s.TrendMaxExtPct
	c.Strategy.ResearchAllow, c.Strategy.ResearchRank = orDefault(s.ResearchAllow, "all"), orDefault(s.ResearchRank, "rs")
	if err := c.Validate(); err != nil {
		return c, err
	}
	return c, nil
}

// RulesNow describes the rules the next backtest will use.
func (e *Engine) RulesNow() (summary, hash string) {
	c, err := e.backtestConfig(e.BacktestSettingsNow())
	if err != nil {
		c = e.cfg.ForBacktest()
	}
	return backtest.DescribeRules(c), backtest.RulesHash(c)
}

// ReportPath is the HTML report of the last run.
func (e *Engine) ReportPath() string { return filepath.Join(e.btDir(), "report.html") }

func (e *Engine) setBT(f func(b *BacktestStatus)) {
	e.mu.Lock()
	f(&e.bt)
	e.mu.Unlock()
}

// StartBacktest downloads history for the universe and runs the backtest in
// the background. It needs today's Kite login (historical data API).
func (e *Engine) StartBacktest(years int) error {
	if years < 1 || years > 15 {
		return errors.New("years must be 1..15")
	}
	e.mu.Lock()
	if e.bt.Running {
		e.mu.Unlock()
		return errors.New("a backtest is already running")
	}
	e.bt = BacktestStatus{Running: true, Progress: "starting", Started: time.Now(), Years: years}
	e.mu.Unlock()
	tok, ok := e.auth.Current()
	if !ok {
		e.setBT(func(b *BacktestStatus) {
			b.Running, b.Error = false, "log in to Kite first (the backtest downloads history)"
		})
		return errors.New("log in to Kite first")
	}
	go func() {
		err := e.runBacktest(years, tok.AccessToken)
		e.setBT(func(b *BacktestStatus) {
			b.Running, b.Finished = false, time.Now()
			if err != nil {
				b.Error = err.Error()
			} else {
				b.Progress = "done"
			}
		})
		if err != nil {
			e.log.Error("backtest failed", "err", err)
		}
	}()
	return nil
}

func (e *Engine) runBacktest(years int, access string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cfg, err := e.backtestConfig(e.BacktestSettingsNow())
	if err != nil {
		return fmt.Errorf("backtest settings: %w", err)
	}
	kite := broker.NewKite(e.auth.APIKey(), access, e.httpc, e.cfg.Orders.MarketProtection, e.cfg.Dev.APIRoot)
	ds := data.NewStore(e.cfg.Paths.DataDir, kite, e.log)
	syms, _, err := data.LoadUniverse(e.cfg.Paths.UniverseFile)
	if err != nil {
		return fmt.Errorf("universe: %w", err)
	}
	now := clock.Midnight(e.clk.Now())
	nse, err := ds.Instruments(ctx, now)
	if err != nil {
		return fmt.Errorf("instruments: %w", err)
	}
	ins, _ := data.Resolve(syms, nse)
	idxIn, ok := data.FindIndex(nse, e.cfg.Market.Index)
	if !ok {
		return fmt.Errorf("index %q not found", e.cfg.Market.Index)
	}
	// Last COMPLETED session: today only after its evening run time.
	to := e.sess.PrevTradingDay(now)
	if e.sess.IsTradingDay(now) && !e.clk.Now().Before(e.sess.At(now, e.sess.EveningRun)) {
		to = now
	}
	simFrom := to.AddDate(-years, 0, 0)
	loadFrom := simFrom.AddDate(0, 0, -historyDays) // warmup before the window
	e.setBT(func(b *BacktestStatus) { b.Progress = "downloading " + e.cfg.Market.Index })
	idx, err := ds.Daily(ctx, idxIn.InstrumentToken, loadFrom, to)
	if err != nil {
		return fmt.Errorf("index history: %w", err)
	}
	// Holdings mode, category market check: the midcap and smallcap indices
	// and each stock's category.
	indices := map[string][]models.Bar{}
	var caps map[string]string
	if cfg.Holdings.Enabled && cfg.Holdings.MarketCheck == "category" {
		for _, name := range []string{cfg.Holdings.MidcapIndex, cfg.Holdings.SmallcapIndex} {
			x, ok := data.FindIndex(nse, name)
			if !ok {
				e.log.Warn("index not found — its stocks are checked against "+cfg.Market.Index, "index", name)
				continue
			}
			e.setBT(func(b *BacktestStatus) { b.Progress = "downloading " + name })
			bars, err := ds.Daily(ctx, x.InstrumentToken, loadFrom, to)
			if err != nil {
				e.log.Warn("index history unavailable", "index", name, "err", err)
				continue
			}
			indices[name] = bars
		}
		var warns []string
		caps, warns, err = data.LoadCaps(cfg.Holdings.CapsFile)
		for _, w := range warns {
			e.log.Warn("caps file", "note", w)
		}
		if err != nil {
			return fmt.Errorf("caps file: %w", err)
		}
	}
	var list []backtest.Instrument
	for n, in := range ins {
		e.setBT(func(b *BacktestStatus) {
			b.Progress = fmt.Sprintf("downloading %s (%d/%d)", in.TradingSymbol, n+1, len(ins))
		})
		bars, err := ds.Daily(ctx, in.InstrumentToken, loadFrom, to)
		if err != nil {
			e.log.Warn("history unavailable", "symbol", in.TradingSymbol, "err", err)
			continue
		}
		list = append(list, backtest.Instrument{Symbol: in.TradingSymbol, Token: in.InstrumentToken, Bars: bars})
	}
	e.setBT(func(b *BacktestStatus) { b.Progress = "simulating" })
	res := backtest.Run(backtest.Input{Instruments: list, Index: idx, From: simFrom, To: to, Config: cfg, Indices: indices, Caps: caps})
	if err := backtest.WriteReport(e.btDir(), res); err != nil {
		return err
	}
	s, _ := json.Marshal(res.Summary)
	e.log.Info("backtest complete", "summary", string(s))
	return nil
}
