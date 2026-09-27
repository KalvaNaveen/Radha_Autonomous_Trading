// Package watchlist loads the stock list you drop in every evening.
//
// File: <watchlist_dir>/<YYYY-MM-DD>.csv where the date is the TRADING day the
// list is for (drop Monday's list on Friday evening). Format, header optional:
//
//	symbol,benchmark
//	RELIANCE,NIFTY ENERGY
//	HDFCBANK,NIFTY BANK
//	TCS
//
// benchmark is an optional NSE index (sector) used by the radar; blank = the
// market index only. Lines starting with # are comments.
package watchlist

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nkalva/kitealgo/internal/broker"
)

// MaxSymbols is the engine's capacity per day.
const MaxSymbols = 50

// Entry is one requested symbol.
type Entry struct {
	Symbol    string
	Benchmark string
}

// Resolved is a validated, tradable watchlist entry.
type Resolved struct {
	Instrument broker.Instrument
	Benchmark  *broker.Instrument // nil = market index only
}

// ErrNoWatchlist means no file exists for the day.
var ErrNoWatchlist = errors.New("watchlist: no file for this trading day")

// PathFor returns the expected file path for a trading day.
func PathFor(dir string, day time.Time) string {
	return filepath.Join(dir, day.Format("2006-01-02")+".csv")
}

// Load reads and normalises the file for day. It returns warnings for
// skipped lines; a missing file returns ErrNoWatchlist.
func Load(dir string, day time.Time) ([]Entry, []string, error) {
	path := PathFor(dir, day)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("%w: expected %s", ErrNoWatchlist, path)
	}
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	return Parse(f)
}

// Parse reads watchlist CSV content.
func Parse(r io.Reader) ([]Entry, []string, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.Comment = '#'
	cr.TrimLeadingSpace = true
	var out []Entry
	var warns []string
	seen := map[string]bool{}
	line := 0
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		line++
		if err != nil {
			return nil, warns, fmt.Errorf("watchlist line %d: %w", line, err)
		}
		if len(rec) == 0 || strings.TrimSpace(rec[0]) == "" {
			continue
		}
		sym := normalise(rec[0])
		if line == 1 && (sym == "SYMBOL" || sym == "TRADINGSYMBOL") {
			continue // header
		}
		var bench string
		if len(rec) > 1 {
			bench = strings.ToUpper(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rec[1]), "NSE:")))
		}
		if seen[sym] {
			warns = append(warns, fmt.Sprintf("duplicate %s ignored", sym))
			continue
		}
		seen[sym] = true
		if len(out) == MaxSymbols {
			warns = append(warns, fmt.Sprintf("more than %d symbols: %s and later ignored", MaxSymbols, sym))
			break
		}
		out = append(out, Entry{Symbol: sym, Benchmark: bench})
	}
	if len(out) == 0 {
		return nil, warns, errors.New("watchlist: file has no symbols")
	}
	return out, warns, nil
}

func normalise(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "NSE:")
	s = strings.TrimSuffix(s, "-EQ")
	return s
}

// Resolve maps entries onto the NSE instrument master. Unknown symbols and
// non-equity instruments are skipped with a warning (never fatal).
func Resolve(entries []Entry, nse []broker.Instrument) ([]Resolved, []string) {
	eq := map[string]broker.Instrument{}
	idx := map[string]broker.Instrument{}
	for _, in := range nse {
		switch {
		case in.Segment == "INDICES":
			idx[strings.ToUpper(in.TradingSymbol)] = in
		case in.InstrumentType == "EQ" && in.Segment == "NSE":
			eq[in.TradingSymbol] = in
		}
	}
	var out []Resolved
	var warns []string
	for _, e := range entries {
		in, ok := eq[e.Symbol]
		if !ok {
			warns = append(warns, fmt.Sprintf("%s: not an NSE EQ instrument — skipped", e.Symbol))
			continue
		}
		r := Resolved{Instrument: in}
		if e.Benchmark != "" {
			if b, ok := idx[e.Benchmark]; ok {
				bb := b
				r.Benchmark = &bb
			} else {
				warns = append(warns, fmt.Sprintf("%s: benchmark %q not found — using market index only", e.Symbol, e.Benchmark))
			}
		}
		out = append(out, r)
	}
	return out, warns
}

// FindIndex returns an index instrument by name (e.g. "NIFTY 50").
func FindIndex(nse []broker.Instrument, name string) (broker.Instrument, bool) {
	name = strings.ToUpper(name)
	for _, in := range nse {
		if in.Segment == "INDICES" && strings.ToUpper(in.TradingSymbol) == name {
			return in, true
		}
	}
	return broker.Instrument{}, false
}
