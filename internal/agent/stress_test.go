package agent

import (
	"context"
	"sort"
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

// recordingTrader timestamps every order-changing call that reaches the broker.
type recordingTrader struct {
	*broker.Paper
	mu    sync.Mutex
	calls []time.Time
}

func (r *recordingTrader) rec() { r.mu.Lock(); r.calls = append(r.calls, time.Now()); r.mu.Unlock() }
func (r *recordingTrader) PlaceOrder(ctx context.Context, q models.OrderRequest) (string, error) {
	r.rec()
	return r.Paper.PlaceOrder(ctx, q)
}
func (r *recordingTrader) ModifyOrder(ctx context.Context, id string, q models.OrderRequest) error {
	r.rec()
	return r.Paper.ModifyOrder(ctx, id, q)
}
func (r *recordingTrader) CancelOrder(ctx context.Context, id string) error {
	r.rec()
	return r.Paper.CancelOrder(ctx, id)
}

// Fifty agents signal on the same bar, trail, then get killed together.
// Asserts: the broker never sees more than 8 order calls in any 1s window,
// every position is flattened by the kill switch, and nothing is left live.
func TestFiftyAgentsUnderOPSCapAndKill(t *testing.T) {
	const n = 50
	cfg := config.Defaults()
	cfg.Risk.MaxOpenPositions = n
	cfg.Risk.Capital = 5_000_000
	log := testLogger()
	day := time.Date(2026, 9, 28, 0, 0, 0, 0, clock.IST)
	sess, _ := clock.NewSession(cfg.Session, nil)
	clk := &simClock{t: day.Add(9*time.Hour + 14*time.Minute)}
	paper := broker.NewPaper(1e9, clk.Now)
	rt := &recordingTrader{Paper: paper}
	om := ordermanager.New(rt, cfg.Orders, log, nil)
	router := ordermanager.NewUpdateRouter(log, nil)
	paper.SetUpdateHandler(router.Dispatch)
	rm := risk.NewManager(cfg.Risk, cfg.Strategy.RoundTripCostPct, nil, log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go om.Run(ctx)

	prof := [][]float64{make([]float64, 375)}
	for i := range prof[0] {
		prof[0][i] = 1000
	}
	seed := make([]float64, 0, 100)
	for i := 0; i < 100; i++ {
		if i < 80 {
			seed = append(seed, 100.5)
		} else {
			seed = append(seed, 100.0)
		}
	}
	agents := make([]*Agent, n)
	chans := make([]chan models.Tick, n)
	for i := 0; i < n; i++ {
		tk := uint32(1000 + i)
		chans[i] = make(chan models.Tick, 1)
		agents[i] = New(Params{
			Instrument: broker.Instrument{InstrumentToken: tk, TradingSymbol: "S", Exchange: "NSE", TickSize: 0.05},
			Day:        day, Profile: indicators.BuildRVOLProfile(prof, 375), Seed: seed,
			Strategy: cfg.Strategy, Orders: cfg.Orders,
		}, Deps{OM: om, Querier: paper, Risk: rm, Session: sess, Clock: clk, Log: log}, chans[i])
		router.Register(tk, agents[i].Updates())
		go agents[i].Run(ctx)
	}

	open := day.Add(9*time.Hour + 15*time.Minute)
	wps := winningPath
	for sec := 0.0; sec/60 <= 72; sec += 5 {
		m := sec / 60
		px := wps[len(wps)-1].px
		for i := 0; i+1 < len(wps); i++ {
			if m >= wps[i].min && m <= wps[i+1].min {
				px = wps[i].px + (m-wps[i].min)/(wps[i+1].min-wps[i].min)*(wps[i+1].px-wps[i].px)
				break
			}
		}
		px = indicators.RoundToTick(px, 0.05, 0)
		ts := open.Add(time.Duration(sec * float64(time.Second)))
		clk.Set(ts)
		for i := 0; i < n; i++ {
			tk := models.Tick{InstrumentToken: uint32(1000 + i), LastPrice: px, BestBid: px - 0.05, BestAsk: px + 0.05,
				VolumeTraded: uint64(3000 * m), AverageTradePrice: 100, ExchangeTime: ts, ReceivedAt: ts}
			paper.OnTick(tk)
			chans[i] <- tk
		}
		// Give the order manager real time to drain at 8 OPS around the signal bar.
		time.Sleep(2 * time.Millisecond)
		for om.Stats().Queued > 0 { // let the 8-OPS pipe drain before the next tick
			time.Sleep(20 * time.Millisecond)
		}
	}
	time.Sleep(2 * time.Second)
	open0 := rm.OpenCount()
	om.BlockEntries()
	for _, a := range agents {
		a.Kill("square-off")
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && rm.OpenCount() > 0 {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(time.Second)

	if open0 == 0 {
		t.Fatal("expected open positions before the kill")
	}
	for i := 0; i < n; i++ {
		if q := paper.NetQuantity(uint32(1000 + i)); q != 0 {
			t.Fatalf("agent %d not flat after kill: %d", i, q)
		}
	}
	ords, _ := paper.Orders(context.Background())
	for _, o := range ords {
		if o.IsLive() {
			t.Fatalf("live order left: %+v", o)
		}
	}
	rt.mu.Lock()
	calls := append([]time.Time(nil), rt.calls...)
	rt.mu.Unlock()
	sort.Slice(calls, func(i, j int) bool { return calls[i].Before(calls[j]) })
	worst := 0
	for i := range calls {
		k := i
		for k < len(calls) && calls[k].Sub(calls[i]) < time.Second {
			k++
		}
		if k-i > worst {
			worst = k - i
		}
	}
	t.Logf("positions before kill=%d, trades=%d, broker order calls=%d, worst 1s window=%d", open0, rm.Summary().Trades, len(calls), worst)
	if worst > cfg.Orders.MaxOPS {
		t.Fatalf("OPS cap violated: %d calls in one second", worst)
	}
}
