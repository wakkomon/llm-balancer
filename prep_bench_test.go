package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func agentBody(n int) []byte {
	msgs := []map[string]any{{"role": "system", "content": strings.Repeat("Du bist Hermes, ein hilfreicher Agent mit Werkzeugen. ", 300)}}
	msgs = append(msgs, map[string]any{"role": "user", "content": "Bitte prüfe die Backups auf dem NAS und fasse zusammen."})
	for i := 0; i < n; i++ {
		msgs = append(msgs, map[string]any{"role": "assistant", "content": "<think>" + strings.Repeat("überlege ", 80) + "</think>Ich rufe das Tool auf.",
			"tool_calls": []any{map[string]any{"id": fmt.Sprint(i), "type": "function", "function": map[string]any{"name": "shell", "arguments": `{"cmd":"ls -la /mnt"}`}}}})
		msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": fmt.Sprint(i), "content": strings.Repeat(fmt.Sprintf("drwxr-xr-x 2 root root 4096 Nov  3 10:%02d datei-%d\n", i%60, i), 60)})
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": "<system-reminder>Kontext</system-reminder> weiter"})
	b, _ := json.Marshal(map[string]any{"model": "auto", "stream": true, "messages": msgs})
	return b
}

func BenchmarkRequestPrep(b *testing.B) {
	body := agentBody(150)
	b.Logf("body %d KB", len(body)/1024)
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	var ss sessionStore
	steps := map[string]func(){
		"unmarshal-map":   func() { var m map[string]any; json.Unmarshal(body, &m) },
		"detectSession":   func() { detectSessionRaw(r, body, "t") },
		"resolve":         func() { ss.resolve("abcd", "Fingerprint", "t", body) },
		"note":            func() { ss.note("abcd", "Fingerprint", "", "", r, body, "t", "m") },
		"analyzeRequest":  func() { analyzeRequest(body) },
		"withStreamUsage": func() { withStreamUsage(body) },
		"withModel":       func() { withModel(body, "qwen3:8b") },
		"pipeline": func() {
			doc := parseDoc(body)
			detectSessionDoc(r, doc, "t")
			ss.resolveDoc("abcd", "Fingerprint", "t", doc)
			ss.noteDoc("abcd", "Fingerprint", "", "", r, doc, "t", "m")
			analyzeDoc(doc)
			withModel(withStreamUsage2(body), "qwen3:8b")
		},
	}
	for name, f := range steps {
		b.Run(name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				f()
			}
		})
	}
}

func withStreamUsage2(b []byte) []byte { o, _ := withStreamUsage(b); return o }
