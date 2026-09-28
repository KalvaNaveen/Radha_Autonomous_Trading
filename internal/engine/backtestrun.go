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
	"github.com/nkalva/kitealgo/internal/data"
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

// RulesNow describes the rules the engine is running with.
func (e *Engine) RulesNow() (summary, hash string) {
	return backtest.DescribeRules(e.cfg), backtest.RulesHash(e.cfg)
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
	res := backtest.Run(backtest.Input{Instruments: list, Index: idx, From: simFrom, To: to, Config: e.cfg})
	if err := backtest.WriteReport(e.btDir(), res); err != nil {
		return err
	}
	s, _ := json.Marshal(res.Summary)
	e.log.Info("backtest complete", "summary", string(s))
	return nil
}
