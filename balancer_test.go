package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeLLM simulates a single-slot LLM server that streams SSE chunks slowly
// and fails the test if it ever sees two requests at once.
type fakeLLM struct {
	t        *testing.T
	name     string
	delay    time.Duration
	inflight atomic.Int32
	served   atomic.Int32
	srv      *httptest.Server
}

func newFakeLLM(t *testing.T, name string, delay time.Duration) *fakeLLM {
	f := &fakeLLM{t: t, name: name, delay: delay}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"data":[]}`))
			return
		}
		if n := f.inflight.Add(1); n > 1 {
			t.Errorf("%s: %d concurrent requests", name, n)
		}
		defer f.inflight.Add(-1)
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 5; i++ {
			fmt.Fprintf(w, "data: {\"backend\":%q,\"i\":%d,\"echo\":%d}\n\n", name, i, len(body))
			w.(http.Flusher).Flush()
			select {
			case <-time.After(f.delay / 5):
			case <-r.Context().Done():
				return
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		f.served.Add(1)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func setup(t *testing.T, mod func(*Config), backends ...*fakeLLM) (*httptest.Server, *Pool) {
	cfg := defaultConfig()
	cfg.HealthPath = ""
	for _, b := range backends {
		cfg.Backends = append(cfg.Backends, BackendConfig{Name: b.name, URL: b.srv.URL})
	}
	if mod != nil {
		mod(&cfg)
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	var store *Store
	if cfg.DataDir != "data" {
		store = NewStore(cfg.DataDir)
	}
	pool, err := NewPool(cfg, cfg.Backends, store)
	if err != nil {
		t.Fatal(err)
	}
	statsDir := ""
	if store != nil {
		statsDir = cfg.DataDir
	}
	pool.stats = NewStats(statsDir, 0)
	px := NewProxy(cfg, pool)
	acc, err := NewAccess(statsDir, cfg.APIKeys)
	if err != nil {
		t.Fatal(err)
	}
	px.access, pool.access = acc, acc
	acc.onChange = pool.kickCaps
	acc.onRename = func(o, n string) { pool.stats.Merge("token", o, n) }
	pool.notifier = NewNotifier(statsDir)
	stop := make(chan struct{})
	go pool.capsLoop(stop)
	srv := httptest.NewServer(newMux(cfg, pool, px))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(stop) })
	testPools = append(testPools, testPool{srv.URL, pool})
	return srv, pool
}

func post(t *testing.T, url string) (string, string) {
	resp, err := http.Post(url+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"qwen","stream":true,"messages":[]}`))
	if err != nil {
		t.Error(err)
		return "", ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.Header.Get("X-LLM-Balancer-Backend"), string(b)
}

func TestSpreadsAndQueues(t *testing.T) {
	a := newFakeLLM(t, "a", 300*time.Millisecond)
	b := newFakeLLM(t, "b", 300*time.Millisecond)
	srv, pool := setup(t, nil, a, b)

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			be, body := post(t, srv.URL)
			if !strings.Contains(body, "[DONE]") || !strings.Contains(body, `"backend":"`+be+`"`) {
				t.Errorf("bad response from %s: %q", be, body)
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	if q := len(pool.Status().Queue); q != 4 {
		t.Errorf("expected 4 queued, got %d", q)
	}
	wg.Wait()
	el := time.Since(start)
	// 6 requests, 2 single-slot servers, 300ms each → ~900ms; strictly serial would be 1.8s
	if el > 1500*time.Millisecond || el < 850*time.Millisecond {
		t.Errorf("unexpected total time %v", el)
	}
	if a.served.Load() != 3 || b.served.Load() != 3 {
		t.Errorf("uneven distribution a=%d b=%d", a.served.Load(), b.served.Load())
	}
}

func TestStreamingIsNotBuffered(t *testing.T) {
	a := newFakeLLM(t, "a", 1000*time.Millisecond)
	srv, _ := setup(t, nil, a)
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	start := time.Now()
	line, _ := bufio.NewReader(resp.Body).ReadString('\n')
	if !strings.HasPrefix(line, "data:") || time.Since(start) > 150*time.Millisecond {
		t.Errorf("first chunk late or wrong: %q after %v", line, time.Since(start))
	}
}

func TestDeadBackendFailover(t *testing.T) {
	a := newFakeLLM(t, "a", 50*time.Millisecond)
	dead := newFakeLLM(t, "dead", 0)
	dead.srv.Close()
	srv, pool := setup(t, func(c *Config) { c.Backends[1].Priority = -1 }, a, dead) // dead is preferred
	for i := 0; i < 3; i++ {
		be, body := post(t, srv.URL)
		if be != "a" || !strings.Contains(body, "[DONE]") {
			t.Fatalf("expected failover to a, got %q", be)
		}
	}
	st := pool.Status()
	for _, b := range st.Backends {
		if b.Name == "dead" && b.Healthy {
			t.Error("dead backend still marked healthy")
		}
	}
}

func TestRetryOn503(t *testing.T) {
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no slot available", http.StatusServiceUnavailable)
	}))
	defer busy.Close()
	a := newFakeLLM(t, "a", 50*time.Millisecond)
	cfg := func(c *Config) {
		c.Backends = append([]BackendConfig{{Name: "busy", URL: busy.URL, Priority: -1}}, c.Backends...)
	}
	srv, _ := setup(t, cfg, a)
	if be, _ := post(t, srv.URL); be != "a" {
		t.Fatalf("expected retry on a, got %q", be)
	}
}

func TestClientCancelWhileQueued(t *testing.T) {
	a := newFakeLLM(t, "a", 400*time.Millisecond)
	srv, pool := setup(t, nil, a)
	go post(t, srv.URL)
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{}`))
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("expected timeout")
	}
	time.Sleep(50 * time.Millisecond)
	if q := len(pool.Status().Queue); q != 0 {
		t.Errorf("cancelled request still queued (%d)", q)
	}
	// slot must still work afterwards
	if be, _ := post(t, srv.URL); be != "a" {
		t.Error("backend unusable after cancel")
	}
	if n := pool.Status().Backends[0].Active; n != 0 {
		t.Errorf("leaked slot: active=%d", n)
	}
}

func TestQueueLimitAndTimeout(t *testing.T) {
	a := newFakeLLM(t, "a", 600*time.Millisecond)
	srv, _ := setup(t, func(c *Config) { c.MaxQueue = 1; c.QueueTimeoutSec = 0 }, a)
	go post(t, srv.URL)
	time.Sleep(30 * time.Millisecond)
	go post(t, srv.URL) // queued
	time.Sleep(30 * time.Millisecond)
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Errorf("expected 503 when queue full, got %d", resp.StatusCode)
	}
}

func TestPassthroughGetDoesNotQueue(t *testing.T) {
	a := newFakeLLM(t, "a", 800*time.Millisecond)
	srv, _ := setup(t, nil, a)
	go post(t, srv.URL)
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if time.Since(start) > 200*time.Millisecond {
		t.Error("GET /v1/models waited for the busy slot")
	}
}

func TestDisableDrains(t *testing.T) {
	a := newFakeLLM(t, "a", 50*time.Millisecond)
	b := newFakeLLM(t, "b", 50*time.Millisecond)
	srv, _ := setup(t, nil, a, b)
	r, _ := http.Post(srv.URL+"/balancer/api/backends/a/disable", "", nil)
	if r.StatusCode != 204 {
		t.Fatalf("disable: %d", r.StatusCode)
	}
	for i := 0; i < 3; i++ {
		if be, _ := post(t, srv.URL); be != "b" {
			t.Errorf("disabled backend used: %s", be)
		}
	}
	resp, _ := http.Get(srv.URL + "/balancer/api/status")
	var st Status
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st.Backends[0].Enabled || len(st.Recent) != 3 {
		t.Errorf("status wrong: %+v", st.Backends[0])
	}
	ui, _ := http.Get(srv.URL + "/balancer/")
	if ui.StatusCode != 200 {
		t.Errorf("ui: %d", ui.StatusCode)
	}
}

func TestParseBackendsEnv(t *testing.T) {
	bs, err := parseBackendsEnv("gpu=http://10.0.0.5:8080*2@-1, http://amd.local:11434")
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 2 || bs[0].Name != "gpu" || bs[0].MaxConcurrent != 2 || bs[0].Priority != -1 ||
		bs[0].URL != "http://10.0.0.5:8080" || bs[1].URL != "http://amd.local:11434" {
		t.Errorf("parsed %+v", bs)
	}
}

// A request retried after a 503 must not keep waiting for other busy servers
// once the server that answered 503 is idle again.
func TestRetryUsesExcludedServerWhenIdleAgain(t *testing.T) {
	var calls atomic.Int32
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer flaky.Close()
	slow := newFakeLLM(t, "slow", 1500*time.Millisecond)
	srv, _ := setup(t, func(c *Config) {
		c.Backends = append([]BackendConfig{{Name: "flaky", URL: flaky.URL, Priority: -1}}, c.Backends...)
	}, slow)
	go post(t, srv.URL) // occupies slow
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	be, _ := post(t, srv.URL) // flaky → 503 → slow busy → must go back to flaky
	if be != "flaky" || time.Since(start) > 500*time.Millisecond {
		t.Errorf("got %s after %v", be, time.Since(start))
	}
}

func TestAllDownFailsFast(t *testing.T) {
	dead := newFakeLLM(t, "dead", 0)
	dead.srv.Close()
	srv, pool := setup(t, nil, dead)
	pool.mu.Lock()
	pool.backends[0].healthy = false // as after a failed health check
	pool.mu.Unlock()
	start := time.Now()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 || time.Since(start) > 200*time.Millisecond {
		t.Errorf("got %d after %v", resp.StatusCode, time.Since(start))
	}
}

type testPool struct {
	url  string
	pool *Pool
}

var testPools []testPool
