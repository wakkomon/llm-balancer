package main

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Usage is the number of LLM tokens a request consumed, as reported by the server.
type Usage struct {
	In    int64 // prompt / input tokens
	Out   int64 // completion / output tokens
	Found bool
}

// usageScanner watches a response as it streams through and extracts token counts.
// It understands OpenAI-style JSON and SSE ("usage" object, also the final chunk of a
// stream with stream_options.include_usage), Anthropic (input_tokens / output_tokens),
// Ollama native NDJSON (prompt_eval_count / eval_count) and llama.cpp "timings".
type usageScanner struct {
	stream bool   // SSE or NDJSON: inspect line by line
	line   []byte // current incomplete line
	body   []byte // non-stream body (bounded)
	u      Usage
}

const maxUsageBody = 8 << 20

func newUsageScanner(contentType string) *usageScanner {
	ct := strings.ToLower(contentType)
	return &usageScanner{stream: strings.Contains(ct, "event-stream") || strings.Contains(ct, "ndjson")}
}

func (s *usageScanner) Write(p []byte) {
	if !s.stream {
		if len(s.body)+len(p) <= maxUsageBody {
			s.body = append(s.body, p...)
		} else {
			// Not a plain JSON body after all (or huge) – fall back to line mode on the rest.
			s.stream = true
			s.body = nil
		}
		if !s.stream {
			return
		}
	}
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if len(s.line)+len(p) <= 1<<20 { // ignore absurdly long lines
				s.line = append(s.line, p...)
			}
			return
		}
		s.line = append(s.line, p[:i]...)
		s.handleLine(s.line)
		s.line = s.line[:0]
		p = p[i+1:]
	}
}

func (s *usageScanner) handleLine(l []byte) {
	l = bytes.TrimSpace(l)
	l = bytes.TrimPrefix(l, []byte("data:"))
	l = bytes.TrimSpace(l)
	if len(l) == 0 || l[0] != '{' {
		return
	}
	if !bytes.Contains(l, []byte(`"usage"`)) && !bytes.Contains(l, []byte(`eval_count"`)) &&
		!bytes.Contains(l, []byte(`"timings"`)) {
		return
	}
	s.parse(l)
}

func (s *usageScanner) Finish() Usage {
	if s.stream {
		if len(s.line) > 0 {
			s.handleLine(s.line)
		}
	} else if len(s.body) > 0 {
		s.parse(s.body)
		if !s.u.Found { // e.g. NDJSON served as application/json
			s.stream = true
			s.Write(s.body)
			s.Finish()
		}
	}
	return s.u
}

type usageFields struct {
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
	InputTokens      *int64 `json:"input_tokens"`
	OutputTokens     *int64 `json:"output_tokens"`
}

func (s *usageScanner) parse(b []byte) {
	var m struct {
		Usage           *usageFields `json:"usage"`
		PromptEvalCount *int64       `json:"prompt_eval_count"`
		EvalCount       *int64       `json:"eval_count"`
		Timings         *struct {
			PromptN    *int64 `json:"prompt_n"`
			PredictedN *int64 `json:"predicted_n"`
		} `json:"timings"`
		Message *struct { // Anthropic stream: message_start carries the usage inside "message"
			Usage *usageFields `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(b, &m) != nil {
		return
	}
	take := func(dst *int64, v *int64) {
		if v != nil && *v > *dst { // max: Anthropic splits in/out over several events
			*dst = *v
			s.u.Found = true
		}
	}
	for _, u := range []*usageFields{m.Usage, msgUsage(m.Message)} {
		if u == nil {
			continue
		}
		take(&s.u.In, u.PromptTokens)
		take(&s.u.In, u.InputTokens)
		take(&s.u.Out, u.CompletionTokens)
		take(&s.u.Out, u.OutputTokens)
	}
	take(&s.u.In, m.PromptEvalCount)
	take(&s.u.Out, m.EvalCount)
	if m.Usage == nil && m.Timings != nil { // llama.cpp without a usage object
		take(&s.u.In, m.Timings.PromptN)
		take(&s.u.Out, m.Timings.PredictedN)
	}
}

func msgUsage(m *struct {
	Usage *usageFields `json:"usage"`
}) *usageFields {
	if m == nil {
		return nil
	}
	return m.Usage
}

// withStreamUsage adds stream_options.include_usage=true to an OpenAI-style streaming request
// that doesn't ask for it, so the final chunk reports token counts. Other fields stay untouched.
func withStreamUsage(body []byte) ([]byte, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body, false
	}
	if _, ok := m["stream_options"]; ok {
		return body, false
	}
	m["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	out, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return out, true
}
