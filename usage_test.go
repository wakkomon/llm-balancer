package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func scanAll(ct string, chunks ...string) Usage {
	s := newUsageScanner(ct)
	for _, c := range chunks {
		s.Write([]byte(c))
	}
	return s.Finish()
}

func TestUsageScanner(t *testing.T) {
	cases := []struct {
		name    string
		ct      string
		chunks  []string
		in, out int64
	}{
		{"openai json", "application/json",
			[]string{`{"id":"x","choices":[],"usage":{"prompt_tokens":12,`, `"completion_tokens":34,"total_tokens":46}}`}, 12, 34},
		{"openai sse final chunk", "text/event-stream",
			[]string{"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n", "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"comp",
				"letion_tokens\":9}}\n\ndata: [DONE]\n\n"}, 7, 9},
		{"anthropic sse", "text/event-stream; charset=utf-8",
			[]string{"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":25,\"output_tokens\":1}}}\n\n",
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":15}}\n\n"}, 25, 15},
		{"ollama ndjson", "application/x-ndjson",
			[]string{"{\"message\":{\"content\":\"a\"},\"done\":false}\n", "{\"done\":true,\"prompt_eval_count\":30,\"eval_count\":40}\n"}, 30, 40},
		{"llama.cpp timings", "text/event-stream",
			[]string{"data: {\"choices\":[],\"timings\":{\"prompt_n\":5,\"predicted_n\":6}}\n\n"}, 5, 6},
		{"no usage", "text/event-stream", []string{"data: {\"choices\":[]}\n\ndata: [DONE]\n\n"}, 0, 0},
	}
	for _, c := range cases {
		u := scanAll(c.ct, c.chunks...)
		if u.In != c.in || u.Out != c.out || u.Found != (c.in+c.out > 0) {
			t.Errorf("%s: got %+v", c.name, u)
		}
	}
}

func TestWithStreamUsage(t *testing.T) {
	out, ok := withStreamUsage([]byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}]}`))
	var m map[string]any
	json.Unmarshal(out, &m)
	if !ok || m["stream_options"].(map[string]any)["include_usage"] != true || m["model"] != "m" {
		t.Errorf("not added: %s", out)
	}
	orig := `{"stream":true,"stream_options":{"include_usage":false}}`
	if out, ok := withStreamUsage([]byte(orig)); ok || string(out) != orig {
		t.Errorf("existing stream_options must stay: %s", out)
	}
	if !strings.Contains(string(out), "messages") {
		t.Error("other fields lost")
	}
}
