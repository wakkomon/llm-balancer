package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// authEcho records the Authorization header it receives.
type authEcho struct {
	mu   sync.Mutex
	seen []string
	want string // if set, answer 401 unless "Bearer <want>"
	srv  *httptest.Server
}

func newAuthEcho(t *testing.T, want string) *authEcho {
	a := &authEcho{want: want}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		a.mu.Lock()
		a.seen = append(a.seen, h+"|"+r.Header.Get("X-Api-Key"))
		a.mu.Unlock()
		if a.want != "" && h != "Bearer "+a.want {
			http.Error(w, `{"error":"unauthorized"}`, 401)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *authEcho) last() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.seen) == 0 {
		return ""
	}
	return a.seen[len(a.seen)-1]
}

func call(t *testing.T, method, url string, hdr map[string]string, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestClientTokens(t *testing.T) {
	be := newAuthEcho(t, "")
	dir := t.TempDir()
	srv, _ := setup(t, func(c *Config) {
		c.DataDir = dir
		c.APIKeys = "litellm=sk-litellm-0123456789"
		c.Backends = []BackendConfig{{Name: "x", URL: be.srv.URL}}
	})
	chat := srv.URL + "/v1/chat/completions"

	if code, body := call(t, "POST", chat, nil, `{}`); code != 401 || !strings.Contains(body, "invalid_api_key") {
		t.Errorf("no token: %d %s", code, body)
	}
	if code, _ := call(t, "GET", srv.URL+"/v1/models", map[string]string{"Authorization": "Bearer wrong"}, ""); code != 401 {
		t.Errorf("wrong token on GET: %d", code)
	}
	if code, _ := call(t, "POST", chat, map[string]string{"Authorization": "Bearer sk-litellm-0123456789"}, `{}`); code != 200 {
		t.Errorf("bearer: %d", code)
	}
	if got := be.last(); got != "|" {
		t.Errorf("client token leaked to backend: %q", got)
	}
	if code, _ := call(t, "POST", chat, map[string]string{"X-Api-Key": "sk-litellm-0123456789"}, `{}`); code != 200 {
		t.Errorf("x-api-key: %d", code)
	}
	if got := be.last(); got != "|" {
		t.Errorf("x-api-key leaked to backend: %q", got)
	}

	// create a token in the UI
	code, body := call(t, "POST", srv.URL+"/balancer/api/tokens", nil, `{"name":"hermes"}`)
	var created map[string]string
	json.Unmarshal([]byte(body), &created)
	tok := created["token"]
	if code != 201 || !strings.HasPrefix(tok, "llmb-") {
		t.Fatalf("create: %d %s", code, body)
	}
	if code, _ := call(t, "POST", chat, map[string]string{"Authorization": "Bearer " + tok}, `{}`); code != 200 {
		t.Errorf("ui token: %d", code)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "access.json"))
	if strings.Contains(string(raw), tok) || !strings.Contains(string(raw), "hermes") {
		t.Errorf("access.json must hold the hash, not the token: %s", raw)
	}
	// list shows usage, never the token
	_, list := call(t, "GET", srv.URL+"/balancer/api/tokens", nil, "")
	if strings.Contains(list, tok) || !strings.Contains(list, `"requests":1`) {
		t.Errorf("list: %s", list)
	}
	// recent requests show the token name
	if st := status(t, srv.URL); st.Recent[0].Token != "hermes" || !st.AccessAuth {
		t.Errorf("recent token: %+v", st.Recent[0])
	}
	// env token can't be deleted, ui token can
	if code, _ := call(t, "DELETE", srv.URL+"/balancer/api/tokens/litellm", nil, ""); code != 400 {
		t.Errorf("delete env token: %d", code)
	}
	if code, _ := call(t, "DELETE", srv.URL+"/balancer/api/tokens/hermes", nil, ""); code != 204 {
		t.Errorf("delete ui token: %d", code)
	}
	if code, _ := call(t, "POST", chat, map[string]string{"Authorization": "Bearer " + tok}, `{}`); code != 401 {
		t.Errorf("deleted token still valid: %d", code)
	}
	// UI and status stay reachable without a client token
	if code, _ := call(t, "GET", srv.URL+"/balancer/api/status", nil, ""); code != 200 {
		t.Errorf("status needs no client token: %d", code)
	}
}

func TestBackendBearerToken(t *testing.T) {
	be := newAuthEcho(t, "backend-secret")
	open := newAuthEcho(t, "")
	dir := t.TempDir()
	srv, _ := setup(t, func(c *Config) {
		c.DataDir = dir
		c.HealthPath = "/v1/models"
		c.Backends = []BackendConfig{{Name: "x", URL: be.srv.URL, APIKey: "backend-secret"}}
	})
	chat := srv.URL + "/v1/chat/completions"
	// without access control the client's own header is replaced by the backend token
	if code, _ := call(t, "POST", chat, map[string]string{"Authorization": "Bearer client"}, `{}`); code != 200 {
		t.Fatalf("with backend token: %d", code)
	}
	if got := be.last(); got != "Bearer backend-secret|" {
		t.Errorf("backend got %q", got)
	}
	// the key never leaves the balancer through the API
	_, list := call(t, "GET", srv.URL+"/balancer/api/backends", nil, "")
	_, st := call(t, "GET", srv.URL+"/balancer/api/status", nil, "")
	if strings.Contains(list+st, "backend-secret") || !strings.Contains(list, `"has_api_key":true`) {
		t.Errorf("key leaked or flag missing: %s", list)
	}
	// edit without api_key keeps it
	api(t, "PUT", srv.URL+"/balancer/api/backends/x", `{"name":"x","url":"`+be.srv.URL+`","priority":1}`, "")
	call(t, "POST", chat, nil, `{}`)
	if got := be.last(); got != "Bearer backend-secret|" {
		t.Errorf("key lost on edit: %q", got)
	}
	fi, _ := os.Stat(filepath.Join(dir, "backends.json"))
	if fi == nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("backends.json permissions: %v", fi)
	}
	// wrong key → 401 from server, backend counts as faulty
	api(t, "PUT", srv.URL+"/balancer/api/backends/x", `{"name":"x","url":"`+be.srv.URL+`","api_key":"wrong"}`, "")
	call(t, "POST", chat, nil, `{}`)
	if s := status(t, srv.URL); s.Backends[0].State != "error" {
		t.Errorf("state after 401: %s", s.Backends[0].State)
	}
	// "" removes it; a server without token gets the client's header passed through
	api(t, "POST", srv.URL+"/balancer/api/backends", `{"name":"open","url":"`+open.srv.URL+`","priority":-1,"api_key":""}`, "")
	call(t, "POST", chat, map[string]string{"Authorization": "Bearer client"}, `{}`)
	if got := open.last(); got != "Bearer client|" {
		t.Errorf("open backend got %q", got)
	}
}

func TestHealthCheckRejectsWrongBackendToken(t *testing.T) {
	be := newAuthEcho(t, "right")
	srv, pool := setup(t, func(c *Config) {
		c.HealthPath = "/v1/models"
		c.Backends = []BackendConfig{{Name: "x", URL: be.srv.URL, APIKey: "wrong"}}
	})
	px := NewProxy(pool.cfg, pool)
	done := make(chan struct{})
	ctx, cancel := contextWithCancel()
	go func() { px.healthLoop(ctx); close(done) }()
	waitFor(t, func() bool { return status(t, srv.URL).Backends[0].State == "down" })
	if msg := status(t, srv.URL).Backends[0].HealthMsg; !strings.Contains(msg, "Token") {
		t.Errorf("health msg: %q", msg)
	}
	cancel()
	<-done
}

func TestBackendKeysEnv(t *testing.T) {
	bs, _ := parseBackendsEnv("gpu=http://a:1,http://b:2")
	if err := applyBackendKeys(bs, "gpu=k1, b:2=k2"); err != nil {
		t.Fatal(err)
	}
	if bs[0].APIKey != "k1" || bs[1].APIKey != "k2" {
		t.Errorf("%+v", bs)
	}
	if err := applyBackendKeys(bs, "nope=k"); err == nil {
		t.Error("unknown name accepted")
	}
}

func contextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 50; i++ {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}
