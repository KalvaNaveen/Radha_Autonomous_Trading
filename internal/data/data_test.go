package data

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nkalva/kitealgo/internal/broker"
	"github.com/nkalva/kitealgo/pkg/models"
)

func TestParseUniverse(t *testing.T) {
	syms, warns, err := ParseUniverse(strings.NewReader("# c\nsymbol\nNSE:INFY, tcs-EQ\tM&M BSE:X\nINFY\n12.5 bad$ \n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(syms, ",") != "INFY,TCS,M&M" || len(warns) != 2 {
		t.Fatalf("got %v %v", syms, warns)
	}
	back, _, _ := ParseUniverse(strings.NewReader(FormatUniverse(syms)))
	if strings.Join(back, ",") != "INFY,TCS,M&M" {
		t.Fatalf("round trip: %v", back)
	}
	if _, _, err := ParseUniverse(strings.NewReader("# nothing")); err == nil {
		t.Fatal("empty universe must be an error")
	}
}

// NSE index downloads: only the Symbol column counts; no 300-symbol cap.
func TestLoadListNSECSV(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nifty500.csv")
	var b strings.Builder
	b.WriteString("Company Name,Industry,Symbol,Series,ISIN Code\n")
	for i := 0; i < 400; i++ {
		b.WriteString("Some Co Ltd.,Capital Goods,SYM" + string(rune('A'+i%26)) + string(rune('A'+i/26)) + ",EQ,INE000A01010\n")
	}
	_ = os.WriteFile(p, []byte(b.String()), 0o644)
	syms, _, err := LoadList(p)
	if err != nil || len(syms) != 400 || syms[0] != "SYMAA" {
		t.Fatalf("%d symbols, first %v, err %v", len(syms), syms[:1], err)
	}
}

func TestAllEquities(t *testing.T) {
	ins := []broker.Instrument{
		{TradingSymbol: "INFY", Name: "INFOSYS", Segment: "NSE", InstrumentType: "EQ"},
		{TradingSymbol: "GOLDIAM", Name: "GOLDIAM INTERNATIONAL", Segment: "NSE", InstrumentType: "EQ"},
		{TradingSymbol: "NIFTYBEES", Name: "NIP IND ETF NIFTY BEES", Segment: "NSE", InstrumentType: "EQ"},
		{TradingSymbol: "SGBMAR29", Name: "SGB 2021-22 SR-X", Segment: "NSE", InstrumentType: "EQ"},
		{TradingSymbol: "ABC-BE", Name: "ABC", Segment: "NSE", InstrumentType: "EQ"},
		{TradingSymbol: "NIFTY 50", Name: "NIFTY 50", Segment: "INDICES", InstrumentType: "EQ"},
	}
	var got []string
	for _, x := range AllEquities(ins) {
		got = append(got, x.TradingSymbol)
	}
	if strings.Join(got, ",") != "INFY,GOLDIAM" {
		t.Fatalf("got %v", got)
	}
}

func TestLoadCaps(t *testing.T) {
	p := filepath.Join(t.TempDir(), "caps.csv")
	_ = os.WriteFile(p, []byte("symbol,cap\nKEI,mid\nnse:rba,Small\nX,huge\n"), 0o644)
	caps, warns, err := LoadCaps(p)
	if err != nil || caps["KEI"] != "mid" || caps["RBA"] != "small" || len(caps) != 2 || len(warns) != 1 {
		t.Fatalf("%v %v %v", caps, warns, err)
	}
	if c, _, err := LoadCaps(filepath.Join(t.TempDir(), "none.csv")); err != nil || len(c) != 0 {
		t.Fatal("a missing caps file is fine")
	}
}

type fakeMD struct {
	bars  []models.Bar
	calls [][2]time.Time
}

func (f *fakeMD) Instruments(context.Context, string) ([]broker.Instrument, error) { return nil, nil }
func (f *fakeMD) Quotes(context.Context, []string) (map[string]models.Quote, error) {
	return nil, nil
}
func (f *fakeMD) DailyCandles(_ context.Context, _ uint32, from, to time.Time) ([]models.Bar, error) {
	f.calls = append(f.calls, [2]time.Time{from, to})
	var out []models.Bar
	for _, b := range f.bars {
		if !b.Date.Before(from) && !b.Date.After(to) {
			out = append(out, b)
		}
	}
	return out, nil
}

// Candles are downloaded once, in chunks, then only the new days.
func TestDailyCacheIncremental(t *testing.T) {
	d0 := time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)
	md := &fakeMD{}
	for i := 0; i < 4000; i++ {
		md.bars = append(md.bars, models.Bar{Date: d0.AddDate(0, 0, i), Close: float64(i)})
	}
	s := NewStore(t.TempDir(), md, nil)
	to := d0.AddDate(0, 0, 3000)
	got, err := s.Daily(context.Background(), 7, d0, to)
	if err != nil || len(got) != 3001 || len(md.calls) != 2 {
		t.Fatalf("first load: %d bars, %d calls, %v", len(got), len(md.calls), err)
	}
	got, _ = s.Daily(context.Background(), 7, d0.AddDate(0, 0, 100), to)
	if len(md.calls) != 2 || len(got) != 2901 {
		t.Fatalf("cached range must not download: %d calls, %d bars", len(md.calls), len(got))
	}
	got, _ = s.Daily(context.Background(), 7, d0, to.AddDate(0, 0, 5))
	if len(md.calls) != 3 || len(got) != 3006 || !md.calls[2][0].Equal(to) {
		t.Fatalf("update must fetch only from the last bar: %d calls %v, %d bars", len(md.calls), md.calls, len(got))
	}
}
