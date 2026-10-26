package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func getStats(t *testing.T, url string, days int) StatsReport {
	t.Helper()
	resp, err := http.Get(url + "/balancer/api/stats?days=" + itoa(int64(days)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var rep StatsReport
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestDailyStatsPerBackend(t *testing.T) {
	a := newFakeLLM(t, "a", 200*time.Millisecond)
	b := newFakeLLM(t, "b", 200*time.Millisecond)
	dir := t.TempDir()
	srv, pool := setup(t, func(c *Config) { c.DataDir = dir }, a, b)

	done := make(chan bool)
	for i := 0; i < 4; i++ {
		go func() { post(t, srv.URL); done <- true }()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
	rep := getStats(t, srv.URL, 7)
	today := dayKey(time.Now())
	if len(rep.Days) != 7 || rep.To != today {
		t.Fatalf("days: %v", rep.Days)
	}
	if len(rep.Backends) != 2 {
		t.Fatalf("backends: %v", rep.Backends)
	}
	var total int64
	for _, name := range []string{"a", "b"} {
		st := rep.Data[today][name]
		if st == nil || st.Requests != 2 || st.Errors != 0 {
			t.Fatalf("%s: %+v", name, st)
		}
		if st.DurMs < 300 || st.MaxDurMs < 150 || st.TTFBCount != 2 || st.Bytes == 0 {
			t.Errorf("%s durations: %+v", name, st)
		}
		total += rep.Totals[name].Requests
	}
	if total != 4 {
		t.Errorf("totals: %d", total)
	}

	// persisted: flush, then a fresh Stats from the same dir sees the numbers
	pool.stats.Flush()
	again := NewStats(dir, 0).Report(1, time.Now())
	if again.Totals["a"] == nil || again.Totals["a"].Requests != 2 {
		t.Errorf("not persisted: %+v", again.Totals)
	}

	// CSV export
	resp, _ := http.Get(srv.URL + "/balancer/api/stats?days=7&format=csv")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if lines := strings.Count(string(body), "\n"); lines != 3 || !strings.HasPrefix(string(body), "Tag;Server;Anfragen") {
		t.Errorf("csv:\n%s", body)
	}

	// reset
	if c := api(t, "POST", srv.URL+"/balancer/api/stats/reset", "", ""); c != 204 {
		t.Fatalf("reset: %d", c)
	}
	if rep := getStats(t, srv.URL, 7); len(rep.Backends) != 0 {
		t.Errorf("not reset: %v", rep.Backends)
	}
}

func TestStatsCountErrorsAndNoBackend(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer broken.Close()
	a := newFakeLLM(t, "a", 300*time.Millisecond)
	srv, _ := setup(t, func(c *Config) {
		c.Backends = append(c.Backends, BackendConfig{Name: "broken", URL: broken.URL, Priority: -1})
		c.MaxQueue = 1
	}, a)
	be, _ := post(t, srv.URL) // → broken (preferred) answers 500 → retried on a
	if be != "a" {
		t.Errorf("500 should be retried on a, got %q", be)
	}
	st := status(t, srv.URL)
	for _, b := range st.Backends {
		if b.Name == "broken" && b.State != "error" {
			t.Errorf("broken state = %s", b.State)
		}
		if b.Name == "a" && b.State != "idle" {
			t.Errorf("a state = %s", b.State)
		}
	}
	// fill both slots + queue, then one more is rejected without a backend
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	defer slow.Close()
	api(t, "PUT", srv.URL+"/balancer/api/backends/broken", `{"name":"broken","url":"`+slow.URL+`","priority":-1}`, "")
	go post(t, srv.URL)
	go post(t, srv.URL)
	time.Sleep(50 * time.Millisecond)
	if st := status(t, srv.URL); st.States["busy"] != 2 {
		t.Errorf("expected 2 busy, got %v", st.States)
	}
	go post(t, srv.URL) // queued
	time.Sleep(50 * time.Millisecond)
	post(t, srv.URL) // queue full → 503, no backend
	rep := getStats(t, srv.URL, 1)
	today := dayKey(time.Now())
	if s := rep.Data[today]["broken"]; s == nil || s.Errors != 1 {
		t.Errorf("broken errors: %+v", s)
	}
	if s := rep.Data[today][NoBackend]; s == nil || s.Requests != 1 || s.Errors != 1 {
		t.Errorf("no-backend: %+v", s)
	}
}

func TestDownAndDisabledState(t *testing.T) {
	a := newFakeLLM(t, "a", 10*time.Millisecond)
	dead := newFakeLLM(t, "dead", 0)
	dead.srv.Close()
	srv, _ := setup(t, func(c *Config) { c.Backends[1].Priority = -1 }, a, dead)
	post(t, srv.URL)
	api(t, "POST", srv.URL+"/balancer/api/backends/a/disable", "", "")
	st := status(t, srv.URL)
	if st.States["down"] != 1 || st.States["disabled"] != 1 {
		t.Errorf("states: %v", st.States)
	}
}

func TestStatsPerModel(t *testing.T) {
	big := newModelLLM(t, "big", 20*time.Millisecond, "qwen3-32b")
	srv, _ := setup(t, func(c *Config) { c.Backends = []BackendConfig{{Name: "big", URL: big.srv.URL}} })
	chatModel(t, srv.URL, "qwen3-32b", nil)
	chatModel(t, srv.URL, "qwen3-32b", nil)
	chatModel(t, srv.URL, "llama", nil)
	// the response reaches the client a moment before the request is booked
	waitFor(t, func() bool { return len(getStats(t, srv.URL, 1).Models) == 2 })
	rep := getStats(t, srv.URL, 1)
	if len(rep.Models) != 2 || rep.Models[0] != "qwen3-32b" {
		t.Fatalf("models: %v", rep.Models)
	}
	if m := rep.ModelTotals["qwen3-32b"]; m.Requests != 2 || m.TokensIn != 2000 || m.TokensOut != 1000 {
		t.Errorf("qwen: %+v", m)
	}
}
