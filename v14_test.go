package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A group with servers that host different models: requests only go to the members that have the model.
func TestGroupMemberByModel(t *testing.T) {
	a := newModelLLM(t, "a", 400*time.Millisecond, "qwen3-32b")
	b := newModelLLM(t, "b", 10*time.Millisecond, "llama3-70b")
	srv, pool := setup(t, func(c *Config) {
		c.Groups = []GroupConfig{{Name: "gross"}}
		c.Backends = []BackendConfig{
			{Name: "b", URL: b.srv.URL, Group: "gross", Priority: -5}, // preferred, but does not have qwen
			{Name: "a", URL: a.srv.URL, Group: "gross"},
		}
	})
	discover(t, pool)
	for i := 0; i < 2; i++ {
		if _, be := chatModel(t, srv.URL, "qwen3-32b", nil); be != "a" {
			t.Errorf("qwen went to %q", be)
		}
	}
	if _, be := chatModel(t, srv.URL, "llama3-70b", nil); be != "b" {
		t.Errorf("llama went to %q", be)
	}
	// a busy: the next qwen request waits for a instead of going to b
	go chatModel(t, srv.URL, "qwen3-32b", nil)
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	if _, be := chatModel(t, srv.URL, "qwen3-32b", nil); be != "a" || time.Since(start) < 250*time.Millisecond {
		t.Errorf("busy case: %q after %v", be, time.Since(start))
	}
	if b.served.Load() != 1 {
		t.Errorf("b got %d requests, want 1 (no failed qwen attempts)", b.served.Load())
	}
	if st := status(t, srv.URL); st.TotalReqs != 5 {
		t.Errorf("total %d", st.TotalReqs)
	}
	// route preview shows only a
	pool.mu.Lock()
	rt := pool.routeLocked("qwen3-32b")
	pool.mu.Unlock()
	if !rt.allows(pool.backends[1]) || rt.allows(pool.backends[0]) {
		t.Errorf("route: %+v", rt.allow)
	}
}

func TestManualServerModelFilter(t *testing.T) {
	// "x" reports nothing (e.g. no /v1/models), "y" reports qwen but the admin switched that off
	x := newModelLLM(t, "x", 10*time.Millisecond)
	y := newModelLLM(t, "y", 10*time.Millisecond, "qwen3-32b", "llama3-70b")
	off := false
	srv, pool := setup(t, func(c *Config) {
		c.Groups = []GroupConfig{{Name: "g", Models: []string{"qwen*", "llama*"}}}
		c.Backends = []BackendConfig{
			{Name: "y", URL: y.srv.URL, Group: "g", Priority: -1, Models: []string{"llama*"}, AutoModels: &off},
			{Name: "x", URL: x.srv.URL, Group: "g", Models: []string{"qwen*"}},
		}
	})
	discover(t, pool)
	if _, be := chatModel(t, srv.URL, "qwen3-32b", nil); be != "x" {
		t.Errorf("qwen → %q, want x (manual filter; y's reported qwen is switched off)", be)
	}
	if _, be := chatModel(t, srv.URL, "llama3-8b", nil); be != "y" {
		t.Errorf("llama → %q", be)
	}
	// /v1/models: manual concrete names and reported ones only where auto is on
	all := strings.Join(pool.AllModels(), ",")
	if strings.Contains(all, "qwen3-32b") {
		t.Errorf("y's reported models should be hidden: %s", all)
	}
	// API: change filter, omitted fields are kept
	if c := api(t, "PUT", srv.URL+"/balancer/api/backends/x", `{"name":"x","url":"`+x.srv.URL+`","models":["mistral*"]}`, ""); c != 204 {
		t.Fatalf("put: %d", c)
	}
	pool.mu.Lock()
	xb := pool.backends[1]
	if strings.Join(xb.manual, ",") != "mistral*" || !xb.autoModels || xb.group != "g" {
		t.Errorf("after put: %v %v %q", xb.manual, xb.autoModels, xb.group)
	}
	pool.mu.Unlock()
	api(t, "PUT", srv.URL+"/balancer/api/backends/x", `{"name":"x","url":"`+x.srv.URL+`","max_concurrent":1}`, "")
	pool.mu.Lock()
	if strings.Join(xb.manual, ",") != "mistral*" {
		t.Errorf("models not kept: %v", xb.manual)
	}
	pool.mu.Unlock()
	st := status(t, srv.URL)
	for _, b := range st.Backends {
		if b.Name == "y" && (b.AutoModels || len(b.Manual) != 1) {
			t.Errorf("status y: %+v", b)
		}
	}
	// now nobody in g serves qwen explicitly → whole group (old behaviour), pattern still matches the group
	if code, be := chatModel(t, srv.URL, "qwen3-32b", nil); code != 200 || be == "" {
		t.Errorf("fallback to whole group: %d %q", code, be)
	}
}

func TestFallbackGroupPicksServingMember(t *testing.T) {
	big := newModelLLM(t, "big", 10*time.Millisecond, "qwen3-32b")
	s1 := newModelLLM(t, "s1", 10*time.Millisecond, "phi4")
	s2 := newModelLLM(t, "s2", 10*time.Millisecond, "llama3.2:3b")
	srv, pool := setup(t, func(c *Config) {
		c.Groups = []GroupConfig{{Name: "gross", Models: []string{"qwen3-32b"}, Fallback: "klein", FallbackModel: "llama3.2:3b"}, {Name: "klein"}}
		c.Backends = []BackendConfig{{Name: "big", URL: big.srv.URL, Group: "gross"},
			{Name: "s1", URL: s1.srv.URL, Group: "klein", Priority: -1}, {Name: "s2", URL: s2.srv.URL, Group: "klein"}}
	})
	discover(t, pool)
	api(t, "POST", srv.URL+"/balancer/api/groups/gross/disable", "", "")
	for i := 0; i < 2; i++ {
		if _, be := chatModel(t, srv.URL, "qwen3-32b", nil); be != "s2" {
			t.Errorf("fallback went to %q, want s2 (has llama3.2:3b)", be)
		}
	}
}

// An exactly known model beats a wildcard rule – for servers inside a group and between groups.
func TestExactModelBeatsWildcard(t *testing.T) {
	x := newModelLLM(t, "x", 10*time.Millisecond)              // only filter "qwen*"
	y := newModelLLM(t, "y", 10*time.Millisecond, "qwen3-32b") // reports the model
	z := newModelLLM(t, "z", 10*time.Millisecond)              // other group, pattern only
	srv, pool := setup(t, func(c *Config) {
		c.Groups = []GroupConfig{{Name: "muster", Models: []string{"qwen*"}}, {Name: "g"}}
		c.Backends = []BackendConfig{
			{Name: "z", URL: z.srv.URL, Group: "muster", Priority: -9},
			{Name: "x", URL: x.srv.URL, Group: "g", Priority: -5, Models: []string{"qwen*"}},
			{Name: "y", URL: y.srv.URL, Group: "g"},
		}
	})
	discover(t, pool)
	for i := 0; i < 2; i++ {
		if _, be := chatModel(t, srv.URL, "qwen3-32b", nil); be != "y" {
			t.Errorf("exact model → %q, want y", be)
		}
	}
	// a model nobody knows by name: wildcard matches of both groups apply, inside g only x (filter)
	pool.mu.Lock()
	rt := pool.routeLocked("qwen2.5-7b")
	pool.mu.Unlock()
	if rt.Group != "muster,g" || !rt.allows(pool.backends[0]) || !rt.allows(pool.backends[1]) || rt.allows(pool.backends[2]) {
		t.Errorf("wildcard route: %s %v", rt.Group, rt.allow)
	}
	// a name listed in the filter counts as exact
	api(t, "PUT", srv.URL+"/balancer/api/backends/x", `{"name":"x","url":"`+x.srv.URL+`","models":["qwen*","qwen2.5-7b"]}`, "")
	pool.mu.Lock()
	rt = pool.routeLocked("qwen2.5-7b")
	pool.mu.Unlock()
	if rt.Group != "g" || !rt.allows(pool.backends[1]) || rt.allows(pool.backends[0]) {
		t.Errorf("exact filter route: %s %v", rt.Group, rt.allow)
	}
}

func TestServerForceModel(t *testing.T) {
	x := newModelLLM(t, "x", 10*time.Millisecond, "qwen3:8b", "phi4")
	srv, pool := setup(t, func(c *Config) {
		c.Backends = []BackendConfig{{Name: "x", URL: x.srv.URL, Models: []string{"qwen*", "gpt-4o"}, ForceModel: "qwen3:8b"}}
	})
	discover(t, pool)
	for model, want := range map[string]string{"qwen2.5-72b": "qwen3:8b", "phi4": "", "gpt-4o": "", "qwen3:8b": ""} {
		chatModel(t, srv.URL, model, nil)
		waitFor(t, func() bool { st := status(t, srv.URL); return len(st.Recent) > 0 && st.Recent[0].Model == model })
		if r := status(t, srv.URL).Recent[0]; r.Effective != want {
			t.Errorf("%s: sent as %q, want %q", model, r.Effective, want)
		}
	}
	// kept when omitted, cleared with ""
	api(t, "PUT", srv.URL+"/balancer/api/backends/x", `{"name":"x","url":"`+x.srv.URL+`"}`, "")
	pool.mu.Lock()
	if pool.backends[0].forceModel != "qwen3:8b" {
		t.Error("force model not kept")
	}
	pool.mu.Unlock()
	api(t, "PUT", srv.URL+"/balancer/api/backends/x", `{"name":"x","url":"`+x.srv.URL+`","force_model":""}`, "")
	pool.mu.Lock()
	if pool.backends[0].forceModel != "" {
		t.Error("force model not cleared")
	}
	pool.mu.Unlock()
}

func TestTokenEditAndModelAllowlist(t *testing.T) {
	a := newModelLLM(t, "a", 10*time.Millisecond, "qwen3:8b", "llama3:70b")
	srv, pool := setup(t, func(c *Config) {
		c.APIKeys = "env=sk-env-value-0001"
		c.Backends = []BackendConfig{{Name: "a", URL: a.srv.URL}}
	})
	discover(t, pool)
	w := newWeb(t, srv.URL)
	_, b := w.do("POST", "/api/tokens", `{"name":"hermes","token":"sk-hermes-00001"}`)
	if !strings.Contains(b, "sk-hermes") {
		t.Fatal(b)
	}
	if c, b := w.do("PUT", "/api/tokens/hermes", `{"name":"agent","models":["qwen*"," "]}`); c != 204 {
		t.Fatalf("edit: %d %s", c, b)
	}
	ask := func(key, model string) int {
		code, _ := chatModel(t, srv.URL, model, map[string]string{"Authorization": "Bearer " + key})
		return code
	}
	if c := ask("sk-hermes-00001", "qwen3:8b"); c != 200 {
		t.Errorf("allowed model: %d", c)
	}
	if c := ask("sk-hermes-00001", "llama3:70b"); c != 403 {
		t.Errorf("forbidden model: %d", c)
	}
	if c := ask("sk-env-value-0001", "llama3:70b"); c != 200 {
		t.Errorf("token without list: %d", c)
	}
	// /v1/models only lists allowed ones
	req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-hermes-00001")
	resp, _ := http.DefaultClient.Do(req)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "llama") || !strings.Contains(string(body), "qwen3:8b") {
		t.Errorf("models list: %s", body)
	}
	// new value: the old one stops working
	c, b := w.do("PUT", "/api/tokens/agent", `{"token":""}`)
	var nv struct{ Token string }
	json.Unmarshal([]byte(b), &nv)
	if c != 200 || !strings.HasPrefix(nv.Token, "llmb-") {
		t.Fatalf("new value: %d %s", c, b)
	}
	if c := ask("sk-hermes-00001", "qwen3:8b"); c != 401 {
		t.Errorf("old value still works: %d", c)
	}
	if c := ask(nv.Token, "qwen3:8b"); c != 200 {
		t.Errorf("new value: %d", c)
	}
	if c, _ := w.do("PUT", "/api/tokens/env", `{"name":"x"}`); c != 400 {
		t.Errorf("rename env token: %d", c)
	}
	if c, _ := w.do("PUT", "/api/tokens/env", `{"models":["qwen*"]}`); c != 204 {
		t.Errorf("models on env token: %d", c)
	}
}
