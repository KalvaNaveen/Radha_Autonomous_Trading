// Package config loads and validates the swing engine configuration (YAML).
package config

import (
	_ "embed"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration.
type Config struct {
	Mode     string         `yaml:"mode"` // "paper" or "live"
	Kite     KiteConfig     `yaml:"kite"`
	Network  NetworkConfig  `yaml:"network"`
	Paths    PathsConfig    `yaml:"paths"`
	Session  SessionConfig  `yaml:"session"`
	Strategy StrategyConfig `yaml:"strategy"`
	Risk     RiskConfig     `yaml:"risk"`
	Costs    CostsConfig    `yaml:"costs"`
	Orders   OrdersConfig   `yaml:"orders"`
	Market   MarketConfig   `yaml:"market"`
	Backtest BacktestConfig `yaml:"backtest"`
	Server   ServerConfig   `yaml:"server"`
	Holidays []string       `yaml:"holidays"`
	Dev      DevConfig      `yaml:"dev"`
}

// KiteConfig holds API credentials (they can also be entered in the control panel).
type KiteConfig struct {
	APIKey      string `yaml:"api_key"`
	APISecret   string `yaml:"api_secret"`
	AccessToken string `yaml:"access_token"`
}

// NetworkConfig binds order traffic to the SEBI-whitelisted static IP.
type NetworkConfig struct {
	BindIP         string        `yaml:"bind_ip"`
	RequestTimeout time.Duration `yaml:"request_timeout"`
}

// PathsConfig holds filesystem locations.
type PathsConfig struct {
	UniverseFile string `yaml:"universe_file"` // symbols the scanner may trade
	DataDir      string `yaml:"data_dir"`      // token, caches, portfolio state
	JournalDir   string `yaml:"journal_dir"`
	LogFile      string `yaml:"log_file"`
}

// SessionConfig holds the daily schedule in IST ("HH:MM:SS").
type SessionConfig struct {
	PrepareAt  string `yaml:"prepare_at"`  // login check, reconcile holdings
	MorningRun string `yaml:"morning_run"` // gap checks, planned exits, new entries
	MarketOpen string `yaml:"market_open"`
	MarketEnd  string `yaml:"market_close"`
	EveningRun string `yaml:"evening_run"` // daily candles final: manage stops, scan
}

// StrategyConfig holds the daily swing rules.
type StrategyConfig struct {
	EMAFast          int     `yaml:"ema_fast"`          // 20
	EMASlow          int     `yaml:"ema_slow"`          // 50
	ATRPeriod        int     `yaml:"atr_period"`        // 14
	VolumeAvgPeriod  int     `yaml:"volume_avg_period"` // 20
	BreakoutLookback int     `yaml:"breakout_lookback"` // 20-day high
	BreakoutVolRatio float64 `yaml:"breakout_volume_ratio"`
	PullbackLookback int     `yaml:"pullback_lookback"` // bars in which the low must touch EMA20
	PullbackTolPct   float64 `yaml:"pullback_tolerance_pct"`
	PullbackVolRatio float64 `yaml:"pullback_volume_ratio"`
	SlopeLookback    int     `yaml:"slope_lookback"`      // EMA50 must be rising over this many bars
	StopATRMult      float64 `yaml:"stop_atr_mult"`       // 2.0
	MinStopPct       float64 `yaml:"min_stop_pct"`        // 3
	MaxStopPct       float64 `yaml:"max_stop_pct"`        // 8
	BreakevenR       float64 `yaml:"breakeven_r"`         // 1.0
	LockR            float64 `yaml:"lock_r"`              // 2.0 → stop to +1R
	TrailATRMult     float64 `yaml:"trail_atr_mult"`      // 3.0 (chandelier)
	ExitBelowEMAFast bool    `yaml:"exit_below_ema_fast"` // after breakeven, close < EMA20 → exit
	MaxHoldBars      int     `yaml:"max_hold_bars"`       // time stop for trades going nowhere
	TimeStopMinR     float64 `yaml:"time_stop_min_r"`     // …if they have not reached this R
	MinPrice         float64 `yaml:"min_price"`
	MinTurnoverCr    float64 `yaml:"min_turnover_cr"` // average daily turnover, ₹ crore
	MaxGapUpPct      float64 `yaml:"max_gap_up_pct"`  // skip entries that open this far above the signal close
	RSLookback       int     `yaml:"rs_lookback"`     // relative-strength window, bars
	CooldownBars     int     `yaml:"cooldown_bars"`   // wait after an exit before re-entering the symbol
}

// RiskConfig holds portfolio limits.
type RiskConfig struct {
	Capital          float64 `yaml:"capital"`            // ₹ the engine may deploy (paper: starting cash)
	RiskPerTradePct  float64 `yaml:"risk_per_trade_pct"` // equity lost if the initial stop is hit
	MaxPositionPct   float64 `yaml:"max_position_pct"`   // max % of equity in one stock
	MaxPositions     int     `yaml:"max_positions"`
	MaxNewPerDay     int     `yaml:"max_new_per_day"`
	DrawdownPausePct float64 `yaml:"drawdown_pause_pct"` // no new entries while equity is this far below its peak
}

// CostsConfig models Zerodha equity-delivery charges.
type CostsConfig struct {
	BrokeragePct float64 `yaml:"brokerage_pct"`  // 0 for delivery
	STTPct       float64 `yaml:"stt_pct"`        // 0.1 each side
	StampBuyPct  float64 `yaml:"stamp_buy_pct"`  // 0.015 on buys
	ExchangePct  float64 `yaml:"exchange_pct"`   // 0.00307 NSE
	SEBIPerCrore float64 `yaml:"sebi_per_crore"` // ₹10
	GSTPct       float64 `yaml:"gst_pct"`        // 18 on brokerage+exchange+SEBI
	DPPerSell    float64 `yaml:"dp_per_sell"`    // ₹ per scrip per sell day
	SlippagePct  float64 `yaml:"slippage_pct"`   // backtest/paper fill slippage per side
}

// OrdersConfig holds order-manager parameters.
type OrdersConfig struct {
	MaxOPS              int           `yaml:"max_ops"`
	MaxPerMinute        int           `yaml:"max_per_minute"`
	MaxPerDay           int           `yaml:"max_per_day"`
	EmergencyReserve    int           `yaml:"emergency_reserve"`
	Workers             int           `yaml:"workers"`
	RetryBackoff        time.Duration `yaml:"retry_backoff"`
	EmergencyRetries    int           `yaml:"emergency_retries"`
	MarketProtection    float64       `yaml:"market_protection"`
	EntryLimitBufferPct float64       `yaml:"entry_limit_buffer_pct"` // entry limit above the ask
	GTTLimitBufferPct   float64       `yaml:"gtt_limit_buffer_pct"`   // stop GTT limit below the trigger
	FillTimeout         time.Duration `yaml:"fill_timeout"`
	EntryMaxAge         time.Duration `yaml:"entry_max_age"`
}

// MarketConfig holds the market-regime filter.
type MarketConfig struct {
	Index        string `yaml:"index"`         // "NIFTY 50"
	RegimeFilter bool   `yaml:"regime_filter"` // new longs only while the index closes above its slow EMA
}

// BacktestConfig holds defaults for the backtester.
type BacktestConfig struct {
	Years int `yaml:"years"`
}

// ServerConfig is the local control panel.
type ServerConfig struct {
	Listen    string `yaml:"listen"`
	PublicURL string `yaml:"public_url"`
}

// DevConfig points the engine at a Kite simulator. Refused in live mode.
type DevConfig struct {
	APIRoot   string `yaml:"api_root"`
	LoginRoot string `yaml:"login_root"`
}

// Defaults returns the full default configuration.
func Defaults() Config {
	return Config{
		Mode:    "paper",
		Network: NetworkConfig{RequestTimeout: 10 * time.Second},
		Paths:   PathsConfig{UniverseFile: "universe.csv", DataDir: "data", JournalDir: "journal", LogFile: "logs/engine.log"},
		Session: SessionConfig{PrepareAt: "08:45:00", MorningRun: "09:20:00", MarketOpen: "09:15:00",
			MarketEnd: "15:30:00", EveningRun: "15:50:00"},
		Strategy: StrategyConfig{
			EMAFast: 20, EMASlow: 50, ATRPeriod: 14, VolumeAvgPeriod: 20,
			BreakoutLookback: 20, BreakoutVolRatio: 1.5,
			PullbackLookback: 3, PullbackTolPct: 1.0, PullbackVolRatio: 1.0, SlopeLookback: 5,
			StopATRMult: 2.0, MinStopPct: 3, MaxStopPct: 8,
			BreakevenR: 1.0, LockR: 2.0, TrailATRMult: 3.0, ExitBelowEMAFast: true,
			MaxHoldBars: 40, TimeStopMinR: 1.0,
			MinPrice: 50, MinTurnoverCr: 10, MaxGapUpPct: 2.0, RSLookback: 60, CooldownBars: 5,
		},
		Risk: RiskConfig{Capital: 100000, RiskPerTradePct: 1.0, MaxPositionPct: 20, MaxPositions: 5,
			MaxNewPerDay: 2, DrawdownPausePct: 15},
		Costs: CostsConfig{BrokeragePct: 0, STTPct: 0.1, StampBuyPct: 0.015, ExchangePct: 0.00307,
			SEBIPerCrore: 10, GSTPct: 18, DPPerSell: 15.34, SlippagePct: 0.10},
		Orders: OrdersConfig{MaxOPS: 8, MaxPerMinute: 200, MaxPerDay: 2000, EmergencyReserve: 100, Workers: 4,
			RetryBackoff: 200 * time.Millisecond, EmergencyRetries: 3, MarketProtection: -1,
			EntryLimitBufferPct: 0.5, GTTLimitBufferPct: 1.0, FillTimeout: 20 * time.Second, EntryMaxAge: 2 * time.Minute},
		Market:   MarketConfig{Index: "NIFTY 50", RegimeFilter: true},
		Backtest: BacktestConfig{Years: 5},
		Server:   ServerConfig{Listen: "127.0.0.1:8080", PublicURL: "http://127.0.0.1:8080"},
	}
}

// Load reads YAML on top of Defaults() and validates.
func Load(path string) (Config, error) {
	cfg := Defaults()
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	if s := os.Getenv("KITE_API_SECRET"); s != "" {
		cfg.Kite.APISecret = s
	}
	if k := os.Getenv("KITE_API_KEY"); k != "" {
		cfg.Kite.APIKey = k
	}
	return cfg, cfg.Validate()
}

// Validate checks consistency.
func (c Config) Validate() error {
	var errs []error
	add := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if c.Mode != "live" && c.Mode != "paper" {
		add("mode must be live or paper, got %q", c.Mode)
	}
	if c.Network.BindIP != "" {
		if ip := net.ParseIP(c.Network.BindIP); ip == nil || ip.To4() == nil {
			add("network.bind_ip %q is not a valid IPv4 address", c.Network.BindIP)
		}
	} else if c.Mode == "live" {
		add("network.bind_ip is required in live mode (SEBI static-IP whitelisting)")
	}
	if c.Mode == "live" && (c.Dev.APIRoot != "" || c.Dev.LoginRoot != "") {
		add("dev.* overrides are not allowed in live mode")
	}
	s := c.Strategy
	if s.EMAFast < 2 || s.EMASlow <= s.EMAFast || s.ATRPeriod < 2 || s.VolumeAvgPeriod < 2 || s.BreakoutLookback < 2 {
		add("strategy: periods must be >= 2 and ema_fast < ema_slow")
	}
	if s.MinStopPct <= 0 || s.MaxStopPct < s.MinStopPct || s.StopATRMult <= 0 {
		add("strategy: need 0 < min_stop_pct <= max_stop_pct and stop_atr_mult > 0")
	}
	if s.BreakevenR <= 0 || s.LockR <= s.BreakevenR {
		add("strategy: need 0 < breakeven_r < lock_r")
	}
	r := c.Risk
	if r.Capital <= 0 || r.RiskPerTradePct <= 0 || r.RiskPerTradePct > 5 || r.MaxPositions < 1 || r.MaxPositionPct <= 0 || r.MaxPositionPct > 100 {
		add("risk: capital > 0, 0 < risk_per_trade_pct <= 5, max_positions >= 1, 0 < max_position_pct <= 100")
	}
	if c.Orders.MaxOPS < 1 || c.Orders.MaxOPS >= 10 {
		add("orders.max_ops must be 1..9 (SEBI registration threshold is 10 OPS)")
	}
	if c.Orders.MarketProtection == 0 {
		add("orders.market_protection must be non-zero (Kite rejects 0); use -1 for auto")
	}
	if c.Orders.Workers < 1 {
		add("orders.workers must be >= 1")
	}
	for _, h := range c.Holidays {
		if _, err := time.Parse("2006-01-02", h); err != nil {
			add("holiday %q is not YYYY-MM-DD", h)
		}
	}
	for name, v := range map[string]string{"prepare_at": c.Session.PrepareAt, "morning_run": c.Session.MorningRun,
		"market_open": c.Session.MarketOpen, "market_close": c.Session.MarketEnd, "evening_run": c.Session.EveningRun} {
		if _, err := time.Parse("15:04:05", v); err != nil {
			add("session.%s %q is not HH:MM:SS", name, v)
		}
	}
	return errors.Join(errs...)
}

//go:embed default.yaml
var defaultYAML []byte

// WriteDefault writes the annotated default config if path does not exist.
func WriteDefault(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return false, err
		}
	}
	return true, os.WriteFile(path, defaultYAML, 0o644)
}

// DefaultYAML exposes the embedded template (for config.example.yaml sync tests).
func DefaultYAML() []byte { return defaultYAML }
