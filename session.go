package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// Session detection: which requests belong to the same conversation / agent run.
//
// 1. an explicit ID sent by the client (header or body field), if there is one;
// 2. otherwise a fingerprint of the conversation start: system prompt + first user message.
//    Agents resend the whole history with every turn, so all turns of one conversation share it.
//
// The result is a short hash (8 hex chars) – raw IDs (often user names or e-mails) are not shown.

var sessionHeaders = []string{"X-Session-Id", "Session-Id", "X-Conversation-Id", "X-Thread-Id", "X-Chat-Id", "X-Litellm-Session-Id"}

// body paths, first match wins
var sessionFields = [][]string{
	{"session_id"}, {"sessionId"}, {"conversation_id"}, {"thread_id"}, {"chat_id"},
	{"metadata", "session_id"}, {"metadata", "conversation_id"}, {"metadata", "thread_id"}, {"metadata", "user_id"},
	{"litellm_session_id"}, {"prompt_cache_key"}, {"user"},
}

func detectSession(r *http.Request, body []byte, token string) (id, src string) {
	raw := ""
	for _, h := range sessionHeaders {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			raw, src = v, "Header "+h
			break
		}
	}
	var doc map[string]any
	if raw == "" && len(body) > 0 && body[0] == '{' {
		if json.Unmarshal(body, &doc) == nil {
		fields:
			for _, path := range sessionFields {
				var cur any = doc
				for _, k := range path {
					m, ok := cur.(map[string]any)
					if !ok {
						continue fields
					}
					cur = m[k]
				}
				switch v := cur.(type) {
				case string:
					if v = strings.TrimSpace(v); v != "" {
						raw, src = v, "Feld "+strings.Join(path, ".")
						break fields
					}
				case float64:
					raw, src = jsonNum(v), "Feld "+strings.Join(path, ".")
					break fields
				}
			}
			if raw == "" {
				if fp := conversationFingerprint(doc); fp != "" {
					raw, src = fp, "Fingerprint (System-Prompt + erste Nachricht)"
				}
			}
		}
	}
	if raw == "" {
		return "", ""
	}
	sum := sha256.Sum256([]byte(token + "\x00" + raw))
	return hex.EncodeToString(sum[:4]), src
}

func jsonNum(f float64) string { b, _ := json.Marshal(f); return string(b) }

// conversationFingerprint covers OpenAI/Ollama chat (messages with system role), Anthropic (top-level
// "system") and plain completions (prompt / Ollama generate + system).
func conversationFingerprint(doc map[string]any) string {
	var sys, first string
	sys = contentText(doc["system"])
	if msgs, ok := doc["messages"].([]any); ok {
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			role, _ := mm["role"].(string)
			switch role {
			case "system", "developer":
				if sys == "" {
					sys = contentText(mm["content"])
				}
			case "user":
				first = contentText(mm["content"])
			}
			if first != "" {
				break
			}
		}
	} else if msgs, ok := doc["input"].([]any); ok { // OpenAI Responses API
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok && mm["role"] == "user" {
				first = contentText(mm["content"])
				break
			}
		}
		if s, ok := doc["instructions"].(string); ok && sys == "" {
			sys = s
		}
	}
	if sys == "" && first == "" {
		return ""
	}
	return "fp\x00" + sys + "\x00" + first
}

func contentText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any: // [{type:"text", text:"…"}, …]
		var sb strings.Builder
		for _, part := range c {
			if p, ok := part.(map[string]any); ok {
				if t, ok := p["text"].(string); ok {
					sb.WriteString(t)
					sb.WriteByte('\n')
				}
			}
		}
		return sb.String()
	}
	return ""
}
