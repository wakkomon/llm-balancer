package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDisabledGroupUsesFallback(t *testing.T) {
	big := newModelLLM(t, "big", 800*time.Millisecond, "qwen3-32b")
	small := newModelLLM(t, "small", 10*time.Millisecond, "llama3.2:3b")
	dir := t.TempDir()
	srv, pool := setup(t, func(c *Config) {
		c.DataDir = dir
		c.Groups = []GroupConfig{{Name: "gross", Models: []string{"qwen3-32b"}, Fallback: "klein", FallbackAfterSec: 60, FallbackModel: "llama3.2:3b"}, {Name: "klein"}}
		c.Backends = []BackendConfig{{Name: "big", URL: big.srv.URL, Group: "gross"}, {Name: "small", URL: small.srv.URL, Group: "klein"}}
	})
	// a request waiting for "gross" moves to the fallback as soon as the group is switched off
	go chatBody(t, srv.URL, "qwen3-32b") // occupies big
	time.Sleep(100 * time.Millisecond)
	done := make(chan string)
	go func() { be, _ := chatBody(t, srv.URL, "qwen3-32b"); done <- be }()
	time.Sleep(100 * time.Millisecond)
	if c := api(t, "POST", srv.URL+"/balancer/api/groups/gross/disable", "", ""); c != 204 {
		t.Fatalf("disable: %d", c)
	}
	select {
	case be := <-done:
		if be != "small" {
			t.Errorf("queued request went to %q", be)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("queued request did not move to the fallback group")
	}
	start := time.Now()
	if be, body := chatBody(t, srv.URL, "qwen3-32b"); be != "small" || time.Since(start) > 300*time.Millisecond || !strings.Contains(body, "llama3.2:3b") {
		t.Errorf("new request: %s after %v: %s", be, time.Since(start), body)
	}
	st := status(t, srv.URL)
	for _, b := range st.Backends {
		if b.Name == "big" && (b.State != "disabled" || !b.GroupOff) {
			t.Errorf("big: state %s groupOff %v", b.State, b.GroupOff)
		}
	}
	if !st.Groups[0].Disabled {
		t.Errorf("group status: %+v", st.Groups[0])
	}
	// editing the group without "disabled" keeps it off; persisted
	api(t, "PUT", srv.URL+"/balancer/api/groups/gross", `{"name":"gross","models":["qwen3-32b"],"fallback":"klein"}`, "")
	pool.mu.Lock()
	off := pool.groupLocked("gross").Disabled
	pool.mu.Unlock()
	if !off {
		t.Error("update re-enabled the group")
	}
	api(t, "POST", srv.URL+"/balancer/api/groups/gross/enable", "", "")
	time.Sleep(900 * time.Millisecond) // big finished its first request
	if be, _ := chatBody(t, srv.URL, "qwen3-32b"); be != "big" {
		t.Errorf("after enable: %s", be)
	}
}

func TestSessionDetection(t *testing.T) {
	mk := func(hdr map[string]string, body string) *http.Request {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		return r
	}
	conv := func(sys, first string, more ...string) string {
		msgs := []map[string]any{{"role": "system", "content": sys}, {"role": "user", "content": first}}
		for _, m := range more {
			msgs = append(msgs, map[string]any{"role": "assistant", "content": "ok"}, map[string]any{"role": "user", "content": m})
		}
		b, _ := json.Marshal(map[string]any{"model": "x", "messages": msgs})
		return string(b)
	}
	a1, src := detectSession(mk(nil, conv("du bist hermes", "hallo")), []byte(conv("du bist hermes", "hallo")), "t")
	a2, _ := detectSession(mk(nil, ""), []byte(conv("du bist hermes", "hallo", "weiter", "und jetzt?")), "t")
	b1, _ := detectSession(mk(nil, ""), []byte(conv("du bist hermes", "andere aufgabe")), "t")
	if a1 == "" || a1 != a2 || a1 == b1 || len(a1) != 8 || !strings.HasPrefix(src, "Fingerprint") {
		t.Errorf("fingerprint: %q %q %q (%s)", a1, a2, b1, src)
	}
	if c, _ := detectSession(mk(nil, ""), []byte(conv("du bist hermes", "hallo")), "other"); c == a1 {
		t.Error("different token should give a different session")
	}
	h1, src := detectSession(mk(map[string]string{"X-Session-Id": "abc"}, ""), []byte(conv("s", "1")), "t")
	h2, _ := detectSession(mk(map[string]string{"X-Session-Id": "abc"}, ""), []byte(conv("s", "2")), "t")
	if h1 != h2 || src != "Header X-Session-Id" {
		t.Errorf("header: %q %q %s", h1, h2, src)
	}
	_, src = detectSession(mk(nil, ""), []byte(`{"model":"x","metadata":{"user_id":"u_1"},"system":"s","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`), "t")
	if src != "Feld metadata.user_id" {
		t.Errorf("anthropic metadata: %s", src)
	}
	_, src = detectSession(mk(nil, ""), []byte(`{"model":"x","system":"s","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`), "t")
	if !strings.HasPrefix(src, "Fingerprint") {
		t.Errorf("anthropic fingerprint: %s", src)
	}
	if id, _ := detectSession(mk(nil, ""), []byte(`{"model":"x"}`), "t"); id != "" {
		t.Errorf("no session expected: %s", id)
	}
}

func TestSessionInRecent(t *testing.T) {
	a := newFakeLLM(t, "a", 10*time.Millisecond)
	srv, _ := setup(t, nil, a)
	chatModel(t, srv.URL, "m", map[string]string{"X-Session-Id": "s1"})
	waitFor(t, func() bool { return len(status(t, srv.URL).Recent) == 1 })
	if r := status(t, srv.URL).Recent[0]; r.Session == "" || r.SessionSrc != "Header X-Session-Id" {
		t.Errorf("recent: %q %q", r.Session, r.SessionSrc)
	}
}

func TestHourlySplitAndQueueStats(t *testing.T) {
	s := NewStats(t.TempDir(), 0)
	base := time.Date(2026, 10, 27, 9, 50, 0, 0, time.Local)
	r := &ReqInfo{Backend: "a", Status: 200, QueuedAt: base.Add(-2 * time.Second), StartedAt: base, EndedAt: base.Add(20 * time.Minute)}
	s.Record(r)
	s.Record(&ReqInfo{Status: 503, QueuedAt: base, EndedAt: base.Add(time.Second)}) // rejected, no backend
	if h := s.hours["2026-10-27T09"]["a"]; h == nil || h.BusyMs != 10*60*1000 || h.Requests != 0 {
		t.Errorf("09: %+v", h)
	}
	if h := s.hours["2026-10-27T10"]["a"]; h == nil || h.BusyMs != 10*60*1000 || h.Requests != 1 {
		t.Errorf("10: %+v", h)
	}
	q := s.qdays["2026-10-27"]
	if q == nil || q.Requests != 2 || q.Waited != 2 || q.Rejected != 1 || q.MaxWaitMs != 2000 {
		t.Errorf("queue: %+v", q)
	}
	// previous day for the average
	prev := base.AddDate(0, 0, -1)
	s.Record(&ReqInfo{Backend: "a", Status: 200, QueuedAt: prev, StartedAt: prev, EndedAt: prev.Add(5 * time.Minute)})
	v := s.Day(base.Add(time.Hour), 7, map[string]int{"a": 1, "b": 1})
	if v.AvgDays != 1 || len(v.Backends) != 2 || v.Today["a"][9].BusyMs != 600000 || v.Avg["a"][9].BusyMs != 300000 {
		t.Errorf("day view: avg_days=%d backends=%v today=%+v avg=%+v", v.AvgDays, v.Backends, v.Today["a"][9], v.Avg["a"][9])
	}
	if v.QueueToday[9].Requests != 2 {
		t.Errorf("queue today: %+v", v.QueueToday[9])
	}
	s.Flush()
	again := NewStats(s.path[:len(s.path)-len("/stats.json")], 0)
	if again.hours["2026-10-27T10"]["a"] == nil || again.qdays["2026-10-27"] == nil {
		t.Error("hourly stats not persisted")
	}
	rep := again.Report(1, base)
	if rep.QueueTotal.Requests != 2 || rep.QueueData["2026-10-27"] == nil {
		t.Errorf("report queue: %+v", rep.QueueTotal)
	}
}

func TestDayViewEndpoint(t *testing.T) {
	a := newFakeLLM(t, "a", 50*time.Millisecond)
	srv, _ := setup(t, nil, a)
	post(t, srv.URL)
	var v DayView
	waitFor(t, func() bool {
		resp, err := http.Get(srv.URL + "/balancer/api/stats/day?days=7")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		json.NewDecoder(resp.Body).Decode(&v)
		return v.Today["a"] != nil && v.Today["a"][time.Now().Hour()].Requests == 1
	})
	if v.Slots["a"] != 1 || len(v.QueueToday) != 24 {
		t.Errorf("day view: %+v", v)
	}
}
