package broker

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"

	"github.com/nkalva/kitealgo/pkg/models"
)

// Kite implements Trader and MarketData on Kite Connect v3 (gokiteconnect v4).
type Kite struct {
	client           *kiteconnect.Client
	marketProtection float64
}

// NewKite builds a Kite client whose REST traffic originates from the static IP.
func NewKite(apiKey, accessToken string, httpClient *http.Client, marketProtection float64, apiRoot string) *Kite {
	c := kiteconnect.New(apiKey)
	if apiRoot != "" {
		c.SetBaseURI(apiRoot)
	}
	if httpClient != nil {
		c.SetHTTPClient(httpClient)
	}
	c.SetAccessToken(accessToken)
	c.SetAppName("radha-swing")
	return &Kite{client: c, marketProtection: marketProtection}
}

// Client exposes the SDK client.
func (k *Kite) Client() *kiteconnect.Client { return k.client }

func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func product(p string) string {
	if p == "" {
		return kiteconnect.ProductCNC
	}
	return p
}

func (k *Kite) params(req models.OrderRequest) kiteconnect.OrderParams {
	p := kiteconnect.OrderParams{
		Exchange: req.Exchange, Tradingsymbol: req.TradingSymbol, Validity: string(req.Validity),
		Product: product(req.Product), OrderType: string(req.OrderType), TransactionType: string(req.TransactionType),
		Quantity: req.Quantity, Tag: req.Tag,
	}
	if p.Validity == "" {
		p.Validity = kiteconnect.ValidityDay
	}
	if req.OrderType == models.OrderLimit {
		p.Price = req.Price
	}
	if req.OrderType == models.OrderMarket {
		mp := req.MarketProtection
		if mp == 0 {
			mp = k.marketProtection
		}
		p.MarketProtection = mp
	}
	return p
}

// PlaceOrder places a regular order.
func (k *Kite) PlaceOrder(ctx context.Context, req models.OrderRequest) (string, error) {
	if err := ctxErr(ctx); err != nil {
		return "", err
	}
	r, err := k.client.PlaceOrder(kiteconnect.VarietyRegular, k.params(req))
	return r.OrderID, err
}

// ModifyOrder modifies an open order.
func (k *Kite) ModifyOrder(ctx context.Context, id string, req models.OrderRequest) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	f := k.params(req)
	_, err := k.client.ModifyOrder(kiteconnect.VarietyRegular, id, kiteconnect.OrderParams{
		OrderType: f.OrderType, Quantity: f.Quantity, Price: f.Price, Validity: f.Validity, MarketProtection: f.MarketProtection})
	return err
}

// CancelOrder cancels an open order.
func (k *Kite) CancelOrder(ctx context.Context, id string) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	_, err := k.client.CancelOrder(kiteconnect.VarietyRegular, id, nil)
	return err
}

// OrderStatus returns the latest state of an order.
func (k *Kite) OrderStatus(ctx context.Context, id string) (models.OrderUpdate, error) {
	if err := ctxErr(ctx); err != nil {
		return models.OrderUpdate{}, err
	}
	h, err := k.client.GetOrderHistory(id)
	if err != nil {
		return models.OrderUpdate{}, err
	}
	if len(h) == 0 {
		return models.OrderUpdate{}, fmt.Errorf("order %s: empty history", id)
	}
	return FromKiteOrder(h[len(h)-1]), nil
}

// Orders returns today's order book.
func (k *Kite) Orders(ctx context.Context) ([]models.OrderUpdate, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	os, err := k.client.GetOrders()
	if err != nil {
		return nil, err
	}
	out := make([]models.OrderUpdate, 0, len(os))
	for _, o := range os {
		out = append(out, FromKiteOrder(o))
	}
	return out, nil
}

func (k *Kite) gttParams(req models.OrderRequest) kiteconnect.GTTParams {
	return kiteconnect.GTTParams{
		Tradingsymbol: req.TradingSymbol, Exchange: req.Exchange, LastPrice: req.LastPrice,
		TransactionType: string(req.TransactionType), Product: product(req.Product),
		Trigger: &kiteconnect.GTTSingleLegTrigger{TriggerParams: kiteconnect.TriggerParams{
			TriggerValue: req.TriggerPrice, LimitPrice: req.Price, Quantity: float64(req.Quantity)}},
	}
}

// PlaceGTT creates a single-leg GTT (the stop). On trigger Kite places a LIMIT order.
func (k *Kite) PlaceGTT(ctx context.Context, req models.OrderRequest) (string, error) {
	if err := ctxErr(ctx); err != nil {
		return "", err
	}
	r, err := k.client.PlaceGTT(k.gttParams(req))
	if err != nil {
		return "", err
	}
	return strconv.Itoa(r.TriggerID), nil
}

// ModifyGTT updates a GTT's trigger/limit/quantity.
func (k *Kite) ModifyGTT(ctx context.Context, id string, req models.OrderRequest) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	n, err := strconv.Atoi(id)
	if err != nil {
		return fmt.Errorf("gtt id %q: %w", id, err)
	}
	_, err = k.client.ModifyGTT(n, k.gttParams(req))
	return err
}

// DeleteGTT removes a GTT.
func (k *Kite) DeleteGTT(ctx context.Context, id string) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	n, err := strconv.Atoi(id)
	if err != nil {
		return fmt.Errorf("gtt id %q: %w", id, err)
	}
	_, err = k.client.DeleteGTT(n)
	return err
}

// GTTs lists active and recent GTTs.
func (k *Kite) GTTs(ctx context.Context) ([]models.GTTInfo, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	gs, err := k.client.GetGTTs()
	if err != nil {
		return nil, err
	}
	out := make([]models.GTTInfo, 0, len(gs))
	for _, g := range gs {
		gi := models.GTTInfo{ID: strconv.Itoa(g.ID), TradingSymbol: g.Condition.Tradingsymbol, Status: g.Status}
		if len(g.Condition.TriggerValues) > 0 {
			gi.TriggerPrice = g.Condition.TriggerValues[0]
		}
		if len(g.Orders) > 0 {
			gi.LimitPrice = g.Orders[0].Price
			gi.Quantity = int(g.Orders[0].Quantity)
		}
		out = append(out, gi)
	}
	return out, nil
}

// Holdings merges settled holdings with today's CNC buys (which appear as
// day positions until settlement).
func (k *Kite) Holdings(ctx context.Context) ([]Holding, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	hs, err := k.client.GetHoldings()
	if err != nil {
		return nil, err
	}
	m := map[string]*Holding{}
	for _, h := range hs {
		m[h.Tradingsymbol] = &Holding{InstrumentToken: h.InstrumentToken, TradingSymbol: h.Tradingsymbol,
			Quantity: h.Quantity + h.T1Quantity, AveragePrice: h.AveragePrice, LastPrice: h.LastPrice}
	}
	ps, err := k.client.GetPositions()
	if err != nil {
		return nil, err
	}
	for _, p := range ps.Net {
		if p.Product != kiteconnect.ProductCNC || p.Quantity == 0 {
			continue
		}
		h := m[p.Tradingsymbol]
		if h == nil {
			h = &Holding{InstrumentToken: p.InstrumentToken, TradingSymbol: p.Tradingsymbol, LastPrice: p.LastPrice, AveragePrice: p.AveragePrice}
			m[p.Tradingsymbol] = h
		}
		h.Quantity += p.Quantity
	}
	out := make([]Holding, 0, len(m))
	for _, h := range m {
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TradingSymbol < out[j].TradingSymbol })
	return out, nil
}

// AvailableCash returns the equity segment's net available funds.
func (k *Kite) AvailableCash(ctx context.Context) (float64, error) {
	if err := ctxErr(ctx); err != nil {
		return 0, err
	}
	m, err := k.client.GetUserSegmentMargins(kiteconnect.MarginsEquity)
	if err != nil {
		return 0, err
	}
	return m.Net, nil
}

// Instruments downloads the instrument master for one exchange.
func (k *Kite) Instruments(ctx context.Context, exchange string) ([]Instrument, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	ins, err := k.client.GetInstrumentsByExchange(exchange)
	if err != nil {
		return nil, err
	}
	out := make([]Instrument, 0, len(ins))
	for _, i := range ins {
		out = append(out, Instrument{InstrumentToken: uint32(i.InstrumentToken), TradingSymbol: i.Tradingsymbol, Name: i.Name,
			Exchange: i.Exchange, Segment: i.Segment, InstrumentType: i.InstrumentType, TickSize: i.TickSize, LotSize: int(i.LotSize)})
	}
	return out, nil
}

// DailyCandles returns day bars in [from, to] (Kite serves up to 2000 days per call).
func (k *Kite) DailyCandles(ctx context.Context, token uint32, from, to time.Time) ([]models.Bar, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	data, err := k.client.GetHistoricalData(int(token), "day", from, to, false, false)
	if err != nil {
		return nil, err
	}
	out := make([]models.Bar, 0, len(data))
	for _, d := range data {
		t := d.Date.Time
		out = append(out, models.Bar{Date: time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()),
			Open: d.Open, High: d.High, Low: d.Low, Close: d.Close, Volume: float64(d.Volume)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	return out, nil
}

// Quotes returns full quotes (with depth and circuit limits).
func (k *Kite) Quotes(ctx context.Context, keys []string) (map[string]models.Quote, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	out := map[string]models.Quote{}
	for i := 0; i < len(keys); i += 400 {
		j := i + 400
		if j > len(keys) {
			j = len(keys)
		}
		q, err := k.client.GetQuote(keys[i:j]...)
		if err != nil {
			return nil, err
		}
		for key, v := range q {
			mq := models.Quote{InstrumentToken: uint32(v.InstrumentToken), LastPrice: v.LastPrice, Open: v.OHLC.Open,
				High: v.OHLC.High, Low: v.OHLC.Low, PrevClose: v.OHLC.Close, UpperCircuit: v.UpperCircuitLimit,
				LowerCircuit: v.LowerCircuitLimit, Time: v.Timestamp.Time}
			if v.Depth.Buy[0].Price > 0 {
				mq.BestBid = v.Depth.Buy[0].Price
			}
			if v.Depth.Sell[0].Price > 0 {
				mq.BestAsk = v.Depth.Sell[0].Price
			}
			out[key] = mq
		}
		if j < len(keys) {
			time.Sleep(1100 * time.Millisecond) // quote endpoint: 1 req/s
		}
	}
	return out, nil
}

// FromKiteOrder converts an SDK order to our model.
func FromKiteOrder(o kiteconnect.Order) models.OrderUpdate {
	tag := o.Tag
	if tag == "" && len(o.Tags) > 0 {
		tag = o.Tags[0]
	}
	ts := o.ExchangeUpdateTimestamp.Time
	if ts.IsZero() {
		ts = o.OrderTimestamp.Time
	}
	return models.OrderUpdate{OrderID: o.OrderID, InstrumentToken: o.InstrumentToken, TradingSymbol: o.TradingSymbol,
		Status: o.Status, StatusMessage: o.StatusMessage, TransactionType: models.TransactionType(o.TransactionType),
		OrderType: models.OrderType(o.OrderType), Product: o.Product, Quantity: int(o.Quantity),
		FilledQuantity: int(o.FilledQuantity), PendingQuantity: int(o.PendingQuantity), AveragePrice: o.AveragePrice,
		Price: o.Price, Tag: tag, UpdatedAt: ts}
}
