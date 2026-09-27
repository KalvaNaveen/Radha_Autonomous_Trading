// Package ticker owns the single KiteTicker WebSocket and fans ticks out to
// per-instrument agent mailboxes.
package ticker

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"
	kitemodels "github.com/zerodha/gokiteconnect/v4/models"
	kiteticker "github.com/zerodha/gokiteconnect/v4/ticker"

	"github.com/nkalva/kitealgo/internal/broker"
	"github.com/nkalva/kitealgo/pkg/models"
)

// mailbox is one subscriber's bounded channel plus its drop counters.
type mailbox struct {
	ch      chan models.Tick
	dropped atomic.Uint64
	high    atomic.Int64
}

// Multiplexer fans ticks out with drop-oldest semantics.
//
// Why not unbuffered channels? With an unbuffered channel a single slow agent
// either stalls the WebSocket reader (blocking send — the whole feed lags and
// Kite eventually disconnects) or loses nearly every tick (non-blocking send).
// A small buffer with drop-OLDEST keeps the reader non-blocking and guarantees
// the agent always sees the freshest price. Nothing essential is lost on a
// drop: volume and VWAP are cumulative fields carried by every tick.
type Multiplexer struct {
	mu        sync.RWMutex
	subs      map[uint32]*mailbox
	taps      []func(models.Tick)
	log       *slog.Logger
	lastTick  atomic.Int64 // unix nanos of last received tick
	received  atomic.Uint64
	onOrder   func(models.OrderUpdate)
	connected atomic.Bool
}

// New creates an empty multiplexer.
func New(log *slog.Logger) *Multiplexer {
	return &Multiplexer{subs: map[uint32]*mailbox{}, log: log.With("component", "ticker")}
}

// Subscribe registers a mailbox for token and returns its receive side.
func (m *Multiplexer) Subscribe(token uint32, buffer int) <-chan models.Tick {
	if buffer < 1 {
		buffer = 1
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	mb, ok := m.subs[token]
	if !ok {
		mb = &mailbox{ch: make(chan models.Tick, buffer)}
		m.subs[token] = mb
	}
	return mb.ch
}

// Tokens returns all subscribed instrument tokens.
func (m *Multiplexer) Tokens() []uint32 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]uint32, 0, len(m.subs))
	for t := range m.subs {
		out = append(out, t)
	}
	return out
}

// AddTap registers a synchronous observer of every tick (e.g. the paper broker).
// Taps run on the reader goroutine and must be fast.
func (m *Multiplexer) AddTap(f func(models.Tick)) {
	m.mu.Lock()
	m.taps = append(m.taps, f)
	m.mu.Unlock()
}

// OnOrderUpdate registers the order postback handler.
func (m *Multiplexer) OnOrderUpdate(f func(models.OrderUpdate)) { m.onOrder = f }

// Publish routes one tick. Never blocks.
func (m *Multiplexer) Publish(t models.Tick) {
	m.received.Add(1)
	m.lastTick.Store(t.ReceivedAt.UnixNano())
	m.mu.RLock()
	mb := m.subs[t.InstrumentToken]
	taps := m.taps
	m.mu.RUnlock()
	for _, tap := range taps {
		tap(t)
	}
	if mb == nil {
		return
	}
	select {
	case mb.ch <- t:
	default:
		// Full: evict the oldest, then retry once.
		select {
		case <-mb.ch:
			mb.dropped.Add(1)
		default:
		}
		select {
		case mb.ch <- t:
		default:
			mb.dropped.Add(1)
		}
	}
	if n := int64(len(mb.ch)); n > mb.high.Load() {
		mb.high.Store(n)
	}
}

// LastTickAge is how long since any tick arrived (large = feed problem).
func (m *Multiplexer) LastTickAge(now time.Time) time.Duration {
	n := m.lastTick.Load()
	if n == 0 {
		return time.Duration(1<<63 - 1)
	}
	return now.Sub(time.Unix(0, n))
}

// Connected reports WebSocket state.
func (m *Multiplexer) Connected() bool { return m.connected.Load() }

// MailboxStats is buffer monitoring for one subscriber.
type MailboxStats struct {
	Token     uint32 `json:"token"`
	Depth     int    `json:"depth"`
	HighWater int64  `json:"high_water"`
	Dropped   uint64 `json:"dropped"`
}

// Stats returns per-mailbox buffer statistics.
func (m *Multiplexer) Stats() []MailboxStats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]MailboxStats, 0, len(m.subs))
	for tok, mb := range m.subs {
		out = append(out, MailboxStats{Token: tok, Depth: len(mb.ch), HighWater: mb.high.Load(), Dropped: mb.dropped.Load()})
	}
	return out
}

// Received returns the total tick count.
func (m *Multiplexer) Received() uint64 { return m.received.Load() }

// Convert maps a gokiteconnect tick to the engine model.
func Convert(k kitemodels.Tick, received time.Time) models.Tick {
	t := models.Tick{
		InstrumentToken:   k.InstrumentToken,
		LastPrice:         k.LastPrice,
		LastTradedQty:     k.LastTradedQuantity,
		VolumeTraded:      uint64(k.VolumeTraded),
		AverageTradePrice: k.AverageTradePrice,
		DayOpen:           k.OHLC.Open,
		ExchangeTime:      k.Timestamp.Time,
		ReceivedAt:        received,
		IsIndex:           k.IsIndex,
	}
	if k.Depth.Buy[0].Price > 0 {
		t.BestBid = k.Depth.Buy[0].Price
	}
	if k.Depth.Sell[0].Price > 0 {
		t.BestAsk = k.Depth.Sell[0].Price
	}
	return t
}

// Run connects the KiteTicker WebSocket and blocks until ctx is done.
// The SDK auto-reconnects with exponential backoff; on every (re)connect we
// resubscribe everything in full mode.
func (m *Multiplexer) Run(ctx context.Context, apiKey, accessToken string, maxDelay time.Duration) {
	kt := kiteticker.New(apiKey, accessToken)
	kt.SetAutoReconnect(true)
	if maxDelay > 0 {
		_ = kt.SetReconnectMaxDelay(maxDelay)
	}
	kt.SetReconnectMaxRetries(1000)

	kt.OnConnect(func() {
		m.connected.Store(true)
		toks := m.Tokens()
		if err := kt.Subscribe(toks); err != nil {
			m.log.Error("subscribe failed", "err", err)
			return
		}
		if err := kt.SetMode(kiteticker.ModeFull, toks); err != nil {
			m.log.Error("set mode failed", "err", err)
			return
		}
		m.log.Info("ticker connected", "instruments", len(toks))
	})
	kt.OnTick(func(k kitemodels.Tick) { m.Publish(Convert(k, time.Now())) })
	kt.OnOrderUpdate(func(o kiteconnect.Order) {
		if f := m.onOrder; f != nil {
			f(broker.FromKiteOrder(o))
		}
	})
	kt.OnError(func(err error) { m.log.Warn("ticker error", "err", err) })
	kt.OnClose(func(code int, reason string) {
		m.connected.Store(false)
		m.log.Warn("ticker closed", "code", code, "reason", reason)
	})
	kt.OnReconnect(func(attempt int, delay time.Duration) {
		m.log.Warn("ticker reconnecting", "attempt", attempt, "delay", delay)
	})
	kt.OnNoReconnect(func(attempt int) { m.log.Error("ticker gave up reconnecting", "attempts", attempt) })

	kt.ServeWithContext(ctx)
	m.connected.Store(false)
}
