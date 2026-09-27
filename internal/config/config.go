// Package config loads and validates the engine configuration (YAML).
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration object.
type Config struct {
	Mode     string         `yaml:"mode"` // "live" or "paper"
	Kite     KiteConfig     `yaml:"kite"`
	Network  NetworkConfig  `yaml:"network"`
	Paths    PathsConfig    `yaml:"paths"`
	Session  SessionConfig  `yaml:"session"`
	Strategy StrategyConfig `yaml:"strategy"`
	Risk     RiskConfig     `yaml:"risk"`
	Orders   OrdersConfig   `yaml:"orders"`
	Radar    RadarConfig    `yaml:"radar"`
	Ticker   TickerConfig   `yaml:"ticker"`
	Server   ServerConfig   `yaml:"server"`
	Holidays []string       `yaml:"holidays"` // YYYY-MM-DD, NSE trading holidays
}

// KiteConfig holds API credentials. The secret may come from the environment.
type KiteConfig struct {
	APIKey    string `yaml:"api_key"`
	APISecret string `yaml:"api_secret"` // or env KITE_API_SECRET
	// AccessToken optionally pins a token (e.g. for testing). Normally empty:
	// the daily login flow stores the token in Paths.TokenFile.
	AccessToken string `yaml:"access_token"`
}

// NetworkConfig binds order traffic to the SEBI-whitelisted static IP.
type NetworkConfig struct {
	// BindIP is the local IPv4 address the REST client dials from. It must be
	// the static IP registered in the Kite developer console. Empty = OS default
	// (only correct if the host has exactly one public IP and it is the registered one).
	BindIP         string        `yaml:"bind_ip"`
	RequestTimeout time.Duration `yaml:"request_timeout"`
}

// PathsConfig holds filesystem locations.
type PathsConfig struct {
	WatchlistDir string `yaml:"watchlist_dir"`
	DataDir      string `yaml:"data_dir"` // history cache, token file
	JournalDir   string `yaml:"journal_dir"`
	LogFile      string `yaml:"log_file"`
}

// SessionConfig holds the IST clock times. All strings are "HH:MM:SS".
type SessionConfig struct {
	PrepareAt    string `yaml:"prepare_at"`    // load watchlist + history
	MarketOpen   string `yaml:"market_open"`   // 09:15:00
	TradingStart string `yaml:"trading_start"` // 09:45:00 end of warmup
	EntryCutoff  string `yaml:"entry_cutoff"`  // 14:45:00 exit-only after this
	SquareOff    string `yaml:"square_off"`    // kill switch
	SafetySweep  string `yaml:"safety_sweep"`  // verify flat at broker
	SessionEnd   string `yaml:"session_end"`   // shut the day down
}

// StrategyConfig holds the signal parameters.
type StrategyConfig struct {
	CandleInterval       time.Duration `yaml:"candle_interval"`         // 5m
	CandleGrace          time.Duration `yaml:"candle_grace"`            // wait for late ticks before closing a bar
	FastEMA              int           `yaml:"fast_ema"`                // 10
	SlowEMA              int           `yaml:"slow_ema"`                // 20
	VWAPBufferPct        float64       `yaml:"vwap_buffer_pct"`         // 0.20
	VWAPSource           string        `yaml:"vwap_source"`             // "exchange" | "computed"
	MinRVOL              float64       `yaml:"min_rvol"`                // 1.5
	RVOLLookbackDays     int           `yaml:"rvol_lookback_days"`      // 10
	BandTolerancePct     float64       `yaml:"band_tolerance_pct"`      // 0.15
	PullbackCandles      int           `yaml:"pullback_candles"`        // 2 (incl. the crossover bar)
	MaxStopPct           float64       `yaml:"max_stop_pct"`            // 0.80
	MinStopPct           float64       `yaml:"min_stop_pct"`            // 0.25
	RoundTripCostPct     float64       `yaml:"round_trip_cost_pct"`     // 0.15
	BreakevenTriggerPct  float64       `yaml:"breakeven_trigger_pct"`   // 0.40 gross
	ProfitLockTriggerPct float64       `yaml:"profit_lock_trigger_pct"` // 0.65 gross
	MinNetProfitPct      float64       `yaml:"min_net_profit_pct"`      // 0.25 net → stop at entry ± (0.25+0.15)
	TrailBufferPct       float64       `yaml:"trail_buffer_pct"`        // 0.25 below/above 10 EMA
	MinStopStepPct       float64       `yaml:"min_stop_step_pct"`       // 0.05 (saves Kite's 25-mod budget)
	MaxTradesPerSymbol   int           `yaml:"max_trades_per_symbol"`
	CooldownCandles      int           `yaml:"cooldown_candles"`
	EntryTimeout         time.Duration `yaml:"entry_timeout"`
}

// RiskConfig holds portfolio-level limits.
type RiskConfig struct {
	Capital           float64       `yaml:"capital"`              // ₹ allocated to the engine
	RiskPerTradePct   float64       `yaml:"risk_per_trade_pct"`   // 0.5 (% of capital lost if stopped at initial SL)
	MaxOpenPositions  int           `yaml:"max_open_positions"`   // 10
	MISLeverage       float64       `yaml:"mis_leverage"`         // 5 (conservative planning figure)
	DailyLossLimitPct float64       `yaml:"daily_loss_limit_pct"` // 2.0 → flatten all + halt
	MarginBufferPct   float64       `yaml:"margin_buffer_pct"`    // keep 10% of margin unused
	MarginRefresh     time.Duration `yaml:"margin_refresh"`
}

// OrdersConfig holds order-manager parameters.
type OrdersConfig struct {
	MaxOPS              int           `yaml:"max_ops"`                // 8 (SEBI threshold is 10)
	MaxPerMinute        int           `yaml:"max_per_minute"`         // 350 (Kite: 400)
	MaxPerDay           int           `yaml:"max_per_day"`            // 4500 (Kite: 5000)
	EmergencyReserve    int           `yaml:"emergency_reserve"`      // daily orders kept back for exits
	Workers             int           `yaml:"workers"`                // concurrent in-flight REST calls
	StopRetryBackoff    time.Duration `yaml:"stop_retry_backoff"`     // 100ms
	EmergencyRetries    int           `yaml:"emergency_retries"`      // 3
	MarketProtection    float64       `yaml:"market_protection"`      // -1 = Kite auto, else percent
	EntryLimitBufferPct float64       `yaml:"entry_limit_buffer_pct"` // 0.5 beyond best bid/ask
	MaxModsPerOrder     int           `yaml:"max_mods_per_order"`     // 24 (Kite: 25) then cancel-and-replace
	ReconcileInterval   time.Duration `yaml:"reconcile_interval"`
	// EntryMaxAge drops entries that waited in the queue longer than this
	// (e.g. 50 simultaneous signals at 8 OPS): a stale signal is not a signal.
	EntryMaxAge time.Duration `yaml:"entry_max_age"`
}

// RadarConfig configures the market-context filter.
type RadarConfig struct {
	MarketIndex string `yaml:"market_index"` // "NIFTY 50"
	// Mode: "strict" requires the benchmark to agree; "permissive" only blocks
	// trades against a benchmark that clearly disagrees (neutral is allowed).
	Mode       string  `yaml:"mode"`
	NeutralPct float64 `yaml:"neutral_pct"` // |ltp-open|/open below this = neutral
}

// TickerConfig configures the WebSocket fan-out.
type TickerConfig struct {
	AgentBuffer       int           `yaml:"agent_buffer"`
	ReconnectMaxDelay time.Duration `yaml:"reconnect_max_delay"`
	StaleAfter        time.Duration `yaml:"stale_after"` // no ticks for this long = feed alarm
}

// ServerConfig is the local HTTP server (daily login callback + status).
type ServerConfig struct {
	Listen string `yaml:"listen"` // e.g. ":8080"
	// PublicURL is how you reach this server from your browser. The Kite app's
	// redirect URL must be set to PublicURL + "/kite/callback".
	PublicURL string `yaml:"public_url"`
}

// Defaults returns a fully populated configuration with the strategy's spec values.
func Defaults() Config {
	return Config{
		Mode:    "paper",
		Network: NetworkConfig{RequestTimeout: 5 * time.Second},
		Paths: PathsConfig{
			WatchlistDir: "watchlists",
			DataDir:      "data",
			JournalDir:   "journal",
		},
		Session: SessionConfig{
			PrepareAt:    "08:30:00",
			MarketOpen:   "09:15:00",
			TradingStart: "09:45:00",
			EntryCutoff:  "14:45:00",
			SquareOff:    "15:08:00",
			SafetySweep:  "15:09:30",
			SessionEnd:   "15:35:00",
		},
		Strategy: StrategyConfig{
			CandleInterval:       5 * time.Minute,
			CandleGrace:          2 * time.Second,
			FastEMA:              10,
			SlowEMA:              20,
			VWAPBufferPct:        0.20,
			VWAPSource:           "exchange",
			MinRVOL:              1.5,
			RVOLLookbackDays:     10,
			BandTolerancePct:     0.15,
			PullbackCandles:      2,
			MaxStopPct:           0.80,
			MinStopPct:           0.25,
			RoundTripCostPct:     0.15,
			BreakevenTriggerPct:  0.40,
			ProfitLockTriggerPct: 0.65,
			MinNetProfitPct:      0.25,
			TrailBufferPct:       0.25,
			MinStopStepPct:       0.05,
			MaxTradesPerSymbol:   3,
			CooldownCandles:      1,
			EntryTimeout:         15 * time.Second,
		},
		Risk: RiskConfig{
			Capital:           100000,
			RiskPerTradePct:   0.5,
			MaxOpenPositions:  10,
			MISLeverage:       5,
			DailyLossLimitPct: 2.0,
			MarginBufferPct:   10,
			MarginRefresh:     30 * time.Second,
		},
		Orders: OrdersConfig{
			MaxOPS:              8,
			MaxPerMinute:        350,
			MaxPerDay:           4500,
			EmergencyReserve:    300,
			Workers:             8,
			StopRetryBackoff:    100 * time.Millisecond,
			EmergencyRetries:    3,
			MarketProtection:    -1,
			EntryLimitBufferPct: 0.5,
			MaxModsPerOrder:     24,
			ReconcileInterval:   3 * time.Second,
			EntryMaxAge:         10 * time.Second,
		},
		Radar: RadarConfig{MarketIndex: "NIFTY 50", Mode: "permissive", NeutralPct: 0.10},
		Ticker: TickerConfig{
			AgentBuffer:       512,
			ReconnectMaxDelay: 10 * time.Second,
			StaleAfter:        30 * time.Second,
		},
		Server: ServerConfig{Listen: "127.0.0.1:8080", PublicURL: "http://127.0.0.1:8080"},
	}
}

// Load reads the YAML file at path on top of Defaults() and validates it.
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

// Validate checks internal consistency. It is deliberately strict: a bad
// config should stop the engine before 09:15, not during the session.
func (c Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.Mode != "live" && c.Mode != "paper" {
		add("mode must be live or paper, got %q", c.Mode)
	}
	if c.Kite.APIKey == "" {
		add("kite.api_key is required")
	}
	if c.Kite.APISecret == "" && c.Kite.AccessToken == "" {
		add("kite.api_secret (or env KITE_API_SECRET) is required for the daily login")
	}
	if c.Network.BindIP != "" {
		ip := net.ParseIP(c.Network.BindIP)
		if ip == nil || ip.To4() == nil {
			add("network.bind_ip %q is not a valid IPv4 address", c.Network.BindIP)
		}
	} else if c.Mode == "live" {
		add("network.bind_ip is required in live mode (SEBI static-IP whitelisting)")
	}
	if c.Orders.MaxOPS < 1 || c.Orders.MaxOPS >= 10 {
		add("orders.max_ops must be 1..9 (SEBI registration threshold is 10 OPS), got %d", c.Orders.MaxOPS)
	}
	if c.Orders.MaxPerMinute < 1 || c.Orders.MaxPerMinute > 400 {
		add("orders.max_per_minute must be 1..400")
	}
	if c.Orders.MaxPerDay < 1 || c.Orders.MaxPerDay > 5000 {
		add("orders.max_per_day must be 1..5000")
	}
	if c.Orders.MaxModsPerOrder < 1 || c.Orders.MaxModsPerOrder > 25 {
		add("orders.max_mods_per_order must be 1..25")
	}
	if c.Orders.MarketProtection == 0 {
		add("orders.market_protection must be non-zero (Kite rejects 0); use -1 for auto")
	}
	if c.Orders.Workers < 1 {
		add("orders.workers must be >= 1")
	}
	s := c.Strategy
	if s.FastEMA < 1 || s.SlowEMA <= s.FastEMA {
		add("strategy: need 1 <= fast_ema < slow_ema")
	}
	if s.MinStopPct <= 0 || s.MaxStopPct < s.MinStopPct {
		add("strategy: need 0 < min_stop_pct <= max_stop_pct")
	}
	if s.BreakevenTriggerPct <= s.RoundTripCostPct {
		add("strategy: breakeven_trigger_pct must exceed round_trip_cost_pct")
	}
	lockStop := s.MinNetProfitPct + s.RoundTripCostPct
	if s.ProfitLockTriggerPct <= lockStop {
		add("strategy: profit_lock_trigger_pct (%.2f) must exceed min_net_profit_pct+round_trip_cost_pct (%.2f), otherwise the lock stop sits at market", s.ProfitLockTriggerPct, lockStop)
	}
	if s.VWAPSource != "exchange" && s.VWAPSource != "computed" {
		add("strategy.vwap_source must be exchange or computed")
	}
	if s.CandleInterval <= 0 || (24*time.Hour)%s.CandleInterval != 0 {
		add("strategy.candle_interval must divide a day evenly")
	}
	if c.Risk.Capital <= 0 || c.Risk.RiskPerTradePct <= 0 || c.Risk.MaxOpenPositions < 1 || c.Risk.MISLeverage < 1 {
		add("risk: capital, risk_per_trade_pct, max_open_positions, mis_leverage must be positive")
	}
	if c.Radar.Mode != "strict" && c.Radar.Mode != "permissive" && c.Radar.Mode != "off" {
		add("radar.mode must be strict, permissive or off")
	}
	for _, h := range c.Holidays {
		if _, err := time.Parse("2006-01-02", h); err != nil {
			add("holiday %q is not YYYY-MM-DD", h)
		}
	}
	for name, v := range map[string]string{
		"prepare_at": c.Session.PrepareAt, "market_open": c.Session.MarketOpen,
		"trading_start": c.Session.TradingStart, "entry_cutoff": c.Session.EntryCutoff,
		"square_off": c.Session.SquareOff, "safety_sweep": c.Session.SafetySweep,
		"session_end": c.Session.SessionEnd,
	} {
		if _, err := time.Parse("15:04:05", v); err != nil {
			add("session.%s %q is not HH:MM:SS", name, v)
		}
	}
	return errors.Join(errs...)
}
