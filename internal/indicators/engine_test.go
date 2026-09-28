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

func TestHeikinAshi(t *testing.T) {
	o := []float64{10, 11}
	h := []float64{12, 13}
	l := []float64{9, 10}
	c := []float64{11, 12}
	hO, hH, hL, hC := HeikinAshi(o, h, l, c)
	// bar0: close (10+12+9+11)/4=10.5, open (10+11)/2=10.5
	// bar1: close (11+13+10+12)/4=11.5, open (10.5+10.5)/2=10.5
	if !near(hC[0], 10.5) || !near(hO[0], 10.5) || !near(hC[1], 11.5) || !near(hO[1], 10.5) || hH[1] != 13 || !near(hL[1], 10) {
		t.Fatalf("ha %v %v %v %v", hO, hH, hL, hC)
	}
}

func TestSupertrendFlips(t *testing.T) {
	var h, l, c []float64
	for i := 0; i < 40; i++ { // rising
		x := 100 + float64(i)
		h, l, c = append(h, x+1), append(l, x-1), append(c, x)
	}
	for i := 0; i < 40; i++ { // falling hard
		x := 140 - 3*float64(i)
		h, l, c = append(h, x+1), append(l, x-1), append(c, x)
	}
	dir, line := Supertrend(h, l, c, 10, 3)
	if dir[35] != 1 || line[35] >= c[35] {
		t.Fatalf("uptrend: dir %d line %.2f close %.2f", dir[35], line[35], c[35])
	}
	if dir[79] != -1 || line[79] <= c[79] {
		t.Fatalf("downtrend: dir %d line %.2f close %.2f", dir[79], line[79], c[79])
	}
	for i := 11; i < 40; i++ { // the green line never falls in an uptrend
		if dir[i] == 1 && dir[i-1] == 1 && line[i] < line[i-1]-1e-9 {
			t.Fatalf("green line fell at %d", i)
		}
	}
}
