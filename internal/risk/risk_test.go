package risk

import (
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nkalva/kitealgo/internal/config"
	"github.com/nkalva/kitealgo/pkg/models"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestSizing(t *testing.T) {
	cfg := config.Defaults().Risk // ₹1L, 0.5% risk, 10 positions, 5x
	m := NewManager(cfg, 0.15, nil, quiet, nil)
	// ₹500 risk / ₹5 stop = 100 shares; notional cap ₹50,000/₹1000 = 50 → 50.
	if q := m.Size(1000, 5); q != 50 {
		t.Fatalf("want 50 (notional cap), got %d", q)
	}
	// ₹500 / ₹0.5 = 1000; cap ₹50,000/₹100 = 500 → 500.
	if q := m.Size(100, 0.5); q != 500 {
		t.Fatalf("want 500, got %d", q)
	}
	if q := m.Size(100, 1); q != 500 {
		t.Fatalf("want 500 (risk = cap), got %d", q)
	}
}

func TestDailyLossBreakerFiresOnce(t *testing.T) {
	cfg := config.Defaults().Risk
	var fired atomic.Int32
	m := NewManager(cfg, 0.15, nil, quiet, func(string) { fired.Add(1) })
	m.UpdateUnrealized(1, -1500)
	m.RecordTrade(2, models.TradeRecord{NetPnL: -600}) // total -2100 ≤ -2000
	m.UpdateUnrealized(1, -1600)
	time.Sleep(50 * time.Millisecond)
	if fired.Load() != 1 || !m.Halted() {
		t.Fatalf("breaker fired %d times, halted=%v", fired.Load(), m.Halted())
	}
	if ok, _ := m.Reserve(3, 1000); ok {
		t.Fatal("reservations must be refused after breach")
	}
}

func TestMaxPositions(t *testing.T) {
	cfg := config.Defaults().Risk
	cfg.MaxOpenPositions = 2
	m := NewManager(cfg, 0.15, nil, quiet, nil)
	a, _ := m.Reserve(1, 1000)
	b, _ := m.Reserve(2, 1000)
	c, _ := m.Reserve(3, 1000)
	if !a || !b || c {
		t.Fatalf("slots: %v %v %v", a, b, c)
	}
	m.Release(1)
	if ok, _ := m.Reserve(3, 1000); !ok {
		t.Fatal("slot should free on release")
	}
}
