// Package research writes a short, source-linked research note on each entry
// candidate before the engine buys it, using Google Gemini with Grounding
// with Google Search (free tier: gemini-2.5-flash, up to 500 searched
// requests a day). Paper/live only: web research cannot be backtested
// without hindsight, because today's search results know how the story ended.
package research

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nkalva/kitealgo/pkg/models"
)

const endpoint = "https://generativelanguage.googleapis.com/v1beta/interactions"

// Verdicts.
const (
	VerdictOK      = "OK"      // the story supports the trade
	VerdictCaution = "CAUTION" // mixed: buy smaller or watch closely
	VerdictAvoid   = "AVOID"   // a red flag (fraud, governance, SEBI action, collapsing results, promoter selling…)
)

// Note and Source are the shared model types.
type (
	Note   = models.AINote
	Source = models.AISource
)

// Client calls the Gemini Interactions API.
type Client struct {
	Key   string
	Model string // gemini-2.5-flash: the free tier includes Google Search grounding
	HTTP  *http.Client
	Dir   string // cache: <Dir>/<date>/<SYMBOL>.json (one call per stock per scan)
	URL   string // API endpoint (tests); default: the Gemini Interactions API

	mu   sync.Mutex
	last time.Time
}

// Ask is what the engine knows about the candidate.
type Ask struct {
	Symbol, Name, Date string
	Close              float64
	Setup, Tag, Why    string // entry setup, research tag and its price/volume evidence
}

const system = `You are a careful equity research analyst for Indian stocks (NSE). A swing-trading system wants to buy the stock below for a few days to a few weeks. Using Google Search, check the latest facts and write a short note.

Rules:
- Use only facts you found in search results from the last 6 months, and give dates. Never guess numbers. If you cannot find something, say "not found".
- Prefer primary sources: NSE/BSE filings and announcements, the company's results and investor presentations, then reputable financial news (Economic Times, Business Standard, Moneycontrol, Mint, Reuters, CNBC-TV18).
- Cover: why the stock is moving now; the latest quarterly results versus a year ago (revenue, profit, margins); orders, contracts, new products or capacity; management, board or promoter changes (resignations, promoter selling or pledging); the sector trend; red flags (SEBI or tax actions, auditor issues, fraud allegations, heavy debt, big equity dilution).
- Verdict: OK if the story supports a short-term long trade, CAUTION if mixed, AVOID on a serious red flag or collapsing fundamentals.

Reply with ONE JSON object and nothing else:
{"verdict":"OK|CAUTION|AVOID","story":"NEW_OPPORTUNITY|TURNAROUND|SECTOR_TAILWIND|RESULTS|NO_CLEAR_STORY","summary":"2-3 sentences","results":"...","business":"...","people":"...","sector":"...","risks":["..."]}`

// Research returns the note for a candidate (cached per scan date).
func (c *Client) Research(ctx context.Context, a Ask) Note {
	path := filepath.Join(c.Dir, a.Date, a.Symbol+".json")
	if raw, err := os.ReadFile(path); err == nil {
		var n Note
		if json.Unmarshal(raw, &n) == nil && n.Error == "" {
			return n
		}
	}
	n := Note{Symbol: a.Symbol, Date: a.Date, Model: c.Model, At: time.Now()}
	if c.Key == "" {
		n.Error = "no Gemini API key — add one in the control panel (free at aistudio.google.com)"
		return n
	}
	name := a.Name
	if name == "" {
		name = a.Symbol
	}
	input := fmt.Sprintf("Stock: %s (NSE: %s). Close on %s: ₹%.2f.\nWhy our scanner picked it: %s setup; price/volume evidence: %s (%s).\nResearch it and reply with the JSON object.",
		name, a.Symbol, a.Date, a.Close, a.Setup, a.Tag, a.Why)
	text, sources, searches, err := c.call(ctx, input)
	if err != nil {
		n.Error = err.Error()
		return n
	}
	if err := parseNote(text, &n); err != nil {
		n.Error = err.Error()
		n.Summary = strings.TrimSpace(text)
	}
	n.Sources, n.Searches = sources, searches
	if len(n.Sources) == 0 && n.Error == "" {
		n.Error = "the answer cites no sources — treat it as unverified"
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if raw, err := json.MarshalIndent(n, "", "  "); err == nil {
		_ = os.WriteFile(path, raw, 0o644)
	}
	return n
}

func (c *Client) gap() time.Duration {
	if c.URL != "" {
		return 0 // test server
	}
	return 4 * time.Second
}

func (c *Client) call(ctx context.Context, input string) (string, []Source, []string, error) {
	c.mu.Lock() // one request at a time, ≥4 s apart (free tier: a few requests per minute)
	if w := c.gap() - time.Since(c.last); w > 0 {
		time.Sleep(w)
	}
	c.last = time.Now()
	c.mu.Unlock()

	body, _ := json.Marshal(map[string]any{
		"model": c.Model, "input": input, "system_instruction": system,
		"tools": []map[string]string{{"type": "google_search"}}, "store": false,
	})
	url := c.URL
	if url == "" {
		url = endpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", nil, nil, err
	}
	req.Header.Set("x-goog-api-key", c.Key)
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 120 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", nil, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := e.Error.Message
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		return "", nil, nil, fmt.Errorf("Gemini %d: %s", resp.StatusCode, msg)
	}
	return parseResponse(raw)
}

// parseResponse reads the Interactions API response: the model's text, its
// url_citation annotations and the search queries it ran.
func parseResponse(raw []byte) (string, []Source, []string, error) {
	var r struct {
		OutputText string `json:"output_text"`
		Steps      []struct {
			Type      string `json:"type"`
			Arguments struct {
				Queries []string `json:"queries"`
			} `json:"arguments"`
			Content []struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Annotations []struct {
					Type  string `json:"type"`
					URL   string `json:"url"`
					Title string `json:"title"`
				} `json:"annotations"`
			} `json:"content"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", nil, nil, fmt.Errorf("unreadable Gemini response: %w", err)
	}
	var text strings.Builder
	var srcs []Source
	var qs []string
	seen := map[string]bool{}
	for _, s := range r.Steps {
		switch s.Type {
		case "google_search_call":
			qs = append(qs, s.Arguments.Queries...)
		case "model_output":
			for _, c := range s.Content {
				if c.Type != "text" {
					continue
				}
				text.WriteString(c.Text)
				for _, a := range c.Annotations {
					if a.Type == "url_citation" && a.URL != "" && !seen[a.URL] {
						seen[a.URL] = true
						srcs = append(srcs, Source{Title: a.Title, URL: a.URL})
					}
				}
			}
		}
	}
	out := text.String()
	if out == "" {
		out = r.OutputText
	}
	if strings.TrimSpace(out) == "" {
		return "", nil, nil, errors.New("Gemini returned no text")
	}
	return out, srcs, qs, nil
}

// parseNote extracts the JSON object from the model's text (it may be wrapped
// in a code fence or prose).
func parseNote(text string, n *Note) error {
	i, j := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if i < 0 || j <= i {
		return errors.New("Gemini did not return the JSON note")
	}
	var v struct {
		Verdict, Story, Summary, Results, Business, People, Sector string
		Risks                                                      []string
	}
	if err := json.Unmarshal([]byte(text[i:j+1]), &v); err != nil {
		return fmt.Errorf("unreadable JSON note: %w", err)
	}
	v.Verdict = strings.ToUpper(strings.TrimSpace(v.Verdict))
	if v.Verdict != VerdictOK && v.Verdict != VerdictCaution && v.Verdict != VerdictAvoid {
		v.Verdict = VerdictCaution
	}
	n.Verdict, n.Story, n.Summary, n.Results, n.Business, n.People, n.Sector, n.Risks =
		v.Verdict, strings.ToUpper(v.Story), v.Summary, v.Results, v.Business, v.People, v.Sector, v.Risks
	return nil
}
