package watchlist

import (
	"strings"
	"testing"

	"github.com/nkalva/kitealgo/internal/broker"
)

func TestParseNormalisesAndDedupes(t *testing.T) {
	in := "symbol,benchmark\n# comment\nnse:reliance, NIFTY ENERGY\nTCS\nTCS\nINFY-EQ\n\n"
	got, warns, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Symbol != "RELIANCE" || got[0].Benchmark != "NIFTY ENERGY" || got[2].Symbol != "INFY" {
		t.Fatalf("parse: %+v", got)
	}
	if len(warns) != 1 {
		t.Fatalf("want 1 duplicate warning, got %v", warns)
	}
}

func TestParseCapsAt50(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 60; i++ {
		b.WriteString("S")
		b.WriteString(strings.Repeat("X", i%5+1))
		b.WriteString(string(rune('A' + i%26)))
		b.WriteString(string(rune('A' + i/26)))
		b.WriteString("\n")
	}
	got, _, err := Parse(strings.NewReader(b.String()))
	if err != nil || len(got) != MaxSymbols {
		t.Fatalf("want %d, got %d (%v)", MaxSymbols, len(got), err)
	}
}

func TestResolve(t *testing.T) {
	nse := []broker.Instrument{
		{InstrumentToken: 1, TradingSymbol: "TCS", Segment: "NSE", InstrumentType: "EQ"},
		{InstrumentToken: 2, TradingSymbol: "NIFTY IT", Segment: "INDICES"},
	}
	res, warns := Resolve([]Entry{{Symbol: "TCS", Benchmark: "NIFTY IT"}, {Symbol: "NOPE"}}, nse)
	if len(res) != 1 || res[0].Benchmark == nil || res[0].Benchmark.InstrumentToken != 2 || len(warns) != 1 {
		t.Fatalf("resolve: %+v %v", res, warns)
	}
}
