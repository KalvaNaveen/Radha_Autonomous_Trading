package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nkalva/kitealgo/internal/config"
	"github.com/nkalva/kitealgo/internal/engine"
)

func newTestServer(t *testing.T) *httptest.Server {
	cfg := config.Defaults()
	dir := t.TempDir()
	cfg.Paths.DataDir, cfg.Paths.JournalDir, cfg.Paths.WatchlistDir = dir+"/data", dir+"/journal", dir+"/watchlists"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng, err := engine.New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(New(eng, NewLogRing(10), log, "test").Handler())
}

func TestUIAndWriteGuard(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	res, _ := http.Get(ts.URL + "/")
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !strings.Contains(string(b), "Radha Control Panel") {
		t.Fatalf("index: %d", res.StatusCode)
	}
	// Writes without the custom header (e.g. a cross-site form post) are refused.
	res, _ = http.Post(ts.URL+"/api/credentials", "application/json", strings.NewReader(`{"api_key":"x"}`))
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("guard: want 403, got %d", res.StatusCode)
	}
}

func TestWatchlistRoundTrip(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	put := func(body string) int {
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/watchlist?date=2026-09-29", strings.NewReader(body))
		req.Header.Set("X-Kitealgo", "1")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode
	}
	if c := put("symbol\n"); c != 400 {
		t.Fatalf("empty list must be rejected, got %d", c)
	}
	if c := put("RELIANCE\r\nTCS,NIFTY IT\r\n"); c != 200 {
		t.Fatalf("save failed: %d", c)
	}
	res, _ := http.Get(ts.URL + "/api/watchlist?date=2026-09-29")
	b, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(b), `"count":2`) {
		t.Fatalf("readback: %s", b)
	}
	res, _ = http.Get(ts.URL + "/api/state")
	if res.StatusCode != 200 {
		t.Fatalf("state: %d", res.StatusCode)
	}
}
