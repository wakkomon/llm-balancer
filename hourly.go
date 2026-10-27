package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// Hourly statistics: how long the slots of each server were busy per hour (for the utilization in
// the day view) and how the queue behaved. Kept for hourKeepDays days.

const hourKeepDays = 62

// HourStat is one server in one hour.
type HourStat struct {
	Requests int64 `json:"requests"` // requests that finished in this hour
	BusyMs   int64 `json:"busy_ms"`  // slot time used in this hour (requests split at hour boundaries)
}

// QueueStat describes the queue in one hour or day.
type QueueStat struct {
	Requests  int64 `json:"requests"`    // requests that needed a slot
	Waited    int64 `json:"waited"`      // … of which waited ≥ 250 ms
	WaitMs    int64 `json:"wait_ms"`     // total waiting time
	MaxWaitMs int64 `json:"max_wait_ms"` // longest wait
	MaxLen    int   `json:"max_len"`     // longest queue
	Rejected  int64 `json:"rejected"`    // queue full / timeout / all servers down
}

func (q *QueueStat) add(o *QueueStat) {
	q.Requests += o.Requests
	q.Waited += o.Waited
	q.WaitMs += o.WaitMs
	q.MaxWaitMs = max(q.MaxWaitMs, o.MaxWaitMs)
	q.MaxLen = max(q.MaxLen, o.MaxLen)
	q.Rejected += o.Rejected
}

func (s *Stats) initHourly() {
	s.hours = map[string]map[string]*HourStat{}
	s.qhours = map[string]*QueueStat{}
	s.qdays = map[string]*QueueStat{}
}

func hourKey(t time.Time) string { return t.In(time.Local).Format("2006-01-02T15") }

const waitedThreshold = 250 * time.Millisecond

// recordHourly is called from Record (s.mu held).
func (s *Stats) recordHourlyLocked(r *ReqInfo, backend string) {
	// slot time, split over the hours the request ran in
	if !r.StartedAt.IsZero() && r.EndedAt.After(r.StartedAt) && backend != NoBackend {
		t := r.StartedAt
		for t.Before(r.EndedAt) {
			next := t.In(time.Local).Truncate(time.Hour).Add(time.Hour)
			if next.After(r.EndedAt) {
				next = r.EndedAt
			}
			s.hourLocked(hourKey(t), backend).BusyMs += next.Sub(t).Milliseconds()
			t = next
		}
	}
	if backend != NoBackend {
		s.hourLocked(hourKey(r.EndedAt), backend).Requests++
	}
	// queue
	wait := r.EndedAt.Sub(r.QueuedAt)
	if !r.StartedAt.IsZero() {
		wait = r.StartedAt.Sub(r.QueuedAt)
	}
	q := QueueStat{Requests: 1, WaitMs: wait.Milliseconds(), MaxWaitMs: wait.Milliseconds()}
	if wait >= waitedThreshold {
		q.Waited = 1
	}
	if r.StartedAt.IsZero() && r.Status != 499 {
		q.Rejected = 1
	}
	at := r.QueuedAt // booked when it was queued
	if at.IsZero() {
		at = r.EndedAt
	}
	s.queueLocked(at, true).add(&q)
	s.queueLocked(at, false).add(&q)
}

func (s *Stats) hourLocked(hour, backend string) *HourStat {
	m := s.hours[hour]
	if m == nil {
		m = map[string]*HourStat{}
		s.hours[hour] = m
	}
	h := m[backend]
	if h == nil {
		h = &HourStat{}
		m[backend] = h
	}
	return h
}

func (s *Stats) queueLocked(t time.Time, hourly bool) *QueueStat {
	mp, key := s.qdays, dayKey(t)
	if hourly {
		mp, key = s.qhours, hourKey(t)
	}
	q := mp[key]
	if q == nil {
		q = &QueueStat{}
		mp[key] = q
	}
	return q
}

// NoteQueueLen records the queue length (called when the queue grows).
func (s *Stats) NoteQueueLen(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, q := range []*QueueStat{s.queueLocked(now, true), s.queueLocked(now, false)} {
		if n > q.MaxLen {
			q.MaxLen = n
			s.dirty = true
		}
	}
}

func (s *Stats) pruneHourlyLocked(now time.Time) {
	cut := hourKey(now.AddDate(0, 0, -hourKeepDays))
	for k := range s.hours {
		if k < cut {
			delete(s.hours, k)
			s.dirty = true
		}
	}
	for k := range s.qhours {
		if k < cut {
			delete(s.qhours, k)
			s.dirty = true
		}
	}
}

// ---- day view ----

type hourAvg struct {
	BusyMs   float64 `json:"busy_ms"`
	Requests float64 `json:"requests"`
}

type queueAvg struct {
	Requests  float64 `json:"requests"`
	Waited    float64 `json:"waited"`
	WaitMs    float64 `json:"wait_ms"`
	MaxWaitMs float64 `json:"max_wait_ms"` // average of the daily maxima
	MaxLen    float64 `json:"max_len"`
}

type DayView struct {
	Date       string                `json:"date"`
	NowHour    int                   `json:"now_hour"`
	NowMinute  int                   `json:"now_minute"`
	AvgDays    int                   `json:"avg_days"` // days the average is made of
	Backends   []string              `json:"backends"`
	Slots      map[string]int        `json:"slots"`
	Today      map[string][]HourStat `json:"today"` // backend → 24 hours
	Avg        map[string][]hourAvg  `json:"avg"`   // backend → 24 hours, average of the previous days
	QueueToday []QueueStat           `json:"queue_today"`
	QueueAvg   []queueAvg            `json:"queue_avg"`
}

// Day builds the day view for the given date (normally today) and the average of the `days` days before.
func (s *Stats) Day(date time.Time, days int, slots map[string]int) DayView {
	date = date.In(time.Local)
	v := DayView{Date: dayKey(date), NowHour: date.Hour(), NowMinute: date.Minute(), Slots: slots,
		Today: map[string][]HourStat{}, Avg: map[string][]hourAvg{},
		QueueToday: make([]QueueStat, 24), QueueAvg: make([]queueAvg, 24), Backends: []string{}}
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			v.Backends = append(v.Backends, name)
			v.Today[name] = make([]HourStat, 24)
			v.Avg[name] = make([]hourAvg, 24)
		}
	}
	for name := range slots {
		add(name)
	}
	for h := 0; h < 24; h++ {
		key := v.Date + "T" + pad2(h)
		for name, st := range s.hours[key] {
			add(name)
			v.Today[name][h] = *st
		}
		if q := s.qhours[key]; q != nil {
			v.QueueToday[h] = *q
		}
	}
	// average over the previous days that have data
	for d := 1; d <= days; d++ {
		day := dayKey(date.AddDate(0, 0, -d))
		has := false
		for h := 0; h < 24; h++ {
			key := day + "T" + pad2(h)
			if len(s.hours[key]) > 0 || s.qhours[key] != nil {
				has = true
				break
			}
		}
		if !has {
			continue
		}
		v.AvgDays++
		for h := 0; h < 24; h++ {
			key := day + "T" + pad2(h)
			for name, st := range s.hours[key] {
				add(name)
				v.Avg[name][h].BusyMs += float64(st.BusyMs)
				v.Avg[name][h].Requests += float64(st.Requests)
			}
			if q := s.qhours[key]; q != nil {
				a := &v.QueueAvg[h]
				a.Requests += float64(q.Requests)
				a.Waited += float64(q.Waited)
				a.WaitMs += float64(q.WaitMs)
				a.MaxWaitMs += float64(q.MaxWaitMs)
				a.MaxLen += float64(q.MaxLen)
			}
		}
	}
	if v.AvgDays > 0 {
		n := float64(v.AvgDays)
		for _, arr := range v.Avg {
			for h := range arr {
				arr[h].BusyMs /= n
				arr[h].Requests /= n
			}
		}
		for h := range v.QueueAvg {
			a := &v.QueueAvg[h]
			a.Requests, a.Waited, a.WaitMs, a.MaxWaitMs, a.MaxLen = a.Requests/n, a.Waited/n, a.WaitMs/n, a.MaxWaitMs/n, a.MaxLen/n
		}
	}
	sort.Strings(v.Backends)
	return v
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

func registerDayView(mux *http.ServeMux, pre string, pool *Pool) {
	mux.HandleFunc("GET "+pre+"/api/stats/day", func(w http.ResponseWriter, r *http.Request) {
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		if days <= 0 || days > hourKeepDays {
			days = 7
		}
		date := time.Now()
		if d := r.URL.Query().Get("date"); d != "" {
			if t, err := time.ParseInLocation("2006-01-02", d, time.Local); err == nil {
				date = t.Add(23*time.Hour + 59*time.Minute)
			}
		}
		pool.mu.Lock()
		slots := map[string]int{}
		for _, b := range pool.backends {
			slots[b.Name] = b.Max
		}
		pool.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(pool.stats.Day(date, days, slots))
	})
}
