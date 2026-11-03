package main

import (
	"context"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// A request aborted by its client (or STOP) after it reached the server keeps the slot for a moment:
// the server is still busy with it, and a single-slot server must not get the next request in parallel.
func TestAbortGraceKeepsSlot(t *testing.T) {
	a := newFakeLLM(t, "a", 2*time.Second) // fails the test on two concurrent requests
	srv, pool := setup(t, func(c *Config) { c.AbortGraceSec = 1 }, a)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10)
	resp.Body.Read(buf) // answer is streaming
	cancel()
	resp.Body.Close()

	waitFor(t, func() bool {
		st := pool.Status()
		return len(st.Backends[0].Running) == 0 && st.Backends[0].Draining == 1
	})
	if st := pool.Status().Backends[0]; st.Active != 1 || st.State != "busy" {
		t.Errorf("slot during grace: %+v", st)
	}
	start := time.Now()
	if be, _ := post(t, srv.URL); be != "a" {
		t.Errorf("next request: %q", be)
	}
	if d := time.Since(start); d < 700*time.Millisecond {
		t.Errorf("next request did not wait for the grace period (%v)", d)
	}
	if st := pool.Status().Backends[0]; st.Active != 0 || st.Draining != 0 {
		t.Errorf("after: %+v", st)
	}
}

// Many clients giving up at random times (and the STOP button) must never push two requests onto a single-slot server.
func TestAbortStressSingleSlot(t *testing.T) {
	a := newFakeLLM(t, "a", 300*time.Millisecond)
	b := newFakeLLM(t, "b", 300*time.Millisecond)
	srv, pool := setup(t, func(c *Config) { c.AbortGraceSec = 1 }, a, b)
	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(100+rand.Intn(900))*time.Millisecond)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true}`))
			if resp, err := http.DefaultClient.Do(req); err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
		time.Sleep(time.Duration(rand.Intn(40)) * time.Millisecond)
		if i%4 == 0 {
			pool.Stop("a", 0) // STOP button
		}
	}
	wg.Wait()
	waitFor(t, func() bool {
		st := pool.Status()
		return st.Backends[0].Active == 0 && st.Backends[1].Active == 0
	})
}
