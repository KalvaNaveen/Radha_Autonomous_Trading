// Package indicators implements the streaming maths used by the strategy:
// time-bucketed OHLCV candles, EMA, intraday VWAP and time-of-day RVOL.
//
// None of these types are safe for concurrent use: each one is owned by a
// single stock agent goroutine. That is deliberate — it keeps the hot path
// lock-free and allocation-free.
package indicators

import (
	"math"
	"time"

	"github.com/nkalva/kitealgo/pkg/models"
)

// ---------------------------------------------------------------------------
// EMA
// ---------------------------------------------------------------------------

// EMA is an exponential moving average with alpha = 2/(N+1), seeded with the
// simple average of the first N inputs (the convention used by most charting
// platforms, so values line up with what you see on Kite charts).
type EMA struct {
	period int
	alpha  float64
	value  float64
	prev   float64
	count  int
	sum    float64
}

// NewEMA returns an EMA of the given period.
func NewEMA(period int) *EMA {
	if period < 1 {
		period = 1
	}
	return &EMA{period: period, alpha: 2.0 / float64(period+1)}
}

// Update feeds one closed-bar value and returns the new EMA (0 until ready).
func (e *EMA) Update(v float64) float64 {
	e.prev = e.value
	e.count++
	if e.count <= e.period {
		e.sum += v
		if e.count == e.period {
			e.value = e.sum / float64(e.period)
		}
		return e.Value()
	}
	e.value = e.alpha*v + (1-e.alpha)*e.value
	return e.value
}

// Seed feeds historical closes (oldest first).
func (e *EMA) Seed(closes []float64) {
	for _, c := range closes {
		e.Update(c)
	}
}

// Ready reports whether at least `period` values were seen.
func (e *EMA) Ready() bool { return e.count >= e.period }

// Value returns the current EMA, or 0 if not ready.
func (e *EMA) Value() float64 {
	if !e.Ready() {
		return 0
	}
	return e.value
}

// Prev returns the EMA before the most recent update, or 0 if it was not ready then.
func (e *EMA) Prev() float64 {
	if e.count <= e.period {
		return 0
	}
	return e.prev
}

// Period returns N.
func (e *EMA) Period() int { return e.period }

// Peek returns what the EMA would be if v were the next close, without mutating state.
func (e *EMA) Peek(v float64) float64 {
	if !e.Ready() {
		return 0
	}
	return e.alpha*v + (1-e.alpha)*e.value
}

// ---------------------------------------------------------------------------
// VWAP
// ---------------------------------------------------------------------------

// VWAP accumulates Σ(P·ΔV)/ΣΔV from the cumulative day volume carried by each
// tick. KiteTicker ticks are ~1s snapshots, not individual trades, so the
// computed value is an approximation; when the exchange ATP is available the
// agent can prefer it (config strategy.vwap_source).
type VWAP struct {
	pv       float64
	vol      float64
	lastCum  uint64
	started  bool
	exchange float64
}

// Update ingests a tick. cumVolume is the day's cumulative traded volume.
func (w *VWAP) Update(price float64, cumVolume uint64, exchangeATP float64) {
	if exchangeATP > 0 {
		w.exchange = exchangeATP
	}
	if !w.started {
		// First observation: we cannot attribute earlier volume to a price
		// path, so attribute it all to the first seen price (exact if the
		// engine starts before the first trade, approximate otherwise).
		w.started = true
		w.lastCum = cumVolume
		if cumVolume > 0 && price > 0 {
			ref := price
			if exchangeATP > 0 {
				ref = exchangeATP
			}
			w.pv = ref * float64(cumVolume)
			w.vol = float64(cumVolume)
		}
		return
	}
	if cumVolume <= w.lastCum || price <= 0 {
		return // no new volume (or feed reset) — nothing to add
	}
	dv := float64(cumVolume - w.lastCum)
	w.lastCum = cumVolume
	w.pv += price * dv
	w.vol += dv
}

// Computed returns the self-computed VWAP (0 until volume is seen).
func (w *VWAP) Computed() float64 {
	if w.vol == 0 {
		return 0
	}
	return w.pv / w.vol
}

// Exchange returns the latest exchange ATP (0 if never received).
func (w *VWAP) Exchange() float64 { return w.exchange }

// Value returns the preferred VWAP: exchange ATP when preferExchange and
// available, otherwise the computed value.
func (w *VWAP) Value(preferExchange bool) float64 {
	if preferExchange && w.exchange > 0 {
		return w.exchange
	}
	return w.Computed()
}

// ---------------------------------------------------------------------------
// RVOL
// ---------------------------------------------------------------------------

// RVOLProfile holds the average cumulative volume at the end of each minute of
// the session, averaged over the lookback days. Index i is the cumulative
// volume at MarketOpen + (i+1) minutes.
type RVOLProfile struct {
	AvgCumulative []float64 `json:"avg_cumulative"`
	Days          int       `json:"days"`
}

// Valid reports whether the profile has data.
func (p *RVOLProfile) Valid() bool { return p != nil && p.Days > 0 && len(p.AvgCumulative) > 0 }

// BaselineAt returns the expected cumulative volume `elapsed` after the open,
// linearly interpolated inside the minute so RVOL does not jump at :00.
func (p *RVOLProfile) BaselineAt(elapsed time.Duration) float64 {
	if !p.Valid() || elapsed <= 0 {
		return 0
	}
	mins := elapsed.Minutes()
	i := int(math.Floor(mins)) // minutes fully elapsed
	frac := mins - float64(i)
	n := len(p.AvgCumulative)
	if i >= n {
		return p.AvgCumulative[n-1]
	}
	var lo float64
	if i > 0 {
		lo = p.AvgCumulative[i-1]
	}
	hi := p.AvgCumulative[i]
	return lo + (hi-lo)*frac
}

// RVOL returns today's cumulative volume divided by the historical baseline at
// the same time of day. Returns 0 when the baseline is unusable (first seconds
// of the session or no history), which the strategy treats as "not confirmed".
func (p *RVOLProfile) RVOL(elapsed time.Duration, cumVolume uint64) float64 {
	base := p.BaselineAt(elapsed)
	if base < 1 {
		return 0
	}
	return float64(cumVolume) / base
}

// BuildRVOLProfile builds a profile from per-day minute volumes. days[d][m] is
// the traded volume in minute m (0 = the 09:15 minute) of day d. Days shorter
// than sessionMinutes are padded with zero volume (e.g. halted stocks).
func BuildRVOLProfile(days [][]float64, sessionMinutes int) RVOLProfile {
	prof := RVOLProfile{AvgCumulative: make([]float64, sessionMinutes)}
	if len(days) == 0 {
		return prof
	}
	for _, day := range days {
		var cum float64
		for m := 0; m < sessionMinutes; m++ {
			if m < len(day) {
				cum += day[m]
			}
			prof.AvgCumulative[m] += cum
		}
	}
	for m := range prof.AvgCumulative {
		prof.AvgCumulative[m] /= float64(len(days))
	}
	prof.Days = len(days)
	return prof
}

// ---------------------------------------------------------------------------
// Candle builder
// ---------------------------------------------------------------------------

// CandleBuilder turns a tick stream into fixed-interval OHLCV candles aligned
// to the market open (09:15, 09:20, ...). Candle volume is derived from the
// cumulative day volume so it is exact regardless of how many ticks arrive.
// Intervals with no trades are emitted as flat zero-volume candles so the EMA
// stays aligned to clock time, matching exchange charting conventions.
type CandleBuilder struct {
	token    uint32
	interval time.Duration
	grace    time.Duration
	open     time.Time // 09:15 IST of the trading day

	cur       models.Candle
	has       bool      // cur is an open, in-progress candle
	lastEnd   time.Time // End of the last emitted candle (zero before the first)
	lastClose float64
	lastCum   uint64
	cumInit   bool
	carryVol  uint64 // volume from late ticks whose bar was already emitted
}

// NewCandleBuilder creates a builder. open is 09:15 IST of the trading day.
func NewCandleBuilder(token uint32, interval, grace time.Duration, open time.Time) *CandleBuilder {
	return &CandleBuilder{token: token, interval: interval, grace: grace, open: open}
}

func (b *CandleBuilder) bucketStart(t time.Time) time.Time {
	n := t.Sub(b.open) / b.interval
	return b.open.Add(n * b.interval)
}

func (b *CandleBuilder) emit(out []models.Candle, c models.Candle) []models.Candle {
	b.lastEnd = c.End
	b.lastClose = c.Close
	return append(out, c)
}

func (b *CandleBuilder) fillUntil(out []models.Candle, start time.Time) []models.Candle {
	if b.lastEnd.IsZero() {
		return out
	}
	for s := b.lastEnd; s.Before(start); s = s.Add(b.interval) {
		out = b.emit(out, b.flat(s))
	}
	return out
}

// Update ingests a tick and returns the candles that closed as a result.
func (b *CandleBuilder) Update(t models.Tick) []models.Candle {
	ts := t.MarketTime()
	if !b.cumInit {
		b.cumInit = true
		b.lastCum = t.VolumeTraded
	}
	var dv uint64
	if t.VolumeTraded > b.lastCum {
		dv = t.VolumeTraded - b.lastCum
		b.lastCum = t.VolumeTraded
	}
	if ts.Before(b.open) || t.LastPrice <= 0 {
		return nil // pre-open auction: volume baseline advanced above, no bar
	}

	start := b.bucketStart(ts)
	var out []models.Candle
	switch {
	case b.has && start.After(b.cur.Start):
		out = b.emit(out, b.cur)
		b.has = false
		out = b.fillUntil(out, start)
	case b.has && start.Before(b.cur.Start):
		start = b.cur.Start // late tick: fold into the open bar
	case !b.has && !b.lastEnd.IsZero() && start.Before(b.lastEnd):
		// Late tick for a bar already emitted by Flush: keep its volume, drop its price.
		b.carryVol += dv
		return nil
	case !b.has:
		out = b.fillUntil(out, start)
	}

	if !b.has {
		b.cur = models.Candle{
			InstrumentToken: b.token, Interval: b.interval,
			Start: start, End: start.Add(b.interval),
			Open: t.LastPrice, High: t.LastPrice, Low: t.LastPrice, Close: t.LastPrice,
			Volume: b.carryVol,
		}
		b.carryVol = 0
		b.has = true
	}
	c := &b.cur
	if t.LastPrice > c.High {
		c.High = t.LastPrice
	}
	if t.LastPrice < c.Low {
		c.Low = t.LastPrice
	}
	c.Close = t.LastPrice
	c.Volume += dv
	c.TickCount++
	return out
}

func (b *CandleBuilder) flat(start time.Time) models.Candle {
	p := b.lastClose
	return models.Candle{InstrumentToken: b.token, Interval: b.interval, Start: start, End: start.Add(b.interval),
		Open: p, High: p, Low: p, Close: p}
}

// Flush closes the open candle once `now` is past its end plus the grace
// period, and emits flat candles for any further fully elapsed intervals.
// Call it from a timer so illiquid stocks still produce bars on time.
func (b *CandleBuilder) Flush(now time.Time) []models.Candle {
	var out []models.Candle
	if b.has {
		if now.Before(b.cur.End.Add(b.grace)) {
			return nil
		}
		out = b.emit(out, b.cur)
		b.has = false
	}
	if b.lastEnd.IsZero() {
		return out
	}
	for !now.Before(b.lastEnd.Add(b.interval).Add(b.grace)) {
		out = b.emit(out, b.flat(b.lastEnd))
	}
	return out
}

// Current returns the in-progress candle.
func (b *CandleBuilder) Current() (models.Candle, bool) { return b.cur, b.has }

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// RoundToTick rounds p to the instrument tick. dir > 0 rounds up, dir < 0 down, 0 nearest.
func RoundToTick(p, tick float64, dir int) float64 {
	if tick <= 0 {
		tick = 0.05
	}
	q := p / tick
	switch {
	case dir > 0:
		q = math.Ceil(q - 1e-9)
	case dir < 0:
		q = math.Floor(q + 1e-9)
	default:
		q = math.Round(q)
	}
	// Round the product to 2dp to remove float noise (NSE ticks are >= 0.01).
	return math.Round(q*tick*100) / 100
}

// Pct converts a percentage (0.40) to a fraction (0.004).
func Pct(p float64) float64 { return p / 100 }
