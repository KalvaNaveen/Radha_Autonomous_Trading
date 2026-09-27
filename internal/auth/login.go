// Package auth handles Kite's daily access-token lifecycle.
//
// Kite access tokens expire every morning (~06:00 IST) and must be obtained
// through Zerodha's interactive login (password + 2FA). Automating that login
// breaches Zerodha's terms, so the engine instead serves a one-click flow:
// open /login, log in on Zerodha's page, Kite redirects to /kite/callback with
// a request_token, and the engine exchanges it for the day's access token.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"

	"github.com/nkalva/kitealgo/internal/clock"
)

// Token is the persisted daily session.
type Token struct {
	AccessToken string    `json:"access_token"`
	UserID      string    `json:"user_id"`
	CreatedAt   time.Time `json:"created_at"`
}

// expiryBoundary is the most recent 06:00 IST at or before now.
func expiryBoundary(now time.Time) time.Time {
	b := clock.Midnight(now).Add(6 * time.Hour)
	if now.Before(b) {
		b = b.AddDate(0, 0, -1)
	}
	return b
}

// ValidAt reports whether the token was issued after the last 06:00 flush.
func (t Token) ValidAt(now time.Time) bool {
	return t.AccessToken != "" && !t.CreatedAt.Before(expiryBoundary(now))
}

// Manager stores the token and runs the login callback server.
type Manager struct {
	apiKey    string
	apiSecret string
	path      string
	publicURL string
	httpc     *http.Client
	clk       clock.Clock
	log       *slog.Logger

	mu      sync.RWMutex
	tok     Token
	changed chan struct{}

	statusFn func() any

	apiRoot, loginRoot string // dev overrides (simulator)
	credPath           string
}

type credentials struct {
	APIKey    string `json:"api_key"`
	APISecret string `json:"api_secret"`
}

// APIKey returns the Kite API key in use.
func (m *Manager) APIKey() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.apiKey
}

// HasSecret reports whether an API secret is configured.
func (m *Manager) HasSecret() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.apiSecret != ""
}

func (m *Manager) secret() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.apiSecret
}

// SetCredentials stores the API key/secret (entered in the UI) with 0600
// permissions under the data directory. Empty values keep the current one.
func (m *Manager) SetCredentials(key, secret string) error {
	m.mu.Lock()
	if key != "" {
		m.apiKey = key
	}
	if secret != "" {
		m.apiSecret = secret
	}
	c := credentials{APIKey: m.apiKey, APISecret: m.apiSecret}
	m.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(m.credPath), 0o700); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(m.credPath, raw, 0o600)
}

// SetRoots points the login flow at a simulator (dev only).
func (m *Manager) SetRoots(apiRoot, loginRoot string) { m.apiRoot, m.loginRoot = apiRoot, loginRoot }

// NewManager creates a token manager persisting to dataDir/kite_token.json.
func NewManager(apiKey, apiSecret, dataDir, publicURL string, httpc *http.Client, clk clock.Clock, log *slog.Logger) *Manager {
	m := &Manager{
		apiKey: apiKey, apiSecret: apiSecret, path: filepath.Join(dataDir, "kite_token.json"),
		credPath:  filepath.Join(dataDir, "kite_credentials.json"),
		publicURL: publicURL, httpc: httpc, clk: clk, log: log, changed: make(chan struct{}),
	}
	// Credentials saved from the UI fill whatever config/env left empty.
	if raw, err := os.ReadFile(m.credPath); err == nil {
		var c credentials
		if json.Unmarshal(raw, &c) == nil {
			if m.apiKey == "" {
				m.apiKey = c.APIKey
			}
			if m.apiSecret == "" {
				m.apiSecret = c.APISecret
			}
		}
	}
	if raw, err := os.ReadFile(m.path); err == nil {
		var t Token
		if json.Unmarshal(raw, &t) == nil {
			m.tok = t
		}
	}
	return m
}

// SetStatusProvider installs the function backing GET /status.
func (m *Manager) SetStatusProvider(f func() any) {
	m.mu.Lock()
	m.statusFn = f
	m.mu.Unlock()
}

// Pin installs a token from config (testing / manual override).
func (m *Manager) Pin(access string) {
	m.store(Token{AccessToken: access, CreatedAt: m.clk.Now()})
}

// Current returns the token if valid now.
func (m *Manager) Current() (Token, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.tok, m.tok.ValidAt(m.clk.Now())
}

// Invalidate discards the stored token (e.g. after a TokenException).
func (m *Manager) Invalidate() {
	m.store(Token{})
}

func (m *Manager) store(t Token) {
	m.mu.Lock()
	m.tok = t
	close(m.changed)
	m.changed = make(chan struct{})
	m.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err == nil {
		raw, _ := json.MarshalIndent(t, "", "  ")
		tmp := m.path + ".tmp"
		if err := os.WriteFile(tmp, raw, 0o600); err == nil {
			_ = os.Rename(tmp, m.path)
		}
	}
}

// LoginURL is the URL to open in a browser.
func (m *Manager) LoginURL() string { return m.publicURL + "/login" }

// WaitForToken blocks until a valid token exists, nagging in the log.
func (m *Manager) WaitForToken(ctx context.Context) (Token, error) {
	nag := time.NewTicker(5 * time.Minute)
	defer nag.Stop()
	for {
		m.mu.RLock()
		t, ch := m.tok, m.changed
		m.mu.RUnlock()
		if t.ValidAt(m.clk.Now()) {
			return t, nil
		}
		m.log.Warn("Kite login required for today — open this URL and log in", "url", m.LoginURL())
		select {
		case <-ctx.Done():
			return Token{}, ctx.Err()
		case <-ch:
		case <-nag.C:
		}
	}
}

// Handler returns the HTTP routes: /login, /kite/callback, /status, /healthz.
func (m *Manager) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		if m.APIKey() == "" || !m.HasSecret() {
			http.Redirect(w, r, "/?login_error="+url.QueryEscape("Enter your Kite API key and secret first"), http.StatusFound)
			return
		}
		c := kiteconnect.New(m.APIKey())
		u := c.GetLoginURL()
		if m.loginRoot != "" {
			u = strings.Replace(u, "https://kite.zerodha.com", m.loginRoot, 1)
		}
		http.Redirect(w, r, u, http.StatusFound)
	})
	mux.HandleFunc("GET /kite/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("status") != "success" || q.Get("request_token") == "" {
			http.Redirect(w, r, "/?login_error="+url.QueryEscape("Zerodha login was not completed"), http.StatusFound)
			return
		}
		c := kiteconnect.New(m.APIKey())
		if m.apiRoot != "" {
			c.SetBaseURI(m.apiRoot)
		}
		if m.httpc != nil {
			c.SetHTTPClient(m.httpc)
		}
		sess, err := c.GenerateSession(q.Get("request_token"), m.secret())
		if err != nil {
			m.log.Error("generate session failed — check the API secret", "err", err)
			http.Redirect(w, r, "/?login_error="+url.QueryEscape("Session exchange failed (usually a wrong API secret): "+err.Error()), http.StatusFound)
			return
		}
		m.store(Token{AccessToken: sess.AccessToken, UserID: sess.UserID, CreatedAt: m.clk.Now()})
		m.log.Info("Kite session established", "user", sess.UserID)
		http.Redirect(w, r, "/?login=ok", http.StatusFound)
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		m.mu.RLock()
		f := m.statusFn
		m.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		var v any = map[string]string{"state": "idle"}
		if f != nil {
			v = f()
		}
		_ = json.NewEncoder(w).Encode(v)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, ok := m.Current()
		fmt.Fprintf(w, "ok token_valid=%v\n", ok)
	})
	return mux
}

// Serve runs the HTTP server until ctx is cancelled.
func (m *Manager) Serve(ctx context.Context, listen string) error {
	srv := &http.Server{Addr: listen, Handler: m.Handler(), ReadHeaderTimeout: 5 * time.Second}
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
