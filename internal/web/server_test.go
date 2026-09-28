package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nkalva/kitealgo/internal/config"
	"github.com/nkalva/kitealgo/internal/engine"
)

func newTestServer(t *testing.T) *httptest.Server {
	cfg := config.Defaults()
	dir := t.TempDir()
	cfg.Paths.DataDir, cfg.Paths.JournalDir, cfg.Paths.UniverseFile = dir+"/data", dir+"/journal", dir+"/universe.csv"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng, err := engine.New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(New(eng, NewLogRing(10), log, "test").Handler())
}

func TestUIStateAndGuard(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	res, _ := http.Get(ts.URL + "/")
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !strings.Contains(string(b), "Radha Swing Panel") {
		t.Fatalf("index: %d", res.StatusCode)
	}
	res, _ = http.Get(ts.URL + "/api/state")
	b, _ = io.ReadAll(res.Body)
	if res.StatusCode != 200 || !strings.Contains(string(b), `"portfolio"`) || !strings.Contains(string(b), `"count":25`) {
		t.Fatalf("state: %s", b)
	}
	res, _ = http.Post(ts.URL+"/api/credentials", "application/json", strings.NewReader(`{"api_key":"x"}`))
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("guard: want 403, got %d", res.StatusCode)
	}
}

func TestUniverseSave(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	put := func(body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/universe", strings.NewReader(body))
		req.Header.Set("X-Kitealgo", "1")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	if c, _ := put("symbol\n"); c != 400 {
		t.Fatalf("empty universe must be rejected, got %d", c)
	}
	if c, b := put("INFY\r\nTCS\r\n"); c != 200 || !strings.Contains(b, `"count":2`) {
		t.Fatalf("save: %d %s", c, b)
	}
	// A pasted watchlist on one line must keep every symbol.
	if c, b := put("NSE:WIPRO,NSE:TCS,NSE:INFY"); c != 200 || !strings.Contains(b, `"count":3`) {
		t.Fatalf("comma list: %d %s", c, b)
	}
	// The UI sends JSON.
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/universe", strings.NewReader(`{"symbols":["SBIN","M&M","sbin"]}`))
	req.Header.Set("X-Kitealgo", "1")
	req.Header.Set("Content-Type", "application/json")
	res, _ := http.DefaultClient.Do(req)
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !strings.Contains(string(b), `"symbols":["SBIN","M\u0026M"]`) {
		t.Fatalf("json save: %d %s", res.StatusCode, b)
	}
	res, _ = http.Get(ts.URL + "/api/universe")
	b, _ = io.ReadAll(res.Body)
	if !strings.Contains(string(b), `"symbols":["SBIN","M\u0026M"]`) || !strings.Contains(string(b), `SBIN\nM\u0026M\n`) {
		t.Fatalf("get after save: %s", b)
	}
}

// Zerodha redirects to the app's Redirect URL; a login landing on "/" (or any
// path) with a request_token must be handled, not silently dropped.
func TestLoginRedirectOnAnyPath(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, p := range []string{"/?status=success&request_token=abc", "/whatever?status=success&request_token=abc", "/kite/callback?status=cancelled"} {
		res, err := c.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		loc := res.Header.Get("Location")
		if res.StatusCode != http.StatusFound || !strings.Contains(loc, "login_error") {
			t.Fatalf("%s: want redirect to the panel with an error (no real Kite here), got %d %q", p, res.StatusCode, loc)
		}
	}
}

func TestBacktestReset(t *testing.T) {
	cfg := config.Defaults()
	dir := t.TempDir()
	cfg.Paths.DataDir, cfg.Paths.JournalDir, cfg.Paths.UniverseFile = dir+"/data", dir+"/journal", dir+"/universe.csv"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng, err := engine.New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(New(eng, NewLogRing(10), log, "test").Handler())
	defer ts.Close()
	for _, f := range []string{"/data/backtest/latest/result.json", "/data/candles/1.json"} {
		_ = os.MkdirAll(filepath.Dir(dir+f), 0o755)
		_ = os.WriteFile(dir+f, []byte(`{"summary":{}}`), 0o644)
	}
	res, _ := http.Get(ts.URL + "/api/backtest")
	b, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(b), `"result"`) || !strings.Contains(string(b), `"rules_hash"`) {
		t.Fatalf("backtest before reset: %s", b)
	}
	reset := func(body string) int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/backtest/reset", strings.NewReader(body))
		req.Header.Set("X-Kitealgo", "1")
		r, _ := http.DefaultClient.Do(req)
		return r.StatusCode
	}
	if c := reset(`{}`); c != 200 {
		t.Fatalf("reset: %d", c)
	}
	if _, err := os.Stat(dir + "/data/backtest/latest/result.json"); !os.IsNotExist(err) {
		t.Fatal("result must be deleted")
	}
	if _, err := os.Stat(dir + "/data/candles/1.json"); err != nil {
		t.Fatal("candles must be kept unless asked")
	}
	if c := reset(`{"clear_candles":true}`); c != 200 {
		t.Fatalf("reset with candles: %d", c)
	}
	if _, err := os.Stat(dir + "/data/candles"); !os.IsNotExist(err) {
		t.Fatal("candles must be deleted when asked")
	}
}
