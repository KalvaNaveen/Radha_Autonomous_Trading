// Package data holds the stock universe, the NSE instrument master and the
// cached daily candles the scanner and the backtester read.
package data

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/nkalva/kitealgo/internal/broker"
)

// MaxUniverse is the most symbols the scanner accepts.
const MaxUniverse = 300

// DefaultUniverse is written on the first run (NIFTY 50 heavyweights).
const DefaultUniverse = `# Stocks the swing scanner may trade — one NSE symbol per line (max 300).
# Edit in the control panel (Universe tab) or here; paste in any format.
symbol
ADANIPORTS
ASIANPAINT
AXISBANK
BAJFINANCE
BHARTIARTL
HCLTECH
HDFCBANK
HINDUNILVR
ICICIBANK
INFY
ITC
KOTAKBANK
LT
M&M
MARUTI
NESTLEIND
NTPC
POWERGRID
RELIANCE
SBIN
SUNPHARMA
TATASTEEL
TCS
TITAN
ULTRACEMCO
`

var (
	symRe    = regexp.MustCompile(`^[A-Z0-9&_-]+$`)
	headerRe = regexp.MustCompile(`(?i)^(symbols?|tradingsymbol|instrument|stock|scrip|ticker)$`)
	splitRe  = regexp.MustCompile(`[,;\t |"']+`)
)

// ParseUniverse reads symbols in any pasted format: one per line, or comma /
// space separated; "NSE:INFY" and "INFY-EQ" become "INFY". Comments (#) and
// a header row are ignored. It returns the unique symbols in order, notes
// about what was dropped, and an error for an empty or oversized list.
func ParseUniverse(r io.Reader) ([]string, []string, error) {
	out, warns, err := parseSymbols(r)
	if err != nil {
		return out, warns, err
	}
	if len(out) == 0 {
		return out, warns, errors.New("the universe is empty — add at least one NSE symbol")
	}
	if len(out) > MaxUniverse {
		return out[:MaxUniverse], warns, fmt.Errorf("the universe has %d symbols; the maximum is %d", len(out), MaxUniverse)
	}
	return out, warns, nil
}

// LoadList reads a broad stock list of any length (e.g. NSE's NIFTY 500
// CSV download: only its "Symbol" column is used).
func LoadList(path string) ([]string, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	out, warns, err := parseSymbols(f)
	if err == nil && len(out) == 0 {
		err = fmt.Errorf("%s has no NSE symbols", path)
	}
	return out, warns, err
}

// parseSymbols reads symbols in any pasted format. A CSV whose header row
// has a "Symbol" column among others (NSE index downloads) is read from that
// column only.
func parseSymbols(r io.Reader) ([]string, []string, error) {
	var out, warns []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	col, first := -1, true
	for sc.Scan() {
		ln := sc.Text()
		if k := strings.IndexByte(ln, '#'); k >= 0 {
			ln = ln[:k]
		}
		if strings.TrimSpace(ln) == "" {
			continue
		}
		if first {
			first = false
			if fs := strings.Split(ln, ","); len(fs) > 1 {
				for k, f := range fs {
					if strings.EqualFold(strings.Trim(strings.TrimSpace(f), `"`+string(rune(0xFEFF))), "symbol") {
						col = k
					}
				}
				if col >= 0 {
					continue
				}
			}
		}
		if col >= 0 {
			fs := strings.Split(ln, ",")
			if col >= len(fs) {
				continue
			}
			ln = fs[col]
		}
		var tk []string
		for _, t := range splitRe.Split(strings.TrimSpace(ln), -1) {
			if t != "" {
				tk = append(tk, t)
			}
		}
		if len(tk) > 0 && headerRe.MatchString(tk[0]) {
			continue
		}
		for _, x := range tk {
			x = strings.ToUpper(strings.TrimPrefix(x, string(rune(0xFEFF))))
			if strings.Contains(x, ":") {
				if !strings.HasPrefix(x, "NSE:") {
					warns = append(warns, x+": only NSE stocks are traded — skipped")
					continue
				}
				x = x[4:]
			}
			x = strings.TrimSuffix(x, "-EQ")
			switch {
			case x == "" || x == "NSE" || x == "EQ" || isNumber(x):
				continue
			case !symRe.MatchString(x):
				warns = append(warns, x+": not a valid NSE symbol — skipped")
				continue
			case seen[x]:
				continue
			}
			seen[x] = true
			out = append(out, x)
		}
	}
	return out, warns, sc.Err()
}

var (
	nonStockSym  = regexp.MustCompile(`(BEES|ETF|IETF)$|^(LIQUID|GILT|SGB)`)
	nonStockName = regexp.MustCompile(`\bETF\b|EXCHANGE TRADED|\bFUND\b|\bFOF\b`)
)

// AllEquities lists every ordinary NSE share in the instrument master: the EQ
// series only (no BE/SM/bond suffixes), without ETFs, index funds and
// sovereign gold bonds. Liquidity is filtered later from the candles.
func AllEquities(nse []broker.Instrument) []broker.Instrument {
	var out []broker.Instrument
	for _, in := range nse {
		s := in.TradingSymbol
		if in.Segment != "NSE" || in.InstrumentType != "EQ" || in.Name == "" || strings.Contains(s, "-") ||
			!symRe.MatchString(s) || nonStockSym.MatchString(s) || nonStockName.MatchString(strings.ToUpper(in.Name)) {
			continue
		}
		out = append(out, in)
	}
	return out
}

func isNumber(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && c != '.' {
			return false
		}
	}
	return true
}

// FormatUniverse is the canonical file: a short header, one symbol per line.
func FormatUniverse(syms []string) string {
	var b strings.Builder
	b.WriteString("# Stocks the swing scanner may trade — one NSE symbol per line (max 300).\n")
	b.WriteString("# Edit in the control panel (Universe tab) or here; paste in any format.\n")
	b.WriteString("symbol\n")
	for _, s := range syms {
		b.WriteString(s)
		b.WriteByte('\n')
	}
	return b.String()
}

// LoadUniverse reads and parses the universe file.
func LoadUniverse(path string) ([]string, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	return ParseUniverse(f)
}

// Market-cap categories used by the category market check.
const (
	CapLarge = "large"
	CapMid   = "mid"
	CapSmall = "small"
)

// LoadCaps reads the optional market-cap file: "SYMBOL,large|mid|small" per
// line (comments and a header allowed). A missing file is not an error.
func LoadCaps(path string) (map[string]string, []string, error) {
	out := map[string]string{}
	if path == "" {
		return out, nil, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil, nil
	}
	if err != nil {
		return out, nil, err
	}
	defer f.Close()
	var warns []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		ln := sc.Text()
		if k := strings.IndexByte(ln, '#'); k >= 0 {
			ln = ln[:k]
		}
		var tk []string
		for _, t := range splitRe.Split(strings.TrimSpace(ln), -1) {
			if t != "" {
				tk = append(tk, t)
			}
		}
		if len(tk) < 2 || headerRe.MatchString(tk[0]) {
			continue
		}
		sym := strings.TrimSuffix(strings.TrimPrefix(strings.ToUpper(tk[0]), "NSE:"), "-EQ")
		switch c := strings.ToLower(tk[1]); c {
		case CapLarge, CapMid, CapSmall:
			out[sym] = c
		default:
			warns = append(warns, fmt.Sprintf("%s: unknown category %q (use large, mid or small)", sym, tk[1]))
		}
	}
	return out, warns, sc.Err()
}

// Resolve maps symbols to NSE equity instruments. Symbols not found are
// reported and left out.
func Resolve(syms []string, nse []broker.Instrument) ([]broker.Instrument, []string) {
	by := make(map[string]broker.Instrument, len(nse))
	for _, in := range nse {
		if in.Segment == "NSE" && in.InstrumentType == "EQ" {
			by[in.TradingSymbol] = in
		}
	}
	var out []broker.Instrument
	var warns []string
	for _, s := range syms {
		if in, ok := by[s]; ok {
			out = append(out, in)
			continue
		}
		if in, ok := by[s+"-BE"]; ok {
			warns = append(warns, s+": trades in the BE (trade-to-trade) series as "+s+"-BE")
			out = append(out, in)
			continue
		}
		warns = append(warns, s+": not found on NSE — skipped")
	}
	return out, warns
}

// FindIndex finds an NSE index (e.g. "NIFTY 50") in the instrument master.
func FindIndex(nse []broker.Instrument, name string) (broker.Instrument, bool) {
	for _, in := range nse {
		if in.Segment == "INDICES" && strings.EqualFold(in.TradingSymbol, name) {
			return in, true
		}
	}
	return broker.Instrument{}, false
}
