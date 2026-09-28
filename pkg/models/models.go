// Package models holds the data types shared across the swing engine.
package models

import (
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Market data
// ---------------------------------------------------------------------------

// Bar is one daily OHLCV candle. Date is the trading day (00:00 IST).
type Bar struct {
	Date   time.Time `json:"d"`
	Open   float64   `json:"o"`
	High   float64   `json:"h"`
	Low    float64   `json:"l"`
	Close  float64   `json:"c"`
	Volume float64   `json:"v"`
}

// Quote is a live snapshot of one instrument.
type Quote struct {
	InstrumentToken uint32
	LastPrice       float64
	Open            float64
	High            float64
	Low             float64
	PrevClose       float64
	BestBid         float64
	BestAsk         float64
	UpperCircuit    float64
	LowerCircuit    float64
	Time            time.Time
}

// ---------------------------------------------------------------------------
// Orders
// ---------------------------------------------------------------------------

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
)

// Validity mirrors Kite's validity values.
type Validity string

const (
	ValidityDay Validity = "DAY"
	ValidityIOC Validity = "IOC"
)

// ProductCNC is delivery. The swing engine trades nothing else.
const ProductCNC = "CNC"

// OrderAction is the REST operation the order manager performs.
type OrderAction int

const (
	ActionPlace OrderAction = iota
	ActionModify
	ActionCancel
	ActionGTTPlace
	ActionGTTModify
	ActionGTTDelete
)

func (a OrderAction) String() string {
	return [...]string{"PLACE", "MODIFY", "CANCEL", "GTT_PLACE", "GTT_MODIFY", "GTT_DELETE"}[a]
}

// Priority orders the outbound queue. Lower value = served first.
type Priority int

const (
	PriorityEmergency Priority = iota // gap-through-stop exits, protective sells
	PriorityStop                      // GTT stop placement / updates, planned exits
	PriorityEntry                     // new positions
	numPriorities
)

// NumPriorities is the number of lanes.
const NumPriorities = int(numPriorities)

func (p Priority) String() string {
	return [...]string{"P1-EMERGENCY", "P2-STOP", "P3-ENTRY"}[p]
}

// OrderRequest describes an order or a GTT.
type OrderRequest struct {
	InstrumentToken uint32 // not sent to Kite; used for routing and paper fills
	Exchange        string
	TradingSymbol   string
	TransactionType TransactionType
	OrderType       OrderType
	Validity        Validity
	Product         string
	Quantity        int
	Price           float64 // LIMIT price (for a GTT: the limit placed on trigger)
	TriggerPrice    float64 // GTT trigger
	LastPrice       float64 // GTT: current LTP (Kite requires it)
	// MarketProtection is mandatory (non-zero) for MARKET orders; -1 = Kite auto.
	MarketProtection float64
	Tag              string
}

// OrderPayload is submitted to the central order manager.
type OrderPayload struct {
	ID              uint64
	Action          OrderAction
	Priority        Priority
	InstrumentToken uint32
	BrokerOrderID   string // order ID, or GTT trigger ID for GTT actions
	Request         OrderRequest
	SubmittedAt     time.Time
	MaxAttempts     int
	Reply           chan<- OrderResult
}

// OrderResult is the order manager's answer.
type OrderResult struct {
	PayloadID     uint64
	Action        OrderAction
	BrokerOrderID string
	Attempts      int
	Err           error
	Latency       time.Duration
}

// Broker order statuses.
const (
	StatusOpen      = "OPEN"
	StatusComplete  = "COMPLETE"
	StatusCancelled = "CANCELLED"
	StatusRejected  = "REJECTED"
)

// OrderUpdate is a broker-side status snapshot for one order.
type OrderUpdate struct {
	OrderID         string
	InstrumentToken uint32
	TradingSymbol   string
	Status          string
	StatusMessage   string
	TransactionType TransactionType
	OrderType       OrderType
	Product         string
	Quantity        int
	FilledQuantity  int
	PendingQuantity int
	AveragePrice    float64
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
func (u OrderUpdate) IsLive() bool { return !u.IsTerminal() && u.Status != "" }

// GTT statuses (Kite).
const (
	GTTActive    = "active"
	GTTTriggered = "triggered"
	GTTDisabled  = "disabled"
	GTTExpired   = "expired"
	GTTCancelled = "cancelled"
	GTTRejected  = "rejected"
	GTTDeleted   = "deleted"
)

// GTTInfo is a broker-side snapshot of one GTT trigger.
type GTTInfo struct {
	ID              string
	InstrumentToken uint32
	TradingSymbol   string
	Status          string
	TriggerPrice    float64
	LimitPrice      float64
	Quantity        int
	OrderID         string // order placed when triggered, if any
}

// Tag format: "ks" + instrument token. Only orders carrying this tag are ever
// touched by the engine — your other holdings and manual orders are left alone.
const tagPrefix = "ks"

// TagForToken returns the order tag for an instrument.
func TagForToken(token uint32) string { return tagPrefix + strconv.FormatUint(uint64(token), 10) }

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
// Swing positions and trades
// ---------------------------------------------------------------------------

// SetupKind is the entry pattern that produced a signal.
type SetupKind string

const (
	SetupPullback SetupKind = "PULLBACK"
	SetupBreakout SetupKind = "BREAKOUT"
)

// StopStage tracks how far the stop has been ratcheted.
type StopStage string

const (
	StageInitial   StopStage = "INITIAL"   // entry − 2×ATR
	StageBreakeven StopStage = "BREAKEVEN" // +1R reached: entry + costs
	StageLocked    StopStage = "LOCKED"    // +2R reached: entry + 1R
	StageTrailing  StopStage = "TRAILING"  // highest close − 3×ATR above the lock
)

// Position is one open swing position (persisted across days).
type Position struct {
	InstrumentToken uint32    `json:"token"`
	Symbol          string    `json:"symbol"`
	Quantity        int       `json:"qty"`
	EntryPrice      float64   `json:"entry_price"`
	EntryDate       time.Time `json:"entry_date"`
	Setup           SetupKind `json:"setup"`
	InitialStop     float64   `json:"initial_stop"`
	Stop            float64   `json:"stop"`
	Stage           StopStage `json:"stage"`
	HighestClose    float64   `json:"highest_close"`
	BarsHeld        int       `json:"bars_held"`
	GTTID           string    `json:"gtt_id,omitempty"`
	GTTTrigger      float64   `json:"gtt_trigger,omitempty"`
	PendingExit     string    `json:"pending_exit,omitempty"` // reason; executed at the next morning run
	LastClose       float64   `json:"last_close"`
	LastPrice       float64   `json:"last_price"`
	EntryCosts      float64   `json:"entry_costs"`
}

// RiskPerShare is 1R.
func (p Position) RiskPerShare() float64 { return p.EntryPrice - p.InitialStop }

// Signal is an entry candidate produced by the evening scan.
type Signal struct {
	InstrumentToken uint32    `json:"token"`
	Symbol          string    `json:"symbol"`
	Date            time.Time `json:"date"` // the bar the signal formed on
	Setup           SetupKind `json:"setup"`
	Close           float64   `json:"close"`
	Stop            float64   `json:"stop"`
	ATR             float64   `json:"atr"`
	RS              float64   `json:"rs"` // relative strength vs the index, % points
	Score           float64   `json:"score"`
	Reason          string    `json:"reason"`
}

// Trade is one completed round trip.
type Trade struct {
	Symbol     string    `json:"symbol"`
	Setup      SetupKind `json:"setup"`
	Quantity   int       `json:"qty"`
	EntryDate  time.Time `json:"entry_date"`
	EntryPrice float64   `json:"entry_price"`
	ExitDate   time.Time `json:"exit_date"`
	ExitPrice  float64   `json:"exit_price"`
	Gross      float64   `json:"gross"`
	Costs      float64   `json:"costs"`
	Net        float64   `json:"net"`
	RMultiple  float64   `json:"r"`
	BarsHeld   int       `json:"bars_held"`
	Stage      StopStage `json:"stage"`
	Reason     string    `json:"reason"`
}
