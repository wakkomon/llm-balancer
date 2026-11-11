package main

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPriorityFillsSlotsInOrder(t *testing.T) {
	a := newFakeLLM(t, "a", 600*time.Millisecond)
	b := newFakeLLM(t, "b", 600*time.Millisecond)
	c := newFakeLLM(t, "c", 600*time.Millisecond)
	b.parallel.Store(3)
	srv, pool := setup(t, func(cfg *Config) {
		cfg.Backends[0].Priority = 0
		cfg.Backends[1].Priority, cfg.Backends[1].MaxConcurrent = 5, 3
		cfg.Backends[2].Priority = 9
	}, a, b, c)
	got := make([]string, 5)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m"}`))
			if err == nil {
				got[i] = resp.Header.Get("X-Llm-Balancer-Backend")
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}(i)
		time.Sleep(60 * time.Millisecond) // one after the other, all overlapping
	}
	wg.Wait()
	if strings.Join(got, ",") != "a,b,b,b,c" {
		t.Errorf("order: %v", got)
	}
	_ = pool
}

// A router target "@group" sends to any server of the group – by priority, each with its own model.
func TestRouterGroupTarget(t *testing.T) {
	a := newFakeLLM(t, "a", 600*time.Millisecond)
	b := newFakeLLM(t, "b", 600*time.Millisecond)
	c := newFakeLLM(t, "c", 600*time.Millisecond)
	x := newFakeLLM(t, "x", 0) // not in the group
	b.parallel.Store(3)
	srv, pool := setup(t, func(cfg *Config) {
		cfg.Groups = []GroupConfig{{Name: "gpu", Models: []string{"*"}}, {Name: "other", Models: []string{"*"}}}
		cfg.Backends[0].Priority, cfg.Backends[0].Group = 0, "gpu"
		cfg.Backends[1].Priority, cfg.Backends[1].MaxConcurrent, cfg.Backends[1].Group, cfg.Backends[1].Models = 5, 3, "gpu", []string{"qwen-b"}
		cfg.Backends[2].Priority, cfg.Backends[2].Group = 9, "gpu"
		cfg.Backends[3].Priority, cfg.Backends[3].Group = -5, "other"
		cfg.Routers = []RouterConfig{{Name: "local", Default: "@gpu"}}
	}, a, b, c, x)
	got := make([]string, 5)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"local"}`))
			if err == nil {
				got[i] = resp.Header.Get("X-Llm-Balancer-Backend")
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}(i)
		time.Sleep(60 * time.Millisecond)
	}
	wg.Wait()
	if strings.Join(got, ",") != "a,b,b,b,c" {
		t.Errorf("order: %v", got)
	}
	for _, r := range pool.RecentAfter(0) {
		if r.Backend == "b" && r.Effective != "qwen-b" {
			t.Errorf("server b got model %q, want its own qwen-b", r.Effective)
		}
		if r.Backend == "a" && r.Effective != "" { // no model of its own: the name stays
			t.Errorf("server a got model %q", r.Effective)
		}
	}
	// renaming the group keeps the router pointing at it
	if err := pool.UpdateGroup("gpu", GroupConfig{Name: "gross", Models: []string{"*"}}, nil); err != nil {
		t.Fatal(err)
	}
	pool.mu.Lock()
	def := pool.routers[0].Default
	pool.mu.Unlock()
	if def != "@gross" {
		t.Errorf("router target after rename: %q", def)
	}
	if be, _ := post(t, srv.URL); be == "" {
		t.Error("request after rename failed")
	}
}
