package broker

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/nkalva/kitealgo/pkg/models"
)

// Paper simulates a delivery account: cash, holdings, orders and GTTs, filled
// against prices the engine feeds in (live Kite quotes in paper mode). Its
// state is kept in memory and snapshotted by the engine's portfolio store;
// it is recreated from the store on restart.
//
// Fills are pessimistic: buys at the ask, sells at the bid, plus SlippagePct.
type Paper struct {
	mu          sync.Mutex
	seq         int
	gseq        int
	cash        float64
	holdings    map[uint32]*Holding
	orders      map[string]*paperOrder
	gtts        map[string]*paperGTT
	quotes      map[uint32]models.Quote
	SlippagePct float64
	now         func() time.Time
	faults      map[models.OrderAction][]ErrOutcome
}

type paperOrder struct {
	u   models.OrderUpdate
	req models.OrderRequest
}

type paperGTT struct {
	info models.GTTInfo
	req  models.OrderRequest
}

// NewPaper creates a paper account with starting cash.
func NewPaper(cash float64, now func() time.Time) *Paper {
	if now == nil {
		now = time.Now
	}
	return &Paper{cash: cash, holdings: map[uint32]*Holding{}, orders: map[string]*paperOrder{},
		gtts: map[string]*paperGTT{}, quotes: map[uint32]models.Quote{}, SlippagePct: 0.1, now: now,
		faults: map[models.OrderAction][]ErrOutcome{}}
}

// Seed restores a holding (from the engine's persisted state).
func (p *Paper) Seed(token uint32, sym string, qty int, avg float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.holdings[token] = &Holding{InstrumentToken: token, TradingSymbol: sym, Quantity: qty, AveragePrice: avg}
}

// SetCash overrides the simulated cash balance.
func (p *Paper) SetCash(c float64) { p.mu.Lock(); p.cash = c; p.mu.Unlock() }

// InjectFault makes the next call of an action fail.
func (p *Paper) InjectFault(a models.OrderAction, o ErrOutcome) {
	p.mu.Lock()
	p.faults[a] = append(p.faults[a], o)
	p.mu.Unlock()
}

func (p *Paper) fault(a models.OrderAction) error {
	if q := p.faults[a]; len(q) > 0 {
		p.faults[a] = q[1:]
		return &SimError{Outcome: q[0], Msg: fmt.Sprintf("paper: injected %s on %s", q[0], a)}
	}
	return nil
}

// OnQuote feeds a price; it can fill resting orders and trigger GTTs.
func (p *Paper) OnQuote(q models.Quote) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.quotes[q.InstrumentToken] = q
	p.evaluateLocked(q.InstrumentToken)
}

// ApplyDailyBar replays a completed day for GTTs that a sparse quote poll may
// have missed (the machine was off, or the low happened between polls):
// if the day's low reached the trigger, the stop is treated as triggered —
// at the open if the stock gapped below it. This mirrors the backtester.
func (p *Paper) ApplyDailyBar(token uint32, b models.Bar) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, g := range p.gtts {
		if g.info.InstrumentToken != token || g.info.Status != models.GTTActive || b.Low > g.info.TriggerPrice {
			continue
		}
		px := g.info.TriggerPrice
		if b.Open < px {
			px = b.Open
		}
		g.info.Status = models.GTTTriggered
		id := p.newOrderLocked(g.req)
		o := p.orders[id]
		p.fillLocked(o, px*(1-p.SlippagePct/100))
		g.info.OrderID = id
	}
}

func (p *Paper) evaluateLocked(token uint32) {
	q, ok := p.quotes[token]
	if !ok {
		return
	}
	for _, g := range p.gtts {
		if g.info.InstrumentToken == token && g.info.Status == models.GTTActive && q.LastPrice <= g.info.TriggerPrice {
			g.info.Status = models.GTTTriggered
			id := p.newOrderLocked(g.req)
			g.info.OrderID = id
		}
	}
	for _, o := range p.orders {
		if o.req.InstrumentToken != token || o.u.IsTerminal() {
			continue
		}
		p.tryFillLocked(o, q)
	}
}

func (p *Paper) px(txn models.TransactionType, q models.Quote) float64 {
	px := q.LastPrice
	if txn == models.TxnBuy && q.BestAsk > 0 {
		px = q.BestAsk
	}
	if txn == models.TxnSell && q.BestBid > 0 {
		px = q.BestBid
	}
	s := px * p.SlippagePct / 100
	if txn == models.TxnBuy {
		return px + s
	}
	return px - s
}

func (p *Paper) tryFillLocked(o *paperOrder, q models.Quote) {
	px := p.px(o.req.TransactionType, q)
	switch o.req.OrderType {
	case models.OrderMarket:
		p.fillLocked(o, px)
	case models.OrderLimit:
		if (o.req.TransactionType == models.TxnBuy && px <= o.req.Price) || (o.req.TransactionType == models.TxnSell && px >= o.req.Price) {
			p.fillLocked(o, px)
		} else if o.req.Validity == models.ValidityIOC {
			o.u.Status, o.u.StatusMessage = models.StatusCancelled, "IOC not filled"
		}
	}
}

func (p *Paper) fillLocked(o *paperOrder, px float64) {
	q := o.req.Quantity
	tok := o.req.InstrumentToken
	h := p.holdings[tok]
	if o.req.TransactionType == models.TxnBuy {
		if h == nil {
			h = &Holding{InstrumentToken: tok, TradingSymbol: o.req.TradingSymbol}
			p.holdings[tok] = h
		}
		h.AveragePrice = (h.AveragePrice*float64(h.Quantity) + px*float64(q)) / float64(h.Quantity+q)
		h.Quantity += q
		p.cash -= px * float64(q)
	} else {
		if h == nil || h.Quantity < q {
			o.u.Status, o.u.StatusMessage = models.StatusRejected, "insufficient holdings"
			return
		}
		h.Quantity -= q
		p.cash += px * float64(q)
		if h.Quantity == 0 {
			delete(p.holdings, tok)
		}
	}
	o.u.Status, o.u.FilledQuantity, o.u.PendingQuantity, o.u.AveragePrice, o.u.UpdatedAt = models.StatusComplete, q, 0, px, p.now()
}

func (p *Paper) newOrderLocked(req models.OrderRequest) string {
	p.seq++
	id := fmt.Sprintf("P%08d", p.seq)
	p.orders[id] = &paperOrder{req: req, u: models.OrderUpdate{OrderID: id, InstrumentToken: req.InstrumentToken,
		TradingSymbol: req.TradingSymbol, Status: models.StatusOpen, TransactionType: req.TransactionType,
		OrderType: req.OrderType, Product: models.ProductCNC, Quantity: req.Quantity, PendingQuantity: req.Quantity,
		Price: req.Price, Tag: req.Tag, UpdatedAt: p.now()}}
	return id
}

// PlaceOrder implements Trader.
func (p *Paper) PlaceOrder(_ context.Context, req models.OrderRequest) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.fault(models.ActionPlace); err != nil {
		return "", err
	}
	if req.OrderType == models.OrderMarket && req.MarketProtection == 0 {
		return "", &SimError{Outcome: OutcomeRejected, Msg: "paper: market_protection required for MARKET"}
	}
	q, ok := p.quotes[req.InstrumentToken]
	if !ok {
		return "", &SimError{Outcome: OutcomeRejected, Msg: "paper: no quote for " + req.TradingSymbol}
	}
	if req.TransactionType == models.TxnBuy && p.px(models.TxnBuy, q)*float64(req.Quantity) > p.cash {
		return "", &SimError{Outcome: OutcomeRejected, Msg: "paper: insufficient funds"}
	}
	if req.TransactionType == models.TxnSell {
		if h := p.holdings[req.InstrumentToken]; h == nil || h.Quantity < req.Quantity {
			return "", &SimError{Outcome: OutcomeRejected, Msg: "paper: insufficient holdings to sell"}
		}
	}
	id := p.newOrderLocked(req)
	p.tryFillLocked(p.orders[id], q)
	return id, nil
}

// ModifyOrder implements Trader.
func (p *Paper) ModifyOrder(_ context.Context, id string, req models.OrderRequest) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	o, ok := p.orders[id]
	if !ok || o.u.IsTerminal() {
		return &SimError{Outcome: OutcomeRejected, Msg: "paper: order not open"}
	}
	if req.OrderType != "" {
		o.req.OrderType = req.OrderType
	}
	if req.Price > 0 {
		o.req.Price = req.Price
	}
	if q, ok := p.quotes[o.req.InstrumentToken]; ok {
		p.tryFillLocked(o, q)
	}
	return nil
}

// CancelOrder implements Trader.
func (p *Paper) CancelOrder(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	o, ok := p.orders[id]
	if !ok || o.u.IsTerminal() {
		return &SimError{Outcome: OutcomeRejected, Msg: "paper: order not open"}
	}
	o.u.Status = models.StatusCancelled
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
	sort.Slice(out, func(i, j int) bool { return out[i].OrderID < out[j].OrderID })
	return out, nil
}

// PlaceGTT implements Trader.
func (p *Paper) PlaceGTT(_ context.Context, req models.OrderRequest) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.fault(models.ActionGTTPlace); err != nil {
		return "", err
	}
	if q, ok := p.quotes[req.InstrumentToken]; ok && req.TransactionType == models.TxnSell && req.TriggerPrice >= q.LastPrice {
		return "", &SimError{Outcome: OutcomeRejected, Msg: "paper: sell GTT trigger must be below LTP"}
	}
	p.gseq++
	id := strconv.Itoa(100000 + p.gseq)
	req.OrderType = models.OrderLimit
	p.gtts[id] = &paperGTT{req: req, info: models.GTTInfo{ID: id, InstrumentToken: req.InstrumentToken,
		TradingSymbol: req.TradingSymbol, Status: models.GTTActive, TriggerPrice: req.TriggerPrice,
		LimitPrice: req.Price, Quantity: req.Quantity}}
	return id, nil
}

// ModifyGTT implements Trader.
func (p *Paper) ModifyGTT(_ context.Context, id string, req models.OrderRequest) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.fault(models.ActionGTTModify); err != nil {
		return err
	}
	g, ok := p.gtts[id]
	if !ok || g.info.Status != models.GTTActive {
		return &SimError{Outcome: OutcomeRejected, Msg: "paper: GTT not active"}
	}
	if q, ok := p.quotes[g.req.InstrumentToken]; ok && req.TriggerPrice >= q.LastPrice {
		return &SimError{Outcome: OutcomeRejected, Msg: "paper: sell GTT trigger must be below LTP"}
	}
	g.req.TriggerPrice, g.req.Price, g.req.Quantity = req.TriggerPrice, req.Price, req.Quantity
	g.info.TriggerPrice, g.info.LimitPrice, g.info.Quantity = req.TriggerPrice, req.Price, req.Quantity
	return nil
}

// DeleteGTT implements Trader.
func (p *Paper) DeleteGTT(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	g, ok := p.gtts[id]
	if !ok || g.info.Status != models.GTTActive {
		return &SimError{Outcome: OutcomeRejected, Msg: "paper: GTT not active"}
	}
	g.info.Status = models.GTTDeleted
	return nil
}

// GTTs implements Trader.
func (p *Paper) GTTs(_ context.Context) ([]models.GTTInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]models.GTTInfo, 0, len(p.gtts))
	for _, g := range p.gtts {
		out = append(out, g.info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// RestoreGTT recreates an active GTT from persisted state after a restart.
func (p *Paper) RestoreGTT(id string, req models.OrderRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	req.OrderType = models.OrderLimit
	p.gtts[id] = &paperGTT{req: req, info: models.GTTInfo{ID: id, InstrumentToken: req.InstrumentToken,
		TradingSymbol: req.TradingSymbol, Status: models.GTTActive, TriggerPrice: req.TriggerPrice,
		LimitPrice: req.Price, Quantity: req.Quantity}}
	if n, err := strconv.Atoi(id); err == nil && n-100000 > p.gseq {
		p.gseq = n - 100000
	}
}

// Holdings implements Trader.
func (p *Paper) Holdings(_ context.Context) ([]Holding, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Holding, 0, len(p.holdings))
	for _, h := range p.holdings {
		c := *h
		if q, ok := p.quotes[h.InstrumentToken]; ok {
			c.LastPrice = q.LastPrice
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TradingSymbol < out[j].TradingSymbol })
	return out, nil
}

// AvailableCash implements Trader.
func (p *Paper) AvailableCash(_ context.Context) (float64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cash, nil
}
