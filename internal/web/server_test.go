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
}
