package research

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The response shape from the Gemini docs (Interactions API + Google Search).
const sample = `{"steps":[
 {"type":"thought","summary":[{"type":"text","text":"..."}]},
 {"type":"google_search_call","arguments":{"queries":["KEI Industries Q1 results","KEI order book"]}},
 {"type":"google_search_result","call_id":"s1","result":[{"search_suggestions":"<div/>"}]},
 {"type":"model_output","content":[{"type":"text","text":"` + "```json\\n" + `{\"verdict\":\"ok\",\"story\":\"results\",\"summary\":\"Profit up 25% YoY.\",\"results\":\"Q1 revenue +20%\",\"business\":\"not found\",\"people\":\"no changes\",\"sector\":\"cables demand strong\",\"risks\":[\"copper prices\"]}` + "\\n```" + `",
   "annotations":[{"type":"url_citation","url":"https://www.nseindia.com/a","title":"nseindia.com","start_index":0,"end_index":10},
                  {"type":"url_citation","url":"https://economictimes.com/b","title":"economictimes.com"},
                  {"type":"url_citation","url":"https://www.nseindia.com/a","title":"dup"}]}]}]}`

func TestResearchNote(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("x-goog-api-key") != "k" {
			t.Errorf("missing API key header")
		}
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		tools, _ := body["tools"].([]any)
		if body["model"] != "gemini-2.5-flash" || len(tools) != 1 || body["store"] != false || !strings.Contains(body["input"].(string), "NSE: KEI") {
			t.Errorf("bad request: %s", raw)
		}
		_, _ = w.Write([]byte(sample))
	}))
	defer srv.Close()
	c := &Client{Key: "k", Model: "gemini-2.5-flash", URL: srv.URL, Dir: t.TempDir()}
	a := Ask{Symbol: "KEI", Name: "KEI INDUSTRIES", Date: "2026-09-28", Close: 4100, Setup: "EMA_CROSS", Tag: "RESULTS", Why: "RESULTS: +7% on 3× volume"}
	n := c.Research(context.Background(), a)
	if n.Error != "" || n.Verdict != VerdictOK || n.Story != "RESULTS" || n.Summary != "Profit up 25% YoY." || len(n.Risks) != 1 {
		t.Fatalf("note: %+v", n)
	}
	if len(n.Sources) != 2 || n.Sources[0].URL != "https://www.nseindia.com/a" || len(n.Searches) != 2 {
		t.Fatalf("sources/searches: %+v %v", n.Sources, n.Searches)
	}
	if n2 := c.Research(context.Background(), a); calls != 1 || n2.Verdict != VerdictOK {
		t.Fatalf("second call on the same scan date must come from the cache (calls=%d)", calls)
	}
}

func TestResearchErrors(t *testing.T) {
	if n := (&Client{Dir: t.TempDir()}).Research(context.Background(), Ask{Symbol: "X", Date: "d"}); !strings.Contains(n.Error, "API key") {
		t.Fatalf("want a missing-key error, got %+v", n)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"code":429,"message":"Resource has been exhausted"}}`))
	}))
	defer srv.Close()
	n := (&Client{Key: "k", URL: srv.URL, Dir: t.TempDir()}).Research(context.Background(), Ask{Symbol: "X", Date: "d"})
	if !strings.Contains(n.Error, "429") || !strings.Contains(n.Error, "exhausted") || n.Verdict != "" {
		t.Fatalf("got %+v", n)
	}
	// A note without citations is flagged, and an unknown verdict becomes CAUTION.
	var m Note
	if err := parseNote(`Here: {"verdict":"buy","summary":"s"}`, &m); err != nil || m.Verdict != VerdictCaution {
		t.Fatalf("%v %+v", err, m)
	}
}
