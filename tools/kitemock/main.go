// Command kitemock is a local Kite Connect simulator for rehearsing the swing
// engine end to end: Zerodha login redirect, session exchange (checksum
// verified), profile, margins, instrument master, daily historical candles
// and full quotes. Paper mode handles orders/GTTs inside the engine.
//
// Scripted market (deterministic):
//
//	history: ~3 years of daily bars for 12 stocks + NIFTY 50 (uptrend)
//	SWUP, SWDN  break out to a 20-day high on 2× volume on the last day
//	            → both become candidates for "today"
//	today:      SWUP climbs +2.5% during the session (held, stop raised later)
//	            SWDN falls −7% (hits its stop → paper GTT fires → exit booked)
//
// Run with -setup DIR to get a matching compressed-schedule engine config.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ist = time.FixedZone("IST", 19800)

type bar struct {
	d             time.Time
	o, h, l, c, v float64
}

type instrument struct {
	token    uint32
	symbol   string
	segment  string
	hist     []bar                   // up to and including the previous trading day
	intraday func(f float64) float64 // today's price as a function of session progress 0..1
}

type server struct {
	apiKey, secret, redirect string
	open, close              time.Time
	ins                      []*instrument
	bySym                    map[string]*instrument
	byTok                    map[uint32]*instrument
	mu                       sync.Mutex
	access                   string
	stats                    map[string]int
}

func weekdaysBack(end time.Time, n int) []time.Time {
	var out []time.Time
	for d := end; len(out) < n; d = d.AddDate(0, 0, -1) {
		if d.Weekday() != time.Saturday && d.Weekday() != time.Sunday {
			out = append([]time.Time{d}, out...)
		}
	}
	return out
}

func genHistory(seed int64, days []time.Time, start, drift, vol, avgVol float64) []bar {
	r := rand.New(rand.NewSource(seed))
	px := start
	out := make([]bar, 0, len(days))
	for _, d := range days {
		o := px * (1 + r.NormFloat64()*vol*0.25)
		c := o * (1 + drift + r.NormFloat64()*vol)
		h := math.Max(o, c) * (1 + math.Abs(r.NormFloat64())*vol*0.4)
		l := math.Min(o, c) * (1 - math.Abs(r.NormFloat64())*vol*0.4)
		out = append(out, bar{d, o, h, l, c, avgVol * (0.7 + 0.6*r.Float64())})
		px = c
	}
	return out
}

// breakout rewrites the last bar into a 20-day-high close on 2× volume.
func breakout(b []bar, avgVol float64) {
	n := len(b) - 1
	hi := 0.0
	for i := n - 20; i < n; i++ {
		hi = math.Max(hi, b[i].h)
	}
	c := hi * 1.02
	b[n] = bar{b[n].d, b[n-1].c, c * 1.004, b[n-1].c * 0.997, c, avgVol * 2.2}
}

func build(open time.Time) []*instrument {
	prev := open.AddDate(0, 0, -1)
	for prev.Weekday() == time.Saturday || prev.Weekday() == time.Sunday {
		prev = prev.AddDate(0, 0, -1)
	}
	prev = time.Date(prev.Year(), prev.Month(), prev.Day(), 0, 0, 0, 0, ist)
	days := weekdaysBack(prev, 780)
	mk := func(tok uint32, sym string, seed int64, start, drift, vol float64) *instrument {
		return &instrument{token: tok, symbol: sym, segment: "NSE", hist: genHistory(seed, days, start, drift, vol, 2e6)}
	}
	var ins []*instrument
	up := mk(6000<<8|1, "SWUP", 11, 400, 0.0012, 0.012)
	breakout(up.hist, 2e6)
	upClose := up.hist[len(up.hist)-1].c
	up.intraday = func(f float64) float64 { return upClose * (1.003 + 0.022*f) }
	dn := mk(6001<<8|1, "SWDN", 12, 800, 0.0012, 0.012)
	breakout(dn.hist, 2e6)
	dnClose := dn.hist[len(dn.hist)-1].c
	dn.intraday = func(f float64) float64 { return dnClose * (1.002 - 0.072*f) }
	ins = append(ins, up, dn)
	for i := 0; i < 10; i++ {
		x := mk(uint32(6010+i)<<8|1, fmt.Sprintf("SWX%02d", i+1), int64(20+i), 200+60*float64(i), 0.0002*float64(i%3), 0.015)
		c := x.hist[len(x.hist)-1].c
		x.intraday = func(f float64) float64 { return c * (1 + 0.002*math.Sin(f*6)) }
		ins = append(ins, x)
	}
	nifty := &instrument{token: 256265, symbol: "NIFTY 50", segment: "INDICES", hist: genHistory(99, days, 18000, 0.0006, 0.007, 0)}
	// Steady advance over the last 80 sessions so the regime filter is risk-on.
	h := nifty.hist
	for i := len(h) - 80; i < len(h); i++ {
		c := h[i-1].c * 1.0015
		h[i] = bar{h[i].d, h[i-1].c, c * 1.002, h[i-1].c * 0.998, c, 0}
	}
	nc := nifty.hist[len(nifty.hist)-1].c
	nifty.intraday = func(f float64) float64 { return nc * (1 + 0.003*f) }
	return append(ins, nifty)
}

func ok(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": data})
}

func fail(w http.ResponseWriter, code int, etype, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "error_type": etype, "message": msg})
}

func (s *server) hit(k string) { s.mu.Lock(); s.stats[k]++; s.mu.Unlock() }

func (s *server) authed(w http.ResponseWriter, r *http.Request) bool {
	s.mu.Lock()
	want := "token " + s.apiKey + ":" + s.access
	has := s.access != ""
	s.mu.Unlock()
	if !has || r.Header.Get("Authorization") != want {
		fail(w, 403, "TokenException", "Incorrect `api_key` or `access_token`.")
		return false
	}
	return true
}

// progress is the session fraction 0..1 at t.
func (s *server) progress(t time.Time) float64 {
	f := t.Sub(s.open).Seconds() / s.close.Sub(s.open).Seconds()
	return math.Max(0, math.Min(1, f))
}

// today builds the day's bar from the intraday path up to now.
func (s *server) today(in *instrument, now time.Time) (bar, bool) {
	if now.Before(s.open) {
		return bar{}, false
	}
	p := s.progress(now)
	o := in.intraday(0)
	h, l := o, o
	for f := 0.0; f <= p; f += 0.01 {
		v := in.intraday(f)
		h, l = math.Max(h, v), math.Min(l, v)
	}
	c := in.intraday(p)
	vol := 0.0
	if in.segment != "INDICES" {
		vol = 2e6 * p
	}
	d := time.Date(s.open.Year(), s.open.Month(), s.open.Day(), 0, 0, 0, 0, ist)
	return bar{d, o, h, l, c, vol}, true
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/connect/login", func(w http.ResponseWriter, r *http.Request) {
		s.hit("login")
		if r.URL.Query().Get("api_key") != s.apiKey {
			http.Error(w, "invalid api_key", 400)
			return
		}
		http.Redirect(w, r, s.redirect+"?action=login&type=login&status=success&request_token=mockreq"+strconv.FormatInt(time.Now().Unix(), 10), http.StatusFound)
	})
	mux.HandleFunc("/session/token", func(w http.ResponseWriter, r *http.Request) {
		s.hit("session")
		_ = r.ParseForm()
		sum := sha256.Sum256([]byte(s.apiKey + r.Form.Get("request_token") + s.secret))
		if r.Form.Get("checksum") != fmt.Sprintf("%x", sum) {
			fail(w, 403, "TokenException", "Invalid checksum")
			return
		}
		s.mu.Lock()
		s.access = "mockaccess" + strconv.FormatInt(time.Now().UnixNano(), 36)
		acc := s.access
		s.mu.Unlock()
		ok(w, map[string]any{"user_id": "RK1234", "user_name": "Radha Mock", "access_token": acc, "api_key": s.apiKey,
			"login_time": time.Now().In(ist).Format("2006-01-02 15:04:05")})
	})
	mux.HandleFunc("/user/profile", func(w http.ResponseWriter, r *http.Request) {
		if s.authed(w, r) {
			ok(w, map[string]any{"user_id": "RK1234", "user_name": "Radha Mock", "broker": "ZERODHA"})
		}
	})
	mux.HandleFunc("/user/margins/equity", func(w http.ResponseWriter, r *http.Request) {
		if s.authed(w, r) {
			ok(w, map[string]any{"enabled": true, "net": 1000000.0})
		}
	})
	mux.HandleFunc("/instruments/NSE", func(w http.ResponseWriter, r *http.Request) {
		s.hit("instruments")
		if !s.authed(w, r) {
			return
		}
		w.Header().Set("Content-Type", "text/csv")
		fmt.Fprintln(w, "instrument_token,exchange_token,tradingsymbol,name,last_price,expiry,strike,tick_size,lot_size,instrument_type,segment,exchange")
		for _, in := range s.ins {
			fmt.Fprintf(w, "%d,%d,%s,\"%s\",0,,0,0.05,1,EQ,%s,NSE\n", in.token, in.token>>8, in.symbol, in.symbol, in.segment)
		}
	})
	mux.HandleFunc("/instruments/historical/", func(w http.ResponseWriter, r *http.Request) {
		s.hit("historical")
		if !s.authed(w, r) {
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/instruments/historical/"), "/")
		tok, _ := strconv.ParseUint(parts[0], 10, 32)
		in := s.byTok[uint32(tok)]
		if in == nil || len(parts) < 2 || parts[1] != "day" {
			fail(w, 400, "InputException", "invalid token or interval")
			return
		}
		from, _ := time.ParseInLocation("2006-01-02 15:04:05", r.URL.Query().Get("from"), ist)
		to, _ := time.ParseInLocation("2006-01-02 15:04:05", r.URL.Query().Get("to"), ist)
		var out [][]any
		add := func(b bar) {
			if !b.d.Before(time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, ist)) && !b.d.After(to) {
				out = append(out, []any{b.d.Format("2006-01-02T15:04:05-0700"), r2(b.o), r2(b.h), r2(b.l), r2(b.c), math.Round(b.v)})
			}
		}
		for _, b := range in.hist {
			add(b)
		}
		if b, ok := s.today(in, time.Now()); ok {
			add(b)
		}
		ok(w, map[string]any{"candles": out})
	})
	mux.HandleFunc("/quote", func(w http.ResponseWriter, r *http.Request) {
		s.hit("quote")
		if !s.authed(w, r) {
			return
		}
		now := time.Now()
		res := map[string]any{}
		for _, k := range r.URL.Query()["i"] {
			in := s.bySym[strings.TrimPrefix(k, "NSE:")]
			if in == nil {
				continue
			}
			last := in.hist[len(in.hist)-1]
			b, live := s.today(in, now)
			if !live {
				b = bar{o: last.c, h: last.c, l: last.c, c: last.c}
			}
			px := r2(b.c)
			var buy, sell []map[string]any
			for i := 1; i <= 5; i++ {
				buy = append(buy, map[string]any{"price": r2(px - 0.05*float64(i)), "quantity": 500, "orders": 5})
				sell = append(sell, map[string]any{"price": r2(px + 0.05*float64(i)), "quantity": 500, "orders": 5})
			}
			res[k] = map[string]any{"instrument_token": in.token, "timestamp": now.In(ist).Format("2006-01-02 15:04:05"),
				"last_price": px, "volume": int(b.v), "ohlc": map[string]any{"open": r2(b.o), "high": r2(b.h), "low": r2(b.l), "close": r2(last.c)},
				"upper_circuit_limit": r2(last.c * 1.2), "lower_circuit_limit": r2(last.c * 0.8),
				"depth": map[string]any{"buy": buy, "sell": sell}}
		}
		ok(w, res)
	})
	mux.HandleFunc("/mock/stats", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(s.stats)
	})
	return mux
}

func r2(v float64) float64 { return math.Round(v*100) / 100 }

func main() {
	listen := flag.String("listen", "127.0.0.1:9000", "listen address")
	apiKey := flag.String("api-key", "mockkey", "api key")
	secret := flag.String("api-secret", "mocksecret", "api secret")
	redirect := flag.String("redirect", "http://127.0.0.1:8080/kite/callback", "engine callback URL")
	setup := flag.String("setup", "", "write a compressed-schedule engine config + universe into this directory")
	session := flag.Duration("session", 4*time.Minute, "length of the simulated market session")
	flag.Parse()

	now := time.Now().In(ist).Truncate(time.Second)
	open := now.Add(45 * time.Second)
	cls := open.Add(*session)
	if *setup != "" {
		if err := writeSetup(*setup, now, open, cls, *apiKey, *secret, *listen); err != nil {
			log.Fatal(err)
		}
	}
	s := &server{apiKey: *apiKey, secret: *secret, redirect: *redirect, open: open, close: cls, ins: build(open),
		bySym: map[string]*instrument{}, byTok: map[uint32]*instrument{}, stats: map[string]int{}}
	for _, in := range s.ins {
		s.bySym[in.symbol], s.byTok[in.token] = in, in
	}
	log.Printf("kitemock on %s — session %s → %s", *listen, open.Format("15:04:05"), cls.Format("15:04:05"))
	log.Fatal(http.ListenAndServe(*listen, s.routes()))
}

func writeSetup(dir string, now, open, cls time.Time, apiKey, secret, listen string) error {
	hms := func(t time.Time) string { return t.Format("15:04:05") }
	cfg := fmt.Sprintf(`# Generated by kitemock -setup: compressed rehearsal schedule. PAPER ONLY.
mode: paper
kite: {api_key: %q, api_secret: %q}
paths: {universe_file: %s/universe.csv, data_dir: %s/data, journal_dir: %s/journal, log_file: %s/logs/engine.log}
session:
  prepare_at: "%s"
  market_open: "%s"
  morning_run: "%s"
  market_close: "%s"
  evening_run: "%s"
risk: {max_new_per_day: 3}
server: {listen: "127.0.0.1:8080", public_url: "http://127.0.0.1:8080"}
dev:
  api_root: "http://%s"
  login_root: "http://%s"
`, apiKey, secret, dir, dir, dir, dir, hms(now.Add(10*time.Second)), hms(open), hms(open.Add(20*time.Second)),
		hms(cls), hms(cls.Add(20*time.Second)), listen, listen)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o644); err != nil {
		return err
	}
	uni := "symbol\nSWUP\nSWDN\n"
	for i := 1; i <= 10; i++ {
		uni += fmt.Sprintf("SWX%02d\n", i)
	}
	uni += "NOTAREALSTOCK\n"
	if err := os.WriteFile(filepath.Join(dir, "universe.csv"), []byte(uni), 0o644); err != nil {
		return err
	}
	log.Printf("wrote %s/config.yaml — start the engine:  radha-engine -config %s/config.yaml", dir, dir)
	return nil
}
