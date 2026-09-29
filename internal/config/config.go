// Package config loads and validates the swing engine configuration (YAML).
package config

import (
	_ "embed"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
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
	MTF      MTFConfig      `yaml:"mtf"`
	Orders   OrdersConfig   `yaml:"orders"`
	Market   MarketConfig   `yaml:"market"`
	Backtest BacktestConfig `yaml:"backtest"`
	Holdings HoldingsConfig `yaml:"holdings"`
	Research ResearchConfig `yaml:"research"`
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

	Setups string `yaml:"setups"` // ema_cross | breakout | pullback | both (= breakout + pullback)

	// ema_cross setup: EMA(fast) crosses above EMA(slow) while Supertrend is green.
	CrossFast        int     `yaml:"ema_cross_fast"`     // 10
	CrossSlow        int     `yaml:"ema_cross_slow"`     // 20
	SupertrendPeriod int     `yaml:"supertrend_period"`  // 10
	SupertrendMult   float64 `yaml:"supertrend_mult"`    // 3
	CrossTrendFilter bool    `yaml:"cross_trend_filter"` // also require close > rising EMA50
	CrossSource      string  `yaml:"cross_source"`       // close | ha — compute the EMA cross and Supertrend on real or Heikin-Ashi candles
	ExitOnEMACross   bool    `yaml:"exit_on_ema_cross"`  // EMA(fast) closes below EMA(slow) → exit next morning
	StopMode         string  `yaml:"stop_mode"`          // atr (entry − stop_atr_mult × ATR) | supertrend (the green line)
	FixedStop        bool    `yaml:"fixed_stop"`         // true: no legacy breakeven/lock/trail ratchet (the options below still apply)
	SwingLowBars     int     `yaml:"swing_low_bars"`     // stop_mode swing_low: lowest low of this many bars
	BreakevenAtR     float64 `yaml:"breakeven_at_r"`     // move the stop to entry + costs once the close reaches +this R (0 = off)
	TrailMode        string  `yaml:"trail_mode"`         // off | atr (highest close − trail_atr_mult × ATR) | supertrend (green line)
	TrailStartR      float64 `yaml:"trail_start_r"`      // start trailing once the close reaches +this R (0 = from entry)
	TargetR          float64 `yaml:"target_r"`           // profit target at entry + this R (0 = off) — backtest only for now
	PartialPct       float64 `yaml:"partial_pct"`        // % of the position sold at the target (100 = all); rest trails, stop → breakeven
	RegimeMode       string  `yaml:"regime_mode"`        // basic: index > rising EMA50 · strict: also index > EMA20 > EMA50

	// ema_cross entry mode: cross = only on the day the cross turns bullish;
	// trend = also a stock already in that uptrend, if the cross is at most
	// trend_max_days old and the close at most trend_max_ext_pct above the
	// slow cross EMA; both = fresh crosses first, then trending stocks.
	EntryMode      string  `yaml:"entry_mode"`        // cross | trend | both
	TrendMaxDays   int     `yaml:"trend_max_days"`    // 15
	TrendMaxExtPct float64 `yaml:"trend_max_ext_pct"` // 8

	// Research tags — why a stock came up in the scan (RESULTS, TURNAROUND,
	// NEW_HIGH, MOMENTUM, NONE). research_allow limits entries to stocks
	// with one of the listed tags ("all" = no filter); research_rank
	// "research" ranks candidates by tag strength before relative strength.
	ResearchAllow string `yaml:"research_allow"` // all | comma list, e.g. RESULTS,NEW_HIGH
	ResearchRank  string `yaml:"research_rank"`  // rs | research

	// Heikin-Ashi filters (signals only — orders, stops and sizing use real prices).
	HAEntry      string  `yaml:"ha_entry"`       // off | green | strong (green with no lower wick)
	HAExit       string  `yaml:"ha_exit"`        // off | red (ha_exit_bars red HA candles in a row) | strong_red
	HAExitBars   int     `yaml:"ha_exit_bars"`   // for ha_exit: red
	HAExitAlways bool    `yaml:"ha_exit_always"` // apply the HA exit before breakeven too
	HAWickPct    float64 `yaml:"ha_wick_pct"`    // a wick ≤ this % of the HA range counts as "no wick"
}

// RiskConfig holds portfolio limits.
type RiskConfig struct {
	Capital           float64 `yaml:"capital"`            // ₹ the engine may deploy (paper: starting cash)
	RiskPerTradePct   float64 `yaml:"risk_per_trade_pct"` // equity lost if the initial stop is hit
	MaxPositionPct    float64 `yaml:"max_position_pct"`   // max % of equity in one stock
	MaxPositions      int     `yaml:"max_positions"`
	MaxNewPerDay      int     `yaml:"max_new_per_day"`
	DrawdownPausePct  float64 `yaml:"drawdown_pause_pct"`  // equity this far below its peak pauses new entries…
	DrawdownPauseDays int     `yaml:"drawdown_pause_days"` // …for this many trading days, then the peak resets
}

// PauseDays is the drawdown pause length (0 in an older config means the default 20).
func (r RiskConfig) PauseDays() int {
	if r.DrawdownPauseDays <= 0 {
		return 20
	}
	return r.DrawdownPauseDays
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

// MTFConfig models Zerodha's Margin Trading Facility (buy with borrowed
// money on approved stocks). Backtest-only for now.
type MTFConfig struct {
	Enabled             bool    `yaml:"enabled"`
	Leverage            float64 `yaml:"leverage"`              // 2 = you fund 50%, Zerodha lends 50% (max 4–5× by stock)
	BorrowOnlyShortfall bool    `yaml:"borrow_only_shortfall"` // true: pay cash first, borrow only what is missing
	InterestPctPerDay   float64 `yaml:"interest_pct_per_day"`  // 0.04 (₹40 per lakh per day, from T+1, calendar days)
	BrokeragePct        float64 `yaml:"brokerage_pct"`         // 0.3% per order…
	BrokerageMax        float64 `yaml:"brokerage_max"`         // …capped at ₹20
	PledgeFee           float64 `yaml:"pledge_fee"`            // ₹15 + GST per stock per buy day
	UnpledgeFee         float64 `yaml:"unpledge_fee"`          // ₹15 + GST per stock per sell
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
	Years        int                `yaml:"years"`
	Capital      float64            `yaml:"capital"` // ₹ starting capital for backtests (0 = risk.capital)
	AutoUniverse AutoUniverseConfig `yaml:"auto_universe"`
}

// AutoUniverseConfig picks the stocks to scan by rule instead of by hand, so
// the backtest has no hindsight in its stock list: on the first session of
// every month, the TopN strongest stocks (return over LookbackDays sessions)
// among those passing min_price and min_turnover_cr on that date.
type AutoUniverseConfig struct {
	Enabled      bool   `yaml:"enabled"`
	Source       string `yaml:"source"`        // all_nse (every NSE share) | file (a broad list, e.g. NIFTY 500 CSV)
	File         string `yaml:"file"`          // for source: file
	TopN         int    `yaml:"top_n"`         // stocks scanned each month
	LookbackDays int    `yaml:"lookback_days"` // strength window, sessions
}

// HoldingsConfig is the "hold N stocks" portfolio mode — backtest only for
// now. The capital is split equally over Slots stocks; every evening each
// holding is checked against the exit rules, and any empty slot is refilled
// the next morning with the best stock in the universe that meets the entry
// rules (no daily limit on new buys; risk-based sizing is not used).
type HoldingsConfig struct {
	Enabled bool `yaml:"enabled"`
	Slots   int  `yaml:"slots"` // stocks held at once
	// Market check before a buy: off | nifty (NIFTY 50 for every stock) |
	// category (the stock's own index: NIFTY 50, midcap or smallcap index,
	// from caps_file). The index must pass regime_mode and, with
	// market_ha_green, show a green Heikin-Ashi candle.
	MarketCheck   string `yaml:"market_check"`
	MarketHAGreen bool   `yaml:"market_ha_green"`
	MidcapIndex   string `yaml:"midcap_index"`   // "NIFTY MIDCAP 150"
	SmallcapIndex string `yaml:"smallcap_index"` // "NIFTY SMLCAP 250"
	CapsFile      string `yaml:"caps_file"`      // SYMBOL,large|mid|small per line; unlisted stocks count as large
}

// ResearchConfig is the AI web research on entry candidates (paper/live
// only), done in the evening run with Google Gemini + Google Search.
type ResearchConfig struct {
	AI           string `yaml:"ai"`             // off | note (attach the note) | gate (also drop AVOID verdicts)
	GeminiAPIKey string `yaml:"gemini_api_key"` // or env GEMINI_API_KEY, or saved from the control panel
	Model        string `yaml:"model"`          // gemini-2.5-flash: its free tier includes Google Search (500/day)
	MaxPerDay    int    `yaml:"max_per_day"`    // candidates researched each evening (strongest first)
}

// SlotCount is the number of holdings (defaults to max_positions).
func (c Config) SlotCount() int {
	if c.Holdings.Slots > 0 {
		return c.Holdings.Slots
	}
	return c.Risk.MaxPositions
}

// ForBacktest is the config a backtest runs with: backtest.capital (when set)
// replaces risk.capital as the starting capital.
func (c Config) ForBacktest() Config {
	if c.Backtest.Capital > 0 {
		c.Risk.Capital = c.Backtest.Capital
	}
	return c
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
			StopATRMult: 3.0, MinStopPct: 3, MaxStopPct: 8,
			BreakevenR: 1.0, LockR: 2.0, TrailATRMult: 3.0, ExitBelowEMAFast: false,
			MaxHoldBars: 0, TimeStopMinR: 1.0,
			MinPrice: 50, MinTurnoverCr: 10, MaxGapUpPct: 2.0, RSLookback: 60, CooldownBars: 5,
			Setups: "ema_cross", RegimeMode: "strict",
			CrossFast: 10, CrossSlow: 20, SupertrendPeriod: 10, SupertrendMult: 3, CrossSource: "close",
			ExitOnEMACross: true, StopMode: "atr", FixedStop: true,
			SwingLowBars: 10, BreakevenAtR: 1.5, TrailMode: "off", TrailStartR: 2, TargetR: 0, PartialPct: 100,
			HAEntry: "green", HAExit: "off", HAExitBars: 2, HAWickPct: 10,
			EntryMode: "cross", TrendMaxDays: 15, TrendMaxExtPct: 8, ResearchAllow: "all", ResearchRank: "rs",
		},
		Risk: RiskConfig{Capital: 100000, RiskPerTradePct: 1.0, MaxPositionPct: 20, MaxPositions: 5,
			MaxNewPerDay: 2, DrawdownPausePct: 15, DrawdownPauseDays: 20},
		MTF: MTFConfig{Enabled: false, Leverage: 2, InterestPctPerDay: 0.04, BrokeragePct: 0.3, BrokerageMax: 20, PledgeFee: 15, UnpledgeFee: 15},
		Costs: CostsConfig{BrokeragePct: 0, STTPct: 0.1, StampBuyPct: 0.015, ExchangePct: 0.00307,
			SEBIPerCrore: 10, GSTPct: 18, DPPerSell: 15.34, SlippagePct: 0.10},
		Orders: OrdersConfig{MaxOPS: 8, MaxPerMinute: 200, MaxPerDay: 2000, EmergencyReserve: 100, Workers: 4,
			RetryBackoff: 200 * time.Millisecond, EmergencyRetries: 3, MarketProtection: -1,
			EntryLimitBufferPct: 0.5, GTTLimitBufferPct: 1.0, FillTimeout: 20 * time.Second, EntryMaxAge: 2 * time.Minute},
		Market:   MarketConfig{Index: "NIFTY 50", RegimeFilter: true},
		Backtest: BacktestConfig{Years: 5, Capital: 500000,
			AutoUniverse: AutoUniverseConfig{Source: "all_nse", File: "nifty500.csv", TopN: 50, LookbackDays: 120}},
		Research: ResearchConfig{AI: "off", Model: "gemini-2.5-flash", MaxPerDay: 8},
		Holdings: HoldingsConfig{Enabled: false, Slots: 5, MarketCheck: "off", MarketHAGreen: true,
			MidcapIndex: "NIFTY MIDCAP 150", SmallcapIndex: "NIFTY SMLCAP 250", CapsFile: "caps.csv"},
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
	if k := os.Getenv("GEMINI_API_KEY"); k != "" {
		cfg.Research.GeminiAPIKey = k
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
	if !oneOf(s.Setups, "", "both", "breakout", "pullback", "ema_cross") {
		add("strategy.setups must be ema_cross, breakout, pullback or both, got %q", s.Setups)
	}
	if s.Setups == "ema_cross" && s.CrossFast > 0 && s.CrossSlow <= s.CrossFast {
		add("strategy: ema_cross_fast must be below ema_cross_slow")
	}
	if !oneOf(s.CrossSource, "", "close", "ha") {
		add("strategy.cross_source must be close or ha, got %q", s.CrossSource)
	}
	if !oneOf(s.StopMode, "", "atr", "supertrend", "swing_low") {
		add("strategy.stop_mode must be atr, supertrend or swing_low, got %q", s.StopMode)
	}
	if !oneOf(s.TrailMode, "", "off", "atr", "supertrend") {
		add("strategy.trail_mode must be off, atr or supertrend, got %q", s.TrailMode)
	}
	if s.TargetR < 0 || s.PartialPct < 0 || s.PartialPct > 100 {
		add("strategy: target_r must be >= 0 and partial_pct 0..100")
	}
	if !oneOf(s.RegimeMode, "", "basic", "strict") {
		add("strategy.regime_mode must be basic or strict, got %q", s.RegimeMode)
	}
	if !oneOf(s.HAEntry, "", "off", "green", "strong") {
		add("strategy.ha_entry must be off, green or strong, got %q", s.HAEntry)
	}
	if !oneOf(s.HAExit, "", "off", "red", "strong_red") {
		add("strategy.ha_exit must be off, red or strong_red, got %q", s.HAExit)
	}
	if s.HAExit == "red" && s.HAExitBars < 1 {
		add("strategy.ha_exit_bars must be >= 1")
	}
	if !oneOf(s.EntryMode, "", "cross", "trend", "both") {
		add("strategy.entry_mode must be cross, trend or both, got %q", s.EntryMode)
	}
	if s.EntryMode == "trend" || s.EntryMode == "both" {
		if s.TrendMaxDays < 1 || s.TrendMaxExtPct <= 0 {
			add("strategy: trend_max_days must be >= 1 and trend_max_ext_pct > 0")
		}
	}
	if !oneOf(s.ResearchRank, "", "rs", "research") {
		add("strategy.research_rank must be rs or research, got %q", s.ResearchRank)
	}
	if a := strings.TrimSpace(s.ResearchAllow); a != "" && !strings.EqualFold(a, "all") {
		for _, t := range strings.Split(a, ",") {
			if !oneOf(strings.ToUpper(strings.TrimSpace(t)), "RESULTS", "TURNAROUND", "NEW_HIGH", "MOMENTUM", "NONE") {
				add("strategy.research_allow: unknown tag %q (use RESULTS, TURNAROUND, NEW_HIGH, MOMENTUM, NONE or all)", t)
			}
		}
	}
	if h := c.Holdings; h.Enabled {
		if h.Slots < 1 || h.Slots > 50 {
			add("holdings.slots must be 1..50")
		}
		if !oneOf(h.MarketCheck, "", "off", "nifty", "category") {
			add("holdings.market_check must be off, nifty or category, got %q", h.MarketCheck)
		}
	}
	if !oneOf(c.Research.AI, "", "off", "note", "gate") {
		add("research.ai must be off, note or gate, got %q", c.Research.AI)
	}
	if c.Backtest.Capital < 0 {
		add("backtest.capital must be >= 0")
	}
	if a := c.Backtest.AutoUniverse; a.Enabled {
		if !oneOf(a.Source, "all_nse", "file") {
			add("backtest.auto_universe.source must be all_nse or file, got %q", a.Source)
		}
		if a.TopN < 5 || a.TopN > 500 || a.LookbackDays < 20 || a.LookbackDays > 500 {
			add("backtest.auto_universe: top_n 5..500 and lookback_days 20..500")
		}
	}
	r := c.Risk
	if r.Capital <= 0 || r.RiskPerTradePct <= 0 || r.RiskPerTradePct > 5 || r.MaxPositions < 1 || r.MaxPositionPct <= 0 || r.MaxPositionPct > 100 {
		add("risk: capital > 0, 0 < risk_per_trade_pct <= 5, max_positions >= 1, 0 < max_position_pct <= 100")
	}
	if c.MTF.Enabled && (c.MTF.Leverage < 1 || c.MTF.Leverage > 5) {
		add("mtf.leverage must be between 1 and 5")
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

func oneOf(v string, opts ...string) bool {
	for _, o := range opts {
		if v == o {
			return true
		}
	}
	return false
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
