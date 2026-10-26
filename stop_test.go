package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// slowLLM waits `prefill`, then streams a chunk every 100 ms for a long time and records whether
// the balancer closed the connection.
type slowLLM struct {
	srv          *httptest.Server
	disconnected atomic.Int32
}

func newSlowLLM(t *testing.T, prefill time.Duration) *slowLLM {
	s := &slowLLM{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		select {
		case <-time.After(prefill):
		case <-r.Context().Done():
			s.disconnected.Add(1)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 200; i++ {
			if _, err := fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"); err != nil {
				s.disconnected.Add(1)
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-time.After(100 * time.Millisecond):
			case <-r.Context().Done():
				s.disconnected.Add(1)
				return
			}
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func streamReq(url string) (int, string, error) {
	resp, err := http.Post(url+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m","stream":true}`))
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

func TestStopRunningStream(t *testing.T) {
	llm := newSlowLLM(t, 0)
	srv, _ := setup(t, func(c *Config) { c.Backends = []BackendConfig{{Name: "s", URL: llm.srv.URL}} })
	type res struct {
		code int
		body string
	}
	first, second := make(chan res, 1), make(chan res, 1)
	go func() { c, b, _ := streamReq(srv.URL); first <- res{c, b} }()
	time.Sleep(400 * time.Millisecond)
	go func() { c, b, _ := streamReq(srv.URL); second <- res{c, b} }() // waits in the queue
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	resp, _ := http.Post(srv.URL+"/balancer/api/backends/s/stop", "", nil)
	var out map[string]int
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out["stopped"] != 1 {
		t.Fatalf("stopped: %v", out)
	}
	r := <-first
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("stop took %v", time.Since(start))
	}
	if r.code != 200 || !strings.Contains(r.body, "data: {\"choices\"") || !strings.Contains(r.body, "stopped by administrator") {
		t.Errorf("first client: %d %q", r.code, r.body[max(0, len(r.body)-200):])
	}
	waitFor(t, func() bool { return llm.disconnected.Load() >= 1 })

	// the queued request got the slot right away; stop it too (by id)
	waitFor(t, func() bool { return len(status(t, srv.URL).Backends[0].Running) == 1 })
	id := status(t, srv.URL).Backends[0].Running[0].ID
	if c := api(t, "POST", fmt.Sprintf("%s/balancer/api/requests/%d/stop", srv.URL, id), "", ""); c != 204 {
		t.Fatalf("stop by id: %d", c)
	}
	<-second
	waitFor(t, func() bool { return len(status(t, srv.URL).Recent) == 2 })
	st := status(t, srv.URL)
	b := st.Backends[0]
	if b.Active != 0 || b.Errors != 0 || b.LastError != "" || b.State != "idle" {
		t.Errorf("server after stop: %+v", b)
	}
	for _, r := range st.Recent {
		if r.Status != 499 || r.Error != stoppedMsg {
			t.Errorf("recent: %d %q", r.Status, r.Error)
		}
	}
	if c := api(t, "POST", fmt.Sprintf("%s/balancer/api/requests/%d/stop", srv.URL, id), "", ""); c != 404 {
		t.Errorf("stopping a finished request: %d", c)
	}
}

func TestStopDuringPromptProcessing(t *testing.T) {
	llm := newSlowLLM(t, 5*time.Second)
	srv, _ := setup(t, func(c *Config) { c.Backends = []BackendConfig{{Name: "s", URL: llm.srv.URL}} })
	done := make(chan [2]any, 1)
	go func() { c, b, _ := streamReq(srv.URL); done <- [2]any{c, b} }()
	time.Sleep(200 * time.Millisecond)
	api(t, "POST", srv.URL+"/balancer/api/backends/s/stop", "", "")
	r := <-done
	if r[0].(int) != 499 || !strings.Contains(r[1].(string), "stopped by administrator") {
		t.Errorf("client: %v", r)
	}
	waitFor(t, func() bool { return llm.disconnected.Load() == 1 })
}

func TestStopNeedsAdminToken(t *testing.T) {
	llm := newSlowLLM(t, 0)
	srv, _ := setup(t, func(c *Config) {
		c.AdminToken = "s3cret"
		c.Backends = []BackendConfig{{Name: "s", URL: llm.srv.URL}}
	})
	if c := api(t, "POST", srv.URL+"/balancer/api/backends/s/stop", "", ""); c != 401 {
		t.Errorf("without token: %d", c)
	}
}
