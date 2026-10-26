package main

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrQueueFull    = errors.New("queue full")
	ErrQueueTimeout = errors.New("timed out waiting in queue")
	ErrNoBackend    = errors.New("no usable backend left")
)

// Backend is the runtime state of one upstream server.
type Backend struct {
	Name     string
	URL      string
	target   *url.URL
	Max      int
	Priority int

	// guarded by Pool.mu
	enabled      bool
	healthy      bool
	healthMsg    string
	healthAt     time.Time
	unhealthyTil time.Time // used when active health checks are disabled
	active       int
	lastAssigned time.Time
	total        int64
	errors       int64
	sumDur       time.Duration
	sumTTFB      time.Duration
	finished     int64
	lastErr      string
	lastErrAt    time.Time
}

func (b *Backend) usable(now time.Time) bool {
	return b.enabled && (b.healthy || (!b.unhealthyTil.IsZero() && now.After(b.unhealthyTil)))
}

// ReqInfo tracks one client request for the status UI.
type ReqInfo struct {
	ID        uint64    `json:"id"`
	Method    string    `json:"method"`
	Path      string    `json:"path"`
	Model     string    `json:"model,omitempty"`
	Stream    bool      `json:"stream"`
	Client    string    `json:"client"`
	State     string    `json:"state"` // queued | running | done
	Backend   string    `json:"backend,omitempty"`
	Attempts  int       `json:"attempts"`
	QueuedAt  time.Time `json:"queued_at"`
	StartedAt time.Time `json:"started_at,omitempty"`
	FirstByte time.Time `json:"-"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	Status    int       `json:"status,omitempty"`
	Error     string    `json:"error,omitempty"`
	bytes     atomic.Int64

	// derived for JSON output
	Bytes   int64 `json:"bytes"`
	QueueMs int64 `json:"queue_ms"`
	TTFBMs  int64 `json:"ttfb_ms,omitempty"`
	DurMs   int64 `json:"duration_ms,omitempty"`
}

type waiter struct {
	req     *ReqInfo
	exclude map[*Backend]bool
	ch      chan *Backend // buffered(1); written under Pool.mu
}

// Pool owns the backends, the FIFO queue and request bookkeeping.
type Pool struct {
	cfg      Config
	mu       sync.Mutex
	backends []*Backend
	queue    []*waiter
	active   map[uint64]*ReqInfo
	recent   []*ReqInfo
	nextID   uint64
	started  time.Time
	rr       int

	totalReqs     int64
	totalRejected int64
	maxQueueSeen  int
}

func NewPool(cfg Config) *Pool {
	p := &Pool{cfg: cfg, active: map[uint64]*ReqInfo{}, started: time.Now()}
	for _, bc := range cfg.Backends {
		u, _ := url.Parse(bc.URL)
		en := bc.Enabled == nil || *bc.Enabled
		p.backends = append(p.backends, &Backend{
			Name: bc.Name, URL: bc.URL, target: u, Max: bc.MaxConcurrent, Priority: bc.Priority,
			enabled: en, healthy: true, // optimistic until the first health check says otherwise
		})
	}
	return p
}

func (p *Pool) NewRequest(method, path, client string) *ReqInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextID++
	p.totalReqs++
	r := &ReqInfo{ID: p.nextID, Method: method, Path: path, Client: client, State: "queued", QueuedAt: time.Now()}
	p.active[r.ID] = r
	return r
}

// pickFree returns the best free backend or nil. Caller holds p.mu.
func (p *Pool) pickFree(exclude map[*Backend]bool) *Backend {
	now := time.Now()
	var best *Backend
	for _, b := range p.backends {
		if exclude[b] || !b.usable(now) || b.active >= b.Max {
			continue
		}
		if best == nil || better(b, best) {
			best = b
		}
	}
	return best
}

// better: priority first, then lower load ratio, then least recently assigned (round robin).
func better(a, b *Backend) bool {
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	la, lb := float64(a.active)/float64(a.Max), float64(b.active)/float64(b.Max)
	if la != lb {
		return la < lb
	}
	return a.lastAssigned.Before(b.lastAssigned)
}

func (p *Pool) assign(b *Backend, r *ReqInfo) {
	b.active++
	b.total++
	b.lastAssigned = time.Now()
	r.Backend = b.Name
	r.State = "running"
	r.Attempts++
	if r.StartedAt.IsZero() {
		r.StartedAt = time.Now()
	}
}

// Acquire returns a backend with a free slot, waiting in the FIFO queue if necessary.
func (p *Pool) Acquire(ctx context.Context, r *ReqInfo, exclude map[*Backend]bool) (*Backend, error) {
	p.mu.Lock()
	if len(exclude) > 0 && !p.anyCandidate(exclude) {
		p.mu.Unlock()
		return nil, ErrNoBackend
	}
	// Only take a slot directly if nobody is waiting (FIFO fairness).
	if len(p.queue) == 0 {
		if b := p.pickFree(exclude); b != nil {
			p.assign(b, r)
			p.mu.Unlock()
			return b, nil
		}
	}
	if p.cfg.MaxQueue > 0 && len(p.queue) >= p.cfg.MaxQueue {
		p.totalRejected++
		p.mu.Unlock()
		return nil, ErrQueueFull
	}
	w := &waiter{req: r, exclude: exclude, ch: make(chan *Backend, 1)}
	r.State = "queued"
	if len(exclude) > 0 {
		// retries already waited once: put them at the front of the queue
		p.queue = append([]*waiter{w}, p.queue...)
	} else {
		p.queue = append(p.queue, w)
	}
	if len(p.queue) > p.maxQueueSeen {
		p.maxQueueSeen = len(p.queue)
	}
	p.dispatchLocked() // a slot may be free for this waiter even if earlier waiters are blocked by exclusions
	p.mu.Unlock()

	var timeout <-chan time.Time
	if d := p.cfg.queueTimeout(); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case b := <-w.ch:
		return b, nil
	case <-ctx.Done():
		return nil, p.abandon(w, ctx.Err())
	case <-timeout:
		return nil, p.abandon(w, ErrQueueTimeout)
	}
}

// abandon removes a waiter; if it was already handed a backend, the slot is given back.
func (p *Pool) abandon(w *waiter, err error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, q := range p.queue {
		if q == w {
			p.queue = append(p.queue[:i], p.queue[i+1:]...)
			return err
		}
	}
	// Already assigned between select and lock.
	b := <-w.ch
	b.active--
	b.total--
	p.dispatchLocked()
	return err
}

// anyCandidate: is there any enabled backend not in exclude (busy or not)? Caller holds p.mu.
func (p *Pool) anyCandidate(exclude map[*Backend]bool) bool {
	for _, b := range p.backends {
		if b.enabled && !exclude[b] {
			return true
		}
	}
	return false
}

// dispatchLocked hands free slots to waiting requests in FIFO order. Caller holds p.mu.
func (p *Pool) dispatchLocked() {
	for i := 0; i < len(p.queue); {
		w := p.queue[i]
		b := p.pickFree(w.exclude)
		if b == nil {
			if len(w.exclude) == 0 {
				return // nothing free for a normal waiter → nothing free for anyone after it
			}
			i++
			continue
		}
		p.queue = append(p.queue[:i], p.queue[i+1:]...)
		p.assign(b, w.req)
		w.ch <- b
	}
}

// Release frees a slot. failed marks a transport/upstream failure for statistics.
func (p *Pool) Release(b *Backend, r *ReqInfo, errMsg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b.active--
	if errMsg != "" {
		b.errors++
		b.lastErr = errMsg
		b.lastErrAt = time.Now()
	}
	p.dispatchLocked()
}

// MarkUnhealthy takes a backend out of rotation until the next successful health check.
func (p *Pool) MarkUnhealthy(b *Backend, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b.healthy = false
	b.healthMsg = msg
	b.healthAt = time.Now()
	if p.cfg.HealthPath == "" {
		b.unhealthyTil = time.Now().Add(time.Duration(p.cfg.UnhealthyCooldownSec) * time.Second)
	}
}

func (p *Pool) SetHealth(b *Backend, ok bool, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b.healthy = ok
	b.healthMsg = msg
	b.healthAt = time.Now()
	if ok {
		b.unhealthyTil = time.Time{}
		p.dispatchLocked()
	}
}

func (p *Pool) SetEnabled(name string, en bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, b := range p.backends {
		if b.Name == name {
			b.enabled = en
			if en {
				p.dispatchLocked()
			}
			return true
		}
	}
	return false
}

// PickPassthrough chooses a backend for requests that don't occupy a slot (e.g. GET /v1/models).
func (p *Pool) PickPassthrough() *Backend {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var cands []*Backend
	for _, b := range p.backends {
		if b.usable(now) {
			cands = append(cands, b)
		}
	}
	if len(cands) == 0 {
		return nil
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].Priority < cands[j].Priority })
	p.rr++
	return cands[p.rr%len(cands)]
}

// Finish records the outcome of a request (successful or not).
func (p *Pool) Finish(r *ReqInfo, b *Backend, status int, errMsg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r.EndedAt = time.Now()
	r.Status = status
	r.Error = errMsg
	r.State = "done"
	delete(p.active, r.ID)
	if b != nil && !r.StartedAt.IsZero() {
		b.finished++
		b.sumDur += r.EndedAt.Sub(r.StartedAt)
		if !r.FirstByte.IsZero() {
			b.sumTTFB += r.FirstByte.Sub(r.StartedAt)
		}
	}
	p.recent = append(p.recent, r)
	if over := len(p.recent) - p.cfg.RecentRequests; over > 0 {
		p.recent = p.recent[over:]
	}
}

func (p *Pool) setFirstByte(r *ReqInfo) {
	p.mu.Lock()
	if r.FirstByte.IsZero() {
		r.FirstByte = time.Now()
	}
	p.mu.Unlock()
}

// ---- status snapshot for the UI / API ----

type BackendStatus struct {
	Name        string     `json:"name"`
	URL         string     `json:"url"`
	Enabled     bool       `json:"enabled"`
	Healthy     bool       `json:"healthy"`
	HealthMsg   string     `json:"health_msg,omitempty"`
	HealthAt    string     `json:"health_at,omitempty"`
	Active      int        `json:"active"`
	Max         int        `json:"max"`
	Priority    int        `json:"priority"`
	Total       int64      `json:"total"`
	Errors      int64      `json:"errors"`
	AvgDurMs    int64      `json:"avg_duration_ms"`
	AvgTTFBMs   int64      `json:"avg_ttfb_ms"`
	LastError   string     `json:"last_error,omitempty"`
	LastErrorAt string     `json:"last_error_at,omitempty"`
	Running     []*ReqInfo `json:"running"`
}

type Status struct {
	Now        time.Time       `json:"now"`
	UptimeSec  int64           `json:"uptime_sec"`
	Backends   []BackendStatus `json:"backends"`
	Queue      []*ReqInfo      `json:"queue"`
	Recent     []*ReqInfo      `json:"recent"`
	TotalReqs  int64           `json:"total_requests"`
	Rejected   int64           `json:"rejected"`
	MaxQueue   int             `json:"max_queue_seen"`
	FreeSlots  int             `json:"free_slots"`
	TotalSlots int             `json:"total_slots"`
}

func snapshot(r *ReqInfo, now time.Time) *ReqInfo {
	c := &ReqInfo{
		ID: r.ID, Method: r.Method, Path: r.Path, Model: r.Model, Stream: r.Stream, Client: r.Client,
		State: r.State, Backend: r.Backend, Attempts: r.Attempts, QueuedAt: r.QueuedAt,
		StartedAt: r.StartedAt, EndedAt: r.EndedAt, Status: r.Status, Error: r.Error,
	}
	c.Bytes = r.bytes.Load()
	end := now
	if !r.EndedAt.IsZero() {
		end = r.EndedAt
	}
	if r.StartedAt.IsZero() {
		c.QueueMs = end.Sub(r.QueuedAt).Milliseconds()
	} else {
		c.QueueMs = r.StartedAt.Sub(r.QueuedAt).Milliseconds()
		c.DurMs = end.Sub(r.StartedAt).Milliseconds()
		if !r.FirstByte.IsZero() {
			c.TTFBMs = r.FirstByte.Sub(r.StartedAt).Milliseconds()
		}
	}
	return c
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

func (p *Pool) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	st := Status{Now: now, UptimeSec: int64(now.Sub(p.started).Seconds()), TotalReqs: p.totalReqs,
		Rejected: p.totalRejected, MaxQueue: p.maxQueueSeen}
	running := map[string][]*ReqInfo{}
	for _, r := range p.active {
		if r.State == "running" {
			running[r.Backend] = append(running[r.Backend], snapshot(r, now))
		}
	}
	for _, b := range p.backends {
		bs := BackendStatus{Name: b.Name, URL: b.URL, Enabled: b.enabled, Healthy: b.healthy || (!b.unhealthyTil.IsZero() && now.After(b.unhealthyTil)),
			HealthMsg: b.healthMsg, HealthAt: fmtTime(b.healthAt), Active: b.active, Max: b.Max, Priority: b.Priority,
			Total: b.total, Errors: b.errors, LastError: b.lastErr, LastErrorAt: fmtTime(b.lastErrAt),
			Running: running[b.Name]}
		if bs.Running == nil {
			bs.Running = []*ReqInfo{}
		}
		sort.Slice(bs.Running, func(i, j int) bool { return bs.Running[i].ID < bs.Running[j].ID })
		if b.finished > 0 {
			bs.AvgDurMs = (b.sumDur / time.Duration(b.finished)).Milliseconds()
			bs.AvgTTFBMs = (b.sumTTFB / time.Duration(b.finished)).Milliseconds()
		}
		st.TotalSlots += b.Max
		if b.enabled && b.usable(now) {
			st.FreeSlots += max(0, b.Max-b.active)
		}
		st.Backends = append(st.Backends, bs)
	}
	st.Queue = []*ReqInfo{}
	for _, w := range p.queue {
		st.Queue = append(st.Queue, snapshot(w.req, now))
	}
	st.Recent = []*ReqInfo{}
	for i := len(p.recent) - 1; i >= 0; i-- {
		st.Recent = append(st.Recent, snapshot(p.recent[i], now))
	}
	return st
}
