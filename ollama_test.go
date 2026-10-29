package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOllama implements the parts of the Ollama API the balancer uses.
type fakeOllama struct {
	mu      sync.Mutex
	models  []string
	loaded  []string
	srv     *httptest.Server
	chats   int
	pullDur time.Duration
}

func newFakeOllama(t *testing.T, models ...string) *fakeOllama {
	f := &fakeOllama{models: models, pullDur: 300 * time.Millisecond}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		model, _ := body["model"].(string)
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/api/version":
			fmt.Fprint(w, `{"version":"0.12.6"}`)
		case "/api/tags", "/v1/models":
			var list []map[string]any
			for _, m := range f.models {
				list = append(list, map[string]any{"name": m, "model": m, "size": 4_000_000_000, "modified_at": "2026-10-20T10:00:00Z",
					"details": map[string]any{"family": "qwen3", "parameter_size": "8.2B", "quantization_level": "Q4_K_M"}})
			}
			json.NewEncoder(w).Encode(map[string]any{"models": list, "data": list})
		case "/api/ps":
			var list []map[string]any
			for _, m := range f.loaded {
				list = append(list, map[string]any{"name": m, "model": m, "size": 5e9, "size_vram": 5e9, "expires_at": "2026-10-29T10:00:00Z"})
			}
			json.NewEncoder(w).Encode(map[string]any{"models": list})
		case "/api/pull":
			if model == "gibts/nicht" {
				w.WriteHeader(500)
				fmt.Fprint(w, `{"error":"pull model manifest: file does not exist"}`)
				return
			}
			f.mu.Unlock()
			fl := w.(http.Flusher)
			fmt.Fprintln(w, `{"status":"pulling manifest"}`)
			for i := 1; i <= 5; i++ {
				select {
				case <-r.Context().Done():
					f.mu.Lock()
					return
				case <-time.After(f.pullDur / 5):
				}
				fmt.Fprintf(w, `{"status":"pulling abc","digest":"sha256:abc","total":1000,"completed":%d}`+"\n", i*200)
				fl.Flush()
			}
			fmt.Fprintln(w, `{"status":"success"}`)
			f.mu.Lock()
			f.models = append(f.models, model)
		case "/api/delete":
			for i, m := range f.models {
				if m == model {
					f.models = append(f.models[:i], f.models[i+1:]...)
					return
				}
			}
			w.WriteHeader(404)
			fmt.Fprintf(w, `{"error":"model '%s' not found"}`, model)
		case "/api/generate", "/api/chat":
			if ka, ok := body["keep_alive"]; ok && fmt.Sprint(ka) == "0" {
				f.loaded = nil
				fmt.Fprint(w, `{"done":true}`)
				return
			}
			f.chats++
			f.loaded = []string{model}
			for i := 0; i < 3; i++ {
				fmt.Fprintf(w, `{"model":%q,"message":{"role":"assistant","content":"x"},"done":false}`+"\n", model)
			}
			fmt.Fprintf(w, `{"model":%q,"done":true,"prompt_eval_count":12,"eval_count":3}`+"\n", model)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func TestOllamaDetectionAndNativeAPI(t *testing.T) {
	ol := newFakeOllama(t, "qwen3:8b")
	oa := newModelLLM(t, "oa", 10*time.Millisecond, "qwen3:8b") // OpenAI-compatible server with the same model
	srv, pool := setup(t, func(c *Config) {
		c.HealthPath = "/v1/models"
		c.Backends = []BackendConfig{{Name: "oa", URL: oa.srv.URL, Priority: -5}, {Name: "ol", URL: ol.srv.URL}}
	})
	px := NewProxy(pool.cfg, pool)
	client := &http.Client{Timeout: 2 * time.Second}
	for _, ht := range pool.idleTargets() {
		if !px.probeOllama(client, ht) {
			px.discoverModels(client, ht, nil)
		}
	}
	if st := status(t, srv.URL); !st.Backends[1].IsOllama || st.Backends[1].Ollama != "0.12.6" || len(st.Backends[1].Models) != 1 {
		t.Fatalf("detection: %+v", st.Backends[1])
	}
	if st := status(t, srv.URL); st.Backends[0].IsOllama {
		t.Error("openai server detected as Ollama")
	}
	// native chat only goes to the Ollama server, even though oa is preferred
	for i := 0; i < 2; i++ {
		resp, err := http.Post(srv.URL+"/api/chat", "application/json", strings.NewReader(`{"model":"qwen3:8b","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.Header.Get("X-LLM-Balancer-Backend") != "ol" || !strings.Contains(string(b), `"done":true`) {
			t.Errorf("chat: %s %s", resp.Header.Get("X-LLM-Balancer-Backend"), b)
		}
	}
	waitFor(t, func() bool { st := status(t, srv.URL); return len(st.Recent) == 2 })
	if r := status(t, srv.URL).Recent[0]; !r.Stream || r.TokensIn != 12 || r.TokensOut != 3 {
		t.Errorf("recent: stream=%v tokens=%d/%d", r.Stream, r.TokensIn, r.TokensOut)
	}
	// aggregated info endpoints
	for path, want := range map[string]string{"/api/tags": `"qwen3:8b"`, "/api/version": `"0.12.6"`} {
		resp, _ := http.Get(srv.URL + path)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(b), want) {
			t.Errorf("%s: %s", path, b)
		}
	}
	// management through the client API is refused
	resp, _ := http.Post(srv.URL+"/api/pull", "application/json", strings.NewReader(`{"model":"x"}`))
	if resp.StatusCode != 403 {
		t.Errorf("pull via client API: %d", resp.StatusCode)
	}
	resp.Body.Close()
	// type "openai" switches detection off
	api(t, "PUT", srv.URL+"/balancer/api/backends/ol", `{"name":"ol","url":"`+ol.srv.URL+`","type":"openai"}`, "")
	pool.mu.Lock()
	isO := pool.backends[1].isOllama()
	pool.mu.Unlock()
	if isO {
		t.Error("type openai should switch Ollama off")
	}
	resp, _ = http.Post(srv.URL+"/api/chat", "application/json", strings.NewReader(`{"model":"qwen3:8b"}`))
	if resp.StatusCode != 404 {
		t.Errorf("no Ollama server left: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestOllamaModelManagement(t *testing.T) {
	ol := newFakeOllama(t, "qwen3:8b")
	ol2 := newFakeOllama(t)
	srv, _ := setup(t, func(c *Config) {
		c.HealthPath = "/v1/models"
		c.Backends = []BackendConfig{{Name: "ol", URL: ol.srv.URL, Type: "ollama"}, {Name: "ol2", URL: ol2.srv.URL, Type: "ollama"}}
	})
	w := newWeb(t, srv.URL)
	code, body := w.do("GET", "/api/ollama/models", "")
	var list []serverModels
	json.Unmarshal([]byte(body), &list)
	if code != 200 || len(list) != 2 || len(list[0].Models) != 1 || list[0].Models[0].Details.QuantizationLevel != "Q4_K_M" {
		t.Fatalf("models: %d %s", code, body)
	}
	// pull on both servers, with progress
	if c, b := w.do("POST", "/api/ollama/pull", `{"model":"llama3.2:3b","servers":["ol","ol2"]}`); c != 200 {
		t.Fatalf("pull: %d %s", c, b)
	}
	if c, _ := w.do("POST", "/api/ollama/pull", `{"model":"llama3.2:3b","servers":["ol"]}`); c != 400 {
		t.Errorf("duplicate pull: %d", c)
	}
	sawProgress := false
	waitFor(t, func() bool {
		_, b := w.do("GET", "/api/ollama/jobs", "")
		var jobs []pullJob
		json.Unmarshal([]byte(b), &jobs)
		done := 0
		for _, j := range jobs {
			if j.State == "running" && j.Completed > 0 && j.Completed < j.Total {
				sawProgress = true
			}
			if j.State == "done" && j.Completed == 1000 {
				done++
			}
		}
		return done == 2
	})
	if !sawProgress {
		t.Error("no progress seen")
	}
	ol.mu.Lock()
	if len(ol.models) != 2 {
		t.Errorf("model not pulled: %v", ol.models)
	}
	ol.mu.Unlock()
	// failing pull
	w.do("POST", "/api/ollama/pull", `{"model":"gibts/nicht","servers":["ol"]}`)
	waitFor(t, func() bool {
		_, b := w.do("GET", "/api/ollama/jobs", "")
		return strings.Contains(b, "file does not exist")
	})
	// cancel a running pull
	ol2.pullDur = 3 * time.Second
	w.do("POST", "/api/ollama/pull", `{"model":"big:70b","servers":["ol2"]}`)
	_, b := w.do("GET", "/api/ollama/jobs", "")
	var jobs []pullJob
	json.Unmarshal([]byte(b), &jobs)
	w.do("POST", fmt.Sprintf("/api/ollama/jobs/%d/cancel", jobs[0].ID), "")
	waitFor(t, func() bool { _, b := w.do("GET", "/api/ollama/jobs", ""); return strings.Contains(b, `"cancelled"`) })
	// unload and delete
	ol.loaded = []string{"qwen3:8b"}
	if c, b := w.do("POST", "/api/ollama/unload", `{"model":"qwen3:8b","servers":["ol"]}`); c != 204 || len(ol.loaded) != 0 {
		t.Errorf("unload: %d %s", c, b)
	}
	if c, b := w.do("POST", "/api/ollama/delete", `{"model":"qwen3:8b","servers":["ol"]}`); c != 204 {
		t.Errorf("delete: %d %s", c, b)
	}
	if c, b := w.do("POST", "/api/ollama/delete", `{"model":"qwen3:8b","servers":["ol"]}`); c != 502 || !strings.Contains(b, "not found") {
		t.Errorf("delete missing: %d %s", c, b)
	}
	if c, _ := w.do("POST", "/api/ollama/pull", `{"model":"x y","servers":["ol"]}`); c != 400 {
		t.Errorf("invalid name: %d", c)
	}
	// roles: operator may unload, not delete
	w.do("POST", "/api/auth/setup", `{"name":"admin","password":"richtig123"}`)
	w.do("POST", "/api/users", `{"name":"op","role":"operator","password":"operator1"}`)
	op := newWeb(t, srv.URL)
	op.do("POST", "/api/auth/login", `{"name":"op","password":"operator1"}`)
	if c, _ := op.do("POST", "/api/ollama/unload", `{"model":"llama3.2:3b","servers":["ol"]}`); c != 204 {
		t.Errorf("operator unload: %d", c)
	}
	if c, _ := op.do("POST", "/api/ollama/delete", `{"model":"llama3.2:3b","servers":["ol"]}`); c != 403 {
		t.Errorf("operator delete: %d", c)
	}
}
