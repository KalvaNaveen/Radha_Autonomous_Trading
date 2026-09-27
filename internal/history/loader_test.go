package history

import (
	"testing"
	"time"

	"github.com/nkalva/kitealgo/internal/broker"
	"github.com/nkalva/kitealgo/internal/clock"
)

func TestBuildProfileAndSeed(t *testing.T) {
	var bars []broker.HistCandle
	for d := 0; d < 12; d++ {
		day := time.Date(2026, 9, 1+d, 9, 15, 0, 0, clock.IST)
		if wd := day.Weekday(); wd == time.Saturday || wd == time.Sunday {
			continue
		}
		for m := 0; m < SessionMinutes; m++ {
			bars = append(bars, broker.HistCandle{Time: day.Add(time.Duration(m) * time.Minute), Close: 100 + float64(m)/100, Volume: 100})
		}
	}
	data := Build(1, bars, 5, 5*time.Minute)
	if data.Profile.Days != 5 {
		t.Fatalf("want 5 lookback days, got %d", data.Profile.Days)
	}
	if got := data.Profile.AvgCumulative[9]; got != 1000 { // 10 minutes × 100
		t.Fatalf("cum at 09:25: want 1000 got %v", got)
	}
	if len(data.Seed5m) != seedCloses {
		t.Fatalf("seed length %d", len(data.Seed5m))
	}
	// Last 5-minute close of a day is the 15:25–15:30 bar's final minute close.
	if last := data.Seed5m[len(data.Seed5m)-1]; last != 100+float64(SessionMinutes-1)/100 {
		t.Fatalf("last seed close %v", last)
	}
}
