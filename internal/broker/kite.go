package broker

import (
	"context"
	"fmt"
	"net/http"
	"sort"
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
func NewKite(apiKey, accessToken string, httpClient *http.Client, marketProtection float64) *Kite {
	c := kiteconnect.New(apiKey)
	if httpClient != nil {
		c.SetHTTPClient(httpClient)
	}
	c.SetAccessToken(accessToken)
	c.SetAppName("kitealgo")
	return &Kite{client: c, marketProtection: marketProtection}
}

// Client exposes the underlying SDK client (used by the auth flow).
func (k *Kite) Client() *kiteconnect.Client { return k.client }

func (k *Kite) params(req models.OrderRequest) kiteconnect.OrderParams {
	p := kiteconnect.OrderParams{
		Exchange:        req.Exchange,
		Tradingsymbol:   req.TradingSymbol,
		Validity:        string(req.Validity),
		Product:         kiteconnect.ProductMIS,
		OrderType:       string(req.OrderType),
		TransactionType: string(req.TransactionType),
		Quantity:        req.Quantity,
		Tag:             req.Tag,
	}
	if p.Validity == "" {
		p.Validity = kiteconnect.ValidityDay
	}
	switch req.OrderType {
	case models.OrderLimit:
		p.Price = req.Price
	case models.OrderSL:
		p.Price = req.Price
		p.TriggerPrice = req.TriggerPrice
	case models.OrderSLM:
		p.TriggerPrice = req.TriggerPrice
	}
	// SEBI/Kite: MARKET and SL-M must carry a non-zero market protection.
	if req.OrderType == models.OrderMarket || req.OrderType == models.OrderSLM {
		mp := req.MarketProtection
		if mp == 0 {
			mp = k.marketProtection
		}
		p.MarketProtection = mp
	}
	return p
}

func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

// PlaceOrder places a regular MIS order.
func (k *Kite) PlaceOrder(ctx context.Context, req models.OrderRequest) (string, error) {
	if err := ctxErr(ctx); err != nil {
		return "", err
	}
	resp, err := k.client.PlaceOrder(kiteconnect.VarietyRegular, k.params(req))
	if err != nil {
		return "", err
	}
	return resp.OrderID, nil
}

// ModifyOrder modifies an open order. Only order type, quantity, price,
// trigger and market protection are sent; Kite ignores the rest on modify.
func (k *Kite) ModifyOrder(ctx context.Context, orderID string, req models.OrderRequest) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	full := k.params(req)
	p := kiteconnect.OrderParams{
		OrderType:        full.OrderType,
		Quantity:         full.Quantity,
		Price:            full.Price,
		TriggerPrice:     full.TriggerPrice,
		Validity:         full.Validity,
		MarketProtection: full.MarketProtection,
	}
	_, err := k.client.ModifyOrder(kiteconnect.VarietyRegular, orderID, p)
	return err
}

// CancelOrder cancels an open order.
func (k *Kite) CancelOrder(ctx context.Context, orderID string) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	_, err := k.client.CancelOrder(kiteconnect.VarietyRegular, orderID, nil)
	return err
}

// OrderStatus returns the latest state of an order.
func (k *Kite) OrderStatus(ctx context.Context, orderID string) (models.OrderUpdate, error) {
	if err := ctxErr(ctx); err != nil {
		return models.OrderUpdate{}, err
	}
	hist, err := k.client.GetOrderHistory(orderID)
	if err != nil {
		return models.OrderUpdate{}, err
	}
	if len(hist) == 0 {
		return models.OrderUpdate{}, fmt.Errorf("order %s: empty history", orderID)
	}
	return FromKiteOrder(hist[len(hist)-1]), nil
}

// Orders returns today's order book.
func (k *Kite) Orders(ctx context.Context) ([]models.OrderUpdate, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	ords, err := k.client.GetOrders()
	if err != nil {
		return nil, err
	}
	out := make([]models.OrderUpdate, 0, len(ords))
	for _, o := range ords {
		out = append(out, FromKiteOrder(o))
	}
	return out, nil
}

// Positions returns today's day positions.
func (k *Kite) Positions(ctx context.Context) ([]Position, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	ps, err := k.client.GetPositions()
	if err != nil {
		return nil, err
	}
	out := make([]Position, 0, len(ps.Day))
	for _, p := range ps.Day {
		out = append(out, Position{
			InstrumentToken: p.InstrumentToken, TradingSymbol: p.Tradingsymbol, Exchange: p.Exchange,
			Product: p.Product, NetQuantity: p.Quantity, AveragePrice: p.AveragePrice, LastPrice: p.LastPrice,
		})
	}
	return out, nil
}

// AvailableMargin returns the equity segment's net available margin.
func (k *Kite) AvailableMargin(ctx context.Context) (float64, error) {
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
		out = append(out, Instrument{
			InstrumentToken: uint32(i.InstrumentToken), TradingSymbol: i.Tradingsymbol, Name: i.Name,
			Exchange: i.Exchange, Segment: i.Segment, InstrumentType: i.InstrumentType,
			TickSize: i.TickSize, LotSize: int(i.LotSize),
		})
	}
	return out, nil
}

func (k *Kite) candles(ctx context.Context, token uint32, interval string, from, to time.Time) ([]HistCandle, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	data, err := k.client.GetHistoricalData(int(token), interval, from, to, false, false)
	if err != nil {
		return nil, err
	}
	out := make([]HistCandle, 0, len(data))
	for _, d := range data {
		v := d.Volume
		if v < 0 {
			v = 0
		}
		out = append(out, HistCandle{Time: d.Date.Time, Open: d.Open, High: d.High, Low: d.Low, Close: d.Close, Volume: uint64(v)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

// MinuteCandles returns 1-minute bars in [from, to].
func (k *Kite) MinuteCandles(ctx context.Context, token uint32, from, to time.Time) ([]HistCandle, error) {
	return k.candles(ctx, token, "minute", from, to)
}

// FiveMinuteCandles returns 5-minute bars in [from, to].
func (k *Kite) FiveMinuteCandles(ctx context.Context, token uint32, from, to time.Time) ([]HistCandle, error) {
	return k.candles(ctx, token, "5minute", from, to)
}

// FromKiteOrder converts an SDK order (REST or WebSocket postback) to our model.
func FromKiteOrder(o kiteconnect.Order) models.OrderUpdate {
	tag := o.Tag
	if tag == "" && len(o.Tags) > 0 {
		tag = o.Tags[0]
	}
	ts := o.ExchangeUpdateTimestamp.Time
	if ts.IsZero() {
		ts = o.OrderTimestamp.Time
	}
	return models.OrderUpdate{
		OrderID:         o.OrderID,
		InstrumentToken: o.InstrumentToken,
		TradingSymbol:   o.TradingSymbol,
		Status:          o.Status,
		StatusMessage:   o.StatusMessage,
		TransactionType: models.TransactionType(o.TransactionType),
		OrderType:       models.OrderType(o.OrderType),
		Quantity:        int(o.Quantity),
		FilledQuantity:  int(o.FilledQuantity),
		PendingQuantity: int(o.PendingQuantity),
		AveragePrice:    o.AveragePrice,
		TriggerPrice:    o.TriggerPrice,
		Price:           o.Price,
		Tag:             tag,
		UpdatedAt:       ts,
	}
}
