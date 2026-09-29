package swing

import (
	"strings"
	"testing"

	"github.com/nkalva/kitealgo/internal/config"
	"github.com/nkalva/kitealgo/pkg/models"
)

func TestResearchTags(t *testing.T) {
	c := config.Defaults()
	// 260 bars falling 600 → ~340, then a recovery back above the 200-day average.
	cl := uptrend(260, 600, -1)
	cl = append(cl, uptrend(60, cl[259], 2)...)
	b := bars(cl, 1e6)
	// A results-type jump 3 sessions before the end: +8% on 4× volume.
	k := len(b) - 3
	b[k].Open, b[k].Close, b[k].High, b[k].Volume = b[k-1].Close*1.05, b[k-1].Close*1.08, b[k-1].Close*1.09, 4e6
	for j := k + 1; j < len(b); j++ {
		b[j].Open, b[j].Close, b[j].High, b[j].Low = b[j-1].Close, b[j-1].Close*1.004, b[j-1].Close*1.01, b[j-1].Close*0.995
	}
	s := NewSeries(b, c.Strategy)
	tag, notes := Research(s, len(b)-1, 5)
	all := strings.Join(notes, " | ")
	if tag != TagResults || !strings.Contains(all, TagTurnaround+":") {
		t.Fatalf("want RESULTS primary with TURNAROUND too, got %s: %s", tag, all)
	}
	if strings.Contains(all, TagNewHigh+":") {
		t.Fatalf("far below its 52-week high, must not be NEW_HIGH: %s", all)
	}
	if !researchAllowed("TURNAROUND", notes) || researchAllowed("NEW_HIGH,MOMENTUM", notes) || !researchAllowed("all", notes) {
		t.Fatal("research_allow must match any of the stock's tags")
	}
	// A steady riser with nothing special is NONE… until it nears its high.
	flat := NewSeries(bars(uptrend(100, 500, -0.5), 1e6), c.Strategy)
	if tag, _ := Research(flat, 99, 0); tag != TagNone {
		t.Fatalf("want NONE, got %s", tag)
	}
}

func TestSortSignals(t *testing.T) {
	sigs := []models.Signal{
		{Symbol: "A", Setup: models.SetupTrend, Research: TagResults, Score: 50},
		{Symbol: "B", Setup: models.SetupEMACross, Research: TagNone, Score: 10},
		{Symbol: "C", Setup: models.SetupEMACross, Research: TagNewHigh, Score: 5},
	}
	p := config.Defaults().Strategy
	SortSignals(sigs, p)
	if sigs[0].Symbol != "B" || sigs[2].Symbol != "A" {
		t.Fatalf("rs: fresh crosses first by strength, got %v %v %v", sigs[0].Symbol, sigs[1].Symbol, sigs[2].Symbol)
	}
	p.ResearchRank = "research"
	SortSignals(sigs, p)
	if sigs[0].Symbol != "C" || sigs[1].Symbol != "B" {
		t.Fatalf("research: stronger tag first among fresh crosses, got %v %v", sigs[0].Symbol, sigs[1].Symbol)
	}
}
