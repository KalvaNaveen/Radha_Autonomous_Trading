// Package radar is the market-context filter: it tracks the broad market index
// (NIFTY 50 by default) and any per-stock benchmark/sector indices from the
// watchlist, and vetoes entries that fight the tape.
package radar

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nkalva/kitealgo/internal/indicators"
	"github.com/nkalva/kitealgo/pkg/models"
)

// Bias is the directional read of an index.
type Bias int32

const (
	Neutral Bias = iota
	Bullish
	Bearish
)

func (b Bias) String() string { return [...]string{"NEUTRAL", "BULLISH", "BEARISH"}[b] }

// Mode controls how strictly the radar gates entries.
type Mode string

const (
	ModeStrict     Mode = "strict"     // benchmarks must agree with the trade
	ModePermissive Mode = "permissive" // benchmarks must not disagree
	ModeOff        Mode = "off"
)

// Index tracks one index.
type Index struct {
	Token uint32
	Name  string
	bias  atomic.Int32

	// owned by the radar goroutine
	fast, slow *indicators.EMA
	candles    *indicators.CandleBuilder
	open       float64
}

// Radar computes a bias per tracked index from (a) LTP vs day open and
// (b) 5-minute fast/slow EMA alignment. Index ticks carry no volume, so VWAP is
// not available for indices; open + EMA trend is the standard substitute.
type Radar struct {
	mode       Mode
	neutralPct float64
	market     uint32
	indices    map[uint32]*Index // immutable after construction
	log        *slog.Logger
	wg         sync.WaitGroup
}

// Spec describes one index to track, with optional 5-minute seed closes.
type Spec struct {
	Token uint32
	Name  string
	Seed  []float64
}

// New builds a radar. market is the broad-market index token.
func New(mode Mode, neutralPct float64, market uint32, specs []Spec, fastN, slowN int,
	interval, grace time.Duration, sessionOpen time.Time, log *slog.Logger) *Radar {
	r := &Radar{mode: mode, neutralPct: neutralPct, market: market, indices: map[uint32]*Index{}, log: log.With("component", "radar")}
	for _, s := range specs {
		ix := &Index{Token: s.Token, Name: s.Name, fast: indicators.NewEMA(fastN), slow: indicators.NewEMA(slowN),
			candles: indicators.NewCandleBuilder(s.Token, interval, grace, sessionOpen)}
		ix.fast.Seed(s.Seed)
		ix.slow.Seed(s.Seed)
		r.indices[s.Token] = ix
	}
	return r
}

// Tokens lists tracked index tokens.
func (r *Radar) Tokens() []uint32 {
	out := make([]uint32, 0, len(r.indices))
	for t := range r.indices {
		out = append(out, t)
	}
	return out
}

// Run consumes ticks for every tracked index. subscribe returns the mailbox for a token.
func (r *Radar) Run(ctx context.Context, subscribe func(uint32) <-chan models.Tick) {
	for _, ix := range r.indices {
		ch := subscribe(ix.Token)
		r.wg.Add(1)
		go func(ix *Index) {
			defer r.wg.Done()
			flush := time.NewTicker(time.Second)
			defer flush.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case t := <-ch:
					r.onTick(ix, t)
				case now := <-flush.C:
					for _, c := range ix.candles.Flush(now) {
						r.onCandle(ix, c)
					}
				}
			}
		}(ix)
	}
	r.wg.Wait()
}

func (r *Radar) onTick(ix *Index, t models.Tick) {
	if t.DayOpen > 0 {
		ix.open = t.DayOpen
	}
	for _, c := range ix.candles.Update(t) {
		r.onCandle(ix, c)
	}
	if ix.open == 0 {
		// Some index packets lack OHLC: fall back to the first traded price.
		ix.open = t.LastPrice
	}
	r.evaluate(ix, t.LastPrice)
}

func (r *Radar) onCandle(ix *Index, c models.Candle) {
	ix.fast.Update(c.Close)
	ix.slow.Update(c.Close)
}

func (r *Radar) evaluate(ix *Index, ltp float64) {
	if ix.open <= 0 || ltp <= 0 {
		return
	}
	dev := (ltp - ix.open) / ix.open
	th := indicators.Pct(r.neutralPct)
	ready := ix.fast.Ready() && ix.slow.Ready()
	up := ix.fast.Value() > ix.slow.Value()
	b := Neutral
	switch {
	case dev > th && (!ready || up):
		b = Bullish
	case dev < -th && (!ready || !up):
		b = Bearish
	}
	if old := Bias(ix.bias.Swap(int32(b))); old != b {
		r.log.Info("bias change", "index", ix.Name, "from", old, "to", b, "ltp", ltp, "open", ix.open)
	}
}

// BiasOf returns the current bias of an index (Neutral if unknown).
func (r *Radar) BiasOf(token uint32) Bias {
	if ix, ok := r.indices[token]; ok {
		return Bias(ix.bias.Load())
	}
	return Neutral
}

func (r *Radar) name(tok uint32) string {
	if ix, ok := r.indices[tok]; ok {
		return ix.Name
	}
	return "index"
}

// Allow reports whether a trade on side is permitted given the market index and
// the stock's benchmark (0 = market only).
func (r *Radar) Allow(side models.Side, benchmark uint32) (bool, string) {
	if r == nil || r.mode == ModeOff {
		return true, ""
	}
	want, against := Bullish, Bearish
	if side == models.SideShort {
		want, against = Bearish, Bullish
	}
	check := []uint32{r.market}
	if benchmark != 0 && benchmark != r.market {
		check = append(check, benchmark)
	}
	for _, tok := range check {
		b := r.BiasOf(tok)
		if (r.mode == ModeStrict && b != want) || (r.mode != ModeStrict && b == against) {
			return false, "radar: " + r.name(tok) + " is " + b.String()
		}
	}
	return true, ""
}
