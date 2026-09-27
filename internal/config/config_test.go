package config

import (
	"strings"
	"testing"
)

func TestDefaultsNeedCredentialsOnly(t *testing.T) {
	c := Defaults()
	c.Kite.APIKey, c.Kite.APISecret = "k", "s"
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults should validate in paper mode: %v", err)
	}
}

func TestValidateCatchesDangerousValues(t *testing.T) {
	c := Defaults()
	c.Kite.APIKey, c.Kite.APISecret = "k", "s"
	c.Mode = "live"                       // needs bind_ip
	c.Orders.MaxOPS = 10                  // at the SEBI threshold
	c.Orders.MarketProtection = 0         // Kite rejects
	c.Strategy.ProfitLockTriggerPct = 0.4 // lock stop would sit at market
	err := c.Validate()
	for _, want := range []string{"bind_ip", "max_ops", "market_protection", "profit_lock_trigger_pct"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("expected validation error mentioning %s, got %v", want, err)
		}
	}
}
