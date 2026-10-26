package main

import (
	"context"
	"errors"
	"log"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrQueueFull    = errors.New("queue full")
	ErrQueueTimeout = errors.New("timed out waiting in queue")
	ErrNoBackend    = errors.New("no usable backend left")
	ErrAllDown      = errors.New("alle Server sind down oder deaktiviert")
)

// Backend is the runtime state of one upstream server.
type Backend struct {
	Name     string
	URL      string
	target   *url.URL
	Max      int
	Priority int
	apiKey   string
	insecure bool // don't verify the TLS certificate (self-signed)
	priceIn  float64
	priceOut float64
	group    string   // guarded by Pool.mu
	models   []string // reported by the server via /v1/models
	modelsAt time.Time

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
	lastOkAt     time.Time
	// tokens per second
	genTokens    int64
	genTime      time.Duration
	promptTokens int64
	promptTime   time.Duration
	lastTPS      float64
	tagErrors    int64
}

func (b *Backend) usable(now time.Time) bool {
	return b.enabled && (b.healthy || (!b.unhealthyTil.IsZero() && now.After(b.unhealthyTil)))
}

// ReqInfo tracks one client request for the status UI.
type ReqInfo struct {
	ID          uint64   `json:"id"`
	Method      string   `json:"method"`
	Path        string   `json:"path"`
	Model       string   `json:"model,omitempty"`
	Stream      bool     `json:"stream"`
	Client      string   `json:"client"`
	Token       string   `json:"token,omitempty"` // name of the client access token
	Group       string   `json:"group,omitempty"`
	TokensIn    int64    `json:"tokens_in,omitempty"`
	TokensOut   int64    `json:"tokens_out,omitempty"`
	Cost        float64  `json:"cost,omitempty"`         // incoming: API token prices
	BackendCost float64  `json:"backend_cost,omitempty"` // outgoing: server prices
	Seq         uint64   `json:"seq,omitempty"`          // order in which requests finished
	TagIssues   []string `json:"tag_issues,omitempty"`
	// tokens per second: generation (output tokens / time after the first byte) and prompt processing
	GenTPS    float64 `json:"gen_tps,omitempty"`
	PromptTPS float64 `json:"prompt_tps,omitempty"`
	LiveTPS   float64 `json:"live_tps,omitempty"`        // running requests: estimate from streamed chunks
	Effective string  `json:"effective_model,omitempty"` // model actually sent (fallback with replacement model)
	chunks    atomic.Int64
	lastChunk atomic.Int64 // unix nano of the latest content chunk
	rewrite   string       // model to send instead (fallback)
	softRetry *Backend     // server that answered 503/429 ("busy") – may be used again once idle
	Trail     string       `json:"trail,omitempty"` // servers tried: "a: 500 → b: 503 → f (Fallback): 200"
	cancel    context.CancelFunc
	stopped   atomic.Bool
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
	model   string // routing is re-evaluated on every dispatch (groups/servers can change while waiting)
	level   int    // route escalation: 1 = fallback group right away, 2 = also other servers with the model
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
	nextSeq  uint64
	started  time.Time
	rr       int

	store         *Store
	initial       []BackendConfig // from BACKENDS / config.json, used for "reset"
	source        string          // "env" or "saved"
	healthKick    chan struct{}
	stats         *Stats
	countersPath  string
	countersSaved []byte
	groups        []GroupConfig
	initialGroups []GroupConfig
	access        *Access

	totalReqs     int64
	totalRejected int64
	maxQueueSeen  int
}

// NewPool builds the pool. initial are the backends from BACKENDS/config.json; if store holds a
// saved list (edited in the UI), that one is used instead.
func NewPool(cfg Config, initial []BackendConfig, store *Store) (*Pool, error) {
	p := &Pool{cfg: cfg, active: map[uint64]*ReqInfo{}, started: time.Now(), store: store,
		initial: initial, source: "env", healthKick: make(chan struct{}, 1),
		initialGroups: append([]GroupConfig{}, cfg.Groups...)}
	list := initial
	p.groups = append([]GroupConfig{}, cfg.Groups...)
	go p.tick()
	if store != nil {
		saved, ok, err := store.Load()
		if err != nil {
			return nil, err
		}
		if ok {
			list, p.source = saved.Backends, "saved"
			p.groups = saved.Groups
		}
	}
	for _, bc := range list {
		p.backends = append(p.backends, newBackend(bc))
	}
	return p, nil
}

// upstreamOf returns a copy of the backend URL and its bearer token (both can be changed at runtime).
func (p *Pool) upstreamOf(b *Backend) (url.URL, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return *b.target, b.apiKey
}

func (p *Pool) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.backends)
}

type healthTarget struct {
	b        *Backend
	url      string
	key      string
	insecure bool
}

func (p *Pool) skipVerify(b *Backend) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return b.insecure
}

// idleTargets lists backends without running requests for the health checker.
func (p *Pool) idleTargets() []healthTarget {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []healthTarget
	for _, b := range p.backends {
		if b.active == 0 {
			out = append(out, healthTarget{b, b.URL, b.apiKey, b.insecure})
		}
	}
	return out
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
func (p *Pool) pickFree(exclude map[*Backend]bool, rt Route) *Backend {
	now := time.Now()
	var best *Backend
	for _, b := range p.backends {
		if exclude[b] || !rt.allows(b) || !b.usable(now) || b.active >= b.Max {
			continue
		}
		if best != nil {
			_, bFb := rt.fallback[b]
			_, bestFb := rt.fallback[best]
			if bFb != bestFb { // a free server of the requested group always beats a fallback server
				if bestFb {
					best = b
				}
				continue
			}
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

// assignRoute assigns b and records whether it was taken as a fallback (and which model to send).
func (p *Pool) assignRoute(b *Backend, r *ReqInfo, rt Route) {
	r.Group, r.rewrite, r.Effective = rt.Group, "", ""
	model := ""
	if g := p.groupLocked(b.group); g != nil {
		model = g.ForceModel // the server's own group may force a model name
	}
	if m, ok := rt.fallback[b]; ok {
		target := b.group
		if target == "" {
			target = "ohne Gruppe"
		}
		r.Group = rt.Group + " → " + target
		if m != "" { // replacement model of the fallback setting wins
			model = m
		}
	}
	if model != "" && !strings.EqualFold(model, r.Model) {
		r.rewrite, r.Effective = model, model
	}
	p.assign(b, r)
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

// Acquire returns a backend with a free slot that may serve the request's model,
// waiting in the FIFO queue if necessary.
func (p *Pool) Acquire(ctx context.Context, r *ReqInfo, exclude map[*Backend]bool) (*Backend, error) {
	p.mu.Lock()
	rt := p.routeWaitLocked(r.Model, 0, 0)
	level := 0
	if rt.Mode == "group" && !p.anyUsableEx(rt, exclude) {
		// every server of the group is down, disabled or already failed for this request:
		// 1. the fallback group right away, 2. any other server that reports the model
		for lv := 1; lv <= 2; lv++ {
			if rf := p.routeWaitLocked(r.Model, 0, lv); p.anyUsableEx(rf, exclude) {
				rt, level = rf, lv
				break
			}
		}
	}
	r.Group = rt.Group
	if rt.Mode == "reject" {
		p.totalRejected++
		p.mu.Unlock()
		return nil, ErrNoRoute
	}
	if len(p.backends) == 0 || (len(exclude) > 0 && !p.anyCandidate(exclude, rt)) {
		p.mu.Unlock()
		return nil, ErrNoBackend
	}
	// Take a free slot directly unless an earlier waiter could use that very server (FIFO fairness).
	if b := p.pickFree(exclude, rt); b != nil && !p.someWaiterWants(b) {
		p.assignRoute(b, r, rt)
		p.mu.Unlock()
		return b, nil
	}
	if !p.anyUsableEx(rt, exclude) {
		if len(exclude) > 0 {
			p.mu.Unlock()
			return nil, ErrNoBackend // every remaining candidate is down: the caller passes the last error on
		}
		// waiting makes no sense if no server could ever pick this up right now
		p.totalRejected++
		p.mu.Unlock()
		return nil, ErrAllDown
	}
	if p.cfg.MaxQueue > 0 && len(p.queue) >= p.cfg.MaxQueue {
		p.totalRejected++
		p.mu.Unlock()
		return nil, ErrQueueFull
	}
	w := &waiter{req: r, model: r.Model, level: level, exclude: exclude, ch: make(chan *Backend, 1)}
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
	p.dispatchLocked()
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
func (p *Pool) anyCandidate(exclude map[*Backend]bool, rt Route) bool {
	for _, b := range p.backends {
		if b.enabled && !exclude[b] && rt.allows(b) {
			return true
		}
	}
	return false
}

// someWaiterWants: could a queued request use server b? Caller holds p.mu.
func (p *Pool) someWaiterWants(b *Backend) bool {
	for _, w := range p.queue {
		if !w.exclude[b] && p.routeWaitLocked(w.model, time.Since(w.req.QueuedAt), w.level).allows(b) {
			return true
		}
	}
	return false
}

// dispatchLocked hands free slots to waiting requests in FIFO order. A waiter that cannot be
// served (its group is busy) does not block waiters for other groups. Caller holds p.mu.
func (p *Pool) dispatchLocked() {
	for i := 0; i < len(p.queue); {
		w := p.queue[i]
		rt := p.routeWaitLocked(w.model, time.Since(w.req.QueuedAt), w.level)
		b := p.pickFree(w.exclude, rt)
		if b == nil && len(w.exclude) > 0 {
			// A retried request avoids the server that just failed/was busy – but if that server
			// is completely idle again (and healthy), using it beats waiting for the others.
			b = p.pickIdleExcluded(w.req, rt)
		}
		if b == nil {
			i++
			continue
		}
		p.queue = append(p.queue[:i], p.queue[i+1:]...)
		p.assignRoute(b, w.req, rt)
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

func (p *Pool) SetHealth(b *Backend, ok bool, msg string, checkedURL string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if b.URL != checkedURL {
		return // URL was changed while the check was running
	}
	b.healthy = ok
	b.healthMsg = msg
	b.healthAt = time.Now()
	if ok {
		b.unhealthyTil = time.Time{}
		p.dispatchLocked()
	}
}

// PickPassthroughEx chooses a backend for requests that don't occupy a slot (e.g. GET /v1/models),
// skipping the ones already tried: healthy ones first (by priority, round robin), then the others.
func (p *Pool) PickPassthroughEx(tried map[*Backend]bool) (*Backend, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var healthy, rest []*Backend
	for _, b := range p.backends {
		if tried[b] || !b.enabled {
			continue
		}
		if b.usable(now) {
			healthy = append(healthy, b)
		} else {
			rest = append(rest, b)
		}
	}
	cands := healthy
	if len(cands) == 0 {
		if len(tried) == 0 {
			return nil, "" // nothing healthy at all
		}
		cands = rest
	}
	if len(cands) == 0 {
		return nil, ""
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].Priority < cands[j].Priority })
	p.rr++
	best := cands[0].Priority
	n := 0
	for n < len(cands) && cands[n].Priority == best {
		n++
	}
	b := cands[p.rr%n]
	return b, b.Name
}

// Finish records the outcome of a request (successful or not).
func (p *Pool) Finish(r *ReqInfo, b *Backend, status int, errMsg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r.EndedAt = time.Now()
	r.Status = status
	r.Error = errMsg
	r.State = "done"
	p.nextSeq++
	r.Seq = p.nextSeq
	delete(p.active, r.ID)
	if b != nil && r.Status > 0 && r.Status < 500 && r.Status != 499 && r.Status != 401 && r.Status != 403 && errMsg == "" {
		b.lastOkAt = r.EndedAt
	}
	if b != nil && r.GenTPS > 0 {
		b.genTokens += r.TokensOut
		b.genTime += r.EndedAt.Sub(r.FirstByte)
		b.lastTPS = r.GenTPS
	}
	if b != nil && r.PromptTPS > 0 {
		b.promptTokens += r.TokensIn
		b.promptTime += r.FirstByte.Sub(r.StartedAt)
	}
	if b != nil && len(r.TagIssues) > 0 {
		b.tagErrors++
	}
	if p.stats != nil {
		p.stats.Record(r)
	}
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
	State       string     `json:"state"`          // idle | busy | error | down | disabled
	AvgTPS      float64    `json:"avg_tps"`        // output tokens/s over all requests with usage
	LastTPS     float64    `json:"last_tps"`       // of the last request
	PromptTPS   float64    `json:"avg_prompt_tps"` // prompt tokens/s (prompt processing until the first byte)
	TagErrors   int64      `json:"tag_errors"`
	HasAPIKey   bool       `json:"has_api_key"`
	TLSSkip     bool       `json:"tls_skip_verify"`
	PriceIn     float64    `json:"price_in"`
	PriceOut    float64    `json:"price_out"`
	Group       string     `json:"group"`
	Models      []string   `json:"models"`
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
	Source     string          `json:"config_source"` // env | saved
	DataFile   string          `json:"data_file,omitempty"`
	AdminToken bool            `json:"admin_token_required"`
	AccessAuth bool            `json:"access_auth"` // clients need an API token
	Groups     []GroupStatus   `json:"groups"`
	Currency   string          `json:"currency"`
	Version    string          `json:"version"`
	States     map[string]int  `json:"states"` // state → number of backends
	TotalSlots int             `json:"total_slots"`
}

func snapshot(r *ReqInfo, now time.Time) *ReqInfo {
	c := &ReqInfo{
		ID: r.ID, Method: r.Method, Path: r.Path, Model: r.Model, Stream: r.Stream, Client: r.Client, Token: r.Token,
		Group: r.Group, TokensIn: r.TokensIn, TokensOut: r.TokensOut, Cost: r.Cost, BackendCost: r.BackendCost, Seq: r.Seq,
		Trail: r.Trail, TagIssues: r.TagIssues, GenTPS: r.GenTPS, PromptTPS: r.PromptTPS, Effective: r.Effective,
		State: r.State, Backend: r.Backend, Attempts: r.Attempts, QueuedAt: r.QueuedAt,
		StartedAt: r.StartedAt, EndedAt: r.EndedAt, Status: r.Status, Error: r.Error,
	}
	c.Bytes = r.bytes.Load()
	if r.State == "running" && !r.FirstByte.IsZero() {
		if n := r.chunks.Load(); n > 1 {
			last := time.Unix(0, r.lastChunk.Load())
			if d := last.Sub(r.FirstByte).Seconds(); d > 0.2 {
				c.LiveTPS = float64(n-1) / d // ≈ one token per streamed chunk
			}
		}
	}
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
		Rejected: p.totalRejected, MaxQueue: p.maxQueueSeen, Source: p.source, AdminToken: p.cfg.AdminToken != ""}
	if p.store != nil {
		st.DataFile = p.store.path
	}
	if p.access != nil {
		st.AccessAuth = p.access.Enabled()
	}
	st.Currency = p.cfg.Currency
	st.Version = versionString()
	st.Groups = []GroupStatus{}
	for _, g := range p.groups {
		gs := GroupStatus{Name: g.Name, Models: append([]string{}, g.Models...), Auto: g.auto(), Members: []string{}, Available: []string{},
			Fallback: g.Fallback, FallbackAfterSec: g.FallbackAfterSec, FallbackModel: g.FallbackModel, ForceModel: g.ForceModel}
		seen := map[string]bool{}
		for _, m := range g.Models {
			if !strings.Contains(m, "*") && !seen[strings.ToLower(m)] {
				seen[strings.ToLower(m)] = true
				gs.Available = append(gs.Available, m)
			}
		}
		for _, b := range p.membersLocked(g.Name) {
			gs.Members = append(gs.Members, b.Name)
			for _, m := range b.models {
				if !seen[strings.ToLower(m)] && (g.auto() || matchesAny(g.Models, m)) {
					seen[strings.ToLower(m)] = true
					gs.Available = append(gs.Available, m)
				}
			}
		}
		for _, w := range p.queue {
			if strings.Contains(","+p.routeLocked(w.model).Group+",", ","+g.Name+",") {
				gs.Queued++
			}
		}
		sort.Slice(gs.Available, func(i, j int) bool { return strings.ToLower(gs.Available[i]) < strings.ToLower(gs.Available[j]) })
		st.Groups = append(st.Groups, gs)
	}
	st.Backends = []BackendStatus{}
	st.States = map[string]int{"idle": 0, "busy": 0, "error": 0, "down": 0, "disabled": 0}
	running := map[string][]*ReqInfo{}
	for _, r := range p.active {
		if r.State == "running" {
			running[r.Backend] = append(running[r.Backend], snapshot(r, now))
		}
	}
	for _, b := range p.backends {
		bs := BackendStatus{Name: b.Name, URL: b.URL, Enabled: b.enabled, Healthy: b.healthy || (!b.unhealthyTil.IsZero() && now.After(b.unhealthyTil)),
			HealthMsg: b.healthMsg, HealthAt: fmtTime(b.healthAt), Active: b.active, Max: b.Max, Priority: b.Priority,
			Total: b.total, Errors: b.errors,
			Running: running[b.Name]}
		bs.State = backendState(b, now)
		if b.lastErr != "" && !b.lastOkAt.After(b.lastErrAt) { // hide once a request succeeded again
			bs.LastError, bs.LastErrorAt = b.lastErr, fmtTime(b.lastErrAt)
		}
		if b.genTime > 0 {
			bs.AvgTPS = float64(b.genTokens) / b.genTime.Seconds()
		}
		if b.promptTime > 0 {
			bs.PromptTPS = float64(b.promptTokens) / b.promptTime.Seconds()
		}
		bs.LastTPS, bs.TagErrors = b.lastTPS, b.tagErrors
		bs.HasAPIKey = b.apiKey != ""
		bs.TLSSkip = b.insecure
		bs.PriceIn, bs.PriceOut = b.priceIn, b.priceOut
		bs.Group = b.group
		bs.Models = append([]string{}, b.models...)
		st.States[bs.State]++
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
	for i := len(p.recent) - 1; i >= 0 && len(st.Recent) < 20; i-- { // full list: /api/recent
		st.Recent = append(st.Recent, snapshot(p.recent[i], now))
	}
	return st
}

// errorWindow: a reachable server whose latest request failed counts as "error" for this long
// (or until a request succeeds again).
const errorWindow = 15 * time.Minute

// backendState classifies a backend for the UI. Caller holds p.mu.
func backendState(b *Backend, now time.Time) string {
	switch {
	case !b.enabled:
		return "disabled"
	case !b.usable(now):
		return "down"
	case b.active > 0:
		return "busy"
	case !b.lastErrAt.IsZero() && b.lastErrAt.After(b.lastOkAt) && now.Sub(b.lastErrAt) < errorWindow:
		return "error"
	}
	return "idle"
}

// pickIdleExcluded returns an excluded but healthy server with no running request. Caller holds p.mu.
// Only the server that answered "busy" (503/429) qualifies – never one that failed with a real error.
func (p *Pool) pickIdleExcluded(r *ReqInfo, rt Route) *Backend {
	now := time.Now()
	var best *Backend
	for _, b := range p.backends {
		if b == r.softRetry && rt.allows(b) && b.usable(now) && b.active == 0 && (best == nil || better(b, best)) {
			best = b
		}
	}
	return best
}

// anyUsable: is at least one server enabled and reachable? Caller holds p.mu.
func (p *Pool) anyUsable(rt Route) bool {
	now := time.Now()
	for _, b := range p.backends {
		if rt.allows(b) && b.usable(now) {
			return true
		}
	}
	return false
}

type GroupStatus struct {
	ForceModel       string   `json:"force_model,omitempty"`
	Fallback         string   `json:"fallback,omitempty"`
	FallbackAfterSec int      `json:"fallback_after_sec"`
	FallbackModel    string   `json:"fallback_model,omitempty"`
	Name             string   `json:"name"`
	Models           []string `json:"models"` // configured names/patterns
	Auto             bool     `json:"auto_models"`
	Members          []string `json:"members"`
	Available        []string `json:"available"` // concrete models this group routes
	Queued           int      `json:"queued"`
}

func matchesAny(patterns []string, model string) bool {
	for _, p := range patterns {
		if modelMatch(p, model) {
			return true
		}
	}
	return false
}

// RecentAfter returns finished requests with Seq > after, newest first.
func (p *Pool) RecentAfter(after uint64) []*ReqInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	out := []*ReqInfo{}
	for i := len(p.recent) - 1; i >= 0 && p.recent[i].Seq > after; i-- {
		out = append(out, snapshot(p.recent[i], now))
	}
	return out
}

// setResult stores usage, token rates and tag issues of a finished response.
func (p *Pool) setResult(r *ReqInfo, b *Backend, u Usage, issues []string, acc *Access) {
	cost := 0.0
	if acc != nil && u.Found {
		cost = acc.AddUsage(r.Token, u)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	r.TagIssues = issues
	if !u.Found {
		return
	}
	r.TokensIn, r.TokensOut, r.Cost = u.In, u.Out, cost
	if b != nil {
		r.BackendCost = (float64(u.In)*b.priceIn + float64(u.Out)*b.priceOut) / 1e6
	}
	now := time.Now()
	if !r.FirstByte.IsZero() {
		if gen := now.Sub(r.FirstByte).Seconds(); u.Out > 1 && gen > 0.05 && r.Stream {
			r.GenTPS = float64(u.Out) / gen
		}
		if pp := r.FirstByte.Sub(r.StartedAt).Seconds(); u.In > 0 && pp > 0.01 && r.Stream {
			r.PromptTPS = float64(u.In) / pp
		}
	}
}

// tick re-runs the dispatcher every second so waiting requests reach their fallback group in time.
func (p *Pool) tick() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for range t.C {
		p.mu.Lock()
		if len(p.queue) > 0 {
			p.dispatchLocked()
		}
		p.mu.Unlock()
	}
}

func (p *Pool) setCancel(r *ReqInfo, cancel context.CancelFunc) {
	p.mu.Lock()
	r.cancel = cancel
	p.mu.Unlock()
}

// Stop cancels requests: every running request of a server (backend != ""), or one request by id
// (running or queued). Cancelling closes the connection to the LLM server, which makes llama.cpp,
// Ollama and vLLM stop generating. Returns the number of stopped requests.
func (p *Pool) Stop(backend string, id uint64) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, r := range p.active {
		hit := id != 0 && r.ID == id || id == 0 && backend != "" && r.Backend == backend && r.State == "running"
		if !hit || r.cancel == nil || r.stopped.Load() {
			continue
		}
		r.stopped.Store(true)
		r.cancel()
		n++
	}
	if n > 0 {
		log.Printf("stopped %d request(s) via UI (server %q, id %d)", n, backend, id)
	}
	return n
}

// anyUsableEx: is there a reachable, enabled server for this route that hasn't failed yet? Caller holds p.mu.
func (p *Pool) anyUsableEx(rt Route, exclude map[*Backend]bool) bool {
	now := time.Now()
	for _, b := range p.backends {
		if rt.allows(b) && !exclude[b] && b.usable(now) {
			return true
		}
	}
	return false
}

func (p *Pool) addTrail(r *ReqInfo, step string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.Trail != "" {
		r.Trail += " → "
	}
	r.Trail += step
}

func (p *Pool) setSoftRetry(r *ReqInfo, b *Backend, soft bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if soft {
		r.softRetry = b
	} else if r.softRetry == b {
		r.softRetry = nil
	}
}

func (p *Pool) failedAttempt(backend string) {
	if p.stats != nil {
		p.stats.RecordFailedAttempt(backend, time.Now())
	}
}
