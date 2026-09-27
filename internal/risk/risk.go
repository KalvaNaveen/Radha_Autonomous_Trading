// Package risk enforces portfolio-level limits: position sizing, max
// concurrent positions, margin sufficiency and the daily loss circuit breaker.
package risk

import (
	"context"
	"encoding/csv"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/nkalva/kitealgo/internal/config"
	"github.com/nkalva/kitealgo/pkg/models"
)

// MarginSource returns available equity margin.
type MarginSource interface {
	AvailableMargin(ctx context.Context) (float64, error)
}

// Manager is shared by all agents; every method is goroutine-safe.
type Manager struct {
	cfg     config.RiskConfig
	costPct float64
	log     *slog.Logger

	mu          sync.RWMutex
	slots       map[uint32]float64 // reserved/open positions → notional
	unrealized  map[uint32]float64
	realized    float64
	trades      int
	halted      bool
	haltReason  string
	margin      float64
	marginAt    time.Time
	onBreach    func(reason string)
	breachFired bool

	journal *Journal
}

// NewManager creates a risk manager. onBreach is called once when the daily
// loss limit is hit (the engine flattens everything and blocks entries).
func NewManager(cfg config.RiskConfig, roundTripCostPct float64, journal *Journal, log *slog.Logger, onBreach func(string)) *Manager {
	return &Manager{cfg: cfg, costPct: roundTripCostPct, log: log.With("component", "risk"),
		slots: map[uint32]float64{}, unrealized: map[uint32]float64{}, onBreach: onBreach, journal: journal,
		margin: math.Inf(1)}
}

// Size returns the quantity for an entry at price with the given stop distance
// (in rupees per share). Returns 0 when the trade cannot be sized.
//
//	qty_risk     = capital × risk% / stop_distance
//	qty_notional = (capital × leverage / max_positions) / price
func (m *Manager) Size(price, stopDistance float64) int {
	if price <= 0 || stopDistance <= 0 {
		return 0
	}
	riskBudget := m.cfg.Capital * m.cfg.RiskPerTradePct / 100
	qRisk := math.Floor(riskBudget / stopDistance)
	perPos := m.cfg.Capital * m.cfg.MISLeverage / float64(m.cfg.MaxOpenPositions)
	qNotional := math.Floor(perPos / price)
	q := math.Min(qRisk, qNotional)
	if q < 1 {
		return 0
	}
	return int(q)
}

// Reserve claims a position slot for token with the given notional. It checks
// the halt flag, the max-open-positions cap and margin sufficiency.
func (m *Manager) Reserve(token uint32, notional float64) (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.halted {
		return false, "risk: halted (" + m.haltReason + ")"
	}
	if _, ok := m.slots[token]; ok {
		return false, "risk: slot already held"
	}
	if len(m.slots) >= m.cfg.MaxOpenPositions {
		return false, fmt.Sprintf("risk: max open positions (%d) reached", m.cfg.MaxOpenPositions)
	}
	var used float64
	for _, n := range m.slots {
		used += n
	}
	need := (used + notional) / m.cfg.MISLeverage
	usable := m.margin * (1 - m.cfg.MarginBufferPct/100)
	if need > usable {
		return false, fmt.Sprintf("risk: margin insufficient (need ₹%.0f, usable ₹%.0f)", need, usable)
	}
	m.slots[token] = notional
	return true, ""
}

// Release frees a slot (entry failed, or position closed).
func (m *Manager) Release(token uint32) {
	m.mu.Lock()
	delete(m.slots, token)
	delete(m.unrealized, token)
	m.mu.Unlock()
}

// Adopt marks a slot as held regardless of limits (a fill we must manage).
func (m *Manager) Adopt(token uint32, notional float64) {
	m.mu.Lock()
	m.slots[token] = notional
	m.mu.Unlock()
}

// UpdateUnrealized records an open position's mark-to-market and checks the breaker.
func (m *Manager) UpdateUnrealized(token uint32, pnl float64) {
	m.mu.Lock()
	m.unrealized[token] = pnl
	m.mu.Unlock()
	m.check()
}

// RecordTrade books a closed round trip and clears the instrument's mark-to-market
// atomically (so the breaker never double counts the same P&L).
func (m *Manager) RecordTrade(token uint32, t models.TradeRecord) {
	m.mu.Lock()
	m.realized += t.NetPnL
	m.trades++
	delete(m.unrealized, token)
	m.mu.Unlock()
	if m.journal != nil {
		if err := m.journal.Write(t); err != nil {
			m.log.Error("journal write failed", "err", err)
		}
	}
	m.log.Info("trade closed", "symbol", t.TradingSymbol, "side", t.Side, "qty", t.Quantity,
		"entry", t.EntryPrice, "exit", t.ExitPrice, "net", round2(t.NetPnL), "reason", t.ExitReason)
	m.check()
}

// EstCosts estimates round-trip friction for a trade.
func (m *Manager) EstCosts(entryPx, exitPx float64, qty int) float64 {
	return (entryPx + exitPx) / 2 * float64(qty) * m.costPct / 100
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// DayPnL returns realized + unrealized.
func (m *Manager) DayPnL() (realized, unrealized float64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, u := range m.unrealized {
		unrealized += u
	}
	return m.realized, unrealized
}

func (m *Manager) check() {
	r, u := m.DayPnL()
	limit := -m.cfg.Capital * m.cfg.DailyLossLimitPct / 100
	if r+u > limit {
		return
	}
	m.mu.Lock()
	if m.breachFired {
		m.mu.Unlock()
		return
	}
	m.breachFired = true
	m.halted = true
	m.haltReason = fmt.Sprintf("daily loss limit hit: ₹%.0f <= ₹%.0f", r+u, limit)
	reason := m.haltReason
	f := m.onBreach
	m.mu.Unlock()
	m.log.Error("DAILY LOSS LIMIT BREACHED", "realized", round2(r), "unrealized", round2(u), "limit", limit)
	if f != nil {
		go f(reason)
	}
}

// Halt blocks new reservations (kill switch).
func (m *Manager) Halt(reason string) {
	m.mu.Lock()
	if !m.halted {
		m.halted, m.haltReason = true, reason
	}
	m.mu.Unlock()
}

// Halted reports the halt state.
func (m *Manager) Halted() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.halted
}

// OpenCount returns the number of held slots.
func (m *Manager) OpenCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.slots)
}

// RunMarginRefresh polls available margin until ctx is done.
func (m *Manager) RunMarginRefresh(ctx context.Context, src MarginSource) {
	refresh := func() {
		v, err := src.AvailableMargin(ctx)
		if err != nil {
			m.log.Warn("margin refresh failed", "err", err)
			return
		}
		m.mu.Lock()
		m.margin, m.marginAt = v, time.Now()
		m.mu.Unlock()
	}
	refresh()
	iv := m.cfg.MarginRefresh
	if iv <= 0 {
		iv = 30 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			refresh()
		}
	}
}

// Summary is a status snapshot.
type Summary struct {
	Realized   float64 `json:"realized"`
	Unrealized float64 `json:"unrealized"`
	Trades     int     `json:"trades"`
	Open       int     `json:"open_positions"`
	Halted     bool    `json:"halted"`
	HaltReason string  `json:"halt_reason,omitempty"`
	Margin     float64 `json:"margin_available"`
}

// Summary returns a snapshot.
func (m *Manager) Summary() Summary {
	r, u := m.DayPnL()
	m.mu.RLock()
	defer m.mu.RUnlock()
	mg := m.margin
	if math.IsInf(mg, 1) {
		mg = -1
	}
	return Summary{Realized: round2(r), Unrealized: round2(u), Trades: m.trades, Open: len(m.slots),
		Halted: m.halted, HaltReason: m.haltReason, Margin: mg}
}

// ---------------------------------------------------------------------------
// Journal
// ---------------------------------------------------------------------------

// Journal appends completed trades to journal_dir/YYYY-MM-DD.csv.
type Journal struct {
	mu   sync.Mutex
	path string
}

// NewJournal opens (creating if needed) the day's journal file.
func NewJournal(dir string, day time.Time) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	j := &Journal{path: filepath.Join(dir, day.Format("2006-01-02")+".csv")}
	if _, err := os.Stat(j.path); os.IsNotExist(err) {
		f, err := os.Create(j.path)
		if err != nil {
			return nil, err
		}
		w := csv.NewWriter(f)
		_ = w.Write([]string{"symbol", "side", "qty", "entry_time", "entry_price", "exit_time", "exit_price",
			"gross_pnl", "est_costs", "net_pnl", "exit_reason", "max_lock"})
		w.Flush()
		_ = f.Close()
	}
	return j, nil
}

// Write appends one trade.
func (j *Journal) Write(t models.TradeRecord) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	f, err := os.OpenFile(j.path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	ff := func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }
	_ = w.Write([]string{t.TradingSymbol, t.Side.String(), strconv.Itoa(t.Quantity),
		t.EntryTime.Format(time.RFC3339), ff(t.EntryPrice), t.ExitTime.Format(time.RFC3339), ff(t.ExitPrice),
		ff(t.GrossPnL), ff(t.EstCosts), ff(t.NetPnL), t.ExitReason, t.MaxLock.String()})
	w.Flush()
	return w.Error()
}
