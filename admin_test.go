package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func api(t *testing.T, method, url, body, token string) int {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Admin-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func status(t *testing.T, url string) Status {
	resp, err := http.Get(url + "/balancer/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st Status
	json.NewDecoder(resp.Body).Decode(&st)
	return st
}

func TestEditBackendsAtRuntimeAndPersist(t *testing.T) {
	a := newFakeLLM(t, "a", 50*time.Millisecond)
	b := newFakeLLM(t, "b", 50*time.Millisecond)
	dir := t.TempDir()
	srv, _ := setup(t, func(c *Config) { c.DataDir = dir; c.Backends = c.Backends[:1] }, a, b) // start with only a

	if st := status(t, srv.URL); st.Source != "env" || len(st.Backends) != 1 {
		t.Fatalf("initial: %+v", st)
	}
	// add b with higher preference
	if c := api(t, "POST", srv.URL+"/balancer/api/backends", `{"name":"b","url":"`+b.srv.URL+`/","priority":-1}`, ""); c != 204 {
		t.Fatalf("add: %d", c)
	}
	if be, _ := post(t, srv.URL); be != "b" {
		t.Errorf("new backend not used: %s", be)
	}
	// duplicate / invalid
	if c := api(t, "POST", srv.URL+"/balancer/api/backends", `{"name":"b","url":"http://x:1"}`, ""); c != 409 {
		t.Errorf("duplicate: %d", c)
	}
	if c := api(t, "POST", srv.URL+"/balancer/api/backends", `{"url":"nope"}`, ""); c != 400 {
		t.Errorf("invalid url: %d", c)
	}
	// rename a, 2 slots
	if c := api(t, "PUT", srv.URL+"/balancer/api/backends/a", `{"name":"gpu","url":"`+a.srv.URL+`","max_concurrent":2}`, ""); c != 204 {
		t.Fatalf("update: %d", c)
	}
	// remove b
	if c := api(t, "DELETE", srv.URL+"/balancer/api/backends/b", "", ""); c != 204 {
		t.Fatalf("delete: %d", c)
	}
	if be, _ := post(t, srv.URL); be != "gpu" {
		t.Errorf("expected gpu, got %s", be)
	}
	st := status(t, srv.URL)
	if st.Source != "saved" || len(st.Backends) != 1 || st.Backends[0].Name != "gpu" || st.Backends[0].Max != 2 {
		t.Fatalf("after edits: %+v", st.Backends)
	}

	// "restart": a new instance with the same data dir ignores the env list
	srv2, _ := setup(t, func(c *Config) { c.DataDir = dir }, a, b)
	st = status(t, srv2.URL)
	if st.Source != "saved" || len(st.Backends) != 1 || st.Backends[0].Name != "gpu" {
		t.Fatalf("not restored: %+v", st.Backends)
	}
	// reset → back to env list (a, b)
	if c := api(t, "POST", srv2.URL+"/balancer/api/reset", "", ""); c != 204 {
		t.Fatalf("reset: %d", c)
	}
	st = status(t, srv2.URL)
	if st.Source != "env" || len(st.Backends) != 2 {
		t.Fatalf("reset: %+v", st.Backends)
	}
	srv3, _ := setup(t, func(c *Config) { c.DataDir = dir; c.Backends = c.Backends[:1] }, a, b)
	if n := len(status(t, srv3.URL).Backends); n != 1 {
		t.Errorf("saved file not deleted on reset, got %d backends", n)
	}
}

func TestRemoveBackendWhileBusy(t *testing.T) {
	a := newFakeLLM(t, "a", 400*time.Millisecond)
	b := newFakeLLM(t, "b", 50*time.Millisecond)
	srv, pool := setup(t, nil, a, b)
	done := make(chan string)
	go func() { _, body := post(t, srv.URL); done <- body }() // lands on a
	time.Sleep(50 * time.Millisecond)
	if c := api(t, "DELETE", srv.URL+"/balancer/api/backends/a", "", ""); c != 204 {
		t.Fatalf("delete: %d", c)
	}
	if be, _ := post(t, srv.URL); be != "b" {
		t.Errorf("removed backend still used: %s", be)
	}
	if body := <-done; !strings.Contains(body, "[DONE]") {
		t.Errorf("running request broken: %q", body)
	}
	if st := pool.Status(); len(st.Backends) != 1 || st.Backends[0].Active != 0 {
		t.Errorf("state: %+v", st.Backends)
	}
}

func TestChangeURLAndSlotsWakesQueue(t *testing.T) {
	a := newFakeLLM(t, "a", 300*time.Millisecond)
	b := newFakeLLM(t, "b", 50*time.Millisecond)
	srv, _ := setup(t, nil, a)
	go post(t, srv.URL)
	time.Sleep(30 * time.Millisecond)
	res := make(chan string)
	go func() { be, _ := post(t, srv.URL); res <- be }() // queued behind a
	time.Sleep(30 * time.Millisecond)
	// add b → queued request must start immediately
	start := time.Now()
	api(t, "POST", srv.URL+"/balancer/api/backends", `{"name":"b","url":"`+b.srv.URL+`"}`, "")
	if be := <-res; be != "b" || time.Since(start) > 200*time.Millisecond {
		t.Errorf("queue not woken by new backend: %s after %v", be, time.Since(start))
	}
}

func TestAdminToken(t *testing.T) {
	a := newFakeLLM(t, "a", 10*time.Millisecond)
	srv, _ := setup(t, func(c *Config) { c.AdminToken = "s3cret" }, a)
	if c := api(t, "POST", srv.URL+"/balancer/api/backends/a/disable", "", ""); c != 401 {
		t.Errorf("no token: %d", c)
	}
	if c := api(t, "POST", srv.URL+"/balancer/api/backends/a/disable", "", "wrong"); c != 401 {
		t.Errorf("wrong token: %d", c)
	}
	if c := api(t, "POST", srv.URL+"/balancer/api/backends/a/disable", "", "s3cret"); c != 204 {
		t.Errorf("good token: %d", c)
	}
	if !status(t, srv.URL).AdminToken {
		t.Error("status should report token requirement")
	}
}

func TestNoBackendsFailsFast(t *testing.T) {
	srv, _ := setup(t, nil)
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Errorf("expected 502 without backends, got %d", resp.StatusCode)
	}
}

func TestLastErrorHiddenAfterSuccess(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "boom", 500)
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer be.Close()
	srv, _ := setup(t, func(c *Config) { c.Backends = []BackendConfig{{Name: "x", URL: be.URL}} })
	post(t, srv.URL)
	waitFor(t, func() bool { return status(t, srv.URL).Backends[0].LastError != "" })
	fail.Store(false)
	post(t, srv.URL)
	waitFor(t, func() bool { return len(status(t, srv.URL).Recent) == 2 })
	if b := status(t, srv.URL).Backends[0]; b.LastError != "" || b.Errors != 1 {
		t.Errorf("last error still shown: %q (errors=%d)", b.LastError, b.Errors)
	}
}
