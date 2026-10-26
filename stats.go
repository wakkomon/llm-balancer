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
	Requests  int64 `json:"requests"`
	Errors    int64 `json:"errors"`    // upstream errors, 5xx, queue timeouts
	Cancelled int64 `json:"cancelled"` // client went away
	DurMs     int64 `json:"duration_ms"`
	MaxDurMs  int64 `json:"max_duration_ms"`
	TTFBMs    int64 `json:"ttfb_ms"`
	TTFBCount int64 `json:"ttfb_count"`
	QueueMs   int64 `json:"queue_ms"`
	Bytes     int64 `json:"bytes"`
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
}

// Stats keeps per-day, per-backend counters and persists them to DATA_DIR/stats.json.
type Stats struct {
	mu       sync.Mutex
	days     map[string]map[string]*DayStat // "2006-01-02" → backend → stat
	path     string
	keepDays int
	dirty    bool
}

func NewStats(dir string, keepDays int) *Stats {
	s := &Stats{days: map[string]map[string]*DayStat{}, keepDays: keepDays}
	if dir != "" {
		s.path = filepath.Join(dir, "stats.json")
		data, err := os.ReadFile(s.path)
		switch {
		case err == nil:
			if err := json.Unmarshal(data, &s.days); err != nil {
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
	d := DayStat{Requests: 1, Bytes: r.bytes.Load()}
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
	s.dirty = true
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
	}
	if !s.dirty || s.path == "" {
		return
	}
	data, _ := json.Marshal(s.days)
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
}

func (s *Stats) Report(days int, now time.Time) StatsReport {
	if days <= 0 {
		days = 30
	}
	rep := StatsReport{Data: map[string]map[string]*DayStat{}, Totals: map[string]*DayStat{}}
	start := now.In(time.Local).AddDate(0, 0, -(days - 1))
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for i := 0; i < days; i++ {
		day := dayKey(start.AddDate(0, 0, i))
		rep.Days = append(rep.Days, day)
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
	rep.From, rep.To = rep.Days[0], rep.Days[len(rep.Days)-1]
	return rep
}

func (s *Stats) loop(stop <-chan struct{}) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.Flush()
		case <-stop:
			s.Flush()
			return
		}
	}
}

func registerStats(mux *http.ServeMux, pre string, stats *Stats, token string) {
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
			cw.Write([]string{"Tag", "Server", "Anfragen", "Fehler", "Abgebrochen", "Gesamtdauer_s", "Avg_Dauer_s", "Max_Dauer_s", "Avg_1.Byte_s", "Avg_Wartezeit_s", "Bytes"})
			sec := func(ms, n int64) string {
				if n == 0 {
					return "0"
				}
				return strconv.FormatFloat(float64(ms)/float64(n)/1000, 'f', 2, 64)
			}
			for _, day := range rep.Days {
				for _, name := range rep.Backends {
					st := rep.Data[day][name]
					if st == nil {
						continue
					}
					cw.Write([]string{day, name, itoa(st.Requests), itoa(st.Errors), itoa(st.Cancelled),
						sec(st.DurMs, 1), sec(st.DurMs, st.Requests), sec(st.MaxDurMs, 1),
						sec(st.TTFBMs, st.TTFBCount), sec(st.QueueMs, st.Requests), itoa(st.Bytes)})
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
		log.Printf("statistics reset via UI")
		w.WriteHeader(http.StatusNoContent)
	}))
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
