// Command kitemock is a local Kite Connect simulator for end-to-end rehearsals
// of the engine: Zerodha login redirect, session token exchange (with real
// checksum verification), profile, margins, instrument master, historical
// minute candles and a KiteTicker-compatible binary WebSocket feed.
//
// It plays a scripted session relative to -open:
//
//	RADHAUP   long setup: crossover + pullback on the 4th bar, trends up, reverses
//	RADHADN   mirror image: short setup
//	RADHAFLAT flat — must never trade
//	NIFTY 50  flat (neutral radar)
//
// It is a test tool only; the engine refuses dev.* overrides in live mode.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var ist = time.FixedZone("IST", 19800)

type instrument struct {
	token     uint32
	symbol    string
	name      string
	segment   string
	itype     string
	scenario  func(minutes float64) float64
	atp       float64
	histBase  float64 // prior-day close level
	histFinal float64 // level of the last 20 minutes of the prior day
}

func interp(wps [][2]float64, m float64) float64 {
	if m <= wps[0][0] {
		return wps[0][1]
	}
	for i := 0; i+1 < len(wps); i++ {
		if m >= wps[i][0] && m <= wps[i+1][0] {
			f := (m - wps[i][0]) / (wps[i+1][0] - wps[i][0])
			return wps[i][1] + f*(wps[i+1][1]-wps[i][1])
		}
	}
	return wps[len(wps)-1][1]
}

var upPath = [][2]float64{
	{-30, 100}, {3, 100}, // warmup (engine: 3 bars)
	{3.1, 100.10}, {3.95, 100.50}, // bar 4: dip into band, close strong → crossover + pullback
	{4.5, 100.55}, {6, 101.00}, // → breakeven lock
	{8, 101.35},                // → profit lock
	{11, 101.70}, {14, 101.75}, // trend, trailing
	{15, 101.10}, {60, 101.10}, // reversal → exit
}

func mirror(p [][2]float64) [][2]float64 {
	out := make([][2]float64, len(p))
	for i, w := range p {
		out[i] = [2]float64{w[0], 200 - w[1]}
	}
	return out
}

func buildInstruments() []*instrument {
	dn := mirror(upPath)
	return []*instrument{
		{token: 5000<<8 | 1, symbol: "RADHAUP", name: "RADHA UP LTD", segment: "NSE", itype: "EQ",
			scenario: func(m float64) float64 { return interp(upPath, m) }, atp: 100, histBase: 100.5, histFinal: 100.0},
		{token: 5001<<8 | 1, symbol: "RADHADN", name: "RADHA DOWN LTD", segment: "NSE", itype: "EQ",
			scenario: func(m float64) float64 { return interp(dn, m) }, atp: 100, histBase: 99.5, histFinal: 100.0},
		{token: 5002<<8 | 1, symbol: "RADHAFLAT", name: "RADHA FLAT LTD", segment: "NSE", itype: "EQ",
			scenario: func(m float64) float64 { return 250 + 0.05*math.Sin(m*3) }, atp: 250, histBase: 250, histFinal: 250},
		{token: 256265, symbol: "NIFTY 50", name: "NIFTY 50", segment: "INDICES", itype: "EQ",
			scenario: func(m float64) float64 { return 25000 + 2*math.Sin(m) }, histBase: 25000, histFinal: 25000},
		{token: 260105, symbol: "NIFTY BANK", name: "NIFTY BANK", segment: "INDICES", itype: "EQ",
			scenario: func(m float64) float64 { return 55000 }, histBase: 55000, histFinal: 55000},
	}
}

type server struct {
	apiKey, secret, redirect string
	open                     time.Time
	ins                      []*instrument
	byToken                  map[uint32]*instrument
	accessToken              string
	upgrader                 websocket.Upgrader
	mu                       sync.Mutex
	stats                    map[string]int
}

func (s *server) hit(k string) {
	s.mu.Lock()
	s.stats[k]++
	s.mu.Unlock()
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

func (s *server) authed(w http.ResponseWriter, r *http.Request) bool {
	want := "token " + s.apiKey + ":" + s.accessToken
	if s.accessToken == "" || r.Header.Get("Authorization") != want {
		fail(w, 403, "TokenException", "Incorrect `api_key` or `access_token`.")
		return false
	}
	return true
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/connect/login", func(w http.ResponseWriter, r *http.Request) {
		s.hit("login")
		if r.URL.Query().Get("api_key") != s.apiKey {
			http.Error(w, "invalid api_key", 400)
			return
		}
		// Real Kite shows a login + 2FA page here; the simulator approves at once.
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
		s.accessToken = "mockaccess" + strconv.FormatInt(time.Now().UnixNano(), 36)
		ok(w, map[string]any{"user_id": "RK1234", "user_name": "Radha Mock", "access_token": s.accessToken,
			"public_token": "pub", "refresh_token": "", "api_key": s.apiKey, "login_time": time.Now().In(ist).Format("2006-01-02 15:04:05")})
	})
	mux.HandleFunc("/user/profile", func(w http.ResponseWriter, r *http.Request) {
		s.hit("profile")
		if s.authed(w, r) {
			ok(w, map[string]any{"user_id": "RK1234", "user_name": "Radha Mock", "email": "mock@example.com", "broker": "ZERODHA"})
		}
	})
	mux.HandleFunc("/user/margins/equity", func(w http.ResponseWriter, r *http.Request) {
		s.hit("margins")
		if s.authed(w, r) {
			ok(w, map[string]any{"enabled": true, "net": 1000000.0, "available": map[string]any{"cash": 1000000.0, "live_balance": 1000000.0}})
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
			fmt.Fprintf(w, "%d,%d,%s,\"%s\",0,,0,0.05,1,%s,%s,NSE\n", in.token, in.token>>8, in.symbol, in.name, in.itype, in.segment)
		}
	})
	mux.HandleFunc("/instruments/historical/", func(w http.ResponseWriter, r *http.Request) {
		s.hit("historical")
		if !s.authed(w, r) {
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/instruments/historical/"), "/")
		tok, _ := strconv.ParseUint(parts[0], 10, 32)
		in := s.byToken[uint32(tok)]
		if in == nil || len(parts) < 2 || parts[1] != "minute" {
			fail(w, 400, "InputException", "invalid token or interval")
			return
		}
		ok(w, map[string]any{"candles": s.history(in)})
	})
	mux.HandleFunc("/ws", s.ws)
	mux.HandleFunc("/mock/stats", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(s.stats)
	})
	return mux
}

// history returns 10 prior weekdays of 09:15–15:29 minute candles.
func (s *server) history(in *instrument) [][]any {
	var days []time.Time
	d := time.Date(s.open.Year(), s.open.Month(), s.open.Day(), 9, 15, 0, 0, ist)
	for len(days) < 10 {
		d = d.AddDate(0, 0, -1)
		if d.Weekday() != time.Saturday && d.Weekday() != time.Sunday {
			days = append([]time.Time{d}, days...)
		}
	}
	vol := 1000.0
	if in.segment == "INDICES" {
		vol = 0
	}
	var out [][]any
	for di, day := range days {
		for m := 0; m < 375; m++ {
			px := in.histBase
			if di == len(days)-1 && m >= 375-20 {
				px = in.histFinal
			}
			ts := day.Add(time.Duration(m) * time.Minute).Format("2006-01-02T15:04:05-0700")
			out = append(out, []any{ts, px, px, px, px, vol})
		}
	}
	return out
}

func paise(p float64) uint32 { return uint32(math.Round(p * 100)) }

func (s *server) packet(in *instrument, now time.Time) []byte {
	m := now.Sub(s.open).Minutes()
	px := math.Round(in.scenario(m)*20) / 20 // tick 0.05
	open := in.scenario(-30)
	if in.segment == "INDICES" {
		b := make([]byte, 32)
		binary.BigEndian.PutUint32(b[0:], in.token)
		binary.BigEndian.PutUint32(b[4:], paise(px))
		binary.BigEndian.PutUint32(b[8:], paise(px+5))
		binary.BigEndian.PutUint32(b[12:], paise(px-5))
		binary.BigEndian.PutUint32(b[16:], paise(open))
		binary.BigEndian.PutUint32(b[20:], paise(open))
		binary.BigEndian.PutUint32(b[28:], uint32(now.Unix()))
		return b
	}
	var cum uint32
	if m > 0 {
		cum = uint32(3000 * m) // 3x the historical 1000/min → RVOL ≈ 3
	}
	b := make([]byte, 184)
	binary.BigEndian.PutUint32(b[0:], in.token)
	binary.BigEndian.PutUint32(b[4:], paise(px))
	binary.BigEndian.PutUint32(b[8:], 10)
	binary.BigEndian.PutUint32(b[12:], paise(in.atp))
	binary.BigEndian.PutUint32(b[16:], cum)
	binary.BigEndian.PutUint32(b[20:], 50000)
	binary.BigEndian.PutUint32(b[24:], 50000)
	binary.BigEndian.PutUint32(b[28:], paise(open))
	binary.BigEndian.PutUint32(b[32:], paise(px+0.5))
	binary.BigEndian.PutUint32(b[36:], paise(px-0.5))
	binary.BigEndian.PutUint32(b[40:], paise(open))
	binary.BigEndian.PutUint32(b[44:], uint32(now.Unix()))
	binary.BigEndian.PutUint32(b[60:], uint32(now.Unix()))
	for i := 0; i < 5; i++ {
		bp, sp := 64+i*12, 124+i*12
		binary.BigEndian.PutUint32(b[bp:], 500)
		binary.BigEndian.PutUint32(b[bp+4:], paise(px-0.05*float64(i+1)))
		binary.BigEndian.PutUint16(b[bp+8:], 5)
		binary.BigEndian.PutUint32(b[sp:], 500)
		binary.BigEndian.PutUint32(b[sp+4:], paise(px+0.05*float64(i+1)))
		binary.BigEndian.PutUint16(b[sp+8:], 5)
	}
	return b
}

func (s *server) ws(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("api_key") != s.apiKey || s.accessToken == "" || q.Get("access_token") != s.accessToken {
		http.Error(w, "unauthorised", 403)
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	s.hit("ws_connect")
	var mu sync.Mutex
	subs := map[uint32]bool{}
	go func() {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req struct {
				A string          `json:"a"`
				V json.RawMessage `json:"v"`
			}
			if json.Unmarshal(msg, &req) != nil {
				continue
			}
			mu.Lock()
			switch req.A {
			case "subscribe":
				var toks []uint32
				_ = json.Unmarshal(req.V, &toks)
				for _, t := range toks {
					subs[t] = true
				}
				log.Printf("ws: subscribe %v", toks)
			case "mode":
				log.Printf("ws: mode %s", req.V)
			}
			mu.Unlock()
		}
	}()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for now := range t.C {
		mu.Lock()
		var pkts [][]byte
		for tok := range subs {
			if in := s.byToken[tok]; in != nil {
				pkts = append(pkts, s.packet(in, now))
			}
		}
		mu.Unlock()
		if len(pkts) == 0 {
			if err := conn.WriteMessage(websocket.BinaryMessage, []byte{0}); err != nil { // heartbeat
				return
			}
			continue
		}
		frame := make([]byte, 2)
		binary.BigEndian.PutUint16(frame, uint16(len(pkts)))
		for _, p := range pkts {
			l := make([]byte, 2)
			binary.BigEndian.PutUint16(l, uint16(len(p)))
			frame = append(frame, l...)
			frame = append(frame, p...)
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
			return
		}
		s.hit("ws_frames")
	}
}

func main() {
	listen := flag.String("listen", "127.0.0.1:9000", "listen address")
	apiKey := flag.String("api-key", "mockkey", "api key the engine uses")
	secret := flag.String("api-secret", "mocksecret", "api secret the engine uses")
	redirect := flag.String("redirect", "http://127.0.0.1:8080/kite/callback", "engine callback URL")
	openStr := flag.String("open", "", "market-open instant, RFC3339 (default: now+75s)")
	setup := flag.String("setup", "", "write a compressed-session engine config + watchlist into this directory")
	flag.Parse()
	open := time.Now().Add(75 * time.Second).Truncate(time.Second)
	if *openStr != "" {
		t, err := time.Parse(time.RFC3339, *openStr)
		if err != nil {
			log.Fatal(err)
		}
		open = t
	}
	if *setup != "" {
		if err := writeSetup(*setup, open, *apiKey, *secret, *listen); err != nil {
			log.Fatal(err)
		}
	}
	s := &server{apiKey: *apiKey, secret: *secret, redirect: *redirect, open: open.In(ist), stats: map[string]int{},
		ins: buildInstruments(), byToken: map[uint32]*instrument{}}
	for _, in := range s.ins {
		s.byToken[in.token] = in
	}
	log.Printf("kitemock on %s — scripted market open at %s", *listen, s.open.Format("15:04:05"))
	log.Fatal(http.ListenAndServe(*listen, s.routes()))
}

// writeSetup writes an engine config whose session is compressed into ~19
// minutes starting now (1-minute candles), plus today's watchlist.
func writeSetup(dir string, open time.Time, apiKey, secret, listen string) error {
	at := func(d time.Duration) string { return open.Add(d).In(ist).Format("15:04:05") }
	cfg := fmt.Sprintf(`# Generated by kitemock -setup: a ~19 minute rehearsal session. PAPER ONLY.
mode: paper
kite: {api_key: %s, api_secret: %s}
paths: {watchlist_dir: %s/watchlists, data_dir: %s/data, journal_dir: %s/journal, log_file: %s/logs/engine.log}
session:
  prepare_at: "%s"
  market_open: "%s"
  trading_start: "%s"
  entry_cutoff: "%s"
  square_off: "%s"
  safety_sweep: "%s"
  session_end: "%s"
strategy: {candle_interval: 1m, candle_grace: 2s}
server: {listen: "127.0.0.1:8080", public_url: "http://127.0.0.1:8080"}
dev:
  api_root: "http://%s"
  login_root: "http://%s"
  ticker_url: "ws://%s/ws"
`, apiKey, secret, dir, dir, dir, dir,
		at(-60*time.Second), at(0), at(3*time.Minute), at(13*time.Minute),
		at(17*time.Minute), at(17*time.Minute+30*time.Second), at(18*time.Minute+30*time.Second),
		listen, listen, listen)
	if err := os.MkdirAll(filepath.Join(dir, "watchlists"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o644); err != nil {
		return err
	}
	wl := "symbol,benchmark\nRADHAUP,NIFTY BANK\nRADHADN\nRADHAFLAT\nNOTAREALSTOCK\n"
	day := open.In(ist).Format("2006-01-02")
	if err := os.WriteFile(filepath.Join(dir, "watchlists", day+".csv"), []byte(wl), 0o644); err != nil {
		return err
	}
	log.Printf("wrote %s/config.yaml and %s/watchlists/%s.csv", dir, dir, day)
	log.Printf("start the engine now:  ./bin/engine -config %s/config.yaml", dir)
	log.Printf("then open http://127.0.0.1:8080/login in your browser (scripted market open %s)", open.In(ist).Format("15:04:05"))
	return nil
}
