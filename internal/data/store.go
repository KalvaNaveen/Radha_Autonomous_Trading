package data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nkalva/kitealgo/internal/broker"
	"github.com/nkalva/kitealgo/pkg/models"
)

const (
	chunkDays  = 1800                   // Kite serves at most 2000 days of daily candles per request
	minRequest = 350 * time.Millisecond // Kite historical API: 3 requests/second
)

// Store caches the NSE instrument master (one file per day) and daily candles
// (one file per instrument token, extended incrementally).
type Store struct {
	dir string
	log *slog.Logger

	mu   sync.Mutex
	md   broker.MarketData
	last time.Time // last historical request (pacing)
}

// NewStore creates a store under dataDir. md may be nil until the Kite login.
func NewStore(dataDir string, md broker.MarketData, log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	return &Store{dir: dataDir, md: md, log: log}
}

// SetMarketData sets the Kite client (after the daily login).
func (s *Store) SetMarketData(md broker.MarketData) {
	s.mu.Lock()
	s.md = md
	s.mu.Unlock()
}

func (s *Store) insDir() string    { return filepath.Join(s.dir, "instruments") }
func (s *Store) candleDir() string { return filepath.Join(s.dir, "candles") }

// Instruments returns the NSE instrument master for day, downloading it once
// per day. Without a Kite login it falls back to the newest cached copy.
func (s *Store) Instruments(ctx context.Context, day time.Time) ([]broker.Instrument, error) {
	path := filepath.Join(s.insDir(), day.Format("2006-01-02")+"-NSE.json")
	if ins, err := readInstruments(path); err == nil && len(ins) > 0 {
		return ins, nil
	}
	s.mu.Lock()
	md := s.md
	s.mu.Unlock()
	if md == nil {
		if ins := s.LatestInstruments(); len(ins) > 0 {
			return ins, nil
		}
		return nil, errors.New("no instrument list yet — log in to Kite first")
	}
	ins, err := md.Instruments(ctx, "NSE")
	if err != nil {
		return nil, err
	}
	if err := writeJSON(path, ins); err != nil {
		s.log.Warn("cannot cache the instrument list", "err", err)
	}
	old, _ := filepath.Glob(filepath.Join(s.insDir(), "*-NSE.json"))
	for _, f := range old {
		if f != path {
			_ = os.Remove(f)
		}
	}
	return ins, nil
}

// LatestInstruments returns the newest cached instrument master (nil if none).
func (s *Store) LatestInstruments() []broker.Instrument {
	files, _ := filepath.Glob(filepath.Join(s.insDir(), "*-NSE.json"))
	if len(files) == 0 {
		return nil
	}
	sort.Strings(files)
	ins, _ := readInstruments(files[len(files)-1])
	return ins
}

func readInstruments(path string) ([]broker.Instrument, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ins []broker.Instrument
	return ins, json.Unmarshal(raw, &ins)
}

// candleFile is the on-disk cache for one instrument.
type candleFile struct {
	From time.Time    `json:"from"`         // history is complete from this date…
	To   time.Time    `json:"to,omitempty"` // …up to this date (last request)
	Bars []models.Bar `json:"bars"`
}

// Daily returns daily candles in [from, to], downloading only what the cache
// does not cover yet.
func (s *Store) Daily(ctx context.Context, token uint32, from, to time.Time) ([]models.Bar, error) {
	path := filepath.Join(s.candleDir(), fmt.Sprintf("%d.json", token))
	var cf candleFile
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &cf)
	}
	if cf.To.IsZero() && len(cf.Bars) > 0 {
		cf.To = cf.Bars[len(cf.Bars)-1].Date
	}
	full := len(cf.Bars) == 0 || from.Before(cf.From)
	stale := !full && dayOf(cf.To).Before(dayOf(to))
	if full || stale {
		s.mu.Lock()
		md := s.md
		s.mu.Unlock()
		if md == nil {
			if len(cf.Bars) == 0 {
				return nil, errors.New("no cached candles — log in to Kite first")
			}
			return nil, fmt.Errorf("cached candles end %s — log in to Kite to update them", cf.To.Format("2006-01-02"))
		}
		start := from
		if stale {
			start = cf.Bars[len(cf.Bars)-1].Date // re-read the last bar in case it was partial
		}
		got, err := s.fetch(ctx, md, token, start, to)
		if err != nil {
			return nil, err
		}
		if full {
			cf.From = from
			cf.Bars = merge(filterFrom(cf.Bars, from), got)
		} else {
			cf.Bars = merge(cf.Bars, got)
		}
		cf.To = to
		if err := writeJSON(path, cf); err != nil {
			s.log.Warn("cannot cache candles", "token", token, "err", err)
		}
	}
	var out []models.Bar
	for _, b := range cf.Bars {
		if !dayOf(b.Date).Before(dayOf(from)) && !dayOf(b.Date).After(dayOf(to)) {
			out = append(out, b)
		}
	}
	return out, nil
}

// fetch downloads [from, to] in chunks, paced under Kite's limit.
func (s *Store) fetch(ctx context.Context, md broker.MarketData, token uint32, from, to time.Time) ([]models.Bar, error) {
	var out []models.Bar
	for a := from; !a.After(to); a = a.AddDate(0, 0, chunkDays+1) {
		b := a.AddDate(0, 0, chunkDays)
		if b.After(to) {
			b = to
		}
		s.pace()
		bars, err := md.DailyCandles(ctx, token, a, b.Add(23*time.Hour+59*time.Minute))
		if err != nil {
			if strings.Contains(err.Error(), "Too many requests") {
				time.Sleep(time.Second)
				s.pace()
				bars, err = md.DailyCandles(ctx, token, a, b.Add(23*time.Hour+59*time.Minute))
			}
			if err != nil {
				return nil, err
			}
		}
		out = append(out, bars...)
	}
	return out, nil
}

func (s *Store) pace() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w := minRequest - time.Since(s.last); w > 0 {
		time.Sleep(w)
	}
	s.last = time.Now()
}

// merge combines two bar lists by date; b wins on the same date.
func merge(a, b []models.Bar) []models.Bar {
	by := make(map[string]models.Bar, len(a)+len(b))
	for _, x := range a {
		by[x.Date.Format("2006-01-02")] = x
	}
	for _, x := range b {
		by[x.Date.Format("2006-01-02")] = x
	}
	out := make([]models.Bar, 0, len(by))
	for _, x := range by {
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	return out
}

func filterFrom(bars []models.Bar, from time.Time) []models.Bar {
	var out []models.Bar
	for _, b := range bars {
		if !dayOf(b.Date).Before(dayOf(from)) {
			out = append(out, b)
		}
	}
	return out
}

func dayOf(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
