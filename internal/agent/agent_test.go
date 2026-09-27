package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nkalva/kitealgo/internal/broker"
	"github.com/nkalva/kitealgo/internal/clock"
	"github.com/nkalva/kitealgo/internal/config"
	"github.com/nkalva/kitealgo/internal/indicators"
	"github.com/nkalva/kitealgo/internal/ordermanager"
	"github.com/nkalva/kitealgo/internal/risk"
	"github.com/nkalva/kitealgo/pkg/models"
)

// ---------------------------------------------------------------------------
// Strategy unit tests
// ---------------------------------------------------------------------------

func strat() *Strategy { return NewStrategy(config.Defaults().Strategy) }

func TestLockTargets(t *testing.T) {
	s := strat()
	entry := 1000.0
	if st, _ := s.LockTarget(models.SideLong, entry, 0.0039); st != models.LockNone {
		t.Fatal("no lock below +0.40%")
	}
	st, px := s.LockTarget(models.SideLong, entry, 0.0040)
	if st != models.LockBreakeven || !approx(px, 1001.5) {
		t.Fatalf("breakeven: %v %v", st, px)
	}
	st, px = s.LockTarget(models.SideLong, entry, 0.0065)
	if st != models.LockProfit || !approx(px, 1004.0) { // 0.25% net + 0.15% costs
		t.Fatalf("profit lock: %v %v", st, px)
	}
	st, px = s.LockTarget(models.SideShort, entry, 0.0070)
	if st != models.LockProfit || !approx(px, 996.0) {
		t.Fatalf("short profit lock: %v %v", st, px)
	}
}

func TestInitialStopClamps(t *testing.T) {
	s := strat()
	if px := s.InitialStop(models.SideLong, 1000, 995); !approx(px, 995) { // 0.5% — inside band
		t.Fatalf("got %v", px)
	}
	if px := s.InitialStop(models.SideLong, 1000, 980); !approx(px, 992) { // 2% → capped 0.80%
		t.Fatalf("max clamp: %v", px)
	}
	if px := s.InitialStop(models.SideLong, 1000, 999.5); !approx(px, 997.5) { // 0.05% → floored 0.25%
		t.Fatalf("min clamp: %v", px)
	}
	if px := s.InitialStop(models.SideShort, 1000, 1004); !approx(px, 1004) {
		t.Fatalf("short: %v", px)
	}
	if px := s.InitialStop(models.SideLong, 1000, 1001); !approx(px, 992) { // EMA on wrong side → max
		t.Fatalf("wrong side: %v", px)
	}
}

func TestEntrySignalRules(t *testing.T) {
	s := strat()
	base := BarInput{Candle: models.Candle{Low: 100.10, Close: 100.50}, PrevFast: 100.0, PrevSlow: 100.04,
		Fast: 100.09, Slow: 100.08, VWAP: 100.0, RVOL: 3}
	if sig := s.OnBar(base); sig.Side != models.SideLong {
		t.Fatalf("expected long, got %q", sig.Reason)
	}
	cases := map[string]func(b *BarInput){
		"vwap buffer": func(b *BarInput) { b.VWAP = 100.4 },        // 100.5 < 100.4*1.002
		"rvol":        func(b *BarInput) { b.RVOL = 1.49 },         // thin volume
		"no pullback": func(b *BarInput) { b.Candle.Low = 100.40 }, // never touched band
		"no cross":    func(b *BarInput) { b.PrevFast = 100.05 },   // was already above
	}
	for name, mut := range cases {
		s := strat()
		b := base
		mut(&b)
		if sig := s.OnBar(b); sig.Side != models.SideNone {
			t.Errorf("%s: must not fire (%s)", name, sig.Reason)
		}
	}
	// Pullback on the bar after the cross is still valid (PullbackCandles=2)…
	s = strat()
	b := base
	b.Candle.Low = 100.40
	s.OnBar(b) // cross bar, no pullback
	b2 := BarInput{Candle: models.Candle{Low: 100.12, Close: 100.55}, PrevFast: 100.09, PrevSlow: 100.08,
		Fast: 100.18, Slow: 100.13, VWAP: 100.0, RVOL: 2}
	if sig := s.OnBar(b2); sig.Side != models.SideLong {
		t.Fatalf("pullback on bar 2 should fire: %s", sig.Reason)
	}
	// …but not on the bar after that.
	s = strat()
	s.OnBar(b)
	b3 := b2
	b3.Candle.Low = 100.9
	s.OnBar(b3)
	b4 := b2
	b4.PrevFast, b4.PrevSlow = b2.Fast, b2.Slow
	if sig := s.OnBar(b4); sig.Side != models.SideNone {
		t.Fatal("window must expire after PullbackCandles")
	}
}

func approx(a, b float64) bool { d := a - b; return d < 1e-6 && d > -1e-6 }

// ---------------------------------------------------------------------------
// End-to-end agent simulation: paper broker + real order manager + router
// ---------------------------------------------------------------------------

type simClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *simClock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *simClock) Set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

type harness struct {
	t      *testing.T
	clk    *simClock
	paper  *broker.Paper
	om     *ordermanager.OrderManager
	router *ordermanager.UpdateRouter
	risk   *risk.Manager
	agent  *Agent
	ticks  chan models.Tick
	cancel context.CancelFunc
	day    time.Time
	sess   *clock.Session
	jdir   string
}

const tok = uint32(101)

func testLogger() *slog.Logger {
	if os.Getenv("AGENT_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newHarness(t *testing.T) *harness {
	cfg := config.Defaults()
	cfg.Orders.StopRetryBackoff = 5 * time.Millisecond
	log := testLogger()
	day := time.Date(2026, 9, 28, 0, 0, 0, 0, clock.IST)
	sess, err := clock.NewSession(cfg.Session, nil)
	if err != nil {
		t.Fatal(err)
	}
	clk := &simClock{t: day.Add(9*time.Hour + 14*time.Minute)}
	paper := broker.NewPaper(1e7, clk.Now)
	om := ordermanager.New(paper, cfg.Orders, log, nil)
	router := ordermanager.NewUpdateRouter(log, nil)
	paper.SetUpdateHandler(router.Dispatch)
	jdir := t.TempDir()
	j, _ := risk.NewJournal(jdir, day)
	rm := risk.NewManager(cfg.Risk, cfg.Strategy.RoundTripCostPct, j, log, nil)

	prof := make([][]float64, 1)
	prof[0] = make([]float64, 375)
	for i := range prof[0] {
		prof[0][i] = 1000
	}
	seed := make([]float64, 0, 100)
	for i := 0; i < 80; i++ {
		seed = append(seed, 100.5)
	}
	for i := 0; i < 20; i++ {
		seed = append(seed, 100.0)
	}
	scfg := cfg.Strategy
	scfg.VWAPSource = "exchange"
	ticks := make(chan models.Tick, 1)
	a := New(Params{
		Instrument: broker.Instrument{InstrumentToken: tok, TradingSymbol: "TESTCO", Exchange: "NSE", TickSize: 0.05},
		Day:        day, Profile: indicators.BuildRVOLProfile(prof, 375), Seed: seed,
		Strategy: scfg, Orders: cfg.Orders,
	}, Deps{OM: om, Querier: paper, Risk: rm, Session: sess, Clock: clk, Log: log}, ticks)
	router.Register(tok, a.Updates())

	ctx, cancel := context.WithCancel(context.Background())
	go om.Run(ctx)
	go a.Run(ctx)
	return &harness{t: t, clk: clk, paper: paper, om: om, router: router, risk: rm, agent: a,
		ticks: ticks, cancel: cancel, day: day, sess: sess, jdir: jdir}
}

// waypoint is (minutes after 09:15, price).
type waypoint struct {
	min float64
	px  float64
}

// drive feeds ticks every 5s of market time, interpolating between waypoints,
// until minute `until`. hook runs after each tick.
func (h *harness) drive(wps []waypoint, until float64, hook func(min float64)) {
	open := h.day.Add(9*time.Hour + 15*time.Minute)
	for sec := 0.0; sec/60 <= until; sec += 5 {
		m := sec / 60
		px := wps[len(wps)-1].px
		for i := 0; i+1 < len(wps); i++ {
			if m >= wps[i].min && m <= wps[i+1].min {
				f := (m - wps[i].min) / (wps[i+1].min - wps[i].min)
				px = wps[i].px + f*(wps[i+1].px-wps[i].px)
				break
			}
		}
		px = indicators.RoundToTick(px, 0.05, 0)
		ts := open.Add(time.Duration(sec * float64(time.Second)))
		h.clk.Set(ts)
		tk := models.Tick{InstrumentToken: tok, LastPrice: px, BestBid: px - 0.05, BestAsk: px + 0.05,
			VolumeTraded: uint64(3000 * m), AverageTradePrice: 100.0, ExchangeTime: ts, ReceivedAt: ts, DayOpen: 100}
		h.paper.OnTick(tk)
		h.ticks <- tk
		time.Sleep(300 * time.Microsecond)
		if hook != nil {
			hook(m)
		}
	}
}

func (h *harness) waitState(want models.AgentState, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if h.agent.Snapshot().State == want.String() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func (h *harness) waitFlat(d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) && h.paper.NetQuantity(tok) != 0 {
		time.Sleep(10 * time.Millisecond)
	}
	h.settle()
}

func (h *harness) settle() { time.Sleep(400 * time.Millisecond) }

func (h *harness) liveOrders() int {
	ords, _ := h.paper.Orders(context.Background())
	n := 0
	for _, o := range ords {
		if o.IsLive() {
			n++
		}
	}
	return n
}

var winningPath = []waypoint{
	{0, 100.0}, {30, 100.0}, // warmup: flat
	{31, 100.10}, {34.8, 100.50}, // 09:45–09:50: pullback to band, close above → crossover bar
	{35.5, 100.55}, {50, 101.00}, // +0.4% → breakeven
	{65, 101.35},                // +0.7% → profit lock
	{85, 101.70}, {135, 101.75}, // trend, trail follows 10 EMA
	{140, 101.10}, {400, 101.10}, // 11:35: sharp drop → close below 10 EMA → exit
}

func TestAgentLongLifecycle(t *testing.T) {
	h := newHarness(t)
	defer h.cancel()

	sawWarmupEntry := false
	var sawTrailing, sawLock bool
	h.drive(winningPath, 355, func(m float64) {
		s := h.agent.Snapshot()
		if m < 30 && s.State != models.StateFlat.String() {
			sawWarmupEntry = true
		}
		if s.State == models.StateTrailing.String() {
			sawTrailing = true
		}
		if s.Position != nil && s.Position.Lock == models.LockProfit {
			sawLock = true
		}
	})
	h.settle()
	if sawWarmupEntry {
		t.Fatal("entered during 09:15–09:45 warmup")
	}
	if !sawTrailing || !sawLock {
		t.Fatalf("expected TRAILING with PROFIT_LOCK (trailing=%v lock=%v)", sawTrailing, sawLock)
	}
	sum := h.risk.Summary()
	if sum.Trades != 1 {
		t.Fatalf("want 1 trade, got %d", sum.Trades)
	}
	if sum.Realized <= 0 {
		t.Fatalf("trade should net positive after locking profit, realized=%v", sum.Realized)
	}
	if q := h.paper.NetQuantity(tok); q != 0 {
		t.Fatalf("must be flat at broker, net=%d", q)
	}
	if n := h.liveOrders(); n != 0 {
		t.Fatalf("no orders may remain live, got %d", n)
	}
	st := h.om.Stats()
	t.Logf("orders placed=%d modified=%d cancelled=%d realized=₹%.2f", st.Placed, st.Modified, st.Cancelled, sum.Realized)
}

func TestKillSwitchFlattens(t *testing.T) {
	h := newHarness(t)
	defer h.cancel()
	killed := false
	h.drive(winningPath, 70, func(m float64) {
		if !killed && h.agent.Snapshot().Position != nil && m > 60 {
			killed = true
			h.om.BlockEntries()
			h.agent.Kill("15:08 square-off")
		}
	})
	if !killed {
		t.Fatal("never got a position to kill")
	}
	if !h.waitState(models.StateHalted, 3*time.Second) {
		t.Fatalf("agent should be HALTED, is %s", h.agent.Snapshot().State)
	}
	if q := h.paper.NetQuantity(tok); q != 0 {
		t.Fatalf("kill must flatten, net=%d", q)
	}
	if n := h.liveOrders(); n != 0 {
		t.Fatalf("kill must leave no live orders, got %d", n)
	}
}

func TestNakedStopFailureFlattens(t *testing.T) {
	h := newHarness(t)
	defer h.cancel()
	// Stop placement fails twice (first try + the one retry) → must flatten.
	h.paper.InjectPlaceFault(models.OrderSLM, broker.OutcomeAmbiguous)
	h.paper.InjectPlaceFault(models.OrderSLM, broker.OutcomeAmbiguous)
	h.drive(winningPath, 40, nil)
	h.waitFlat(3 * time.Second)
	if q := h.paper.NetQuantity(tok); q != 0 {
		t.Fatalf("naked position must be flattened, net=%d", q)
	}
	sum := h.risk.Summary()
	if sum.Trades != 1 {
		t.Fatalf("want the entry+emergency exit booked as 1 trade, got %d", sum.Trades)
	}
}

func TestStopHitClosesPosition(t *testing.T) {
	h := newHarness(t)
	defer h.cancel()
	// Enter, then collapse through the initial stop before breakeven.
	path := []waypoint{{0, 100.0}, {30, 100.0}, {31, 100.10}, {34.8, 100.50}, {36, 100.55}, {38, 99.90}, {120, 99.90}}
	h.drive(path, 60, nil)
	h.settle()
	if q := h.paper.NetQuantity(tok); q != 0 {
		t.Fatalf("stop must have closed the position, net=%d", q)
	}
	sum := h.risk.Summary()
	if sum.Trades != 1 || sum.Realized >= 0 {
		t.Fatalf("want one losing trade, got trades=%d realized=%v", sum.Trades, sum.Realized)
	}
	// Loss must be bounded by the initial stop (max 0.80% + costs + slippage).
	if loss := -sum.Realized; loss > 0.012*100.6*float64(h.agentQty()) {
		t.Fatalf("loss %.2f exceeds stop bound", loss)
	}
}

func (h *harness) agentQty() int {
	ords, _ := h.paper.Orders(context.Background())
	for _, o := range ords {
		if o.OrderType == models.OrderLimit && o.FilledQuantity > 0 {
			return o.FilledQuantity
		}
	}
	return 1
}
