package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// NoBackend is the stats key for requests that never reached a server (queue timeout, queue full, …).
const NoBackend = "(kein Server)"

// DayStat aggregates the requests of one backend on one day.
type DayStat struct {
	Requests  int64   `json:"requests"`
	Errors    int64   `json:"errors"`    // upstream errors, 5xx, queue timeouts
	Cancelled int64   `json:"cancelled"` // client went away
	DurMs     int64   `json:"duration_ms"`
	MaxDurMs  int64   `json:"max_duration_ms"`
	TTFBMs    int64   `json:"ttfb_ms"`
	TTFBCount int64   `json:"ttfb_count"`
	QueueMs   int64   `json:"queue_ms"`
	Bytes     int64   `json:"bytes"`
	TokensIn  int64   `json:"tokens_in"`
	TokensOut int64   `json:"tokens_out"`
	GenMs     int64   `json:"gen_ms"`     // generation time (after the first byte) of requests with known output tokens
	GenTokens int64   `json:"gen_tokens"` // their output tokens → tokens/s = GenTokens / GenMs
	TagErrors int64   `json:"tag_errors"`
	Revenue   float64 `json:"revenue"` // incoming: API token prices
	Cost      float64 `json:"cost"`    // outgoing: server prices
}

// NoModel is the stats key for requests without a model field.
const NoModel = "(ohne Modell)"

// TokStat aggregates the requests of one client access token on one day.
type TokStat struct {
	Requests  int64   `json:"requests"`
	TokensIn  int64   `json:"tokens_in"`
	TokensOut int64   `json:"tokens_out"`
	Cost      float64 `json:"cost"`
}

func (t *TokStat) add(o *TokStat) {
	t.Requests += o.Requests
	t.TokensIn += o.TokensIn
	t.TokensOut += o.TokensOut
	t.Cost += o.Cost
}

func (d *DayStat) add(o *DayStat) {
	d.Requests += o.Requests
	d.Errors += o.Errors
	d.Cancelled += o.Cancelled
	d.DurMs += o.DurMs
	d.MaxDurMs = max(d.MaxDurMs, o.MaxDurMs)
	d.TTFBMs += o.TTFBMs
	d.TTFBCount += o.TTFBCount
	d.QueueMs += o.QueueMs
	d.Bytes += o.Bytes
	d.TokensIn += o.TokensIn
	d.TokensOut += o.TokensOut
	d.GenMs += o.GenMs
	d.GenTokens += o.GenTokens
	d.TagErrors += o.TagErrors
	d.Revenue += o.Revenue
	d.Cost += o.Cost
}

// Stats keeps per-day, per-backend counters and persists them to DATA_DIR/stats.json.
type Stats struct {
	mu       sync.Mutex
	days     map[string]map[string]*DayStat // "2006-01-02" → backend → stat
	tokdays  map[string]map[string]*TokStat // "2006-01-02" → client token name → stat
	moddays  map[string]map[string]*DayStat // "2006-01-02" → model → stat
	path     string
	keepDays int
	dirty    bool
}

func NewStats(dir string, keepDays int) *Stats {
	s := &Stats{days: map[string]map[string]*DayStat{}, tokdays: map[string]map[string]*TokStat{}, moddays: map[string]map[string]*DayStat{}, keepDays: keepDays}
	if dir != "" {
		s.path = filepath.Join(dir, "stats.json")
		data, err := os.ReadFile(s.path)
		switch {
		case err == nil:
			var f statsFile
			if err := json.Unmarshal(data, &f); err == nil && f.Backends != nil {
				s.days = f.Backends
				if f.Tokens != nil {
					s.tokdays = f.Tokens
				}
				if f.Models != nil {
					s.moddays = f.Models
				}
			} else if err := json.Unmarshal(data, &s.days); err != nil { // format of older versions
				log.Printf("stats: %s unreadable, starting fresh: %v", s.path, err)
				s.days = map[string]map[string]*DayStat{}
			}
		case !errors.Is(err, os.ErrNotExist):
			log.Printf("stats: %v", err)
		}
	}
	return s
}

func dayKey(t time.Time) string { return t.In(time.Local).Format("2006-01-02") }

// Record adds a finished request.
func (s *Stats) Record(r *ReqInfo) {
	backend := r.Backend
	if backend == "" {
		backend = NoBackend
	}
	d := DayStat{Requests: 1, Bytes: r.bytes.Load(), TokensIn: r.TokensIn, TokensOut: r.TokensOut,
		Revenue: r.Cost, Cost: r.BackendCost}
	if len(r.TagIssues) > 0 {
		d.TagErrors = 1
	}
	if r.GenTPS > 0 {
		d.GenMs = r.EndedAt.Sub(r.FirstByte).Milliseconds()
		d.GenTokens = r.TokensOut
	}
	switch {
	case r.Status == 499:
		d.Cancelled = 1
	case r.Status >= 500 || r.Status == 0:
		d.Errors = 1
	}
	if !r.StartedAt.IsZero() {
		d.QueueMs = r.StartedAt.Sub(r.QueuedAt).Milliseconds()
		d.DurMs = r.EndedAt.Sub(r.StartedAt).Milliseconds()
		d.MaxDurMs = d.DurMs
		if !r.FirstByte.IsZero() {
			d.TTFBMs = r.FirstByte.Sub(r.StartedAt).Milliseconds()
			d.TTFBCount = 1
		}
	} else {
		d.QueueMs = r.EndedAt.Sub(r.QueuedAt).Milliseconds()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	day := dayKey(r.EndedAt)
	m := s.days[day]
	if m == nil {
		m = map[string]*DayStat{}
		s.days[day] = m
	}
	cur := m[backend]
	if cur == nil {
		cur = &DayStat{}
		m[backend] = cur
	}
	cur.add(&d)
	model := r.Effective
	if model == "" {
		model = r.Model
	}
	if model == "" {
		model = NoModel
	}
	mm := s.moddays[day]
	if mm == nil {
		mm = map[string]*DayStat{}
		s.moddays[day] = mm
	}
	if mm[model] == nil {
		mm[model] = &DayStat{}
	}
	mm[model].add(&d)
	if r.Token != "" {
		tm := s.tokdays[day]
		if tm == nil {
			tm = map[string]*TokStat{}
			s.tokdays[day] = tm
		}
		ts := tm[r.Token]
		if ts == nil {
			ts = &TokStat{}
			tm[r.Token] = ts
		}
		ts.add(&TokStat{Requests: 1, TokensIn: r.TokensIn, TokensOut: r.TokensOut, Cost: r.Cost})
	}
	s.dirty = true
}

type statsFile struct {
	Backends map[string]map[string]*DayStat `json:"backends"`
	Tokens   map[string]map[string]*TokStat `json:"tokens"`
	Models   map[string]map[string]*DayStat `json:"models"`
}

// Flush writes the stats file if something changed and prunes old days.
func (s *Stats) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keepDays > 0 {
		cutoff := dayKey(time.Now().AddDate(0, 0, -s.keepDays))
		for d := range s.days {
			if d < cutoff {
				delete(s.days, d)
				s.dirty = true
			}
		}
		for d := range s.tokdays {
			if d < cutoff {
				delete(s.tokdays, d)
				s.dirty = true
			}
		}
		for d := range s.moddays {
			if d < cutoff {
				delete(s.moddays, d)
				s.dirty = true
			}
		}
	}
	if !s.dirty || s.path == "" {
		return
	}
	data, _ := json.Marshal(statsFile{Backends: s.days, Tokens: s.tokdays, Models: s.moddays})
	tmp := s.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		log.Printf("stats: %v", err)
		return
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Printf("stats: %v", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("stats: %v", err)
		return
	}
	s.dirty = false
}

func (s *Stats) Reset() {
	s.mu.Lock()
	s.days = map[string]map[string]*DayStat{}
	s.tokdays = map[string]map[string]*TokStat{}
	s.moddays = map[string]map[string]*DayStat{}
	s.dirty = true
	s.mu.Unlock()
	s.Flush()
}

// StatsReport is what the UI gets for a time range.
type StatsReport struct {
	From     string                         `json:"from"`
	To       string                         `json:"to"`
	Days     []string                       `json:"days"`     // every day in range, oldest first
	Backends []string                       `json:"backends"` // every backend name with data in range
	Data     map[string]map[string]*DayStat `json:"data"`     // day → backend → stat (only non-empty)
	Totals   map[string]*DayStat            `json:"totals"`   // backend → stat over the range
	// per client access token
	Tokens      []string                       `json:"tokens"`
	TokenData   map[string]map[string]*TokStat `json:"token_data"`   // day → token → stat
	TokenTotals map[string]*TokStat            `json:"token_totals"` // token → stat over the range
	// per model
	Models      []string                       `json:"models"`
	ModelData   map[string]map[string]*DayStat `json:"model_data"`
	ModelTotals map[string]*DayStat            `json:"model_totals"`
}

func (s *Stats) Report(days int, now time.Time) StatsReport {
	if days <= 0 {
		days = 30
	}
	rep := StatsReport{Backends: []string{}, Data: map[string]map[string]*DayStat{}, Totals: map[string]*DayStat{},
		Tokens: []string{}, TokenData: map[string]map[string]*TokStat{}, TokenTotals: map[string]*TokStat{},
		Models: []string{}, ModelData: map[string]map[string]*DayStat{}, ModelTotals: map[string]*DayStat{}}
	start := now.In(time.Local).AddDate(0, 0, -(days - 1))
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for i := 0; i < days; i++ {
		day := dayKey(start.AddDate(0, 0, i))
		rep.Days = append(rep.Days, day)
		if tm := s.tokdays[day]; len(tm) > 0 {
			out := map[string]*TokStat{}
			for name, ts := range tm {
				c := *ts
				out[name] = &c
				if rep.TokenTotals[name] == nil {
					rep.TokenTotals[name] = &TokStat{}
					rep.Tokens = append(rep.Tokens, name)
				}
				rep.TokenTotals[name].add(ts)
			}
			rep.TokenData[day] = out
		}
		if mm := s.moddays[day]; len(mm) > 0 {
			out := map[string]*DayStat{}
			for name, ms := range mm {
				c := *ms
				out[name] = &c
				if rep.ModelTotals[name] == nil {
					rep.ModelTotals[name] = &DayStat{}
					rep.Models = append(rep.Models, name)
				}
				rep.ModelTotals[name].add(ms)
			}
			rep.ModelData[day] = out
		}
		m := s.days[day]
		if len(m) == 0 {
			continue
		}
		out := map[string]*DayStat{}
		for name, st := range m {
			c := *st
			out[name] = &c
			if !seen[name] {
				seen[name] = true
				rep.Backends = append(rep.Backends, name)
				rep.Totals[name] = &DayStat{}
			}
			rep.Totals[name].add(st)
		}
		rep.Data[day] = out
	}
	sort.Strings(rep.Backends)
	sort.Strings(rep.Tokens)
	sort.Slice(rep.Models, func(i, j int) bool { // most used first
		return rep.ModelTotals[rep.Models[i]].Requests > rep.ModelTotals[rep.Models[j]].Requests
	})
	rep.From, rep.To = rep.Days[0], rep.Days[len(rep.Days)-1]
	return rep
}

func registerStats(mux *http.ServeMux, pre string, stats *Stats, token string, onReset func()) {
	mux.HandleFunc("GET "+pre+"/api/stats", func(w http.ResponseWriter, r *http.Request) {
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		if days > 3660 {
			days = 3660
		}
		rep := stats.Report(days, time.Now())
		if r.URL.Query().Get("format") == "csv" {
			w.Header().Set("Content-Type", "text/csv; charset=utf-8")
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="llm-balancer-stats_%s_%s.csv"`, rep.From, rep.To))
			cw := csv.NewWriter(w)
			cw.Comma = ';'
			by := "Server"
			if r.URL.Query().Get("by") == "model" {
				by = "Modell"
			}
			cw.Write([]string{"Tag", by, "Anfragen", "Fehler", "Abgebrochen", "Gesamtdauer_s", "Avg_Dauer_s", "Max_Dauer_s", "Avg_1.Byte_s", "Avg_Wartezeit_s", "Bytes", "Tokens_Prompt", "Tokens_Antwort", "Tokens_pro_s", "Tag_Fehler", "Einnahmen_API", "Kosten_Server"})
			sec := func(ms, n int64) string {
				if n == 0 {
					return "0"
				}
				return strconv.FormatFloat(float64(ms)/float64(n)/1000, 'f', 2, 64)
			}
			names, data := rep.Backends, rep.Data
			if by == "Modell" {
				names, data = rep.Models, rep.ModelData
			}
			for _, day := range rep.Days {
				for _, name := range names {
					st := data[day][name]
					if st == nil {
						continue
					}
					cw.Write([]string{day, name, itoa(st.Requests), itoa(st.Errors), itoa(st.Cancelled),
						sec(st.DurMs, 1), sec(st.DurMs, st.Requests), sec(st.MaxDurMs, 1),
						sec(st.TTFBMs, st.TTFBCount), sec(st.QueueMs, st.Requests), itoa(st.Bytes), itoa(st.TokensIn), itoa(st.TokensOut), tps(st), itoa(st.TagErrors), money(st.Revenue), money(st.Cost)})
				}
			}
			cw.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(rep)
	})
	mux.HandleFunc("POST "+pre+"/api/stats/reset", adminAuth(token, func(w http.ResponseWriter, r *http.Request) {
		stats.Reset()
		if onReset != nil {
			onReset()
		}
		log.Printf("statistics reset via UI")
		w.WriteHeader(http.StatusNoContent)
	}))
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func tps(st *DayStat) string {
	if st.GenMs <= 0 {
		return ""
	}
	return strconv.FormatFloat(float64(st.GenTokens)/(float64(st.GenMs)/1000), 'f', 1, 64)
}

func money(v float64) string { return strconv.FormatFloat(v, 'f', 6, 64) }

// RecordFailedAttempt counts a failed attempt on a server whose request was then retried elsewhere:
// it shows up as an error of that server (the request itself is counted where it finally ran).
func (s *Stats) RecordFailedAttempt(backend string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	day := dayKey(at)
	m := s.days[day]
	if m == nil {
		m = map[string]*DayStat{}
		s.days[day] = m
	}
	if m[backend] == nil {
		m[backend] = &DayStat{}
	}
	m[backend].Errors++
	s.dirty = true
}
