package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHeuristicTier(t *testing.T) {
	cases := map[string]string{
		"hallo":        "SIMPLE",
		"Was ist DNS?": "SIMPLE",
		"Schreib mir eine Python-Funktion mit einer Klasse, die die Datenbank per SQL abfragt und das Ergebnis als JSON zurückgibt": "MEDIUM", // code alone: 0.30 < 0.35 (as in LiteLLM)
		"Analysiere Schritt für Schritt die Vor- und Nachteile beider Architekturen":                                                "REASONING",
	}
	for text, want := range cases {
		if h := heuristicTier(text, nil); h.Tier != want {
			t.Errorf("%q → %s (%.2f %v), want %s", text, h.Tier, h.Score, h.Parts, want)
		}
	}
	turns := conversationTurns([]byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"erste"},{"role":"assistant","content":"a"},
		{"role":"user","content":"<system-reminder>x</system-reminder>zweite"},{"role":"tool","content":"t"}]}`))
	if len(turns) != 3 || lastUserText(turns) != "zweite" {
		t.Errorf("turns: %+v", turns)
	}
}

// fakeClassifier answers like a small model: Arch-Router routes or a tier, depending on the prompt.
func newFakeClassifier(t *testing.T, delay time.Duration) (*httptest.Server, *atomic.Int32) {
	calls := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `{"data":[{"id":"arch-router:1.5b"},{"id":"qwen3:0.6b"}]}`)
			return
		}
		calls.Add(1)
		time.Sleep(delay)
		var body struct {
			Messages []turn `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		all := ""
		for _, m := range body.Messages {
			all += m.Content
		}
		ans := `{"tier": "MEDIUM"}`
		switch {
		case strings.Contains(all, "<routes>") && strings.Contains(all, "Python"):
			ans = `{"route": "code_generation"}`
		case strings.Contains(all, "<routes>"):
			ans = `{"route": "other"}`
		case strings.Contains(all, "Beweis"):
			ans = `{"tier": "REASONING"}`
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": ans}}}})
	}))
	t.Cleanup(srv.Close)
	return srv, calls
}

func TestArchAndLLMClassifier(t *testing.T) {
	cls, calls := newFakeClassifier(t, 0)
	small := newModelLLM(t, "small", 10*time.Millisecond, "qwen3:8b")
	coder := newModelLLM(t, "coder", 10*time.Millisecond, "qwen3-coder:30b")
	big := newModelLLM(t, "big", 10*time.Millisecond, "qwen3:32b")
	srv, pool := setup(t, func(c *Config) {
		c.Backends = []BackendConfig{{Name: "cls", URL: cls.URL}, {Name: "small", URL: small.srv.URL}, {Name: "coder", URL: coder.srv.URL}, {Name: "big", URL: big.srv.URL}}
		c.Routers = []RouterConfig{
			{Name: "arch", Default: "qwen3:8b", Classifier: "arch", ClassModel: "arch-router:1.5b", Routes: []RouteDef{
				{Name: "code_generation", Description: "Code schreiben", Models: []string{"qwen3-coder:30b"}},
				{Name: "general", Description: "allgemeine Fragen", Models: []string{"qwen3:8b"}}}},
			{Name: "smart", Classifier: "llm", ClassModel: "qwen3:0.6b", Default: "qwen3:8b",
				Tiers: map[string][]string{"SIMPLE": {"qwen3:8b"}, "MEDIUM": {"qwen3:8b"}, "REASONING": {"qwen3:32b"}},
				Rules: []RouterRule{{Name: "Kubernetes", Pattern: "kubernetes", Tier: "COMPLEX"}}},
			{Name: "heur", Classifier: "heuristic", Tiers: map[string][]string{"SIMPLE": {"qwen3:8b"}, "COMPLEX": {"qwen3-coder:30b"}}},
		}
	})
	discover(t, pool)
	ask := func(model, text string) (string, string) {
		b, _ := json.Marshal(map[string]any{"model": model, "messages": []map[string]any{{"role": "user", "content": text}}})
		return chatBodyJSON(t, srv.URL, string(b), nil)
	}
	for _, c := range []struct{ model, text, backend string }{
		{"arch", "Schreib ein Python-Skript", "coder"},
		{"arch", "Wie wird das Wetter?", "small"}, // "other" → default
		{"smart", "Ein Beweis, dass √2 irrational ist", "big"},
		{"smart", "Erklär mir DNS", "small"},
		{"smart", "Kubernetes-Cluster planen", "big"}, // rule → tier COMPLEX (empty) → escalates to REASONING
		{"heur", "hallo", "small"},
		{"heur", "Schreib mir eine Python-Funktion mit einer Klasse, die die Datenbank per SQL abfragt und JSON liefert", "coder"},
	} {
		if be, _ := ask(c.model, c.text); be != c.backend {
			t.Errorf("%s %q → %s, want %s", c.model, c.text, be, c.backend)
		}
	}
	if n := calls.Load(); n != 4 { // arch ×2, smart ×2 (the rule needs no call)
		t.Errorf("classifier calls: %d", n)
	}
	waitFor(t, func() bool { return len(status(t, srv.URL).Recent) == 7 })
	rec := status(t, srv.URL).Recent
	if !strings.Contains(rec[len(rec)-1].Router, "arch-router:1.5b auf cls: code_generation") {
		t.Errorf("router note: %q", rec[len(rec)-1].Router)
	}
	// classifier down → fallback
	cls.Close()
	pool.mu.Lock()
	pool.routers[1].ClassFallback = "heuristic"
	pool.mu.Unlock()
	if be, _ := ask("smart", "hallo"); be != "small" {
		t.Errorf("fallback heuristic: %s", be)
	}
	if be, _ := ask("arch", "Schreib ein Python-Skript"); be != "small" {
		t.Errorf("fallback default: %s", be)
	}
}

func TestClassifierTimeout(t *testing.T) {
	cls, _ := newFakeClassifier(t, 2*time.Second)
	small := newModelLLM(t, "small", 10*time.Millisecond, "qwen3:8b")
	srv, pool := setup(t, func(c *Config) {
		c.Backends = []BackendConfig{{Name: "cls", URL: cls.URL}, {Name: "small", URL: small.srv.URL}}
		c.Routers = []RouterConfig{{Name: "arch", Default: "qwen3:8b", Classifier: "arch", ClassModel: "arch-router:1.5b", ClassTimeoutMs: 200,
			Routes: []RouteDef{{Name: "x", Description: "x", Models: []string{"qwen3:8b"}}}}}
	})
	discover(t, pool)
	start := time.Now()
	if be, _ := chatBodyJSON(t, srv.URL, `{"model":"arch","messages":[{"role":"user","content":"hi"}]}`, nil); be != "small" || time.Since(start) > time.Second {
		t.Errorf("timeout: %s after %v", be, time.Since(start))
	}
}
