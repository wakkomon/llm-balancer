package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func chatBody(t *testing.T, url, model string) (string, string) {
	t.Helper()
	resp, err := http.Post(url+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"`+model+`","messages":[]}`))
	if err != nil {
		t.Error(err)
		return "", ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.Header.Get("X-LLM-Balancer-Backend"), string(b)
}

func TestFallbackGroupAfterWait(t *testing.T) {
	big := newModelLLM(t, "big", 2500*time.Millisecond, "qwen3-32b")
	small := newModelLLM(t, "small", 50*time.Millisecond, "llama3.2:3b")
	srv, _ := setup(t, func(c *Config) {
		c.Groups = []GroupConfig{
			{Name: "gross", Fallback: "klein", FallbackAfterSec: 1, FallbackModel: "llama3.2:3b"},
			{Name: "klein"},
		}
		c.Backends = []BackendConfig{
			{Name: "big", URL: big.srv.URL, Group: "gross"},
			{Name: "small", URL: small.srv.URL, Group: "klein"},
		}
	})
	discoverAll(t, srv.URL)
	go chatBody(t, srv.URL, "qwen3-32b") // occupies big for 2.5 s
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	be, body := chatBody(t, srv.URL, "qwen3-32b")
	waited := time.Since(start)
	if be != "small" || waited < 900*time.Millisecond || waited > 2200*time.Millisecond {
		t.Errorf("expected fallback to small after ~1s, got %s after %v", be, waited)
	}
	if !strings.Contains(body, `"model":"llama3.2:3b"`) {
		t.Errorf("model not replaced: %s", body)
	}
	st := status(t, srv.URL)
	r := st.Recent[0]
	if r.Group != "gross → klein" || r.Effective != "llama3.2:3b" || r.Model != "qwen3-32b" {
		t.Errorf("recent: group=%q effective=%q model=%q", r.Group, r.Effective, r.Model)
	}
}

func TestFallbackImmediatelyWhenGroupDown(t *testing.T) {
	big := newModelLLM(t, "big", 10*time.Millisecond, "qwen3-32b")
	small := newModelLLM(t, "small", 10*time.Millisecond, "llama3.2:3b")
	srv, pool := setup(t, func(c *Config) {
		c.Groups = []GroupConfig{{Name: "gross", Models: []string{"qwen3-32b"}, Fallback: "klein", FallbackAfterSec: 60}, {Name: "klein"}}
		c.Backends = []BackendConfig{{Name: "big", URL: big.srv.URL, Group: "gross"}, {Name: "small", URL: small.srv.URL, Group: "klein"}}
	})
	pool.mu.Lock()
	pool.backends[0].healthy = false
	pool.mu.Unlock()
	start := time.Now()
	be, body := chatBody(t, srv.URL, "qwen3-32b")
	if be != "small" || time.Since(start) > 300*time.Millisecond || !strings.Contains(body, `"model":"qwen3-32b"`) {
		t.Errorf("got %s after %v: %s", be, time.Since(start), body)
	}
}

func TestFallbackGroupValidation(t *testing.T) {
	a := newModelLLM(t, "a", 10*time.Millisecond)
	srv, _ := setup(t, func(c *Config) { c.Backends = []BackendConfig{{Name: "a", URL: a.srv.URL}} })
	if c := api(t, "POST", srv.URL+"/balancer/api/groups", `{"name":"x","fallback":"nope"}`, ""); c != 400 {
		t.Errorf("unknown fallback: %d", c)
	}
	api(t, "POST", srv.URL+"/balancer/api/groups", `{"name":"b"}`, "")
	if c := api(t, "POST", srv.URL+"/balancer/api/groups", `{"name":"x","fallback":"b","fallback_after_sec":30}`, ""); c != 204 {
		t.Fatalf("add: %d", c)
	}
	if c := api(t, "PUT", srv.URL+"/balancer/api/groups/x", `{"name":"x","fallback":"x"}`, ""); c != 400 {
		t.Errorf("self fallback: %d", c)
	}
	api(t, "PUT", srv.URL+"/balancer/api/groups/b", `{"name":"b2"}`, "")
	st := status(t, srv.URL)
	if st.Groups[0].Fallback != "b2" && st.Groups[1].Fallback != "b2" {
		t.Errorf("rename not followed: %+v", st.Groups)
	}
	api(t, "DELETE", srv.URL+"/balancer/api/groups/b2", "", "")
	if st := status(t, srv.URL); st.Groups[0].Fallback != "" {
		t.Errorf("delete not followed: %+v", st.Groups)
	}
}

// discoverAll triggers model discovery through the pool behind a test server.
func discoverAll(t *testing.T, url string) {
	t.Helper()
	for _, p := range testPools {
		if p.url == url {
			discover(t, p.pool)
		}
	}
}

func TestForceModelPerGroup(t *testing.T) {
	big := newModelLLM(t, "big", 600*time.Millisecond, "qwen3-32b-q4")
	small := newModelLLM(t, "small", 10*time.Millisecond, "llama")
	srv, _ := setup(t, func(c *Config) {
		c.Groups = []GroupConfig{
			{Name: "gross", Models: []string{"qwen*"}, ForceModel: "qwen3-32b-q4", Fallback: "klein", FallbackAfterSec: 0},
			{Name: "klein", Models: []string{"llama*"}, ForceModel: "llama"},
		}
		c.Backends = []BackendConfig{{Name: "big", URL: big.srv.URL, Group: "gross"}, {Name: "small", URL: small.srv.URL, Group: "klein"}}
	})
	// own group: forced model
	go func() {
		if be, body := chatBody(t, srv.URL, "qwen3"); be != "big" || !strings.Contains(body, `"model":"qwen3-32b-q4"`) {
			t.Errorf("own group: %s %s", be, body)
		}
	}()
	time.Sleep(100 * time.Millisecond)
	// fallback without replacement model: the fallback group's forced model applies
	if be, body := chatBody(t, srv.URL, "qwen3"); be != "small" || !strings.Contains(body, `"model":"llama"`) {
		t.Errorf("fallback: %s %s", be, body)
	}
	time.Sleep(700 * time.Millisecond)
	st := status(t, srv.URL)
	for _, r := range st.Recent {
		if r.Model != "qwen3" || r.Effective == "" {
			t.Errorf("recent: model=%q effective=%q", r.Model, r.Effective)
		}
	}
}

func TestFallbackWhenGroupDisabledWhileQueued(t *testing.T) {
	a := newModelLLM(t, "a", 1500*time.Millisecond, "m")
	b := newModelLLM(t, "b", 1500*time.Millisecond, "m")
	f := newModelLLM(t, "f", 10*time.Millisecond, "x")
	srv, _ := setup(t, func(c *Config) {
		c.Groups = []GroupConfig{{Name: "g", Models: []string{"m"}, Fallback: "fb", FallbackAfterSec: 60}, {Name: "fb"}}
		c.Backends = []BackendConfig{{Name: "a", URL: a.srv.URL, Group: "g"}, {Name: "b", URL: b.srv.URL, Group: "g"}, {Name: "f", URL: f.srv.URL, Group: "fb"}}
	})
	go chatModel(t, srv.URL, "m", nil)
	go chatModel(t, srv.URL, "m", nil)
	time.Sleep(100 * time.Millisecond)
	done := make(chan string, 1)
	go func() { _, be := chatModel(t, srv.URL, "m", nil); done <- be }() // waits: a and b busy
	time.Sleep(100 * time.Millisecond)
	api(t, "POST", srv.URL+"/balancer/api/backends/a/disable", "", "")
	start := time.Now()
	api(t, "POST", srv.URL+"/balancer/api/backends/b/disable", "", "")
	select {
	case be := <-done:
		if be != "f" || time.Since(start) > 500*time.Millisecond {
			t.Errorf("went to %q after %v", be, time.Since(start))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued request did not move to the fallback group")
	}
}

func TestFallbackWhenGroupGoesDownWhileQueued(t *testing.T) {
	a := newModelLLM(t, "a", 1500*time.Millisecond, "m")
	f := newModelLLM(t, "f", 10*time.Millisecond, "x")
	srv, pool := setup(t, func(c *Config) {
		c.Groups = []GroupConfig{{Name: "g", Models: []string{"m"}, Fallback: "fb", FallbackAfterSec: 60}, {Name: "fb"}}
		c.Backends = []BackendConfig{{Name: "a", URL: a.srv.URL, Group: "g"}, {Name: "f", URL: f.srv.URL, Group: "fb"}}
	})
	go chatModel(t, srv.URL, "m", nil)
	time.Sleep(100 * time.Millisecond)
	done := make(chan string, 1)
	go func() { _, be := chatModel(t, srv.URL, "m", nil); done <- be }()
	time.Sleep(100 * time.Millisecond)
	pool.MarkUnhealthy(pool.backends[0], "health check failed")
	select {
	case be := <-done:
		if be != "f" {
			t.Errorf("went to %q", be)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued request did not move to the fallback group")
	}
}
