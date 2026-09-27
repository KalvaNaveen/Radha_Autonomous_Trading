package broker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nkalva/kitealgo/pkg/models"
)

// Paper is a fill simulator driven by the live tick feed. It implements
// Trader with Kite-like semantics (IOC limits, SL-M triggers, modify-to-market,
// status transitions) so the full engine — rate limiter, reconciliation, agent
// state machines — runs unchanged without touching real money.
//
// Fills are deliberately pessimistic: marketable orders fill at the far side of
// the book (ask for buys, bid for sells) plus SlippageBps.
type Paper struct {
	mu          sync.Mutex
	seq         int
	orders      map[string]*paperOrder
	quotes      map[uint32]models.Tick
	netQty      map[uint32]int
	avgPx       map[uint32]float64
	symbols     map[uint32]string
	SlippageBps float64
	Margin      float64
	now         func() time.Time

	updates chan models.OrderUpdate
	handler func(models.OrderUpdate)
	hmu     sync.RWMutex

	faults     map[models.OrderAction][]ErrOutcome
	typeFaults map[models.OrderType][]ErrOutcome
	// OnSuccessFault, when set for an action, makes the call act at the broker
	// and still return an ambiguous error (to test PLACE de-duplication).
	actThenFail map[models.OrderAction]int
}

type paperOrder struct {
	u   models.OrderUpdate
	req models.OrderRequest
}

// NewPaper creates a paper broker. margin is the simulated available margin.
func NewPaper(margin float64, now func() time.Time) *Paper {
	if now == nil {
		now = time.Now
	}
	p := &Paper{
		orders: map[string]*paperOrder{}, quotes: map[uint32]models.Tick{},
		netQty: map[uint32]int{}, avgPx: map[uint32]float64{}, symbols: map[uint32]string{},
		SlippageBps: 2, Margin: margin, now: now,
		updates:     make(chan models.OrderUpdate, 4096),
		faults:      map[models.OrderAction][]ErrOutcome{},
		typeFaults:  map[models.OrderType][]ErrOutcome{},
		actThenFail: map[models.OrderAction]int{},
	}
	go p.dispatch()
	return p
}

// SetUpdateHandler registers the order-postback callback (like KiteTicker.OnOrderUpdate).
func (p *Paper) SetUpdateHandler(h func(models.OrderUpdate)) {
	p.hmu.Lock()
	p.handler = h
	p.hmu.Unlock()
}

func (p *Paper) dispatch() {
	for u := range p.updates {
		p.hmu.RLock()
		h := p.handler
		p.hmu.RUnlock()
		if h != nil {
			h(u)
		}
	}
}

// InjectFault makes the next call of the given action fail with outcome.
func (p *Paper) InjectFault(a models.OrderAction, o ErrOutcome) {
	p.mu.Lock()
	p.faults[a] = append(p.faults[a], o)
	p.mu.Unlock()
}

// InjectPlaceFault makes the next PLACE of the given order type fail.
func (p *Paper) InjectPlaceFault(t models.OrderType, o ErrOutcome) {
	p.mu.Lock()
	p.typeFaults[t] = append(p.typeFaults[t], o)
	p.mu.Unlock()
}

// InjectActThenFail makes the next call of action succeed at the broker but
// return an ambiguous error to the caller (simulates a timeout after acceptance).
func (p *Paper) InjectActThenFail(a models.OrderAction) {
	p.mu.Lock()
	p.actThenFail[a]++
	p.mu.Unlock()
}

func (p *Paper) takeFault(a models.OrderAction) error {
	if q := p.faults[a]; len(q) > 0 {
		p.faults[a] = q[1:]
		return &SimError{Outcome: q[0], Msg: fmt.Sprintf("paper: injected %s on %s", q[0], a)}
	}
	return nil
}

func (p *Paper) takeActThenFail(a models.OrderAction) bool {
	if p.actThenFail[a] > 0 {
		p.actThenFail[a]--
		return true
	}
	return false
}

// OnTick feeds a market-data tick; it may trigger or fill resting orders.
func (p *Paper) OnTick(t models.Tick) {
	p.mu.Lock()
	p.quotes[t.InstrumentToken] = t
	var out []models.OrderUpdate
	for _, o := range p.orders {
		if o.req.InstrumentToken != t.InstrumentToken || o.u.IsTerminal() {
			continue
		}
		if u, ok := p.tryFill(o, t); ok {
			out = append(out, u)
		}
	}
	p.mu.Unlock()
	for _, u := range out {
		p.updates <- u
	}
}

func (p *Paper) fillPrice(txn models.TransactionType, t models.Tick) float64 {
	px := t.LastPrice
	if txn == models.TxnBuy && t.BestAsk > 0 {
		px = t.BestAsk
	}
	if txn == models.TxnSell && t.BestBid > 0 {
		px = t.BestBid
	}
	slip := px * p.SlippageBps / 10000
	if txn == models.TxnBuy {
		return px + slip
	}
	return px - slip
}

// tryFill must be called with p.mu held.
func (p *Paper) tryFill(o *paperOrder, t models.Tick) (models.OrderUpdate, bool) {
	r := o.req
	ltp := t.LastPrice
	switch r.OrderType {
	case models.OrderMarket:
		return p.fill(o, p.fillPrice(r.TransactionType, t)), true
	case models.OrderLimit:
		px := p.fillPrice(r.TransactionType, t)
		if (r.TransactionType == models.TxnBuy && px <= r.Price) || (r.TransactionType == models.TxnSell && px >= r.Price) {
			return p.fill(o, px), true
		}
	case models.OrderSLM:
		if (r.TransactionType == models.TxnSell && ltp <= r.TriggerPrice) || (r.TransactionType == models.TxnBuy && ltp >= r.TriggerPrice) {
			return p.fill(o, p.fillPrice(r.TransactionType, t)), true
		}
	}
	return models.OrderUpdate{}, false
}

// fill must be called with p.mu held.
func (p *Paper) fill(o *paperOrder, px float64) models.OrderUpdate {
	q := o.req.Quantity
	tok := o.req.InstrumentToken
	sign := 1
	if o.req.TransactionType == models.TxnSell {
		sign = -1
	}
	prev := p.netQty[tok]
	next := prev + sign*q
	switch {
	case next == 0:
		p.avgPx[tok] = 0
	case prev == 0 || (prev > 0) != (next > 0):
		p.avgPx[tok] = px
	case (prev > 0) == (sign > 0):
		p.avgPx[tok] = (p.avgPx[tok]*float64(abs(prev)) + px*float64(q)) / float64(abs(next))
	}
	p.netQty[tok] = next
	o.u.Status = models.StatusComplete
	o.u.FilledQuantity = q
	o.u.PendingQuantity = 0
	o.u.AveragePrice = px
	o.u.UpdatedAt = p.now()
	return o.u
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// PlaceOrder implements Trader.
func (p *Paper) PlaceOrder(_ context.Context, req models.OrderRequest) (string, error) {
	p.mu.Lock()
	if err := p.takeFault(models.ActionPlace); err != nil {
		p.mu.Unlock()
		return "", err
	}
	if q := p.typeFaults[req.OrderType]; len(q) > 0 {
		p.typeFaults[req.OrderType] = q[1:]
		p.mu.Unlock()
		return "", &SimError{Outcome: q[0], Msg: fmt.Sprintf("paper: injected %s on PLACE %s", q[0], req.OrderType)}
	}
	if (req.OrderType == models.OrderMarket || req.OrderType == models.OrderSLM) && req.MarketProtection == 0 {
		p.mu.Unlock()
		return "", &SimError{Outcome: OutcomeRejected, Msg: "paper: market_protection required for MARKET/SL-M"}
	}
	t, ok := p.quotes[req.InstrumentToken]
	if !ok {
		p.mu.Unlock()
		return "", &SimError{Outcome: OutcomeRejected, Msg: "paper: no quote for instrument"}
	}
	if req.OrderType == models.OrderSLM {
		if (req.TransactionType == models.TxnSell && req.TriggerPrice >= t.LastPrice) ||
			(req.TransactionType == models.TxnBuy && req.TriggerPrice <= t.LastPrice) {
			p.mu.Unlock()
			return "", &SimError{Outcome: OutcomeRejected, Msg: "paper: trigger price on wrong side of LTP"}
		}
	}
	p.seq++
	id := fmt.Sprintf("P%08d", p.seq)
	p.symbols[req.InstrumentToken] = req.TradingSymbol
	o := &paperOrder{req: req, u: models.OrderUpdate{
		OrderID: id, InstrumentToken: req.InstrumentToken, TradingSymbol: req.TradingSymbol,
		TransactionType: req.TransactionType, OrderType: req.OrderType, Quantity: req.Quantity,
		PendingQuantity: req.Quantity, Price: req.Price, TriggerPrice: req.TriggerPrice, Tag: req.Tag,
		Status: models.StatusOpen, UpdatedAt: p.now(),
	}}
	if req.OrderType == models.OrderSLM {
		o.u.Status = models.StatusTriggerPending
	}
	p.orders[id] = o
	var out []models.OrderUpdate
	out = append(out, o.u)
	if u, ok := p.tryFill(o, t); ok {
		out = append(out, u)
	} else if req.Validity == models.ValidityIOC {
		o.u.Status = models.StatusCancelled
		o.u.StatusMessage = "IOC not filled"
		out = append(out, o.u)
	}
	failAfter := p.takeActThenFail(models.ActionPlace)
	p.mu.Unlock()
	for _, u := range out {
		p.updates <- u
	}
	if failAfter {
		return "", &SimError{Outcome: OutcomeAmbiguous, Msg: "paper: simulated timeout after acceptance"}
	}
	return id, nil
}

// ModifyOrder implements Trader.
func (p *Paper) ModifyOrder(_ context.Context, id string, req models.OrderRequest) error {
	p.mu.Lock()
	if err := p.takeFault(models.ActionModify); err != nil {
		p.mu.Unlock()
		return err
	}
	o, ok := p.orders[id]
	if !ok || o.u.IsTerminal() {
		p.mu.Unlock()
		return &SimError{Outcome: OutcomeRejected, Msg: "paper: order not open"}
	}
	if (req.OrderType == models.OrderMarket || req.OrderType == models.OrderSLM) && req.MarketProtection == 0 {
		p.mu.Unlock()
		return &SimError{Outcome: OutcomeRejected, Msg: "paper: market_protection required"}
	}
	t := p.quotes[o.req.InstrumentToken]
	if req.OrderType == models.OrderSLM {
		if (o.req.TransactionType == models.TxnSell && req.TriggerPrice >= t.LastPrice) ||
			(o.req.TransactionType == models.TxnBuy && req.TriggerPrice <= t.LastPrice) {
			p.mu.Unlock()
			return &SimError{Outcome: OutcomeRejected, Msg: "paper: trigger price on wrong side of LTP"}
		}
	}
	if req.OrderType != "" {
		o.req.OrderType = req.OrderType
		o.u.OrderType = req.OrderType
	}
	if req.Quantity > 0 {
		o.req.Quantity = req.Quantity
		o.u.Quantity = req.Quantity
		o.u.PendingQuantity = req.Quantity
	}
	o.req.Price, o.u.Price = req.Price, req.Price
	o.req.TriggerPrice, o.u.TriggerPrice = req.TriggerPrice, req.TriggerPrice
	o.req.MarketProtection = req.MarketProtection
	if o.req.OrderType == models.OrderSLM {
		o.u.Status = models.StatusTriggerPending
	} else {
		o.u.Status = models.StatusOpen
	}
	o.u.UpdatedAt = p.now()
	out := []models.OrderUpdate{o.u}
	if u, ok := p.tryFill(o, t); ok {
		out = append(out, u)
	}
	p.mu.Unlock()
	for _, u := range out {
		p.updates <- u
	}
	return nil
}

// CancelOrder implements Trader.
func (p *Paper) CancelOrder(_ context.Context, id string) error {
	p.mu.Lock()
	if err := p.takeFault(models.ActionCancel); err != nil {
		p.mu.Unlock()
		return err
	}
	o, ok := p.orders[id]
	if !ok || o.u.IsTerminal() {
		p.mu.Unlock()
		return &SimError{Outcome: OutcomeRejected, Msg: "paper: order not open"}
	}
	o.u.Status = models.StatusCancelled
	o.u.UpdatedAt = p.now()
	u := o.u
	p.mu.Unlock()
	p.updates <- u
	return nil
}

// OrderStatus implements Trader.
func (p *Paper) OrderStatus(_ context.Context, id string) (models.OrderUpdate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	o, ok := p.orders[id]
	if !ok {
		return models.OrderUpdate{}, &SimError{Outcome: OutcomeRejected, Msg: "paper: unknown order"}
	}
	return o.u, nil
}

// Orders implements Trader.
func (p *Paper) Orders(_ context.Context) ([]models.OrderUpdate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]models.OrderUpdate, 0, len(p.orders))
	for _, o := range p.orders {
		out = append(out, o.u)
	}
	return out, nil
}

// Positions implements Trader.
func (p *Paper) Positions(_ context.Context) ([]Position, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Position, 0, len(p.netQty))
	for tok, q := range p.netQty {
		out = append(out, Position{InstrumentToken: tok, TradingSymbol: p.symbols[tok], Exchange: "NSE",
			Product: "MIS", NetQuantity: q, AveragePrice: p.avgPx[tok], LastPrice: p.quotes[tok].LastPrice})
	}
	return out, nil
}

// AvailableMargin implements Trader.
func (p *Paper) AvailableMargin(_ context.Context) (float64, error) { return p.Margin, nil }

// NetQuantity returns the simulated net position (for tests).
func (p *Paper) NetQuantity(token uint32) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.netQty[token]
}
