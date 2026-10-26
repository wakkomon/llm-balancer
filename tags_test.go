package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func checkTags(spec string, complete bool, pieces ...string) []string {
	t := newTagChecker(spec)
	for _, p := range pieces {
		t.Feed(p)
	}
	return t.Finish(complete)
}

func TestTagChecker(t *testing.T) {
	cases := []struct {
		name   string
		pieces []string
		want   string // substring of the joined issues, "" = no issues
	}{
		{"balanced", []string{"<think>hmm</think>Antwort"}, ""},
		{"split across chunks", []string{"<thi", "nk>a</th", "ink> b <tool_", "call>{}</tool_call>"}, ""},
		{"implicit open by template", []string{"reasoning…</think>\n\nAntwort"}, ""},
		{"not closed", []string{"<think>endlos"}, "<think> nicht geschlossen"},
		{"close without open", []string{"<tool_call>{}</tool_call></tool_call>"}, "</tool_call> ohne öffnendes"},
		{"wrong order", []string{"<think><tool_call>x</think></tool_call>"}, "</think> schließt, aber <tool_call> ist noch offen"},
		{"opened twice", []string{"<think>a<think>b</think></think>"}, "<think> erneut geöffnet"},
		{"other html ignored", []string{"<div><br>a < b and x<y></div>"}, ""},
		{"code like a<b>c ignored", []string{"vec<int> v; <think>ok</think>"}, ""},
	}
	for _, c := range cases {
		got := strings.Join(checkTags(DefaultTagCheck, true, c.pieces...), " | ")
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	// "*" checks every tag
	if got := checkTags("*", true, "<div><b>x</div>"); len(got) == 0 {
		t.Error("* should report <b>")
	}
	// truncated stream: unclosed tags are not reported (the stream itself failed)
	if got := checkTags(DefaultTagCheck, false, "<think>abc"); len(got) != 0 {
		t.Errorf("incomplete: %v", got)
	}
	if newTagChecker("off") != nil {
		t.Error("off should disable")
	}
}

func TestScannerFindsTagIssuesInStream(t *testing.T) {
	sse := func(content string) string {
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": content}}}})
		return "data: " + string(b) + "\n\n"
	}
	s := newUsageScanner("text/event-stream")
	s.tags = newTagChecker(DefaultTagCheck)
	chunks := 0
	s.onContent = func() { chunks++ }
	for _, c := range []string{sse("<tool"), sse("_call>{\"a\":1}"), sse(" fertig"),
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", "data: [DONE]\n\n"} {
		s.Write([]byte(c))
	}
	s.Finish()
	issues := s.TagIssues(true)
	if len(issues) != 1 || !strings.Contains(issues[0], "<tool_call> nicht geschlossen") || chunks != 3 {
		t.Errorf("issues=%v chunks=%d", issues, chunks)
	}
	// Ollama native and Anthropic
	o := newUsageScanner("application/x-ndjson")
	o.tags = newTagChecker(DefaultTagCheck)
	o.Write([]byte("{\"message\":{\"content\":\"<think>x\"},\"done\":false}\n{\"message\":{\"content\":\"</think>ok\"},\"done\":true}\n"))
	o.Finish()
	if got := o.TagIssues(true); len(got) != 0 {
		t.Errorf("ollama: %v", got)
	}
	a := newUsageScanner("application/json")
	a.tags = newTagChecker(DefaultTagCheck)
	a.Write([]byte(`{"type":"message","content":[{"type":"text","text":"</answer> oops"}],"usage":{"input_tokens":3,"output_tokens":4}}`))
	if u := a.Finish(); u.Out != 4 || len(a.TagIssues(true)) != 1 {
		t.Errorf("anthropic: %+v %v", u, a.TagIssues(true))
	}
}

func TestTokensPerSecondAndTagErrorsInStatus(t *testing.T) {
	// streams 10 chunks over ~1s after 300ms of "prompt processing", then usage and a broken tag
	srv := newSSEServer(t, 300*time.Millisecond, 10, 100*time.Millisecond, "<think>never closed", 2000, 10)
	bal, _ := setup(t, func(c *Config) { c.Backends = []BackendConfig{{Name: "s", URL: srv.URL}} })
	go chatStream(t, bal.URL)
	time.Sleep(800 * time.Millisecond)
	st := status(t, bal.URL)
	if r := st.Backends[0].Running; len(r) != 1 || r[0].LiveTPS < 5 || r[0].LiveTPS > 15 {
		t.Errorf("live tok/s: %+v", r)
	}
	time.Sleep(900 * time.Millisecond)
	st = status(t, bal.URL)
	b := st.Backends[0]
	if b.AvgTPS < 7 || b.AvgTPS > 13 || b.PromptTPS < 4000 || b.PromptTPS > 8000 || b.TagErrors != 1 {
		t.Errorf("backend: tps=%.1f prompt=%.0f tagerr=%d", b.AvgTPS, b.PromptTPS, b.TagErrors)
	}
	if r := st.Recent[0]; len(r.TagIssues) != 1 || r.GenTPS == 0 {
		t.Errorf("recent: %+v", r)
	}
}

func newSSEServer(t *testing.T, prefill time.Duration, n int, every time.Duration, first string, promptTok, outTok int) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		time.Sleep(prefill)
		for i := 0; i < n; i++ {
			c := "x"
			if i == 0 {
				c = first
			}
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": c}}}})
			fmt.Fprintf(w, "data: %s\n\n", b)
			w.(http.Flusher).Flush()
			time.Sleep(every)
		}
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":%d}}\n\ndata: [DONE]\n\n", promptTok, outTok)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func chatStream(t *testing.T, url string) {
	resp, err := http.Post(url+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m","stream":true}`))
	if err != nil {
		t.Error(err)
		return
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
}
