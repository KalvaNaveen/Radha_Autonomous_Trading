package indicators

import (
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestEMA(t *testing.T) {
	e := EMA([]float64{1, 2, 3, 4}, 3) // alpha 0.5, seed SMA(1,2,3)=2
	if e[1] != 0 || e[2] != 2 || e[3] != 3 {
		t.Fatalf("ema %v", e)
	}
}

func TestSMA(t *testing.T) {
	s := SMA([]float64{1, 2, 3, 4, 5}, 2)
	if s[0] != 0 || s[1] != 1.5 || s[4] != 4.5 {
		t.Fatalf("sma %v", s)
	}
}

func TestATRWilder(t *testing.T) {
	h := []float64{10, 11, 12, 13, 14}
	l := []float64{9, 10, 11, 12, 13}
	c := []float64{9.5, 10.5, 11.5, 12.5, 13.5}
	// TR from i=1: max(1, |11-9.5|=1.5, |10-9.5|=.5) = 1.5 each bar.
	a := ATR(h, l, c, 2)
	if !near(a[2], 1.5) || !near(a[4], 1.5) || a[1] != 0 {
		t.Fatalf("atr %v", a)
	}
}

func TestPriorHighest(t *testing.T) {
	p := PriorHighest([]float64{5, 7, 6, 9, 8}, 3)
	if p[2] != 0 || p[3] != 7 || p[4] != 9 {
		t.Fatalf("prior highest %v", p)
	}
}

func TestRoundToTick(t *testing.T) {
	if RoundToTick(100.03, 0.05, 1) != 100.05 || RoundToTick(100.03, 0.05, -1) != 100.00 {
		t.Fatal("round")
	}
}
