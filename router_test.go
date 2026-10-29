package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAnalyzeRequest(t *testing.T) {
	f := analyzeRequest([]byte(`{"model":"x","messages":[{"role":"system","content":"Du bist ein Coder"},
		{"role":"user","content":[{"type":"text","text":"was ist auf dem Bild?"},{"type":"image_url","image_url":{"url":"data:..."}}]}],
		"tools":[{"type":"function","function":{"name":"f"}}],"response_format":{"type":"json_object"},"reasoning_effort":"high"}`))
	if !f.Images || !f.Tools || !f.JSON || !f.Reasoning || !strings.Contains(f.last, "Bild") || !strings.Contains(f.system, "Coder") || f.Tokens == 0 {
		t.Errorf("features: %+v", f)
	}
	o := analyzeRequest([]byte(`{"model":"x","messages":[{"role":"user","content":"hi","images":["abc"]}],"think":true,"format":"json"}`))
	if !o.Images || !o.Reasoning || !o.JSON || o.Tools {
		t.Errorf("ollama features: %+v", o)
	}
	big := analyzeRequest([]byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("wort ", 7000) + `"}]}`))
	if big.Tokens < 9000 || big.Tokens > 11000 {
		t.Errorf("token estimate: %d", big.Tokens)
	}
}

func chatBodyJSON(t *testing.T, url, body string, hdr map[string]string) (string, string) {
	t.Helper()
	w := newWeb(t, url)
	for k, v := range hdr {
		w.hdr[k] = v
	}
	req, _ := jsonReq(url+"/v1/chat/completions", body, hdr)
	resp, err := w.c.Do(req)
	if err != nil {
		t.Error(err) // also called from goroutines
		return "", ""
	}
	defer resp.Body.Close()
	var out struct{ Backend, Model string }
	json.NewDecoder(resp.Body).Decode(&out)
	return out.Backend, out.Model
}

func TestRouterRules(t *testing.T) {
	small := newModelLLM(t, "small", 10*time.Millisecond, "qwen3:8b")
	coder := newModelLLM(t, "coder", 10*time.Millisecond, "qwen3-coder:30b")
	vision := newModelLLM(t, "vision", 10*time.Millisecond, "gemma3:27b")
	big := newModelLLM(t, "big", 400*time.Millisecond, "qwen3:32b")
	yes, no := true, false
	srv, pool := setup(t, func(c *Config) {
		c.Backends = []BackendConfig{
			{Name: "small", URL: small.srv.URL, Features: []ModelFeature{{Model: "qwen3:8b", Vision: &no, Context: 8192}}},
			{Name: "coder", URL: coder.srv.URL},
			{Name: "vision", URL: vision.srv.URL, Features: []ModelFeature{{Model: "gemma3*", Vision: &yes, Context: 131072}}},
			{Name: "big", URL: big.srv.URL, Features: []ModelFeature{{Model: "qwen3:32b", Context: 32768}}},
		}
		c.Routers = []RouterConfig{{Name: "auto", Default: "qwen3:8b", Models: []string{"gpt-4*"}, Rules: []RouterRule{
			{Name: "Bilder", Images: &yes, Needs: []string{"vision"}},
			{Name: "Code", Pattern: "```|programmier|funktion", Model: "qwen3-coder:30b"},
			{Name: "Groß", MinTokens: 6000, Model: "qwen3:32b"},
			{Name: "Hermes", Tokens: []string{"hermes"}, Model: "qwen3:32b", Disabled: true},
			{Name: "Ausweichen", WhenBusy: "qwen3:32b", Model: "qwen3:8b"},
		}}}
	})
	discover(t, pool)
	msg := func(text string) string {
		b, _ := json.Marshal(map[string]any{"model": "auto", "messages": []map[string]any{{"role": "user", "content": text}}})
		return string(b)
	}
	cases := []struct{ body, backend, model string }{
		{msg("hallo"), "small", "qwen3:8b"},
		{msg("bitte programmiere eine Funktion"), "coder", "qwen3-coder:30b"},
		{`{"model":"auto","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`, "vision", "gemma3:27b"},
		{msg(strings.Repeat("text ", 6000)), "big", "qwen3:32b"},
		{strings.Replace(msg("hallo"), `"auto"`, `"gpt-4o"`, 1), "small", "qwen3:8b"},                 // real model name taken over
		{strings.Replace(msg("hallo"), `"auto"`, `"qwen3-coder:30b"`, 1), "coder", "qwen3-coder:30b"}, // no router: untouched
	}
	for _, c := range cases {
		be, model := chatBodyJSON(t, srv.URL, c.body, nil)
		if be != c.backend || model != c.model {
			t.Errorf("%.60s… → %s/%s, want %s/%s", c.body, be, model, c.backend, c.model)
		}
	}
	waitFor(t, func() bool { return len(status(t, srv.URL).Recent) == len(cases) })
	r := status(t, srv.URL).Recent
	// newest first: the untouched request, then gpt-4o → qwen3:8b …
	if r[0].Requested != "" || r[0].Router != "" {
		t.Errorf("untouched: %+v", r[0])
	}
	if r[1].Requested != "gpt-4o" || r[1].Effective != "qwen3:8b" || !strings.Contains(r[1].Router, "auto: Standard") {
		t.Errorf("taken over: requested=%q effective=%q router=%q", r[1].Requested, r[1].Effective, r[1].Router)
	}
	// auto caps: images to the default model (no vision) switch to gemma even without a matching rule
	pool.mu.Lock()
	pool.routers[0].Rules[0].Disabled = true
	pool.mu.Unlock()
	if be, _ := chatBodyJSON(t, srv.URL, `{"model":"auto","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`, nil); be != "vision" {
		t.Errorf("auto caps: %s", be)
	}
	// busy rule: big occupied → "Groß" would pick 32b, but rule order: Groß matches first. Use a small prompt:
	go chatBodyJSON(t, srv.URL, `{"model":"qwen3:32b","messages":[]}`, nil)
	time.Sleep(80 * time.Millisecond)
	pool.mu.Lock()
	pool.routers[0].Default = "qwen3:32b"
	pool.mu.Unlock()
	if be, _ := chatBodyJSON(t, srv.URL, msg("hallo"), nil); be != "small" {
		t.Errorf("when busy: %s", be)
	}
	// /v1/models lists the virtual model
	if !strings.Contains(strings.Join(pool.AllModels(), ","), "auto") {
		t.Error("router not in /v1/models")
	}
	// test endpoint explains
	w := newWeb(t, srv.URL)
	_, body := w.do("POST", "/api/routers/test", `{"request":`+msg("hallo")+`}`)
	if !strings.Contains(body, `"router":"auto"`) || !strings.Contains(body, "Muster passt nicht") {
		t.Errorf("test: %s", body)
	}
}

func TestRouterSticky(t *testing.T) {
	a := newModelLLM(t, "a", 10*time.Millisecond, "m-small")
	b := newModelLLM(t, "b", 10*time.Millisecond, "m-code")
	srv, pool := setup(t, func(c *Config) {
		c.Backends = []BackendConfig{{Name: "a", URL: a.srv.URL}, {Name: "b", URL: b.srv.URL}}
		c.Routers = []RouterConfig{{Name: "auto", Default: "m-small", Sticky: true, Rules: []RouterRule{{Name: "Code", Pattern: "code", Model: "m-code"}}}}
	})
	discover(t, pool)
	hdr := map[string]string{"X-Session-Id": "s1"}
	first := `{"model":"auto","messages":[{"role":"user","content":"schreib code"}]}`
	second := `{"model":"auto","messages":[{"role":"user","content":"danke"}]}`
	if be, _ := chatBodyJSON(t, srv.URL, first, hdr); be != "b" {
		t.Fatalf("first: %s", be)
	}
	if be, _ := chatBodyJSON(t, srv.URL, second, hdr); be != "b" {
		t.Errorf("sticky session should stay on m-code: %s", be)
	}
	if be, _ := chatBodyJSON(t, srv.URL, second, map[string]string{"X-Session-Id": "s2"}); be != "a" {
		t.Errorf("other session: %s", be)
	}
	pool.mu.Lock()
	st := pool.routerStatusLocked()
	pool.mu.Unlock()
	if st[0].Bound != 2 || st[0].Hits["Code"] != 2 || st[0].Hits[""] != 1 {
		t.Errorf("status: %+v", st[0])
	}
}

func TestRouterAPIValidationAndPersistence(t *testing.T) {
	a := newModelLLM(t, "a", 10*time.Millisecond, "m")
	dir := t.TempDir()
	srv, _ := setup(t, func(c *Config) { c.DataDir = dir })
	_ = a
	if c := api(t, "POST", srv.URL+"/balancer/api/routers", `{"name":"auto","rules":[{"name":"x","pattern":"("}]}`, ""); c != 400 {
		t.Errorf("bad regex: %d", c)
	}
	if c := api(t, "POST", srv.URL+"/balancer/api/routers", `{"name":"auto","rules":[{"name":"x"}]}`, ""); c != 400 {
		t.Errorf("rule without target: %d", c)
	}
	if c := api(t, "POST", srv.URL+"/balancer/api/routers", `{"name":"auto","default":"m","sticky":true,"rules":[{"name":"Nachts","hours":"22-6","model":"m"}]}`, ""); c != 204 {
		t.Fatalf("add: %d", c)
	}
	st, _, err := NewStore(dir).Load()
	if err != nil || len(st.Routers) != 1 || st.Routers[0].Rules[0].Hours != "22-6" {
		t.Errorf("not persisted: %+v %v", st.Routers, err)
	}
	if !inHours("22-6", time.Date(2026, 1, 1, 23, 0, 0, 0, time.Local)) || inHours("22-6", time.Date(2026, 1, 1, 12, 0, 0, 0, time.Local)) {
		t.Error("hours")
	}
}

func jsonReq(url, body string, hdr map[string]string) (*http.Request, error) {
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return req, nil
}
