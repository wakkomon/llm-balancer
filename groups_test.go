package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// modelLLM is a single-slot server that reports a fixed model list and records which model it got.
type modelLLM struct {
	name   string
	models []string
	delay  time.Duration
	busy   atomic.Int32
	served atomic.Int32
	srv    *httptest.Server
}

func newModelLLM(t *testing.T, name string, delay time.Duration, models ...string) *modelLLM {
	m := &modelLLM{name: name, models: models, delay: delay}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			var data []map[string]string
			for _, x := range m.models {
				data = append(data, map[string]string{"id": x})
			}
			json.NewEncoder(w).Encode(map[string]any{"data": data})
			return
		}
		if m.busy.Add(1) > 1 {
			t.Errorf("%s: concurrent requests", name)
		}
		defer m.busy.Add(-1)
		var body struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		time.Sleep(m.delay)
		m.served.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"backend":%q,"model":%q,"usage":{"prompt_tokens":1000,"completion_tokens":500}}`, name, body.Model)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func chatModel(t *testing.T, url, model string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url+"/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[]}`))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("X-LLM-Balancer-Backend")
}

func discover(t *testing.T, pool *Pool) {
	t.Helper()
	px := NewProxy(pool.cfg, pool)
	client := &http.Client{Timeout: 2 * time.Second}
	for _, ht := range pool.idleTargets() {
		px.discoverModels(client, ht, nil)
	}
}

func TestGroupRoutingByModel(t *testing.T) {
	big1 := newModelLLM(t, "big1", 200*time.Millisecond, "qwen3-32b")
	big2 := newModelLLM(t, "big2", 200*time.Millisecond, "qwen3-32b")
	small := newModelLLM(t, "small", 50*time.Millisecond, "llama3.2:3b")
	srv, pool := setup(t, func(c *Config) {
		c.Groups = []GroupConfig{{Name: "gross"}, {Name: "klein", Models: []string{"llama*"}}}
		c.Backends = []BackendConfig{
			{Name: "big1", URL: big1.srv.URL, Group: "gross"},
			{Name: "big2", URL: big2.srv.URL, Group: "gross"},
			{Name: "small", URL: small.srv.URL, Group: "klein"},
		}
	})
	discover(t, pool)

	// auto-discovered model → group "gross" only
	done := make(chan string, 4)
	for i := 0; i < 4; i++ {
		go func() { _, be := chatModel(t, srv.URL, "qwen3-32b", nil); done <- be }()
	}
	time.Sleep(50 * time.Millisecond)
	// while "gross" is full and has a queue, a request for the small group must not wait
	start := time.Now()
	code, be := chatModel(t, srv.URL, "llama3.2:3b", nil)
	if code != 200 || be != "small" || time.Since(start) > 150*time.Millisecond {
		t.Errorf("small group blocked or misrouted: %d %s after %v", code, be, time.Since(start))
	}
	// pattern match (llama*) for a model nobody reports
	if _, be := chatModel(t, srv.URL, "llama-unknown", nil); be != "small" {
		t.Errorf("pattern route: %s", be)
	}
	for i := 0; i < 4; i++ {
		if be := <-done; be != "big1" && be != "big2" {
			t.Errorf("qwen went to %s", be)
		}
	}
	if small.served.Load() != 2 || big1.served.Load()+big2.served.Load() != 4 {
		t.Errorf("served: big1=%d big2=%d small=%d", big1.served.Load(), big2.served.Load(), small.served.Load())
	}

	// unknown model → all servers (default)
	if code, _ := chatModel(t, srv.URL, "mistral", nil); code != 200 {
		t.Errorf("unknown model: %d", code)
	}
	// route preview
	resp, _ := http.Get(srv.URL + "/balancer/api/route?model=QWEN3-32B")
	var rt map[string]any
	json.NewDecoder(resp.Body).Decode(&rt)
	resp.Body.Close()
	if rt["group"] != "gross" || len(rt["backends"].([]any)) != 2 {
		t.Errorf("route: %v", rt)
	}
	// aggregated model list
	resp, _ = http.Get(srv.URL + "/v1/models")
	var list struct {
		Data []struct{ ID string } `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Data) != 2 {
		t.Errorf("model list: %+v", list.Data)
	}
	// status shows groups with members and models
	st := status(t, srv.URL)
	if len(st.Groups) != 2 || st.Groups[0].Name != "gross" || len(st.Groups[0].Members) != 2 ||
		strings.Join(st.Groups[0].Available, ",") != "qwen3-32b" {
		t.Errorf("groups: %+v", st.Groups)
	}
}

func TestUnknownModelReject(t *testing.T) {
	a := newModelLLM(t, "a", 10*time.Millisecond, "m1")
	srv, pool := setup(t, func(c *Config) {
		c.UnknownModel = "reject"
		c.Groups = []GroupConfig{{Name: "g"}}
		c.Backends = []BackendConfig{{Name: "a", URL: a.srv.URL, Group: "g"}}
	})
	discover(t, pool)
	if code, _ := chatModel(t, srv.URL, "m1", nil); code != 200 {
		t.Errorf("known: %d", code)
	}
	if code, _ := chatModel(t, srv.URL, "other", nil); code != 404 {
		t.Errorf("unknown should be 404: %d", code)
	}
}

func TestGroupAdminAPIAndPersistence(t *testing.T) {
	a := newModelLLM(t, "a", 10*time.Millisecond)
	b := newModelLLM(t, "b", 10*time.Millisecond)
	dir := t.TempDir()
	srv, _ := setup(t, func(c *Config) {
		c.DataDir = dir
		c.Backends = []BackendConfig{{Name: "a", URL: a.srv.URL}, {Name: "b", URL: b.srv.URL}}
	})
	if c := api(t, "POST", srv.URL+"/balancer/api/groups", `{"name":"coder","models":["qwen*coder*"],"members":["b"]}`, ""); c != 204 {
		t.Fatalf("add group: %d", c)
	}
	if _, be := chatModel(t, srv.URL, "qwen2.5-coder-32b", nil); be != "b" {
		t.Errorf("routed to %s", be)
	}
	if c := api(t, "POST", srv.URL+"/balancer/api/groups", `{"name":"coder"}`, ""); c != 409 {
		t.Errorf("duplicate: %d", c)
	}
	// move a into the group via the server dialog, rename group
	api(t, "PUT", srv.URL+"/balancer/api/backends/a", `{"name":"a","url":"`+a.srv.URL+`","group":"coder"}`, "")
	if c := api(t, "PUT", srv.URL+"/balancer/api/groups/coder", `{"name":"code","models":["qwen*coder*"]}`, ""); c != 204 {
		t.Fatalf("rename: %d", c)
	}
	if c := api(t, "PUT", srv.URL+"/balancer/api/backends/a", `{"name":"a","url":"`+a.srv.URL+`","group":"nope"}`, ""); c != 400 {
		t.Errorf("unknown group accepted: %d", c)
	}
	srv2, _ := setup(t, func(c *Config) { c.DataDir = dir }, nil...)
	st := status(t, srv2.URL)
	if len(st.Groups) != 1 || st.Groups[0].Name != "code" || len(st.Groups[0].Members) != 2 {
		t.Fatalf("not persisted: %+v", st.Groups)
	}
	if c := api(t, "DELETE", srv2.URL+"/balancer/api/groups/code", "", ""); c != 204 {
		t.Fatalf("delete: %d", c)
	}
	if st := status(t, srv2.URL); len(st.Groups) != 0 || st.Backends[0].Group != "" {
		t.Errorf("after delete: %+v", st.Backends[0])
	}
}

func TestPricesAndTokenCounting(t *testing.T) {
	a := newModelLLM(t, "a", 10*time.Millisecond, "m")
	srv, _ := setup(t, func(c *Config) {
		c.APIKeys = "litellm=sk-litellm-0123456789"
		c.Backends = []BackendConfig{{Name: "a", URL: a.srv.URL}}
	})
	auth := map[string]string{"Authorization": "Bearer sk-litellm-0123456789"}
	if c := api(t, "PUT", srv.URL+"/balancer/api/tokens/litellm", `{"price_in":0.5,"price_out":2}`, ""); c != 204 {
		t.Fatalf("set price: %d", c)
	}
	for i := 0; i < 2; i++ {
		chatModel(t, srv.URL, "m", auth)
	}
	_, list := call(t, "GET", srv.URL+"/balancer/api/tokens", nil, "")
	var toks []tokenView
	json.Unmarshal([]byte(list), &toks)
	// 2 × (1000 in × 0.5 + 500 out × 2) / 1e6 = 0.003
	if len(toks) != 1 || toks[0].TokensIn != 2000 || toks[0].TokensOut != 1000 || fmt.Sprintf("%.4f", toks[0].Cost) != "0.0030" {
		t.Errorf("tokens: %+v", toks)
	}
	st := status(t, srv.URL)
	if r := st.Recent[0]; r.TokensIn != 1000 || r.TokensOut != 500 || fmt.Sprintf("%.4f", r.Cost) != "0.0015" {
		t.Errorf("recent: %+v", r)
	}
	rep := getStats(t, srv.URL, 1)
	day := rep.Days[0]
	if s := rep.Data[day]["a"]; s == nil || s.TokensIn != 2000 || s.TokensOut != 1000 {
		t.Errorf("day stats: %+v", s)
	}
	if ts := rep.TokenTotals["litellm"]; ts == nil || ts.Requests != 2 || fmt.Sprintf("%.4f", ts.Cost) != "0.0030" {
		t.Errorf("token stats: %+v", ts)
	}
}
