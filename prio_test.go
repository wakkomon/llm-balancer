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
