// Package history prepares, before the open, everything that depends on past
// sessions: the RVOL time-of-day volume profile and the 5-minute closes used
// to seed the EMAs (so the 10/20 EMA are valid at 09:45, not at 11:00).
package history

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/nkalva/kitealgo/internal/broker"
	"github.com/nkalva/kitealgo/internal/clock"
	"github.com/nkalva/kitealgo/internal/indicators"
)

// SessionMinutes is 09:15–15:30.
const SessionMinutes = 375

// seedCloses is how many prior 5-minute closes seed the EMAs (> 3×slow period).
const seedCloses = 100

// Data is the prepared history for one instrument.
type Data struct {
	Token   uint32                 `json:"token"`
	Profile indicators.RVOLProfile `json:"profile"`
	Seed5m  []float64              `json:"seed_5m"`
	Days    []string               `json:"days"`
}

// Loader fetches and caches history. The Kite historical API allows 3 req/s;
// the loader paces itself at ~2.5 req/s and needs one request per instrument.
type Loader struct {
	md       broker.MarketData
	dir      string
	lookback int
	log      *slog.Logger
	pace     time.Duration
}

// NewLoader creates a loader caching under dataDir/history/<day>/.
func NewLoader(md broker.MarketData, dataDir string, lookbackDays int, log *slog.Logger) *Loader {
	return &Loader{md: md, dir: filepath.Join(dataDir, "history"), lookback: lookbackDays, log: log.With("component", "history"), pace: 400 * time.Millisecond}
}

// Load returns data for every token for trading day `day` (history strictly before it).
func (l *Loader) Load(ctx context.Context, day time.Time, tokens []uint32, interval time.Duration) (map[uint32]Data, error) {
	out := make(map[uint32]Data, len(tokens))
	cacheDir := filepath.Join(l.dir, day.Format("2006-01-02"))
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, err
	}
	var last time.Time
	for _, tok := range tokens {
		path := filepath.Join(cacheDir, fmt.Sprintf("%d.json", tok))
		if raw, err := os.ReadFile(path); err == nil {
			var d Data
			if json.Unmarshal(raw, &d) == nil && d.Token == tok {
				out[tok] = d
				continue
			}
		}
		if wait := l.pace - time.Since(last); wait > 0 {
			select {
			case <-ctx.Done():
				return out, ctx.Err()
			case <-time.After(wait):
			}
		}
		last = time.Now()
		d, err := l.fetch(ctx, day, tok, interval)
		if err != nil {
			l.log.Warn("history unavailable — instrument will trade without RVOL/EMA seed until it builds its own", "token", tok, "err", err)
			out[tok] = Data{Token: tok}
			continue
		}
		out[tok] = d
		if raw, err := json.Marshal(d); err == nil {
			_ = os.WriteFile(path, raw, 0o644)
		}
	}
	return out, nil
}

func (l *Loader) fetch(ctx context.Context, day time.Time, tok uint32, interval time.Duration) (Data, error) {
	dayStart := clock.Midnight(day)
	// ~2.5 calendar days per trading day covers weekends and holidays.
	from := dayStart.AddDate(0, 0, -(l.lookback*7/4 + 7))
	to := dayStart.Add(-time.Second)
	bars, err := l.md.MinuteCandles(ctx, tok, from, to)
	if err != nil {
		return Data{}, err
	}
	return Build(tok, bars, l.lookback, interval), nil
}

// Build turns minute bars into a profile + EMA seed. Exposed for testing.
func Build(tok uint32, bars []broker.HistCandle, lookback int, interval time.Duration) Data {
	type dayBars struct {
		key  string
		mins []float64
		bars []broker.HistCandle
	}
	byDay := map[string]*dayBars{}
	for _, b := range bars {
		t := b.Time.In(clock.IST)
		key := t.Format("2006-01-02")
		d := byDay[key]
		if d == nil {
			d = &dayBars{key: key, mins: make([]float64, SessionMinutes)}
			byDay[key] = d
		}
		open := clock.Midnight(t).Add(9*time.Hour + 15*time.Minute)
		m := int(t.Sub(open) / time.Minute)
		if m < 0 || m >= SessionMinutes {
			continue
		}
		d.mins[m] += float64(b.Volume)
		d.bars = append(d.bars, b)
	}
	keys := make([]string, 0, len(byDay))
	for k := range byDay {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// RVOL: last `lookback` days that actually traded.
	var vols [][]float64
	var used []string
	for i := len(keys) - 1; i >= 0 && len(vols) < lookback; i-- {
		d := byDay[keys[i]]
		var total float64
		for _, v := range d.mins {
			total += v
		}
		if total == 0 {
			continue // index or suspended day: no volume information
		}
		vols = append(vols, d.mins)
		used = append(used, d.key)
	}
	data := Data{Token: tok, Profile: indicators.BuildRVOLProfile(vols, SessionMinutes), Days: used}

	// EMA seed: aggregate minute bars into session-aligned interval closes.
	var closes []float64
	for _, k := range keys {
		d := byDay[k]
		sort.Slice(d.bars, func(i, j int) bool { return d.bars[i].Time.Before(d.bars[j].Time) })
		open := clock.Midnight(d.bars[0].Time).Add(9*time.Hour + 15*time.Minute)
		var bucket time.Time
		var last float64
		for _, b := range d.bars {
			bs := open.Add(b.Time.Sub(open) / interval * interval)
			if !bucket.IsZero() && bs.After(bucket) {
				closes = append(closes, last)
			}
			bucket, last = bs, b.Close
		}
		if !bucket.IsZero() {
			closes = append(closes, last)
		}
	}
	if len(closes) > seedCloses {
		closes = closes[len(closes)-seedCloses:]
	}
	data.Seed5m = closes
	return data
}
