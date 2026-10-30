package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// Merge moves the statistics of one server ("backend") or API token ("token") onto another name –
// used when a server or token is renamed, and from the UI for entries left over from earlier renames.
func (s *Stats) Merge(kind, from, to string) (int, error) {
	from, to = strings.TrimSpace(from), strings.TrimSpace(to)
	if from == "" || to == "" || from == to {
		return 0, errors.New("alter und neuer Name nötig")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	switch kind {
	case "backend":
		for _, m := range s.days {
			if st := m[from]; st != nil {
				if m[to] == nil {
					m[to] = &DayStat{}
				}
				m[to].add(st)
				delete(m, from)
				n++
			}
		}
		for _, m := range s.hours {
			if h := m[from]; h != nil {
				if m[to] == nil {
					m[to] = &HourStat{}
				}
				m[to].Requests += h.Requests
				m[to].BusyMs += h.BusyMs
				delete(m, from)
			}
		}
	case "token":
		for _, m := range s.tokdays {
			if st := m[from]; st != nil {
				if m[to] == nil {
					m[to] = &TokStat{}
				}
				m[to].add(st)
				delete(m, from)
				n++
			}
		}
	default:
		return 0, fmt.Errorf("unbekannte Art %q", kind)
	}
	if n > 0 {
		s.dirty = true
		log.Printf("stats: %s %q merged into %q (%d days)", kind, from, to, n)
	}
	return n, nil
}

func registerStatsMerge(mux *http.ServeMux, pre string, stats *Stats, token string) {
	mux.HandleFunc("POST "+pre+"/api/stats/merge", adminAuth(token, func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Kind, From, To string }
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<10)).Decode(&in); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		n, err := stats.Merge(in.Kind, in.From, in.To)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		go stats.Flush()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int{"days": n})
	}))
}
