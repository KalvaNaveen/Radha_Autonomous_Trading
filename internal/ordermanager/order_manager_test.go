package ordermanager

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/nkalva/kitealgo/internal/broker"
	"github.com/nkalva/kitealgo/internal/config"
	"github.com/nkalva/kitealgo/pkg/models"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeClock is a manually advanced clock for deterministic limiter tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) Now() time.Time      { f.mu.Lock(); defer f.mu.Unlock(); return f.t }
func (f *fakeClock) Add(d time.Duration) { f.mu.Lock(); f.t = f.t.Add(d); f.mu.Unlock() }

func TestSlidingWindowNeverExceedsLimitInAnyWindow(t *testing.T) {
	fc := &fakeClock{t: time.Unix(0, 0)}
	rl := NewRateLimiter(fc.Now, Limit{Window: time.Second, Max: 8})
	var admitted []time.Time
	// Hammer it every 10ms for 5 seconds.
	for i := 0; i < 500; i++ {
		if ok, _ := rl.TryAcquire(); ok {
			admitted = append(admitted, fc.Now())
		}
		fc.Add(10 * time.Millisecond)
	}
	for i := range admitted {
		n := 0
		for j := i; j < len(admitted) && admitted[j].Sub(admitted[i]) < time.Second; j++ {
			n++
		}
		if n > 8 {
			t.Fatalf("window starting %v admitted %d > 8", admitted[i], n)
		}
	}
	if len(admitted) < 5*8-1 {
		t.Fatalf("limiter too conservative: %d admitted in 5s", len(admitted))
	}
}

func TestTokenBucketCounterExample(t *testing.T) {
	// Documents why we don't use a burst token bucket: burst 8 + refill 8/s
	// admits 16 inside one second. Our limiter admits 8.
	fc := &fakeClock{t: time.Unix(0, 0)}
	rl := NewRateLimiter(fc.Now, Limit{Window: time.Second, Max: 8})
	n := 0
	for i := 0; i < 100; i++ { // 0..990ms
		if ok, _ := rl.TryAcquire(); ok {
			n++
		}
		fc.Add(10 * time.Millisecond)
	}
	if n != 8 {
		t.Fatalf("want exactly 8 in first second, got %d", n)
	}
}

func TestMultiWindow(t *testing.T) {
	fc := &fakeClock{t: time.Unix(0, 0)}
	rl := NewRateLimiter(fc.Now, Limit{Window: time.Second, Max: 8}, Limit{Window: time.Minute, Max: 20})
	n := 0
	for i := 0; i < 6000; i++ { // 60s at 10ms
		if ok, _ := rl.TryAcquire(); ok {
			n++
		}
		fc.Add(10 * time.Millisecond)
	}
	if n != 20 {
		t.Fatalf("per-minute cap: want 20 got %d", n)
	}
}

func TestPriorityQueueOrdering(t *testing.T) {
	q := NewPriorityQueue()
	push := func(p models.Priority, id uint64) { q.Push(&models.OrderPayload{ID: id, Priority: p}) }
	push(models.PriorityEntry, 1)
	push(models.PriorityStop, 2)
	push(models.PriorityEntry, 3)
	push(models.PriorityEmergency, 4)
	push(models.PriorityStop, 5)
	var got []uint64
	for p := q.Pop(); p != nil; p = q.Pop() {
		got = append(got, p.ID)
	}
	want := []uint64{4, 2, 5, 1, 3}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order: got %v want %v", got, want)
		}
	}
}

func testCfg() config.OrdersConfig {
	c := config.Defaults().Orders
	c.StopRetryBackoff = 5 * time.Millisecond
	return c
}

func seedQuote(p *broker.Paper, tok uint32, px float64) {
	p.OnTick(models.Tick{InstrumentToken: tok, LastPrice: px, BestBid: px - 0.05, BestAsk: px + 0.05})
}

func TestOMThroughputRespectsOPS(t *testing.T) {
	paper := broker.NewPaper(1e7, nil)
	seedQuote(paper, 1, 100)
	om := New(paper, testCfg(), quiet, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go om.Run(ctx)

	const n = 24
	reply := make(chan models.OrderResult, n)
	var mu sync.Mutex
	var times []time.Time
	for i := 0; i < n; i++ {
		_ = om.Submit(&models.OrderPayload{Action: models.ActionPlace, Priority: models.PriorityStop, InstrumentToken: 1,
			Request: models.OrderRequest{InstrumentToken: 1, TradingSymbol: "X", TransactionType: models.TxnSell,
				OrderType: models.OrderSLM, TriggerPrice: 90, Quantity: 1, MarketProtection: -1}, Reply: reply})
	}
	for i := 0; i < n; i++ {
		r := <-reply
		if r.Err != nil {
			t.Fatalf("unexpected err %v", r.Err)
		}
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	if el := times[n-1].Sub(times[0]); el < 1900*time.Millisecond {
		t.Fatalf("24 orders at 8 OPS must take >= ~2s, took %v", el)
	}
}

func TestStopRetryThenFail(t *testing.T) {
	paper := broker.NewPaper(1e7, nil)
	seedQuote(paper, 1, 100)
	paper.InjectFault(models.ActionPlace, broker.OutcomeThrottled)
	paper.InjectFault(models.ActionPlace, broker.OutcomeThrottled)
	om := New(paper, testCfg(), quiet, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go om.Run(ctx)
	reply := make(chan models.OrderResult, 1)
	_ = om.Submit(&models.OrderPayload{Action: models.ActionPlace, Priority: models.PriorityStop, Purpose: models.PurposeStopPlace,
		InstrumentToken: 1, Request: models.OrderRequest{InstrumentToken: 1, TransactionType: models.TxnSell,
			OrderType: models.OrderSLM, TriggerPrice: 90, Quantity: 1, MarketProtection: -1}, Reply: reply})
	r := <-reply
	if r.Err == nil || r.Attempts != 2 {
		t.Fatalf("stop placement must retry exactly once then fail: %+v", r)
	}
}

func TestAmbiguousPlaceIsDeduplicated(t *testing.T) {
	paper := broker.NewPaper(1e7, nil)
	seedQuote(paper, 7, 100)
	paper.InjectActThenFail(models.ActionPlace)
	om := New(paper, testCfg(), quiet, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go om.Run(ctx)
	reply := make(chan models.OrderResult, 1)
	_ = om.Submit(&models.OrderPayload{Action: models.ActionPlace, Priority: models.PriorityEmergency, Purpose: models.PurposeExitPlace,
		InstrumentToken: 7, Request: models.OrderRequest{InstrumentToken: 7, TransactionType: models.TxnSell,
			OrderType: models.OrderMarket, Quantity: 10, MarketProtection: -1}, Reply: reply})
	r := <-reply
	if r.Err != nil || r.BrokerOrderID == "" {
		t.Fatalf("ambiguous place should resolve to the existing order: %+v", r)
	}
	if q := paper.NetQuantity(7); q != -10 {
		t.Fatalf("exit must execute exactly once, net qty %d", q)
	}
}

func TestKillSwitchDropsEntries(t *testing.T) {
	paper := broker.NewPaper(1e7, nil)
	om := New(paper, testCfg(), quiet, nil)
	reply := make(chan models.OrderResult, 2)
	_ = om.Submit(&models.OrderPayload{Action: models.ActionPlace, Priority: models.PriorityEntry, Reply: reply})
	om.BlockEntries()
	_ = om.Submit(&models.OrderPayload{Action: models.ActionPlace, Priority: models.PriorityEntry, Reply: reply})
	for i := 0; i < 2; i++ {
		if r := <-reply; r.Err != ErrEntriesBlocked {
			t.Fatalf("want ErrEntriesBlocked, got %v", r.Err)
		}
	}
}

func TestRouterDedupAndTerminalGuard(t *testing.T) {
	r := NewUpdateRouter(quiet, nil)
	ch := make(chan models.OrderUpdate, 10)
	r.Register(5, ch)
	tag := models.TagForToken(5)
	r.Dispatch(models.OrderUpdate{OrderID: "a", Tag: tag, Status: models.StatusOpen})
	r.Dispatch(models.OrderUpdate{OrderID: "a", Tag: tag, Status: models.StatusOpen})
	r.Dispatch(models.OrderUpdate{OrderID: "a", Tag: tag, Status: models.StatusComplete, FilledQuantity: 1})
	r.Dispatch(models.OrderUpdate{OrderID: "a", Tag: tag, Status: models.StatusOpen}) // stale poll
	r.Dispatch(models.OrderUpdate{OrderID: "b", Tag: "manual", Status: models.StatusOpen})
	if len(ch) != 2 {
		t.Fatalf("want 2 deliveries, got %d", len(ch))
	}
}
