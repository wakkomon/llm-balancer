package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// codeServer answers POSTs with a fixed status and counts calls.
type codeServer struct {
	srv   *httptest.Server
	calls atomic.Int32
}

func newCodeServer(t *testing.T, code int, body string) *codeServer {
	c := &codeServer{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"data":[{"id":"m"}]}`))
			return
		}
		c.calls.Add(1)
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		w.Write([]byte(body))
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func groupSetup(t *testing.T, a, b, f *codeServer) *httptest.Server {
	srv, _ := setup(t, func(c *Config) {
		c.Groups = []GroupConfig{{Name: "g", Models: []string{"m"}, Fallback: "fb", FallbackAfterSec: 60}, {Name: "fb"}}
		c.Backends = []BackendConfig{
			{Name: "a", URL: a.srv.URL, Group: "g"},
			{Name: "b", URL: b.srv.URL, Group: "g", Priority: 1},
			{Name: "f", URL: f.srv.URL, Group: "fb"},
		}
	})
	return srv
}

func doChat(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()
	resp, err := http.Post(url+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func lastRecent(t *testing.T, url string) *ReqInfo {
	t.Helper()
	waitFor(t, func() bool { return len(status(t, url).Recent) > 0 })
	return status(t, url).Recent[0]
}

func TestRetryOtherServerInGroupOn500(t *testing.T) {
	a, b, f := newCodeServer(t, 500, `{"error":"a broke"}`), newCodeServer(t, 200, `{"ok":"b"}`), newCodeServer(t, 200, `{"ok":"f"}`)
	srv := groupSetup(t, a, b, f)
	code, body, h := doChat(t, srv.URL)
	if code != 200 || body != `{"ok":"b"}` || h.Get("X-LLM-Balancer-Backend") != "b" || f.calls.Load() != 0 {
		t.Fatalf("got %d %s via %s (f calls %d)", code, body, h.Get("X-LLM-Balancer-Backend"), f.calls.Load())
	}
	r := lastRecent(t, srv.URL)
	if r.Trail != "a: 500 → b: 200" || r.Attempts != 2 {
		t.Errorf("trail %q attempts %d", r.Trail, r.Attempts)
	}
	st := status(t, srv.URL)
	if st.Backends[0].Errors != 1 || st.Backends[1].Errors != 0 {
		t.Errorf("errors a=%d b=%d", st.Backends[0].Errors, st.Backends[1].Errors)
	}
}

func TestFallbackGroupWhenWholeGroupFails(t *testing.T) {
	a, b, f := newCodeServer(t, 500, `{"error":"a"}`), newCodeServer(t, 503, `{"error":"b busy"}`), newCodeServer(t, 200, `{"ok":"f"}`)
	srv := groupSetup(t, a, b, f)
	code, body, _ := doChat(t, srv.URL)
	if code != 200 || body != `{"ok":"f"}` {
		t.Fatalf("got %d %s", code, body)
	}
	if r := lastRecent(t, srv.URL); r.Trail != "a: 500 → b: 503 → f (Fallback): 200" || r.Group != "g → fb" {
		t.Errorf("trail %q group %q", r.Trail, r.Group)
	}
	if a.calls.Load() != 1 || b.calls.Load() != 1 {
		t.Errorf("calls a=%d b=%d", a.calls.Load(), b.calls.Load())
	}
}

func TestPassLastErrorWhenEverythingFails(t *testing.T) {
	a, b, f := newCodeServer(t, 500, `{"error":"a"}`), newCodeServer(t, 502, `{"error":"b"}`), newCodeServer(t, 500, `{"error":{"message":"fallback out of memory"}}`)
	srv := groupSetup(t, a, b, f)
	code, body, h := doChat(t, srv.URL)
	if code != 500 || body != `{"error":{"message":"fallback out of memory"}}` || h.Get("X-LLM-Balancer-Attempts") != "3" {
		t.Fatalf("got %d %s attempts=%s", code, body, h.Get("X-LLM-Balancer-Attempts"))
	}
	r := lastRecent(t, srv.URL)
	if r.Status != 500 || !strings.Contains(r.Error, "alle Server fehlgeschlagen") || r.Trail != "a: 500 → b: 502 → f (Fallback): 500" {
		t.Errorf("recent: %d %q %q", r.Status, r.Error, r.Trail)
	}
	if a.calls.Load()+b.calls.Load()+f.calls.Load() != 3 {
		t.Errorf("each server should be tried exactly once")
	}
}

func TestClientErrorIsNotRetried(t *testing.T) {
	a, b, f := newCodeServer(t, 400, `{"error":"context too long"}`), newCodeServer(t, 200, `{}`), newCodeServer(t, 200, `{}`)
	srv := groupSetup(t, a, b, f)
	if code, body, _ := doChat(t, srv.URL); code != 400 || body != `{"error":"context too long"}` || b.calls.Load() != 0 {
		t.Errorf("400 must be passed on directly: %d %s (b calls %d)", code, body, b.calls.Load())
	}
}

func TestAllUnreachableListsErrors(t *testing.T) {
	a, b, f := newCodeServer(t, 200, ``), newCodeServer(t, 200, ``), newCodeServer(t, 200, ``)
	a.srv.Close()
	b.srv.Close()
	f.srv.Close()
	srv := groupSetup(t, a, b, f)
	code, body, _ := doChat(t, srv.URL)
	if code != 502 || !strings.Contains(body, "all servers failed") || strings.Count(body, "connect ") != 3 {
		t.Errorf("got %d %s", code, body)
	}
}

func TestRetryStatusEnv(t *testing.T) {
	t.Setenv("BACKENDS", "http://x:1")
	t.Setenv("RETRY_STATUS", "404, 429")
	cfg, err := loadConfig()
	if err != nil || len(cfg.RetryStatus) != 2 || cfg.RetryStatus[1] != 429 {
		t.Errorf("%v %v", cfg.RetryStatus, err)
	}
	t.Setenv("RETRY_STATUS", "200")
	if _, err := loadConfig(); err == nil {
		t.Error("200 accepted")
	}
}

// modelServer reports models via GET /v1/models, answers POSTs with code, counts calls.
func modelServer(t *testing.T, models string, code int) *codeServer {
	c := &codeServer{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if r.URL.Path == "/v1/models" {
				w.Write([]byte(`{"data":[` + models + `]}`))
				return
			}
			c.calls.Add(1)
			w.WriteHeader(code)
			w.Write([]byte(`{"get":"` + r.URL.Path + `"}`))
			return
		}
		c.calls.Add(1)
		io.ReadAll(r.Body)
		w.WriteHeader(code)
		w.Write([]byte(`{"code":` + http.StatusText(code) + `}`))
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// Group and its fallback both fail with 404 – a server outside both that reports the model takes over.
func TestLastResortOtherServerWithModel(t *testing.T) {
	a := modelServer(t, `{"id":"qwen3-32b"}`, 404)
	f := modelServer(t, `{"id":"llama"}`, 404)
	other := modelServer(t, `{"id":"qwen3-32b"}`, 200)
	srv, pool := setup(t, func(c *Config) {
		c.Groups = []GroupConfig{
			{Name: "g", Models: []string{"qwen*"}, AutoModels: boolPtr(false), Fallback: "fb", FallbackAfterSec: 60},
			{Name: "fb", AutoModels: boolPtr(false)},
			{Name: "rest", AutoModels: boolPtr(false)},
		}
		c.Backends = []BackendConfig{
			{Name: "a", URL: a.srv.URL, Group: "g"}, {Name: "f", URL: f.srv.URL, Group: "fb"}, {Name: "o", URL: other.srv.URL, Group: "rest"},
		}
	})
	discover(t, pool)
	code, be := chatModel(t, srv.URL, "qwen3-32b", nil)
	if code != 200 || be != "o" {
		t.Fatalf("got %d via %q", code, be)
	}
	r := lastRecent(t, srv.URL)
	if r.Trail != "a: 404 → f (Fallback): 404 → o (Fallback): 200" || r.Group != "g → rest" {
		t.Errorf("trail %q group %q", r.Trail, r.Group)
	}
}

func TestGroupWithoutFallbackUsesOtherServerWithModel(t *testing.T) {
	a := modelServer(t, `{"id":"m"}`, 404)
	b := modelServer(t, `{"id":"m"}`, 200)
	srv, pool := setup(t, func(c *Config) {
		c.Groups = []GroupConfig{{Name: "g1", Models: []string{"m"}, AutoModels: boolPtr(false)}, {Name: "g2", AutoModels: boolPtr(false)}}
		c.Backends = []BackendConfig{{Name: "a", URL: a.srv.URL, Group: "g1"}, {Name: "b", URL: b.srv.URL, Group: "g2"}}
	})
	discover(t, pool)
	if code, be := chatModel(t, srv.URL, "m", nil); code != 200 || be != "b" {
		t.Errorf("got %d via %q", code, be)
	}
}

func TestRetrieveModelAndPassthroughRetry(t *testing.T) {
	a := modelServer(t, `{"id":"qwen3-32b"}`, 404)
	b := modelServer(t, `{"id":"other"}`, 200)
	srv, pool := setup(t, func(c *Config) {
		c.Backends = []BackendConfig{{Name: "a", URL: a.srv.URL}, {Name: "b", URL: b.srv.URL}}
	})
	discover(t, pool)
	resp, _ := http.Get(srv.URL + "/v1/models/qwen3-32b")
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	resp.Body.Close()
	if resp.StatusCode != 200 || m["id"] != "qwen3-32b" {
		t.Errorf("retrieve model: %d %v", resp.StatusCode, m)
	}
	// any other GET: a answers 404, b is tried and answers 200
	for i := 0; i < 4; i++ {
		resp, _ = http.Get(srv.URL + "/v1/something")
		resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("X-LLM-Balancer-Backend") != "b" {
			t.Errorf("passthrough %d: %d via %s", i, resp.StatusCode, resp.Header.Get("X-LLM-Balancer-Backend"))
		}
	}
	// unknown model: every server is asked, the last 404 is passed on
	resp, _ = http.Get(srv.URL + "/v1/models/nope")
	resp.Body.Close()
	if resp.StatusCode != 200 { // b answers 200 for any GET in this fake
		t.Errorf("unknown model passthrough: %d", resp.StatusCode)
	}
}

func boolPtr(b bool) *bool { return &b }
