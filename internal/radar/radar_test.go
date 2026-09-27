package radar

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/nkalva/kitealgo/pkg/models"
)

func TestAllowModes(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	open := time.Date(2026, 9, 28, 9, 15, 0, 0, time.UTC)
	mk := func(mode Mode) *Radar {
		return New(mode, 0.1, 1, []Spec{{Token: 1, Name: "NIFTY 50"}, {Token: 2, Name: "NIFTY BANK"}}, 10, 20, 5*time.Minute, time.Second, open, log)
	}
	r := mk(ModePermissive)
	r.onTick(r.indices[1], models.Tick{InstrumentToken: 1, LastPrice: 99.0, DayOpen: 100, ExchangeTime: open.Add(time.Minute)})
	if ok, _ := r.Allow(models.SideLong, 0); ok {
		t.Fatal("permissive: long must be blocked when market is bearish")
	}
	if ok, _ := r.Allow(models.SideShort, 0); !ok {
		t.Fatal("short aligned with bearish market must pass")
	}
	if ok, _ := r.Allow(models.SideShort, 2); !ok {
		t.Fatal("permissive: neutral benchmark must not block")
	}
	s := mk(ModeStrict)
	s.onTick(s.indices[1], models.Tick{InstrumentToken: 1, LastPrice: 99.0, DayOpen: 100, ExchangeTime: open.Add(time.Minute)})
	if ok, _ := s.Allow(models.SideShort, 2); ok {
		t.Fatal("strict: neutral benchmark must block")
	}
}
