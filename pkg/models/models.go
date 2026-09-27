// Package models holds the data types shared by every component of the engine.
// Nothing in here performs I/O; these are plain values passed over channels.
package models

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Market data
// ---------------------------------------------------------------------------

// Tick is the engine's normalised view of one KiteTicker packet (full mode).
type Tick struct {
	InstrumentToken uint32
	LastPrice       float64
	LastTradedQty   uint32
	// VolumeTraded is the exchange's cumulative traded volume for the day.
	VolumeTraded uint64
	// AverageTradePrice is the exchange-computed volume weighted average price
	// for the day (i.e. the exchange VWAP). Zero for indices.
	AverageTradePrice float64
	BestBid           float64
	BestAsk           float64
	DayOpen           float64
	// ExchangeTime is the exchange timestamp of the packet. Falls back to
	// ReceivedAt when the exchange does not send one (indices in some modes).
	ExchangeTime time.Time
	ReceivedAt   time.Time
	IsIndex      bool
}

// MarketTime returns the timestamp the strategy should use for bucketing.
func (t Tick) MarketTime() time.Time {
	if !t.ExchangeTime.IsZero() {
		return t.ExchangeTime
	}
	return t.ReceivedAt
}

// Candle is an OHLCV bar. Start is inclusive, End exclusive.
type Candle struct {
	InstrumentToken uint32
	Interval        time.Duration
	Start           time.Time
	End             time.Time
	Open            float64
	High            float64
	Low             float64
	Close           float64
	Volume          uint64
	TickCount       int
}

func (c Candle) String() string {
	return fmt.Sprintf("%s %s O=%.2f H=%.2f L=%.2f C=%.2f V=%d",
		c.Interval, c.Start.Format("15:04"), c.Open, c.High, c.Low, c.Close, c.Volume)
}

// ---------------------------------------------------------------------------
// Orders
// ---------------------------------------------------------------------------

// Side is the direction of a position.
type Side int

const (
	SideNone Side = iota
	SideLong
	SideShort
)

func (s Side) String() string {
	switch s {
	case SideLong:
		return "LONG"
	case SideShort:
		return "SHORT"
	default:
		return "NONE"
	}
}

// Sign returns +1 for long, -1 for short, 0 otherwise.
func (s Side) Sign() float64 {
	switch s {
	case SideLong:
		return 1
	case SideShort:
		return -1
	default:
		return 0
	}
}

// EntryTxn is the transaction that opens a position on this side.
func (s Side) EntryTxn() TransactionType {
	if s == SideShort {
		return TxnSell
	}
	return TxnBuy
}

// ExitTxn is the transaction that closes a position on this side.
func (s Side) ExitTxn() TransactionType {
	if s == SideShort {
		return TxnBuy
	}
	return TxnSell
}

// TransactionType mirrors Kite's BUY / SELL.
type TransactionType string

const (
	TxnBuy  TransactionType = "BUY"
	TxnSell TransactionType = "SELL"
)

// OrderType mirrors Kite's order types.
type OrderType string

const (
	OrderMarket OrderType = "MARKET"
	OrderLimit  OrderType = "LIMIT"
	OrderSL     OrderType = "SL"
	OrderSLM    OrderType = "SL-M"
)

// Validity mirrors Kite's validity values.
type Validity string

const (
	ValidityDay Validity = "DAY"
	ValidityIOC Validity = "IOC"
)

// OrderAction is the REST operation the order manager must perform.
type OrderAction int

const (
	ActionPlace OrderAction = iota
	ActionModify
	ActionCancel
)

func (a OrderAction) String() string {
	switch a {
	case ActionPlace:
		return "PLACE"
	case ActionModify:
		return "MODIFY"
	case ActionCancel:
		return "CANCEL"
	default:
		return "UNKNOWN"
	}
}

// Priority orders the outbound queue. Lower value = served first.
type Priority int

const (
	// PriorityEmergency: kill switch, emergency flatten, naked-position exits.
	PriorityEmergency Priority = iota
	// PriorityStop: stop-loss placement, modification, trailing moves.
	PriorityStop
	// PriorityEntry: new positions. Dropped wholesale by the kill switch.
	PriorityEntry
	numPriorities
)

// NumPriorities is the number of distinct priority lanes.
const NumPriorities = int(numPriorities)

func (p Priority) String() string {
	switch p {
	case PriorityEmergency:
		return "P1-EMERGENCY"
	case PriorityStop:
		return "P2-STOP"
	case PriorityEntry:
		return "P3-ENTRY"
	default:
		return "P?"
	}
}

// OrderPurpose tells the agent what a request was for when the result returns.
type OrderPurpose int

const (
	PurposeEntry       OrderPurpose = iota // open a position (marketable limit IOC)
	PurposeEntryCancel                     // cancel an entry that did not fill in time
	PurposeStopPlace                       // place the protective SL-M
	PurposeStopModify                      // move the protective SL-M (breakeven / trail)
	PurposeStopCancel                      // cancel the SL-M (cancel-and-replace, or before a plain exit)
	PurposeExitViaStop                     // convert the SL-M into a MARKET order (atomic exit)
	PurposeExitPlace                       // place a fresh MARKET exit
)

func (p OrderPurpose) String() string {
	return [...]string{"ENTRY", "ENTRY_CANCEL", "STOP_PLACE", "STOP_MODIFY", "STOP_CANCEL", "EXIT_VIA_STOP", "EXIT_PLACE"}[p]
}

// IsProtective reports whether a failure of this request can leave a position unhedged.
func (p OrderPurpose) IsProtective() bool {
	switch p {
	case PurposeStopPlace, PurposeStopModify, PurposeExitViaStop, PurposeExitPlace:
		return true
	}
	return false
}

// OrderRequest is the broker-level description of an order.
type OrderRequest struct {
	InstrumentToken uint32 // not sent to Kite; used for routing and paper fills
	Exchange        string
	TradingSymbol   string
	TransactionType TransactionType
	OrderType       OrderType
	Validity        Validity
	Quantity        int
	Price           float64 // LIMIT / SL only
	TriggerPrice    float64 // SL / SL-M only
	// MarketProtection is mandatory (non-zero) for MARKET and SL-M orders.
	// -1 asks Kite to apply its automatic protection band.
	MarketProtection float64
	Tag              string
}

// OrderPayload is what a stock agent submits to the central order manager.
type OrderPayload struct {
	ID              uint64 // assigned by the order manager
	Action          OrderAction
	Priority        Priority
	Purpose         OrderPurpose
	InstrumentToken uint32
	BrokerOrderID   string // required for modify / cancel
	Request         OrderRequest
	SubmittedAt     time.Time
	// MaxAttempts overrides the priority's default retry budget (0 = default).
	MaxAttempts int
	// Reply receives exactly one OrderResult. Must be buffered (cap >= 1) or
	// have a dedicated reader; the order manager never blocks on it.
	Reply chan<- OrderResult
}

// OrderResult is the order manager's answer to an OrderPayload.
type OrderResult struct {
	PayloadID     uint64
	Action        OrderAction
	Purpose       OrderPurpose
	BrokerOrderID string
	Attempts      int
	Err           error
	Latency       time.Duration
}

// Broker order statuses we act on (Kite strings).
const (
	StatusOpen           = "OPEN"
	StatusComplete       = "COMPLETE"
	StatusCancelled      = "CANCELLED"
	StatusRejected       = "REJECTED"
	StatusTriggerPending = "TRIGGER PENDING"
)

// OrderUpdate is a broker-side status snapshot for one order, either from the
// WebSocket postback stream or from the reconciliation poller.
type OrderUpdate struct {
	OrderID         string
	InstrumentToken uint32
	TradingSymbol   string
	Status          string
	StatusMessage   string
	TransactionType TransactionType
	OrderType       OrderType
	Quantity        int
	FilledQuantity  int
	PendingQuantity int
	AveragePrice    float64
	TriggerPrice    float64
	Price           float64
	Tag             string
	UpdatedAt       time.Time
}

// IsTerminal reports whether the order can no longer change.
func (u OrderUpdate) IsTerminal() bool {
	switch u.Status {
	case StatusComplete, StatusCancelled, StatusRejected:
		return true
	}
	return false
}

// IsLive reports whether the order is resting at the exchange.
func (u OrderUpdate) IsLive() bool {
	return !u.IsTerminal() && u.Status != ""
}

// Tag format: "ka" + decimal instrument token. Kite tags are <= 20 chars
// alphanumeric; this lets the router map any broker update back to the agent
// that owns it without waiting for the place-order response.
const tagPrefix = "ka"

// TagForToken returns the order tag for an instrument.
func TagForToken(token uint32) string {
	return tagPrefix + strconv.FormatUint(uint64(token), 10)
}

// TokenFromTag parses a tag produced by TagForToken.
func TokenFromTag(tag string) (uint32, bool) {
	if !strings.HasPrefix(tag, tagPrefix) {
		return 0, false
	}
	v, err := strconv.ParseUint(tag[len(tagPrefix):], 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(v), true
}

// ---------------------------------------------------------------------------
// Agent state
// ---------------------------------------------------------------------------

// AgentState is the lifecycle state of one stock agent.
type AgentState int

const (
	StateFlat AgentState = iota
	StatePendingEntry
	StateInPosition
	StateTrailing
	StatePendingExit
	StateHalted // done for the day (kill switch fired / max trades hit / fatal error)
)

func (s AgentState) String() string {
	return [...]string{"FLAT", "PENDING_ENTRY", "IN_POSITION", "TRAILING", "PENDING_EXIT", "HALTED"}[s]
}

// LockStage tracks how far the stop has been ratcheted.
type LockStage int

const (
	LockNone      LockStage = iota // initial hard stop (20 EMA / max 0.80%)
	LockBreakeven                  // stop at entry ± round-trip cost
	LockProfit                     // stop locks the minimum net profit
)

func (l LockStage) String() string {
	return [...]string{"NONE", "BREAKEVEN", "PROFIT_LOCK"}[l]
}

// Position is an open position owned by one agent.
type Position struct {
	InstrumentToken uint32
	TradingSymbol   string
	Side            Side
	Quantity        int
	EntryPrice      float64
	EntryTime       time.Time
	StopPrice       float64
	StopOrderID     string
	StopModCount    int
	Lock            LockStage
}

// UnrealizedPnL at the given price.
func (p Position) UnrealizedPnL(ltp float64) float64 {
	return p.Side.Sign() * (ltp - p.EntryPrice) * float64(p.Quantity)
}

// GrossReturn is the fractional move in the position's favour.
func (p Position) GrossReturn(ltp float64) float64 {
	if p.EntryPrice == 0 {
		return 0
	}
	return p.Side.Sign() * (ltp - p.EntryPrice) / p.EntryPrice
}

// TradeRecord is one completed round trip, written to the daily journal.
type TradeRecord struct {
	TradingSymbol string
	Side          Side
	Quantity      int
	EntryTime     time.Time
	EntryPrice    float64
	ExitTime      time.Time
	ExitPrice     float64
	GrossPnL      float64
	EstCosts      float64
	NetPnL        float64
	ExitReason    string
	MaxLock       LockStage
}

// AgentSnapshot is a read-only view of an agent for the status endpoint.
type AgentSnapshot struct {
	TradingSymbol string    `json:"symbol"`
	State         string    `json:"state"`
	LastPrice     float64   `json:"ltp"`
	VWAP          float64   `json:"vwap"`
	EMA10         float64   `json:"ema10"`
	EMA20         float64   `json:"ema20"`
	RVOL          float64   `json:"rvol"`
	Trades        int       `json:"trades_today"`
	RealizedPnL   float64   `json:"realized_pnl"`
	Position      *Position `json:"position,omitempty"`
	LastSignal    string    `json:"last_signal,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}
