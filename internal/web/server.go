// Package web serves the swing engine's local control panel: setup
// checklist, Kite login, portfolio, tomorrow's candidates, backtests, trade
// journal, universe editor and logs. Plain HTML/JS embedded in the binary.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nkalva/kitealgo/internal/clock"
	"github.com/nkalva/kitealgo/internal/data"
	"github.com/nkalva/kitealgo/internal/engine"
	"github.com/nkalva/kitealgo/internal/swing"
	"github.com/nkalva/kitealgo/pkg/models"
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

// Handler returns all routes.
func (s *Server) Handler() http.Handler {
	authH := s.eng.Auth().Handler()
	mux := http.NewServeMux()
	mux.Handle("GET /login", authH)
	mux.Handle("GET /kite/callback", authH)
	mux.Handle("GET /healthz", authH)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		// Zerodha redirects to whatever Redirect URL the Kite app has; accept
		// the login on any path so "http://127.0.0.1:8080" alone also works.
		if r.URL.Query().Get("request_token") != "" {
			s.eng.Auth().Callback(w, r)
			return
		}
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		b, _ := assets.ReadFile("index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(b)
	})
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("GET /api/logs", s.logs)
	mux.HandleFunc("GET /api/universe", s.getUniverse)
	mux.HandleFunc("PUT /api/universe", s.guard(s.putUniverse))
	mux.HandleFunc("POST /api/credentials", s.guard(s.postCredentials))
	mux.HandleFunc("POST /api/retry", s.guard(func(w http.ResponseWriter, r *http.Request) {
		s.eng.Retry()
		writeJSON(w, map[string]string{"ok": "retry requested"})
	}))
	mux.HandleFunc("POST /api/backtest", s.guard(s.postBacktest))
	mux.HandleFunc("GET /api/backtest", s.getBacktest)
	mux.HandleFunc("POST /api/backtest/reset", s.guard(s.resetBacktest))
	mux.HandleFunc("GET /backtest/report", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, s.eng.ReportPath())
	})
	mux.HandleFunc("GET /api/journal", s.journal)
	return mux
}

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
// It also listens on the IPv6 loopback when the configured host is
// 127.0.0.1, because Windows browsers often resolve "localhost" to ::1.
func (s *Server) Serve(ctx context.Context, listen string) error {
	h := s.Handler()
	srv := &http.Server{Addr: listen, Handler: h, ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 2)
	go func() { errc <- srv.ListenAndServe() }()
	var srv6 *http.Server
	if host, port, err := net.SplitHostPort(listen); err == nil && (host == "127.0.0.1" || host == "localhost") {
		if ln, err := net.Listen("tcp6", "[::1]:"+port); err == nil {
			srv6 = &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
			go func() { _ = srv6.Serve(ln) }()
		}
	}
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		if srv6 != nil {
			_ = srv6.Shutdown(sctx)
		}
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

type positionView struct {
	models.Position
	PnL    float64 `json:"pnl"`
	PnLPct float64 `json:"pnl_pct"`
	RNow   float64 `json:"r_now"`
	Value  float64 `json:"value"`
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
	st := s.eng.Store().Snapshot()
	var pos []positionView
	for _, p := range st.SortedPositions() {
		px := p.LastPrice
		if px <= 0 {
			px = p.EntryPrice
		}
		v := positionView{Position: *p, Value: px * float64(p.Quantity)}
		v.PnL = (px - p.EntryPrice) * float64(p.Quantity)
		v.PnLPct = (px/p.EntryPrice - 1) * 100
		if rps := p.RiskPerShare(); rps > 0 {
			v.RNow = (px - p.EntryPrice) / rps
		}
		pos = append(pos, v)
	}
	eq := st.Equity()
	dd := 0.0
	if st.PeakEquity > 0 {
		dd = (1 - eq/st.PeakEquity) * 100
	}
	syms, uwarns, uerr := data.LoadUniverse(cfg.Paths.UniverseFile)
	uinfo := map[string]any{"count": len(syms), "warnings": uwarns}
	if uerr != nil {
		uinfo["error"] = uerr.Error()
	}
	writeJSON(w, map[string]any{
		"now": time.Now().In(clock.IST), "mode": cfg.Mode, "version": s.version,
		"engine":   s.eng.State(),
		"backtest": s.eng.BacktestState(),
		"setup": map[string]any{
			"api_key_set": key != "", "api_key_hint": hint, "secret_set": am.HasSecret(),
			"token_valid": valid, "user_id": tok.UserID, "redirect_url": cfg.Server.PublicURL + "/kite/callback",
			"dev_mock": cfg.Dev.APIRoot != "", "bind_ip": cfg.Network.BindIP,
		},
		"universe": uinfo,
		"portfolio": map[string]any{
			"cash": st.Cash, "equity": eq, "invested": eq - st.Cash, "realized": st.Realized, "peak": st.PeakEquity,
			"drawdown_pct": dd, "positions": pos, "pending": st.Pending, "pending_for": st.PendingFor,
			"scanned": st.Scanned, "regime": st.Regime, "last_evening": st.LastEvening, "last_morning": st.LastMorning,
		},
		"config": map[string]any{
			"capital": cfg.Risk.Capital, "risk_per_trade_pct": cfg.Risk.RiskPerTradePct, "max_positions": cfg.Risk.MaxPositions,
			"max_position_pct": cfg.Risk.MaxPositionPct, "max_new_per_day": cfg.Risk.MaxNewPerDay, "morning_run": cfg.Session.MorningRun, "evening_run": cfg.Session.EveningRun,
			"backtest_years": cfg.Backtest.Years,
		},
	})
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	writeJSON(w, s.ring.Since(after, 400))
}

func (s *Server) getUniverse(w http.ResponseWriter, r *http.Request) {
	path := s.eng.Config().Paths.UniverseFile
	raw, _ := os.ReadFile(path)
	syms, warns, err := data.ParseUniverse(strings.NewReader(string(raw)))
	out := map[string]any{"content": string(raw), "file": path, "symbols": syms, "warnings": warns,
		"max": data.MaxUniverse, "unknown": s.unknownSymbols(syms)}
	if err != nil {
		out["error"] = err.Error()
	}
	writeJSON(w, out)
}

// unknownSymbols lists symbols missing from the cached NSE instrument master
// (empty when no master has been downloaded yet, i.e. before the first login).
func (s *Server) unknownSymbols(syms []string) []string {
	ins := s.eng.Data().LatestInstruments()
	if len(ins) == 0 {
		return []string{}
	}
	eq := map[string]bool{}
	for _, in := range ins {
		if in.Segment == "NSE" && in.InstrumentType == "EQ" {
			eq[in.TradingSymbol] = true
		}
	}
	out := []string{}
	for _, v := range syms {
		if !eq[v] {
			out = append(out, v)
		}
	}
	return out
}

// putUniverse accepts either {"symbols":[...]} JSON or raw text in any
// pasted format, and always stores the canonical one-symbol-per-line file.
func (s *Server) putUniverse(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 256<<10))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	text := string(body)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var in struct {
			Symbols []string `json:"symbols"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			writeErr(w, 400, "bad JSON: "+err.Error())
			return
		}
		text = strings.Join(in.Symbols, "\n")
	}
	syms, warns, err := data.ParseUniverse(strings.NewReader(text))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	unknown := s.unknownSymbols(syms)
	for _, u := range unknown {
		warns = append(warns, u+": not found on NSE — it will be skipped by the scan")
	}
	path := s.eng.Config().Paths.UniverseFile
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(data.FormatUniverse(syms)), 0o644); err != nil {
		writeErr(w, 500, "cannot write "+path+": "+err.Error())
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		writeErr(w, 500, "cannot replace "+path+" (is it open in Excel?): "+err.Error())
		return
	}
	s.log.Info("universe saved from UI", "symbols", len(syms), "unknown", len(unknown))
	writeJSON(w, map[string]any{"ok": true, "count": len(syms), "symbols": syms, "warnings": warns, "unknown": unknown})
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

func (s *Server) postBacktest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Years int `json:"years"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in)
	if in.Years == 0 {
		in.Years = s.eng.Config().Backtest.Years
	}
	if err := s.eng.StartBacktest(in.Years); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) resetBacktest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ClearCandles bool `json:"clear_candles"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in)
	if err := s.eng.ResetBacktest(in.ClearCandles); err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) getBacktest(w http.ResponseWriter, r *http.Request) {
	rules, hash := s.eng.RulesNow()
	cur, _ := json.Marshal(map[string]string{"rules": rules, "rules_hash": hash})
	raw, err := s.eng.LatestBacktest()
	if err != nil {
		writeJSON(w, map[string]any{"status": s.eng.BacktestState(), "current": json.RawMessage(cur)})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	st, _ := json.Marshal(s.eng.BacktestState())
	_, _ = w.Write([]byte(`{"status":` + string(st) + `,"current":` + string(cur) + `,"result":`))
	_, _ = w.Write(raw)
	_, _ = w.Write([]byte("}"))
}

type monthRow struct {
	Month  string  `json:"month"`
	Trades int     `json:"trades"`
	Wins   int     `json:"wins"`
	Net    float64 `json:"net"`
}

func (s *Server) journal(w http.ResponseWriter, r *http.Request) {
	trades, _ := s.eng.Journal().ReadAll()
	byMonth := map[string]*monthRow{}
	var order []string
	for _, t := range trades {
		k := t.ExitDate.Format("2006-01")
		m := byMonth[k]
		if m == nil {
			m = &monthRow{Month: k}
			byMonth[k] = m
			order = append(order, k)
		}
		m.Trades++
		m.Net += t.Net
		if t.Net > 0 {
			m.Wins++
		}
	}
	months := make([]monthRow, 0, len(order))
	for _, k := range order {
		months = append(months, *byMonth[k])
	}
	// newest trades first for the table
	rev := make([]models.Trade, len(trades))
	for i, t := range trades {
		rev[len(trades)-1-i] = t
	}
	writeJSON(w, map[string]any{"trades": rev, "months": months})
}

var _ = swing.DateKey
