// Package broker abstracts the brokerage so the engine can run live against
// Kite Connect or in paper mode against a local fill simulator, with the same
// order manager, agents and risk code in both.
package broker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"

	"github.com/nkalva/kitealgo/pkg/models"
)

// Trader is the order-side API. Every call that creates or changes an order
// or a GTT goes through the order manager's rate limiter; reads do not.
type Trader interface {
	PlaceOrder(ctx context.Context, req models.OrderRequest) (orderID string, err error)
	ModifyOrder(ctx context.Context, orderID string, req models.OrderRequest) error
	CancelOrder(ctx context.Context, orderID string) error
	OrderStatus(ctx context.Context, orderID string) (models.OrderUpdate, error)
	Orders(ctx context.Context) ([]models.OrderUpdate, error)

	PlaceGTT(ctx context.Context, req models.OrderRequest) (gttID string, err error)
	ModifyGTT(ctx context.Context, gttID string, req models.OrderRequest) error
	DeleteGTT(ctx context.Context, gttID string) error
	GTTs(ctx context.Context) ([]models.GTTInfo, error)

	Holdings(ctx context.Context) ([]Holding, error)
	AvailableCash(ctx context.Context) (float64, error)
}

// MarketData is the reference / history / quote side of the API.
type MarketData interface {
	Instruments(ctx context.Context, exchange string) ([]Instrument, error)
	DailyCandles(ctx context.Context, token uint32, from, to time.Time) ([]models.Bar, error)
	// Quotes takes "EXCHANGE:SYMBOL" keys (e.g. "NSE:INFY", "NSE:NIFTY 50").
	Quotes(ctx context.Context, keys []string) (map[string]models.Quote, error)
}

// Holding is a delivery position at the broker: settled + T1 + today's CNC buys.
type Holding struct {
	InstrumentToken uint32
	TradingSymbol   string
	Quantity        int
	AveragePrice    float64
	LastPrice       float64
}

// Instrument is one row of the instruments dump.
type Instrument struct {
	InstrumentToken uint32
	TradingSymbol   string
	Name            string
	Exchange        string
	Segment         string
	InstrumentType  string
	TickSize        float64
	LotSize         int
}

// ---------------------------------------------------------------------------
// Error classification
// ---------------------------------------------------------------------------

// ErrOutcome describes what a failed REST call means for the order's state.
type ErrOutcome int

const (
	// OutcomeRejected: the broker definitely did not act (4xx input/order errors).
	OutcomeRejected ErrOutcome = iota
	// OutcomeThrottled: 429 — definitely not acted on; safe to retry after backoff.
	OutcomeThrottled
	// OutcomeAmbiguous: timeout / 5xx / connection reset — the broker MAY have
	// acted. A PLACE must be de-duplicated against the order book before retry.
	OutcomeAmbiguous
	// OutcomeAuth: token expired or invalid — no retry will help.
	OutcomeAuth
)

func (o ErrOutcome) String() string {
	return [...]string{"REJECTED", "THROTTLED", "AMBIGUOUS", "AUTH"}[o]
}

// Classify maps an error from a Trader call to an outcome.
func Classify(err error) ErrOutcome {
	if err == nil {
		return OutcomeRejected
	}
	var ke kiteconnect.Error
	if errors.As(err, &ke) {
		switch {
		case ke.Code == http.StatusTooManyRequests:
			return OutcomeThrottled
		case ke.ErrorType == kiteconnect.TokenError:
			return OutcomeAuth
		case ke.ErrorType == kiteconnect.NetworkError, ke.ErrorType == kiteconnect.DataError, ke.Code >= 500:
			return OutcomeAmbiguous
		default:
			return OutcomeRejected
		}
	}
	var se *SimError
	if errors.As(err, &se) {
		return se.Outcome
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return OutcomeAmbiguous
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return OutcomeAmbiguous
	}
	return OutcomeAmbiguous // unknown failure: assume the worst
}

// Retriable reports whether retrying the same request can succeed.
func Retriable(err error) bool {
	switch Classify(err) {
	case OutcomeThrottled, OutcomeAmbiguous:
		return true
	}
	return false
}

// SimError lets the paper broker (and tests) inject specific failure outcomes.
type SimError struct {
	Outcome ErrOutcome
	Msg     string
}

func (e *SimError) Error() string { return e.Msg }
