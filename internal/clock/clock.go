// Package clock centralises IST time handling: the trading calendar and the
// intraday phase windows (warmup / active / exit-only / closed).
package clock

import (
	"fmt"
	"time"
	_ "time/tzdata" // embed tz database so Asia/Kolkata resolves on minimal hosts

	"github.com/nkalva/kitealgo/internal/config"
)

// IST is India Standard Time.
var IST = mustLoad("Asia/Kolkata")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		// IST has had no DST since 1945; a fixed zone is exact for our purposes.
		return time.FixedZone("IST", 5*3600+1800)
	}
	return loc
}

// Clock abstracts wall time so the engine can be driven by a simulator.
type Clock interface {
	Now() time.Time
}

// System is the real clock.
type System struct{}

// Now returns the current time in IST.
func (System) Now() time.Time { return time.Now().In(IST) }

// Phase is the intraday execution window.
type Phase int

const (
	PhasePreOpen  Phase = iota // before 09:15
	PhaseWarmup                // 09:15–09:45 ingest only
	PhaseActive                // 09:45–14:45 entries allowed
	PhaseExitOnly              // 14:45–square-off
	PhaseClosed                // after square-off
)

func (p Phase) String() string {
	return [...]string{"PRE_OPEN", "WARMUP", "ACTIVE", "EXIT_ONLY", "CLOSED"}[p]
}

// Offsets are durations since IST midnight.
type Session struct {
	PrepareAt, MarketOpen, TradingStart, EntryCutoff, SquareOff, SafetySweep, SessionEnd time.Duration
	holidays                                                                             map[string]struct{}
}

// NewSession parses the configured HH:MM:SS strings.
func NewSession(sc config.SessionConfig, holidays []string) (*Session, error) {
	p := func(s string) (time.Duration, error) {
		t, err := time.Parse("15:04:05", s)
		if err != nil {
			return 0, fmt.Errorf("parse %q: %w", s, err)
		}
		return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute + time.Duration(t.Second())*time.Second, nil
	}
	var s Session
	var err error
	fields := []struct {
		dst *time.Duration
		v   string
	}{
		{&s.PrepareAt, sc.PrepareAt}, {&s.MarketOpen, sc.MarketOpen}, {&s.TradingStart, sc.TradingStart},
		{&s.EntryCutoff, sc.EntryCutoff}, {&s.SquareOff, sc.SquareOff}, {&s.SafetySweep, sc.SafetySweep},
		{&s.SessionEnd, sc.SessionEnd},
	}
	for _, f := range fields {
		if *f.dst, err = p(f.v); err != nil {
			return nil, err
		}
	}
	if !(s.PrepareAt < s.MarketOpen && s.MarketOpen < s.TradingStart && s.TradingStart < s.EntryCutoff &&
		s.EntryCutoff < s.SquareOff && s.SquareOff < s.SafetySweep && s.SafetySweep < s.SessionEnd) {
		return nil, fmt.Errorf("session times must be strictly increasing: prepare < open < trading_start < entry_cutoff < square_off < safety_sweep < session_end")
	}
	s.holidays = make(map[string]struct{}, len(holidays))
	for _, h := range holidays {
		s.holidays[h] = struct{}{}
	}
	return &s, nil
}

// Midnight returns 00:00 IST of t's IST calendar day.
func Midnight(t time.Time) time.Time {
	t = t.In(IST)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, IST)
}

// At returns the absolute time of offset on t's IST day.
func (s *Session) At(t time.Time, offset time.Duration) time.Time {
	return Midnight(t).Add(offset)
}

// OpenTime returns 09:15 on t's day.
func (s *Session) OpenTime(t time.Time) time.Time { return s.At(t, s.MarketOpen) }

// IsTradingDay reports whether NSE is open on t's IST day (weekday, not a listed holiday).
func (s *Session) IsTradingDay(t time.Time) bool {
	t = t.In(IST)
	if wd := t.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false
	}
	_, hol := s.holidays[t.Format("2006-01-02")]
	return !hol
}

// NextTradingDay returns midnight of the first trading day strictly after t's day.
func (s *Session) NextTradingDay(t time.Time) time.Time {
	d := Midnight(t)
	for i := 0; i < 15; i++ {
		d = d.AddDate(0, 0, 1)
		if s.IsTradingDay(d) {
			return d
		}
	}
	return d
}

// PhaseAt classifies t into an execution window.
func (s *Session) PhaseAt(t time.Time) Phase {
	off := t.In(IST).Sub(Midnight(t))
	switch {
	case off < s.MarketOpen:
		return PhasePreOpen
	case off < s.TradingStart:
		return PhaseWarmup
	case off < s.EntryCutoff:
		return PhaseActive
	case off < s.SquareOff:
		return PhaseExitOnly
	default:
		return PhaseClosed
	}
}
