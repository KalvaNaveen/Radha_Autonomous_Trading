package swing

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/nkalva/kitealgo/pkg/models"
)

// State is the engine's persistent book: open positions, tomorrow's entry
// candidates, cash and cooldowns. It survives restarts (data/portfolio.json).
type State struct {
	Cash        float64                     `json:"cash"`
	PeakEquity  float64                     `json:"peak_equity"`
	Realized    float64                     `json:"realized"`
	Positions   map[string]*models.Position `json:"positions"`
	Pending     []models.Signal             `json:"pending"`      // entries for PendingFor
	PendingFor  string                      `json:"pending_for"`  // YYYY-MM-DD trading day
	Scanned     []ScanRow                   `json:"scanned"`      // last evening's verdicts (for the UI)
	Cooldown    map[string]string           `json:"cooldown"`     // symbol → first date re-entry is allowed
	LastEvening string                      `json:"last_evening"` // date of the last completed evening run
	LastMorning string                      `json:"last_morning"`
	Regime      string                      `json:"regime"`
	PauseUntil  string                      `json:"pause_until,omitempty"` // drawdown pause: no entries before this date
	UpdatedAt   time.Time                   `json:"updated_at"`
}

// ScanRow is one symbol's verdict from the evening scan.
type ScanRow struct {
	Symbol string  `json:"symbol"`
	Close  float64 `json:"close"`
	Signal bool    `json:"signal"`
	Setup  string  `json:"setup,omitempty"`
	RS     float64 `json:"rs"`
	Reason string  `json:"reason"`
}

// Equity is cash plus positions marked at their last known price.
func (s *State) Equity() float64 {
	e := s.Cash
	for _, p := range s.Positions {
		px := p.LastPrice
		if px <= 0 {
			px = p.LastClose
		}
		if px <= 0 {
			px = p.EntryPrice
		}
		e += px * float64(p.Quantity)
	}
	return e
}

// Invested is the market value of open positions.
func (s *State) Invested() float64 { return s.Equity() - s.Cash }

// SortedPositions returns positions ordered by symbol.
func (s *State) SortedPositions() []*models.Position {
	out := make([]*models.Position, 0, len(s.Positions))
	for _, p := range s.Positions {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}

// Store persists State atomically.
type Store struct {
	mu   sync.Mutex
	path string
	st   State
}

// OpenStore loads (or initialises with startingCash) the state file.
func OpenStore(path string, startingCash float64) (*Store, error) {
	s := &Store{path: path}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.st = State{Cash: startingCash, PeakEquity: startingCash}
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, &s.st); err != nil {
			return nil, err
		}
	}
	if s.st.Positions == nil {
		s.st.Positions = map[string]*models.Position{}
	}
	if s.st.Cooldown == nil {
		s.st.Cooldown = map[string]string{}
	}
	return s, nil
}

// Read calls f with a read-only view (do not retain pointers).
func (s *Store) Read(f func(st *State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.st)
}

// Update calls f to mutate the state, then saves it.
func (s *Store) Update(f func(st *State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.st)
	if eq := s.st.Equity(); eq > s.st.PeakEquity {
		s.st.PeakEquity = eq
	}
	s.st.UpdatedAt = time.Now()
	return s.saveLocked()
}

// Snapshot returns a deep copy.
func (s *Store) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, _ := json.Marshal(s.st)
	var cp State
	_ = json.Unmarshal(raw, &cp)
	if cp.Positions == nil {
		cp.Positions = map[string]*models.Position{}
	}
	return cp
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// ---------------------------------------------------------------------------
// Journal
// ---------------------------------------------------------------------------

var journalHeader = []string{"symbol", "setup", "qty", "entry_date", "entry_price", "exit_date", "exit_price",
	"gross", "costs", "net", "r_multiple", "days_held", "stage", "reason"}

// Journal appends closed trades to journal_dir/trades-YYYY-MM.csv.
type Journal struct {
	mu  sync.Mutex
	dir string
}

// NewJournal creates a journal writer.
func NewJournal(dir string) *Journal { return &Journal{dir: dir} }

// Write appends a trade.
func (j *Journal) Write(t models.Trade) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := os.MkdirAll(j.dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(j.dir, "trades-"+t.ExitDate.Format("2006-01")+".csv")
	_, statErr := os.Stat(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if os.IsNotExist(statErr) {
		_ = w.Write(journalHeader)
	}
	ff := func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }
	_ = w.Write([]string{t.Symbol, string(t.Setup), strconv.Itoa(t.Quantity), t.EntryDate.Format("2006-01-02"),
		ff(t.EntryPrice), t.ExitDate.Format("2006-01-02"), ff(t.ExitPrice), ff(t.Gross), ff(t.Costs), ff(t.Net),
		ff(t.RMultiple), strconv.Itoa(t.BarsHeld), string(t.Stage), t.Reason})
	w.Flush()
	return w.Error()
}

// ReadAll returns every journaled trade, oldest first.
func (j *Journal) ReadAll() ([]models.Trade, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	files, _ := filepath.Glob(filepath.Join(j.dir, "trades-*.csv"))
	sort.Strings(files)
	var out []models.Trade
	num := func(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }
	for _, fn := range files {
		f, err := os.Open(fn)
		if err != nil {
			continue
		}
		recs, err := csv.NewReader(f).ReadAll()
		f.Close()
		if err != nil {
			continue
		}
		for i, r := range recs {
			if i == 0 || len(r) < len(journalHeader) {
				continue
			}
			q, _ := strconv.Atoi(r[2])
			bh, _ := strconv.Atoi(r[11])
			ed, _ := time.Parse("2006-01-02", r[3])
			xd, _ := time.Parse("2006-01-02", r[5])
			out = append(out, models.Trade{Symbol: r[0], Setup: models.SetupKind(r[1]), Quantity: q, EntryDate: ed,
				EntryPrice: num(r[4]), ExitDate: xd, ExitPrice: num(r[6]), Gross: num(r[7]), Costs: num(r[8]),
				Net: num(r[9]), RMultiple: num(r[10]), BarsHeld: bh, Stage: models.StopStage(r[12]), Reason: r[13]})
		}
	}
	return out, nil
}

// CloseTrade builds a Trade record for a position sold at exitPx.
func CloseTrade(p *models.Position, exitPx float64, exitDate time.Time, reason string, costs Costs) models.Trade {
	val := exitPx * float64(p.Quantity)
	sellCost := costs.Sell(val)
	gross := (exitPx - p.EntryPrice) * float64(p.Quantity)
	tot := p.EntryCosts + sellCost
	r := 0.0
	if risk := p.RiskPerShare() * float64(p.Quantity); risk > 0 {
		r = (gross - tot) / risk
	}
	return models.Trade{Symbol: p.Symbol, Setup: p.Setup, Quantity: p.Quantity, EntryDate: p.EntryDate,
		EntryPrice: p.EntryPrice, ExitDate: exitDate, ExitPrice: exitPx, Gross: gross, Costs: tot, Net: gross - tot,
		RMultiple: r, BarsHeld: p.BarsHeld, Stage: p.Stage, Reason: reason}
}
