package swing

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/nkalva/kitealgo/internal/config"
	"github.com/nkalva/kitealgo/pkg/models"
)

// Research tags: why a stock came up in the scan, read from its own price and
// volume history (so the backtest can test them without hindsight). Listed
// from the strongest evidence to the weakest; a stock gets every tag that
// fits and the first one as its primary tag.
const (
	TagResults    = "RESULTS"    // a results/news-type move: big gap or jump on heavy volume in the last 10 sessions
	TagTurnaround = "TURNAROUND" // fell ≥30% within the year, now back above a 200-day average it was below recently
	TagNewHigh    = "NEW_HIGH"   // at or within 3% of its 52-week high — breaking into new ground
	TagMomentum   = "MOMENTUM"   // established uptrend: above a rising 200-day average, EMA50 above it, beating NIFTY
	TagNone       = "NONE"       // the entry signal alone, no supporting story
)

// ResearchTags is the priority order used for ranking.
var ResearchTags = []string{TagResults, TagTurnaround, TagNewHigh, TagMomentum, TagNone}

const (
	resultsLookback = 10   // sessions
	resultsGapPct   = 4.0  // open ≥ 4% above the previous close…
	resultsMovePct  = 6.0  // …or close ≥ 6% above it…
	resultsVolRatio = 2.5  // …on ≥ 2.5× average volume
	newHighWithin   = 3.0  // % below the 52-week high
	turnFallPct     = 30.0 // % fall from the 1-year high
	momentumRS      = 10.0 // % points ahead of the index over rs_lookback
)

// Research classifies the stock at bar i. rs is its relative strength vs the
// index. It returns the primary tag and one line of evidence per tag found.
func Research(s *Series, i int, rs float64) (string, []string) {
	var tags, notes []string
	add := func(tag, note string) {
		tags = append(tags, tag)
		notes = append(notes, tag+": "+note)
	}
	b := s.Bars
	// Results / news: the biggest qualifying move in the last 10 sessions.
	best, bestK := 0.0, -1
	for k := i; k > i-resultsLookback && k > 0; k-- {
		prev := b[k-1].Close
		if prev <= 0 || s.VolAvg[k-1] <= 0 {
			continue
		}
		gap := (b[k].Open/prev - 1) * 100
		move := (b[k].Close/prev - 1) * 100
		vr := b[k].Volume / s.VolAvg[k-1]
		if (gap >= resultsGapPct || move >= resultsMovePct) && vr >= resultsVolRatio && math.Max(gap, move) > best {
			best, bestK = math.Max(gap, move), k
		}
	}
	if bestK >= 0 {
		add(TagResults, fmt.Sprintf("+%.1f%% on %.1f× volume on %s", best, b[bestK].Volume/s.VolAvg[bestK-1], b[bestK].Date.Format("02 Jan")))
	}
	// Turnaround: a ≥30% fall within the year, then back above EMA200 after
	// being below it in the last ~6 months.
	if i >= 200 {
		lo := i
		for k := i; k > i-250 && k >= 0; k-- {
			if b[k].Close < b[lo].Close {
				lo = k
			}
		}
		hi := lo
		for k := lo; k > i-250 && k >= 0; k-- {
			if b[k].Close > b[hi].Close {
				hi = k
			}
		}
		fall := (1 - b[lo].Close/b[hi].Close) * 100
		wasBelow := false
		for k := i - 120; k <= i-5; k++ {
			if k >= 0 && b[k].Close < s.EMA200[k] {
				wasBelow = true
				break
			}
		}
		if fall >= turnFallPct && wasBelow && b[i].Close > s.EMA200[i] {
			add(TagTurnaround, fmt.Sprintf("fell %.0f%% to %.2f (%s), now %.1f%% above its 200-day average", fall, b[lo].Close, b[lo].Date.Format("Jan 2006"), (b[i].Close/s.EMA200[i]-1)*100))
		}
	}
	// New 52-week high.
	if i >= 60 {
		hi := 0.0
		for k := i; k > i-252 && k >= 0; k-- {
			hi = math.Max(hi, b[k].High)
		}
		if below := (1 - b[i].Close/hi) * 100; below <= newHighWithin {
			add(TagNewHigh, fmt.Sprintf("close %.2f is %.1f%% below its 52-week high %.2f", b[i].Close, below, hi))
		}
	}
	// Established momentum.
	if i >= 220 && b[i].Close > s.EMA200[i] && s.EMASlow[i] > s.EMA200[i] && s.EMA200[i] > s.EMA200[i-20] && rs >= momentumRS {
		add(TagMomentum, fmt.Sprintf("above a rising 200-day average, EMA50 above it, %+.1f%% vs NIFTY", rs))
	}
	if len(tags) == 0 {
		return TagNone, []string{TagNone + ": the entry signal alone — no results move, turnaround, 52-week high or strong uptrend behind it"}
	}
	return tags[0], notes
}

// researchAllowed reports whether any of the stock's tags is in the allowed
// list ("" or "all" allows everything). The primary tag is the strongest,
// but a stock tagged RESULTS and NEW_HIGH passes a NEW_HIGH-only filter too.
func researchAllowed(allow string, notes []string) bool {
	if allow == "" || strings.EqualFold(allow, "all") {
		return true
	}
	for _, a := range strings.Split(allow, ",") {
		a = strings.ToUpper(strings.TrimSpace(a))
		for _, n := range notes {
			if strings.HasPrefix(n, a+":") {
				return true
			}
		}
	}
	return false
}

func tagRank(tag string) int {
	for k, t := range ResearchTags {
		if t == tag {
			return k
		}
	}
	return len(ResearchTags)
}

// SortSignals orders tomorrow's candidates the same way for the live engine
// and the backtest: fresh crosses before trend entries; then, with
// research_rank "research", the stronger research tag first; then relative
// strength.
func SortSignals(sigs []models.Signal, p config.StrategyConfig) {
	sort.SliceStable(sigs, func(a, b int) bool {
		fa, fb := sigs[a].Setup != models.SetupTrend, sigs[b].Setup != models.SetupTrend
		if fa != fb {
			return fa
		}
		if p.ResearchRank == "research" {
			if ra, rb := tagRank(sigs[a].Research), tagRank(sigs[b].Research); ra != rb {
				return ra < rb
			}
		}
		return sigs[a].Score > sigs[b].Score
	})
}
