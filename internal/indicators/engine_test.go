package indicators

import (
	"math"
	"testing"
	"time"

	"github.com/nkalva/kitealgo/pkg/models"
)

var ist = time.FixedZone("IST", 19800)

func near(a, b, eps float64) bool { return math.Abs(a-b) <= eps }

func TestEMASeedAndRecurrence(t *testing.T) {
	e := NewEMA(3) // alpha = 0.5
	if e.Update(1) != 0 || e.Update(2) != 0 {
		t.Fatal("EMA must report 0 until N values")
	}
	if v := e.Update(3); v != 2 { // SMA seed
		t.Fatalf("seed: want 2 got %v", v)
	}
	if v := e.Update(4); v != 3 { // 0.5*4 + 0.5*2
		t.Fatalf("recurrence: want 3 got %v", v)
	}
	if e.Prev() != 2 {
		t.Fatalf("prev: want 2 got %v", e.Prev())
	}
	if p := e.Peek(6); p != 4.5 {
		t.Fatalf("peek: want 4.5 got %v", p)
	}
	if e.Value() != 3 {
		t.Fatal("peek must not mutate")
	}
}

func TestVWAPComputed(t *testing.T) {
	var w VWAP
	w.Update(100, 0, 0)  // baseline, no volume yet
	w.Update(100, 10, 0) // 10 @ 100
	w.Update(110, 30, 0) // 20 @ 110
	w.Update(110, 30, 0) // no new volume
	want := (100*10 + 110*20) / 30.0
	if !near(w.Computed(), want, 1e-9) {
		t.Fatalf("vwap: want %v got %v", want, w.Computed())
	}
	w.Update(111, 40, 105.5)
	if w.Value(true) != 105.5 {
		t.Fatal("exchange ATP should win when preferred")
	}
	if w.Value(false) == 105.5 {
		t.Fatal("computed must be used when not preferred")
	}
}

func TestRVOLInterpolation(t *testing.T) {
	// Two days, 3 session minutes: minute volumes 100,100,100 and 300,300,300.
	p := BuildRVOLProfile([][]float64{{100, 100, 100}, {300, 300, 300}}, 3)
	if p.AvgCumulative[0] != 200 || p.AvgCumulative[2] != 600 {
		t.Fatalf("profile wrong: %v", p.AvgCumulative)
	}
	// 1.5 minutes in: baseline = 200 + (400-200)*0.5 = 300
	if b := p.BaselineAt(90 * time.Second); !near(b, 300, 1e-9) {
		t.Fatalf("baseline: want 300 got %v", b)
	}
	if r := p.RVOL(90*time.Second, 450); !near(r, 1.5, 1e-9) {
		t.Fatalf("rvol: want 1.5 got %v", r)
	}
	if p.RVOL(0, 1000) != 0 {
		t.Fatal("rvol at t=0 must be 0 (no baseline)")
	}
}

func tick(ts time.Time, px float64, cum uint64) models.Tick {
	return models.Tick{InstrumentToken: 1, LastPrice: px, VolumeTraded: cum, ExchangeTime: ts}
}

func TestCandleBuilder(t *testing.T) {
	open := time.Date(2026, 9, 28, 9, 15, 0, 0, ist)
	b := NewCandleBuilder(1, 5*time.Minute, 2*time.Second, open)

	if out := b.Update(tick(open.Add(-time.Minute), 99, 500)); out != nil {
		t.Fatal("pre-open tick must not create a bar")
	}
	b.Update(tick(open.Add(10*time.Second), 100, 600))
	b.Update(tick(open.Add(60*time.Second), 103, 700))
	b.Update(tick(open.Add(120*time.Second), 98, 800))
	b.Update(tick(open.Add(290*time.Second), 101, 900))
	out := b.Update(tick(open.Add(301*time.Second), 102, 950))
	if len(out) != 1 {
		t.Fatalf("want 1 closed bar, got %d", len(out))
	}
	c := out[0]
	if c.Open != 100 || c.High != 103 || c.Low != 98 || c.Close != 101 || c.Volume != 400 {
		t.Fatalf("bad bar %v", c)
	}
	// Skip 10 minutes: 09:20 bar closes, then two flat bars (09:25, 09:30).
	out = b.Update(tick(open.Add(20*time.Minute+time.Second), 104, 1000))
	if len(out) != 3 || out[1].Volume != 0 || out[1].Close != 102 || out[2].Start != open.Add(15*time.Minute) {
		t.Fatalf("gap fill wrong: %v", out)
	}
	// Timer flush after 09:40 + grace.
	if out := b.Flush(open.Add(25*time.Minute + time.Second)); out != nil {
		t.Fatal("flush inside grace must not close")
	}
	out = b.Flush(open.Add(25*time.Minute + 3*time.Second))
	if len(out) != 1 || out[0].Close != 104 {
		t.Fatalf("flush wrong: %v", out)
	}
	// Late tick for the flushed bar: volume carried, no bar emitted.
	if out := b.Update(tick(open.Add(24*time.Minute+59*time.Second), 105, 1010)); out != nil {
		t.Fatal("late tick must not emit")
	}
	b.Update(tick(open.Add(25*time.Minute+5*time.Second), 106, 1020))
	cur, ok := b.Current()
	if !ok || cur.Volume != 20 || cur.Open != 106 {
		t.Fatalf("carry volume wrong: %+v", cur)
	}
}

func TestRoundToTick(t *testing.T) {
	cases := []struct {
		p, tick float64
		dir     int
		want    float64
	}{
		{100.03, 0.05, 1, 100.05}, {100.03, 0.05, -1, 100.00}, {100.03, 0.05, 0, 100.05},
		{100.05, 0.05, 1, 100.05}, {250.123, 0.01, -1, 250.12},
	}
	for _, c := range cases {
		if got := RoundToTick(c.p, c.tick, c.dir); got != c.want {
			t.Errorf("RoundToTick(%v,%v,%d) = %v want %v", c.p, c.tick, c.dir, got, c.want)
		}
	}
}
