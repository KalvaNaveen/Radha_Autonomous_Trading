package ordermanager

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nkalva/kitealgo/internal/broker"
	"github.com/nkalva/kitealgo/pkg/models"
)

// UpdateRouter delivers broker order updates to the agent that owns the
// instrument (identified by the order tag). Updates arrive from two sources —
// the WebSocket postback stream and the REST reconciliation poller — so the
// router de-duplicates by (order, status, filled qty, trigger, type).
//
// Order updates are never dropped: a lost fill is a lost position. Agent
// inboxes are sized generously and a full inbox blocks (and is logged).
type UpdateRouter struct {
	mu     sync.RWMutex
	subs   map[uint32]chan<- models.OrderUpdate
	seen   map[string]string
	log    *slog.Logger
	strays func(models.OrderUpdate)
}

// NewUpdateRouter creates a router. strays (optional) receives updates for
// tagged orders whose instrument has no agent.
func NewUpdateRouter(log *slog.Logger, strays func(models.OrderUpdate)) *UpdateRouter {
	return &UpdateRouter{subs: map[uint32]chan<- models.OrderUpdate{}, seen: map[string]string{},
		log: log.With("component", "update_router"), strays: strays}
}

// Register attaches an agent inbox for an instrument.
func (r *UpdateRouter) Register(token uint32, ch chan<- models.OrderUpdate) {
	r.mu.Lock()
	r.subs[token] = ch
	r.mu.Unlock()
}

func signature(u models.OrderUpdate) string {
	return fmt.Sprintf("%s|%d|%.2f|%s|%d", u.Status, u.FilledQuantity, u.TriggerPrice, u.OrderType, u.Quantity)
}

// Dispatch routes one update. Safe for concurrent use.
func (r *UpdateRouter) Dispatch(u models.OrderUpdate) {
	token, ok := models.TokenFromTag(u.Tag)
	if !ok {
		return // not ours (manual order in the same account)
	}
	sig := signature(u)
	r.mu.Lock()
	if r.seen[u.OrderID] == sig {
		r.mu.Unlock()
		return
	}
	// Never let a stale REST snapshot overwrite a terminal state we already delivered.
	if prev := r.seen[u.OrderID]; isTerminalSig(prev) && !u.IsTerminal() {
		r.mu.Unlock()
		return
	}
	r.seen[u.OrderID] = sig
	ch := r.subs[token]
	r.mu.Unlock()

	if ch == nil {
		if r.strays != nil {
			r.strays(u)
		}
		return
	}
	select {
	case ch <- u:
	default:
		r.log.Error("agent order inbox full — blocking until delivered", "token", token, "order", u.OrderID)
		ch <- u
	}
}

func isTerminalSig(sig string) bool {
	return strings.HasPrefix(sig, models.StatusComplete+"|") ||
		strings.HasPrefix(sig, models.StatusCancelled+"|") ||
		strings.HasPrefix(sig, models.StatusRejected+"|")
}

// Reconciler polls the order book so a missed WebSocket postback (reconnects,
// packet loss) can never leave an agent believing an order is still pending.
type Reconciler struct {
	trader   broker.Trader
	router   *UpdateRouter
	interval time.Duration
	log      *slog.Logger
}

// NewReconciler creates a poller.
func NewReconciler(trader broker.Trader, router *UpdateRouter, interval time.Duration, log *slog.Logger) *Reconciler {
	if interval <= 0 {
		interval = 3 * time.Second
	}
	return &Reconciler{trader: trader, router: router, interval: interval, log: log.With("component", "reconciler")}
}

// Run polls until ctx is done.
func (c *Reconciler) Run(ctx context.Context) {
	t := time.NewTicker(c.interval)
	defer t.Stop()
	fails := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := c.Once(ctx); err != nil {
			fails++
			if fails == 1 || fails%20 == 0 {
				c.log.Warn("order book poll failed", "err", err, "consecutive", fails)
			}
			continue
		}
		fails = 0
	}
}

// Once performs a single poll.
func (c *Reconciler) Once(ctx context.Context) error {
	ords, err := c.trader.Orders(ctx)
	if err != nil {
		return err
	}
	for _, o := range ords {
		c.router.Dispatch(o)
	}
	return nil
}
