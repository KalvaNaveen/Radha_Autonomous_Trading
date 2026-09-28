// Package clock centralises IST time handling: the trading calendar and the
// daily swing schedule (prepare → morning run → market hours → evening run).
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
		return time.FixedZone("IST", 5*3600+1800)
	}
	return loc
}

// Clock abstracts wall time.
type Clock interface{ Now() time.Time }

// System is the real clock.
type System struct{}

// Now returns the current time in IST.
func (System) Now() time.Time { return time.Now().In(IST) }

// Session holds the daily schedule as offsets from IST midnight.
type Session struct {
	PrepareAt, MarketOpen, MorningRun, MarketClose, EveningRun time.Duration
	holidays                                                   map[string]struct{}
}

func parseHMS(s string) (time.Duration, error) {
	t, err := time.Parse("15:04:05", s)
	if err != nil {
		return 0, fmt.Errorf("parse %q: %w", s, err)
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute + time.Duration(t.Second())*time.Second, nil
}

// NewSession parses the configured times.
func NewSession(sc config.SessionConfig, holidays []string) (*Session, error) {
	s := &Session{holidays: map[string]struct{}{}}
	for _, f := range []struct {
		dst *time.Duration
		v   string
	}{{&s.PrepareAt, sc.PrepareAt}, {&s.MarketOpen, sc.MarketOpen}, {&s.MorningRun, sc.MorningRun},
		{&s.MarketClose, sc.MarketEnd}, {&s.EveningRun, sc.EveningRun}} {
		d, err := parseHMS(f.v)
		if err != nil {
			return nil, err
		}
		*f.dst = d
	}
	if !(s.PrepareAt <= s.MarketOpen && s.MarketOpen < s.MorningRun && s.MorningRun < s.MarketClose && s.MarketClose < s.EveningRun) {
		return nil, fmt.Errorf("session times must satisfy prepare_at <= market_open < morning_run < market_close < evening_run")
	}
	for _, h := range holidays {
		s.holidays[h] = struct{}{}
	}
	return s, nil
}

// Midnight returns 00:00 IST of t's IST day.
func Midnight(t time.Time) time.Time {
	t = t.In(IST)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, IST)
}

// At returns offset on t's IST day.
func (s *Session) At(t time.Time, offset time.Duration) time.Time { return Midnight(t).Add(offset) }

// IsTradingDay reports whether NSE is open on t's day.
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

// PrevTradingDay returns midnight of the last trading day strictly before t's day.
func (s *Session) PrevTradingDay(t time.Time) time.Time {
	d := Midnight(t)
	for i := 0; i < 15; i++ {
		d = d.AddDate(0, 0, -1)
		if s.IsTradingDay(d) {
			return d
		}
	}
	return d
}

// MarketOpenNow reports whether t is inside market hours on a trading day.
func (s *Session) MarketOpenNow(t time.Time) bool {
	if !s.IsTradingDay(t) {
		return false
	}
	off := t.In(IST).Sub(Midnight(t))
	return off >= s.MarketOpen && off < s.MarketClose
}
