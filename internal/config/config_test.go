package config

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDefaultsValidate(t *testing.T) {
	if err := Defaults().Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
}

func TestEmbeddedYAMLMatchesDefaults(t *testing.T) {
	var c Config = Defaults()
	if err := yaml.Unmarshal(DefaultYAML(), &c); err != nil {
		t.Fatal(err)
	}
	if c.Strategy != Defaults().Strategy || c.Risk != Defaults().Risk || c.Costs != Defaults().Costs {
		t.Fatalf("default.yaml drifted from Defaults():\n%+v\n%+v", c.Strategy, Defaults().Strategy)
	}
	ex, err := os.ReadFile("../../config.example.yaml")
	if err == nil && !bytes.Equal(ex, DefaultYAML()) {
		t.Fatal("config.example.yaml must be identical to internal/config/default.yaml")
	}
}

func TestValidateCatchesDangerousValues(t *testing.T) {
	c := Defaults()
	c.Mode = "live"
	c.Orders.MaxOPS = 10
	c.Orders.MarketProtection = 0
	c.Risk.RiskPerTradePct = 10
	err := c.Validate()
	for _, want := range []string{"bind_ip", "max_ops", "market_protection", "risk_per_trade_pct"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("expected error mentioning %s, got %v", want, err)
		}
	}
}
