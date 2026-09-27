// Package web serves the engine's local control panel: setup checklist, Kite
// login, watchlist editor, live agents, positions, trades, journal history and
// logs. It is plain HTML/JS embedded in the binary — no build step, no CDN.
package web

import (
	"context"
	"embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nkalva/kitealgo/internal/broker"
	"github.com/nkalva/kitealgo/internal/clock"
	"github.com/nkalva/kitealgo/internal/engine"
	"github.com/nkalva/kitealgo/internal/watchlist"
)

//go:embed index.html
var assets embed.FS

// Server is the control-panel HTTP server.
type Server struct {
	eng     *engine.Engine
	ring    *LogRing
	log     *slog.Logger
	version string
}

// New creates the server.
func New(eng *engine.Engine, ring *LogRing, log *slog.Logger, version string) *Server {
	return &Server{eng: eng, ring: ring, log: log.With("component", "web"), version: version}
}

// Handler returns all routes (UI + API + Kite login callback).
func (s *Server) Handler() http.Handler {
	authH := s.eng.Auth().Handler()
	mux := http.NewServeMux()
	mux.Handle("/login", authH)
	mux.Handle("/kite/callback", authH)
	mux.Handle("/status", authH)
	mux.Handle("/healthz", authH)

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := assets.ReadFile("index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(b)
	})
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("GET /api/logs", s.logs)
	mux.HandleFunc("GET /api/watchlist", s.getWatchlist)
	mux.HandleFunc("PUT /api/watchlist", s.guard(s.putWatchlist))
	mux.HandleFunc("POST /api/credentials", s.guard(s.postCredentials))
	mux.HandleFunc("POST /api/retry", s.guard(func(w http.ResponseWriter, r *http.Request) {
		s.eng.Retry()
		writeJSON(w, map[string]string{"ok": "retry requested"})
	}))
	mux.HandleFunc("GET /api/journal", s.journal)
	return mux
}

// guard rejects cross-site writes: browsers cannot attach a custom header to
// a cross-origin request without a CORS preflight, which we never approve.
func (s *Server) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Kitealgo") != "1" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

// Serve runs until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, listen string) error {
	srv := &http.Server{Addr: listen, Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// ---------------------------------------------------------------------------

type watchlistInfo struct {
	Date     string   `json:"date"`
	Path     string   `json:"path"`
	Exists   bool     `json:"exists"`
	Count    int      `json:"count"`
	Symbols  []string `json:"symbols"`
	Warnings []string `json:"warnings"`
	Error    string   `json:"error,omitempty"`
}

func (s *Server) watchlistInfo(day time.Time) watchlistInfo {
	cfg := s.eng.Config()
	wi := watchlistInfo{Date: day.Format("2006-01-02"), Path: watchlist.PathFor(cfg.Paths.WatchlistDir, day)}
	entries, warns, err := watchlist.Load(cfg.Paths.WatchlistDir, day)
	wi.Warnings = warns
	if err != nil {
		wi.Exists = !errors.Is(err, watchlist.ErrNoWatchlist)
		wi.Error = err.Error()
		return wi
	}
	wi.Exists = true
	wi.Count = len(entries)
	for _, e := range entries {
		wi.Symbols = append(wi.Symbols, e.Symbol)
	}
	wi.Warnings = append(wi.Warnings, s.checkSymbols(entries)...)
	return wi
}

// checkSymbols validates against the most recent cached instrument master.
func (s *Server) checkSymbols(entries []watchlist.Entry) []string {
	dir := filepath.Join(s.eng.Config().Paths.DataDir, "instruments")
	files, _ := filepath.Glob(filepath.Join(dir, "*-NSE.json"))
	if len(files) == 0 {
		return nil
	}
	sort.Strings(files)
	raw, err := os.ReadFile(files[len(files)-1])
	if err != nil {
		return nil
	}
	var ins []broker.Instrument
	if json.Unmarshal(raw, &ins) != nil {
		return nil
	}
	_, warns := watchlist.Resolve(entries, ins)
	return warns
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	cfg := s.eng.Config()
	am := s.eng.Auth()
	tok, valid := am.Current()
	key := am.APIKey()
	hint := ""
	if len(key) > 4 {
		hint = key[:4] + strings.Repeat("•", 6)
	}
	st := s.eng.State()
	target, _ := time.ParseInLocation("2006-01-02", st.TargetDay, clock.IST)
	out := map[string]any{
		"now":     time.Now().In(clock.IST),
		"mode":    cfg.Mode,
		"version": s.version,
		"engine":  st,
		"setup": map[string]any{
			"api_key_set":   key != "",
			"api_key_hint":  hint,
			"secret_set":    am.HasSecret(),
			"token_valid":   valid,
			"user_id":       tok.UserID,
			"token_created": tok.CreatedAt,
			"redirect_url":  cfg.Server.PublicURL + "/kite/callback",
			"login_url":     "/login",
			"bind_ip":       cfg.Network.BindIP,
			"dev_mock":      cfg.Dev.APIRoot != "",
		},
		"watchlist": s.watchlistInfo(target),
		"config": map[string]any{
			"capital": cfg.Risk.Capital, "risk_per_trade_pct": cfg.Risk.RiskPerTradePct,
			"max_open_positions": cfg.Risk.MaxOpenPositions, "daily_loss_limit_pct": cfg.Risk.DailyLossLimitPct,
			"prepare_at": cfg.Session.PrepareAt, "market_open": cfg.Session.MarketOpen,
			"trading_start": cfg.Session.TradingStart, "entry_cutoff": cfg.Session.EntryCutoff,
			"square_off": cfg.Session.SquareOff, "session_end": cfg.Session.SessionEnd,
		},
	}
	writeJSON(w, out)
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	writeJSON(w, s.ring.Since(after, 400))
}

func (s *Server) dayParam(r *http.Request) (time.Time, error) {
	q := r.URL.Query().Get("date")
	if q == "" {
		return s.eng.TargetDay(), nil
	}
	return time.ParseInLocation("2006-01-02", q, clock.IST)
}

func (s *Server) getWatchlist(w http.ResponseWriter, r *http.Request) {
	day, err := s.dayParam(r)
	if err != nil {
		writeErr(w, 400, "date must be YYYY-MM-DD")
		return
	}
	raw, err := os.ReadFile(watchlist.PathFor(s.eng.Config().Paths.WatchlistDir, day))
	content := ""
	if err == nil {
		content = string(raw)
	}
	writeJSON(w, map[string]any{"date": day.Format("2006-01-02"), "content": content, "info": s.watchlistInfo(day),
		"trading_day": s.eng.Session().IsTradingDay(day)})
}

func (s *Server) putWatchlist(w http.ResponseWriter, r *http.Request) {
	day, err := s.dayParam(r)
	if err != nil {
		writeErr(w, 400, "date must be YYYY-MM-DD")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	text := strings.ReplaceAll(string(body), "\r\n", "\n")
	if _, _, err := watchlist.Parse(strings.NewReader(text)); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	path := watchlist.PathFor(s.eng.Config().Paths.WatchlistDir, day)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.log.Info("watchlist saved from UI", "date", day.Format("2006-01-02"))
	s.eng.WatchlistChanged()
	writeJSON(w, map[string]any{"ok": true, "info": s.watchlistInfo(day)})
}

func (s *Server) postCredentials(w http.ResponseWriter, r *http.Request) {
	var in struct {
		APIKey    string `json:"api_key"`
		APISecret string `json:"api_secret"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		writeErr(w, 400, "bad JSON")
		return
	}
	in.APIKey, in.APISecret = strings.TrimSpace(in.APIKey), strings.TrimSpace(in.APISecret)
	if in.APIKey == "" && in.APISecret == "" {
		writeErr(w, 400, "nothing to save")
		return
	}
	if err := s.eng.Auth().SetCredentials(in.APIKey, in.APISecret); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.log.Info("Kite credentials saved from UI")
	writeJSON(w, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// Journal
// ---------------------------------------------------------------------------

type tradeRow struct {
	Symbol     string  `json:"symbol"`
	Side       string  `json:"side"`
	Qty        int     `json:"qty"`
	EntryTime  string  `json:"entry_time"`
	EntryPrice float64 `json:"entry_price"`
	ExitTime   string  `json:"exit_time"`
	ExitPrice  float64 `json:"exit_price"`
	Gross      float64 `json:"gross"`
	Costs      float64 `json:"costs"`
	Net        float64 `json:"net"`
	Reason     string  `json:"reason"`
	MaxLock    string  `json:"max_lock"`
}

type daySummary struct {
	Date   string  `json:"date"`
	Trades int     `json:"trades"`
	Wins   int     `json:"wins"`
	Net    float64 `json:"net"`
}

func readJournal(path string) ([]tradeRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, err
	}
	var out []tradeRow
	num := func(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }
	for i, r := range recs {
		if i == 0 || len(r) < 12 {
			continue
		}
		q, _ := strconv.Atoi(r[2])
		out = append(out, tradeRow{Symbol: r[0], Side: r[1], Qty: q, EntryTime: r[3], EntryPrice: num(r[4]),
			ExitTime: r[5], ExitPrice: num(r[6]), Gross: num(r[7]), Costs: num(r[8]), Net: num(r[9]),
			Reason: r[10], MaxLock: r[11]})
	}
	return out, nil
}

func (s *Server) journal(w http.ResponseWriter, r *http.Request) {
	dir := s.eng.Config().Paths.JournalDir
	files, _ := filepath.Glob(filepath.Join(dir, "*.csv"))
	sort.Strings(files)
	var days []daySummary
	for _, f := range files {
		rows, err := readJournal(f)
		if err != nil {
			continue
		}
		d := daySummary{Date: strings.TrimSuffix(filepath.Base(f), ".csv")}
		for _, t := range rows {
			d.Trades++
			d.Net += t.Net
			if t.Net > 0 {
				d.Wins++
			}
		}
		days = append(days, d)
	}
	date := r.URL.Query().Get("date")
	if date == "" && len(days) > 0 {
		date = days[len(days)-1].Date
	}
	var rows []tradeRow
	if date != "" {
		if _, err := time.Parse("2006-01-02", date); err == nil {
			rows, _ = readJournal(filepath.Join(dir, date+".csv"))
		}
	}
	writeJSON(w, map[string]any{"days": days, "date": date, "rows": rows})
}
