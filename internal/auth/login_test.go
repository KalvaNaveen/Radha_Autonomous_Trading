package auth

import (
	"testing"
	"time"

	"github.com/nkalva/kitealgo/internal/clock"
)

func TestTokenExpiryAt6AM(t *testing.T) {
	d := time.Date(2026, 9, 28, 0, 0, 0, 0, clock.IST)
	tok := Token{AccessToken: "x", CreatedAt: d.Add(8 * time.Hour)} // 08:00 Mon
	if !tok.ValidAt(d.Add(15 * time.Hour)) {
		t.Fatal("valid same day")
	}
	if !tok.ValidAt(d.Add(29 * time.Hour)) { // 05:00 Tue
		t.Fatal("valid until 06:00 next day")
	}
	if tok.ValidAt(d.Add(30*time.Hour + time.Minute)) { // 06:01 Tue
		t.Fatal("expired after 06:00")
	}
}
