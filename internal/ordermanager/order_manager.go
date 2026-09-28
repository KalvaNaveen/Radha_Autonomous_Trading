package ordermanager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nkalva/kitealgo/internal/broker"
	"github.com/nkalva/kitealgo/internal/config"
	"github.com/nkalva/kitealgo/pkg/models"
)

var (
	// ErrEntriesBlocked is returned for entries after the kill switch / loss halt.
	ErrEntriesBlocked = errors.New("ordermanager: new entries are blocked")
	// ErrDailyBudget is returned for entries when the remaining daily order
	// budget is reserved for exits.
	ErrDailyBudget = errors.New("ordermanager: daily order budget reserved for exits")
	// ErrStaleEntry is returned for entries that queued longer than EntryMaxAge.
	ErrStaleEntry = errors.New("ordermanager: entry went stale in the queue")
	// ErrStopped is returned when the manager shuts down with the request queued.
	ErrStopped = errors.New("ordermanager: stopped")
)

// opsGuard widens the 1-second window to absorb dispatch jitter.
const opsGuard = 150 * time.Millisecond

// Window indices in the limiter.
const (
	winSecond = iota
	winMinute
	winDay
)

// OrderManager serialises all order-changing traffic through one limiter.
type OrderManager struct {
	trader  broker.Trader
	cfg     config.OrdersConfig
	log     *slog.Logger
	limiter *RateLimiter
	queue   *PriorityQueue
	sem     chan struct{}

	seq            atomic.Uint64
	entriesBlocked atomic.Bool

	known   sync.Map // broker order IDs this engine created (for PLACE de-duplication)
	wg      sync.WaitGroup
	running atomic.Bool

	// counters
	placed, modified, cancelled, failed, retried, deduped atomic.Uint64
}

// New creates an order manager. now may be nil.
func New(trader broker.Trader, cfg config.OrdersConfig, log *slog.Logger, now func() time.Time) *OrderManager {
	// The per-second window carries a guard band. The limiter admits a request
	// at time T, but the HTTP request leaves a few ms later (goroutine start,
	// GC, TLS write) and that delay varies per request — so admissions spaced
	// exactly 1s apart can reach the broker less than 1s apart. The stress test
	// (50 agents entering on one bar) measured 10 broker calls inside one
	// second without the guard; with it the worst window is <= MaxOPS.
	lim := NewRateLimiter(now,
		Limit{Window: time.Second + opsGuard, Max: cfg.MaxOPS},
		Limit{Window: time.Minute, Max: cfg.MaxPerMinute},
		Limit{Window: 24 * time.Hour, Max: cfg.MaxPerDay},
	)
	return &OrderManager{
		trader: trader, cfg: cfg, log: log.With("component", "order_manager"),
		limiter: lim, queue: NewPriorityQueue(), sem: make(chan struct{}, cfg.Workers),
	}
}

// Limiter exposes the limiter (monitoring / tests).
func (m *OrderManager) Limiter() *RateLimiter { return m.limiter }

// Submit enqueues a payload. The result is delivered on p.Reply.
func (m *OrderManager) Submit(p *models.OrderPayload) error {
	if p.Reply == nil || cap(p.Reply) < 1 {
		return fmt.Errorf("ordermanager: payload reply channel must be buffered")
	}
	p.ID = m.seq.Add(1)
	p.SubmittedAt = time.Now()
	if p.Priority == models.PriorityEntry && m.entriesBlocked.Load() {
		m.reply(p, models.OrderResult{Err: ErrEntriesBlocked})
		return nil
	}
	if p.Request.Tag == "" {
		p.Request.Tag = models.TagForToken(p.InstrumentToken)
	}
	m.queue.Push(p)
	return nil
}

// BlockEntries rejects queued and future entries (kill switch / loss halt).
func (m *OrderManager) BlockEntries() {
	if m.entriesBlocked.Swap(true) {
		return
	}
	dropped := m.queue.Drain(models.PriorityEntry)
	for _, p := range dropped {
		m.reply(p, models.OrderResult{Err: ErrEntriesBlocked})
	}
	m.log.Warn("entries blocked", "dropped_queued_entries", len(dropped))
}

// EntriesBlocked reports the block flag.
func (m *OrderManager) EntriesBlocked() bool { return m.entriesBlocked.Load() }

func (m *OrderManager) reply(p *models.OrderPayload, r models.OrderResult) {
	r.PayloadID = p.ID
	r.Action = p.Action
	if r.BrokerOrderID == "" {
		r.BrokerOrderID = p.BrokerOrderID
	}
	if !p.SubmittedAt.IsZero() {
		r.Latency = time.Since(p.SubmittedAt)
	}
	select {
	case p.Reply <- r:
	default:
		m.log.Error("reply channel full — result dropped", "payload", p.ID, "action", p.Action)
	}
}

// Run dispatches queued payloads until ctx is done, then waits for in-flight calls.
func (m *OrderManager) Run(ctx context.Context) {
	m.running.Store(true)
	defer m.running.Store(false)
	for {
		select {
		case <-ctx.Done():
			m.wg.Wait()
			for p := m.queue.Pop(); p != nil; p = m.queue.Pop() {
				m.reply(p, models.OrderResult{Err: ErrStopped})
			}
			return
		case <-m.queue.Signal():
		}
	drain:
		for m.queue.Len() > 0 {
			// 1. a worker slot, 2. a rate-limit slot, 3. THEN pick the item, so
			// an emergency that arrives while we wait is served first.
			select {
			case m.sem <- struct{}{}:
			case <-ctx.Done():
				break drain
			}
			if err := m.limiter.Wait(ctx); err != nil {
				<-m.sem
				break drain
			}
			p := m.queue.Pop()
			if p == nil {
				<-m.sem
				break drain
			}
			if p.Priority == models.PriorityEntry {
				if m.entriesBlocked.Load() {
					<-m.sem
					m.reply(p, models.OrderResult{Err: ErrEntriesBlocked})
					continue
				}
				if m.cfg.EntryMaxAge > 0 && time.Since(p.SubmittedAt) > m.cfg.EntryMaxAge {
					<-m.sem
					m.reply(p, models.OrderResult{Err: ErrStaleEntry})
					continue
				}
				if m.limiter.Remaining(winDay) < m.cfg.EmergencyReserve {
					<-m.sem
					m.reply(p, models.OrderResult{Err: ErrDailyBudget})
					continue
				}
			}
			m.wg.Add(1)
			go func(p *models.OrderPayload) {
				defer m.wg.Done()
				defer func() { <-m.sem }()
				m.execute(ctx, p)
			}(p)
		}
	}
}

// maxAttempts by priority: emergency exits are the last line of defence and
// get several tries; stop moves get exactly one retry (spec: 100ms backoff);
// entries are never retried — a missed entry costs nothing.
func (m *OrderManager) maxAttempts(p *models.OrderPayload) int {
	if p.MaxAttempts > 0 {
		return p.MaxAttempts
	}
	switch p.Priority {
	case models.PriorityEmergency:
		return 1 + max(1, m.cfg.EmergencyRetries)
	case models.PriorityStop:
		return 2
	default:
		return 1
	}
}

// execute performs one payload with retries. The first attempt's rate-limit
// slot was acquired by Run; each retry acquires its own slot.
func (m *OrderManager) execute(ctx context.Context, p *models.OrderPayload) {
	// Use a context that survives engine shutdown for in-flight protective
	// calls: abandoning a stop placement half-way is worse than finishing it.
	callCtx := context.WithoutCancel(ctx)
	attempts := m.maxAttempts(p)
	backoff := m.cfg.RetryBackoff
	if backoff <= 0 {
		backoff = 100 * time.Millisecond
	}
	var lastErr error
	for i := 1; i <= attempts; i++ {
		if i > 1 {
			time.Sleep(backoff)
			backoff *= 2
			if err := m.limiter.Wait(callCtx); err != nil {
				lastErr = err
				break
			}
			m.retried.Add(1)
		}
		start := time.Now()
		id, err := m.call(callCtx, p)
		if err == nil {
			if p.Action == models.ActionPlace || p.Action == models.ActionGTTPlace {
				m.known.Store(id, struct{}{})
			}
			m.count(p.Action)
			m.log.Debug("order op ok", "action", p.Action, "prio", p.Priority,
				"symbol", p.Request.TradingSymbol, "order_id", id, "attempt", i, "rtt", time.Since(start))
			m.reply(p, models.OrderResult{BrokerOrderID: id, Attempts: i})
			return
		}
		lastErr = err
		outcome := broker.Classify(err)
		m.log.Warn("order op failed", "action", p.Action, "prio", p.Priority,
			"symbol", p.Request.TradingSymbol, "attempt", i, "outcome", outcome, "err", err)

		if outcome == broker.OutcomeAmbiguous && p.Action == models.ActionGTTPlace {
			if found, ok := m.findGTT(callCtx, p); ok {
				m.known.Store(found, struct{}{})
				m.deduped.Add(1)
				m.reply(p, models.OrderResult{BrokerOrderID: found, Attempts: i})
				return
			}
		}
		if outcome == broker.OutcomeAmbiguous && p.Action == models.ActionPlace {
			// The order may exist. Never blindly re-place: look for it first.
			if found, ok := m.findPlaced(callCtx, p, start); ok {
				m.known.Store(found, struct{}{})
				m.deduped.Add(1)
				m.count(p.Action)
				m.log.Warn("ambiguous PLACE resolved: order exists at broker", "order_id", found, "symbol", p.Request.TradingSymbol)
				m.reply(p, models.OrderResult{BrokerOrderID: found, Attempts: i})
				return
			}
		}
		if !broker.Retriable(err) {
			break
		}
	}
	m.failed.Add(1)
	m.reply(p, models.OrderResult{Err: lastErr, Attempts: attempts})
}

func (m *OrderManager) call(ctx context.Context, p *models.OrderPayload) (string, error) {
	switch p.Action {
	case models.ActionPlace:
		return m.trader.PlaceOrder(ctx, p.Request)
	case models.ActionModify:
		return p.BrokerOrderID, m.trader.ModifyOrder(ctx, p.BrokerOrderID, p.Request)
	case models.ActionCancel:
		return p.BrokerOrderID, m.trader.CancelOrder(ctx, p.BrokerOrderID)
	case models.ActionGTTPlace:
		return m.trader.PlaceGTT(ctx, p.Request)
	case models.ActionGTTModify:
		return p.BrokerOrderID, m.trader.ModifyGTT(ctx, p.BrokerOrderID, p.Request)
	case models.ActionGTTDelete:
		return p.BrokerOrderID, m.trader.DeleteGTT(ctx, p.BrokerOrderID)
	}
	return "", fmt.Errorf("unknown action %v", p.Action)
}

// findGTT looks for an active GTT matching an ambiguous GTT placement.
func (m *OrderManager) findGTT(ctx context.Context, p *models.OrderPayload) (string, bool) {
	time.Sleep(150 * time.Millisecond)
	gs, err := m.trader.GTTs(ctx)
	if err != nil {
		return "", false
	}
	for _, g := range gs {
		if g.Status != models.GTTActive || g.TradingSymbol != p.Request.TradingSymbol || g.Quantity != p.Request.Quantity {
			continue
		}
		if _, seen := m.known.Load(g.ID); seen {
			continue
		}
		if d := g.TriggerPrice - p.Request.TriggerPrice; d > 0.01 || d < -0.01 {
			continue
		}
		return g.ID, true
	}
	return "", false
}

// Do submits a payload and waits for its result (or ctx).
func (m *OrderManager) Do(ctx context.Context, p *models.OrderPayload) models.OrderResult {
	reply := make(chan models.OrderResult, 1)
	p.Reply = reply
	if err := m.Submit(p); err != nil {
		return models.OrderResult{Err: err}
	}
	select {
	case r := <-reply:
		return r
	case <-ctx.Done():
		return models.OrderResult{Err: ctx.Err()}
	}
}

// findPlaced searches the order book for an order matching p that this engine
// has not seen before and that was created around the failed attempt.
func (m *OrderManager) findPlaced(ctx context.Context, p *models.OrderPayload, since time.Time) (string, bool) {
	time.Sleep(150 * time.Millisecond) // let the OMS register it
	ords, err := m.trader.Orders(ctx)
	if err != nil {
		m.log.Error("dedupe lookup failed", "err", err)
		return "", false
	}
	r := p.Request
	for _, o := range ords {
		if o.Tag != r.Tag || o.TransactionType != r.TransactionType || o.Quantity != r.Quantity {
			continue
		}
		if _, seen := m.known.Load(o.OrderID); seen {
			continue
		}
		if !o.UpdatedAt.IsZero() && o.UpdatedAt.Before(since.Add(-5*time.Second)) {
			continue
		}
		return o.OrderID, true
	}
	return "", false
}

func (m *OrderManager) count(a models.OrderAction) {
	switch a {
	case models.ActionPlace:
		m.placed.Add(1)
	case models.ActionModify:
		m.modified.Add(1)
	case models.ActionCancel:
		m.cancelled.Add(1)
	}
}

// Stats is a monitoring snapshot.
type Stats struct {
	Placed, Modified, Cancelled, Failed, Retried, Deduped uint64
	Queued                                                int
	QueueHighWater                                        [models.NumPriorities]int
	RemainingSecond, RemainingMinute, RemainingDay        int
	EntriesBlocked                                        bool
}

// Stats returns counters.
func (m *OrderManager) Stats() Stats {
	return Stats{
		Placed: m.placed.Load(), Modified: m.modified.Load(), Cancelled: m.cancelled.Load(),
		Failed: m.failed.Load(), Retried: m.retried.Load(), Deduped: m.deduped.Load(),
		Queued: m.queue.Len(), QueueHighWater: m.queue.HighWater(),
		RemainingSecond: m.limiter.Remaining(winSecond), RemainingMinute: m.limiter.Remaining(winMinute),
		RemainingDay: m.limiter.Remaining(winDay), EntriesBlocked: m.entriesBlocked.Load(),
	}
}
