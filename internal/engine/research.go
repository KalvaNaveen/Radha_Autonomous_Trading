package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nkalva/kitealgo/internal/research"
	"github.com/nkalva/kitealgo/internal/swing"
	"github.com/nkalva/kitealgo/pkg/models"
)

// ResearchSettings are the AI research choices saved from the control panel
// (they override config.yaml's research.ai and research.gemini_api_key).
type ResearchSettings struct {
	Mode string `json:"mode"` // off | note | gate
	Key  string `json:"key"`
}

func (e *Engine) researchPath() string { return filepath.Join(e.cfg.Paths.DataDir, "research", "settings.json") }

// researchSettings merges config.yaml with what was saved from the UI.
func (e *Engine) researchSettings() ResearchSettings {
	s := ResearchSettings{Mode: orDefault(e.cfg.Research.AI, "off"), Key: e.cfg.Research.GeminiAPIKey}
	if raw, err := os.ReadFile(e.researchPath()); err == nil {
		var u ResearchSettings
		if json.Unmarshal(raw, &u) == nil {
			if u.Mode != "" {
				s.Mode = u.Mode
			}
			if u.Key != "" && e.cfg.Research.GeminiAPIKey == "" {
				s.Key = u.Key
			}
		}
	}
	return s
}

// ResearchState is shown in the control panel (the key only as a hint).
func (e *Engine) ResearchState() map[string]any {
	s := e.researchSettings()
	hint := ""
	if n := len(s.Key); n > 6 {
		hint = s.Key[:4] + "…" + s.Key[n-2:]
	}
	return map[string]any{"mode": s.Mode, "key_set": s.Key != "", "key_hint": hint, "model": e.researchModel(),
		"max_per_day": e.cfg.Research.MaxPerDay}
}

// SaveResearch stores the mode and (if given) the Gemini key with 0600 permissions.
func (e *Engine) SaveResearch(mode, key string) error {
	if mode != "off" && mode != "note" && mode != "gate" {
		return errors.New("mode must be off, note or gate")
	}
	cur := ResearchSettings{}
	if raw, err := os.ReadFile(e.researchPath()); err == nil {
		_ = json.Unmarshal(raw, &cur)
	}
	cur.Mode = mode
	if key = strings.TrimSpace(key); key != "" {
		cur.Key = key
	}
	if cur.Mode != "off" && cur.Key == "" && e.cfg.Research.GeminiAPIKey == "" {
		return errors.New("add a Gemini API key first (free at aistudio.google.com)")
	}
	if err := os.MkdirAll(filepath.Dir(e.researchPath()), 0o700); err != nil {
		return err
	}
	raw, _ := json.Marshal(cur)
	return os.WriteFile(e.researchPath(), raw, 0o600)
}

func (e *Engine) researchModel() string { return orDefault(e.cfg.Research.Model, "gemini-2.5-flash") }

func (e *Engine) researchClient(key string) *research.Client {
	return &research.Client{Key: key, Model: e.researchModel(), Dir: filepath.Join(e.cfg.Paths.DataDir, "research")}
}

// applyResearch writes a web research note on the strongest candidates
// (research.max_per_day). In gate mode, candidates with an AVOID verdict are
// dropped; a failed lookup never blocks a trade. It returns the kept signals
// and, for dropped ones, the reason.
func (e *Engine) applyResearch(ctx context.Context, sigs []models.Signal, day time.Time, names map[string]string) ([]models.Signal, map[string]string) {
	s := e.researchSettings()
	dropped := map[string]string{}
	if s.Mode == "off" || len(sigs) == 0 {
		return sigs, dropped
	}
	c := e.researchClient(s.Key)
	max := e.cfg.Research.MaxPerDay
	if max <= 0 {
		max = 8
	}
	kept := sigs[:0:0]
	for k := range sigs {
		sg := sigs[k]
		if k < max {
			e.setStage("evening", fmt.Sprintf("AI research %d/%d: %s", k+1, min(max, len(sigs)), sg.Symbol))
			n := c.Research(ctx, research.Ask{Symbol: sg.Symbol, Name: names[sg.Symbol], Date: swing.DateKey(day), Close: sg.Close,
				Setup: string(sg.Setup), Tag: sg.Research, Why: sg.ResearchNote})
			sg.AI = &n
			if n.Error != "" {
				e.log.Warn("AI research failed — candidate kept", "symbol", sg.Symbol, "err", n.Error)
			} else {
				e.log.Info("AI research", "symbol", sg.Symbol, "verdict", n.Verdict, "story", n.Story, "sources", len(n.Sources))
			}
			if s.Mode == "gate" && n.Error == "" && n.Verdict == research.VerdictAvoid {
				dropped[sg.Symbol] = "AI research: AVOID — " + n.Summary
				continue
			}
		}
		kept = append(kept, sg)
	}
	return kept, dropped
}

// ResearchPending re-runs the AI research on tomorrow's queued candidates
// (the "Research now" button). Runs in the background.
func (e *Engine) ResearchPending() error {
	s := e.researchSettings()
	if s.Key == "" {
		return errors.New("add a Gemini API key first (free at aistudio.google.com)")
	}
	snap := e.store.Snapshot()
	if len(snap.Pending) == 0 {
		return errors.New("no candidates queued — the evening scan fills the list")
	}
	day, err := time.ParseInLocation("2006-01-02", snap.LastEvening, time.Local)
	if err != nil {
		day = time.Now()
	}
	names := map[string]string{}
	for _, in := range e.data.LatestInstruments() {
		if in.Segment == "NSE" {
			names[in.TradingSymbol] = in.Name
		}
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		// An explicit request: attach notes to the queue, never drop a candidate.
		sigs := append([]models.Signal(nil), snap.Pending...)
		c := e.researchClient(s.Key)
		for k := range sigs {
			n := c.Research(ctx, research.Ask{Symbol: sigs[k].Symbol, Name: names[sigs[k].Symbol], Date: swing.DateKey(day),
				Close: sigs[k].Close, Setup: string(sigs[k].Setup), Tag: sigs[k].Research, Why: sigs[k].ResearchNote})
			e.log.Info("AI research", "symbol", sigs[k].Symbol, "verdict", n.Verdict, "err", n.Error)
			sym := sigs[k].Symbol
			_ = e.store.Update(func(st *swing.State) { // show each note as soon as it is ready
				for j := range st.Pending {
					if st.Pending[j].Symbol == sym {
						st.Pending[j].AI = &n
					}
				}
			})
		}
	}()
	return nil
}
