package main

import (
	"encoding/json"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Session details for the popup in "Letzte Anfragen": where the requests came from and what the
// conversation is about – kept in memory only (the last sessions), never written to disk.

const maxSessionInfos = 500

type SessionInfo struct {
	ID         string            `json:"id"`
	Source     string            `json:"source"`                // how it was detected
	ExplicitID string            `json:"explicit_id,omitempty"` // ID sent by the client
	First      time.Time         `json:"first"`
	Last       time.Time         `json:"last"`
	Requests   int               `json:"requests"`
	Tokens     []string          `json:"tokens"`  // API token names
	Clients    []string          `json:"clients"` // TCP peer addresses
	Forwarded  []string          `json:"forwarded,omitempty"`
	UserAgents []string          `json:"user_agents,omitempty"`
	Models     []string          `json:"models"`
	Headers    map[string]string `json:"headers,omitempty"` // identifying headers (title, referer, user …)
	Fields     map[string]string `json:"fields,omitempty"`  // identifying body fields (user, metadata …)
	Title      string            `json:"title"`             // best guess: what the session is about
	System     string            `json:"system,omitempty"`  // start of the system prompt
	FirstMsg   string            `json:"first_message,omitempty"`
	LastMsg    string            `json:"last_message,omitempty"`      // latest message of the conversation (any role)
	LastRole   string            `json:"last_role,omitempty"`         // user | assistant | tool
	LastUser   string            `json:"last_user_message,omitempty"` // latest user message
	LastAnswer string            `json:"last_answer,omitempty"`       // start of the latest generated answer
	Turns      int               `json:"turns"`                       // user turns in the latest request
	Linked     []string          `json:"linked,omitempty"`            // later fingerprints joined after context compression
}

type sessionStore struct {
	mu    sync.Mutex
	m     map[string]*SessionInfo
	keys  []string             // oldest first
	links map[string]*sessLink // session → recent message keys (compression linking)
	alias map[string]string    // later fingerprint → session it continues
}

var identHeaders = []string{"X-Title", "Http-Referer", "Referer", "Origin", "X-Client-Name", "X-Client-Id", "X-App", "X-Application",
	"X-Openwebui-User-Name", "X-Openwebui-User-Email", "X-Openwebui-Chat-Id", "X-Openwebui-User-Id", "X-User", "X-User-Id",
	"X-Session-Id", "Session-Id", "X-Conversation-Id", "X-Thread-Id", "X-Chat-Id", "X-Litellm-Session-Id", "X-Request-Id"}

func addUniq(list []string, v string, max int) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return list
	}
	for _, x := range list {
		if x == v {
			return list
		}
	}
	if len(list) >= max {
		return list
	}
	return append(list, v)
}

func peerAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// guessTitle: a short label for the conversation – an explicit title field if the client sent one,
// else the first user message (first line, shortened).
func guessTitle(doc map[string]any, first string) string {
	for _, path := range [][]string{{"title"}, {"metadata", "title"}, {"metadata", "chat_title"}, {"metadata", "conversation_title"}, {"metadata", "session_name"}} {
		var cur any = doc
		for _, k := range path {
			m, _ := cur.(map[string]any)
			cur = m[k]
		}
		if s, ok := cur.(string); ok && strings.TrimSpace(s) != "" {
			return clip(strings.TrimSpace(s), 120)
		}
	}
	line := strings.TrimSpace(first)
	if i := strings.IndexAny(line, "\r\n"); i > 0 {
		line = line[:i]
	}
	return clip(line, 120)
}

func (s *sessionStore) note(id, src, explicit, linkedFrom string, r *http.Request, body []byte, token, model string) {
	if id == "" {
		return
	}
	var doc map[string]any
	_ = json.Unmarshal(body, &doc)
	turns := conversationTurns(body)
	var firstUser string
	users := 0
	for _, t := range turns {
		if t.Role == "user" {
			if firstUser == "" {
				firstUser = t.Content
			}
			users++
		}
	}
	sys := contentText(doc["system"])
	if msgs, ok := doc["messages"].([]any); ok && sys == "" {
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok && (mm["role"] == "system" || mm["role"] == "developer") {
				sys = contentText(mm["content"])
				break
			}
		}
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]*SessionInfo{}
	}
	si := s.m[id]
	if si == nil {
		si = &SessionInfo{ID: id, Source: src, ExplicitID: clip(explicit, 200), First: now, Headers: map[string]string{}, Fields: map[string]string{}}
		s.m[id] = si
		s.keys = append(s.keys, id)
		if len(s.keys) > maxSessionInfos {
			delete(s.m, s.keys[0])
			s.keys = s.keys[1:]
		}
	}
	si.Last = now
	si.Requests++
	if linkedFrom != "" {
		si.Linked = addUniq(si.Linked, linkedFrom, 20)
	}
	si.Tokens = addUniq(si.Tokens, token, 5)
	si.Clients = addUniq(si.Clients, peerAddr(r), 10)
	si.Forwarded = addUniq(si.Forwarded, r.Header.Get("X-Forwarded-For"), 10)
	si.Forwarded = addUniq(si.Forwarded, r.Header.Get("X-Real-Ip"), 10)
	si.UserAgents = addUniq(si.UserAgents, clip(r.Header.Get("User-Agent"), 160), 5)
	si.Models = addUniq(si.Models, model, 10)
	for _, h := range identHeaders {
		if v := r.Header.Get(h); v != "" {
			si.Headers[h] = clip(v, 200)
		}
	}
	for _, k := range []string{"user", "session_id", "conversation_id", "chat_id", "thread_id", "prompt_cache_key"} {
		if v, ok := doc[k]; ok && v != nil && v != "" {
			si.Fields[k] = clip(strings.Trim(jsonString(v), `"`), 200)
		}
	}
	if md, ok := doc["metadata"].(map[string]any); ok && len(md) > 0 {
		si.Fields["metadata"] = clip(jsonString(md), 400)
	}
	if first := clip(strings.TrimSpace(firstUser), 400); first != "" && si.FirstMsg == "" {
		si.FirstMsg = first
	}
	if si.Title == "" {
		si.Title = guessTitle(doc, firstUser)
	}
	if t := strings.TrimSpace(sys); t != "" && si.System == "" {
		si.System = clip(t, 500)
	}
	if last := lastUserText(turns); last != "" {
		si.LastUser = clip(strings.TrimSpace(last), 400)
	}
	if role, text := lastMessage(doc); text != "" {
		si.LastRole, si.LastMsg = role, clip(text, 400)
	}
	si.Turns = users
}

// lastMessage: role and text of the newest non-system message (agents append tool results and
// assistant turns while the user message stays the same).
func lastMessage(doc map[string]any) (string, string) {
	msgs, _ := doc["messages"].([]any)
	if msgs == nil {
		msgs, _ = doc["input"].([]any)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		m, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role == "system" || role == "developer" {
			continue
		}
		text := strings.TrimSpace(systemReminderRe.ReplaceAllString(contentText(m["content"]), ""))
		if text == "" {
			if tc, ok := m["tool_calls"].([]any); ok && len(tc) > 0 { // assistant turn that only calls tools
				var names []string
				for _, c := range tc {
					if cm, ok := c.(map[string]any); ok {
						if f, ok := cm["function"].(map[string]any); ok {
							if n, ok := f["name"].(string); ok {
								names = append(names, n)
							}
						}
					}
				}
				text = "→ Tool-Aufruf: " + strings.Join(names, ", ")
			} else if m["type"] == "function_call_output" {
				text = clip(jsonString(m["output"]), 400)
			}
		}
		if text != "" {
			if role == "" {
				role, _ = m["type"].(string)
			}
			return role, text
		}
	}
	return "", ""
}

func jsonString(v any) string { b, _ := json.Marshal(v); return string(b) }

func (s *sessionStore) get(id string) *SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	si := s.m[id]
	if si == nil {
		return nil
	}
	c := *si
	c.Headers, c.Fields = map[string]string{}, map[string]string{}
	for k, v := range si.Headers {
		c.Headers[k] = v
	}
	for k, v := range si.Fields {
		c.Fields[k] = v
	}
	sort.Strings(c.Tokens)
	return &c
}

func registerSessions(mux *http.ServeMux, pre string, px *Proxy) {
	mux.HandleFunc("GET "+pre+"/api/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		si := px.sessions.get(r.PathValue("id"))
		if si == nil {
			writeAPIError(w, http.StatusNotFound, errSessionGone)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(si)
	})
}

var errSessionGone = errorString("keine Details mehr zu dieser Session (nur die letzten 500 werden im Speicher gehalten, nicht über Neustarts)")

type errorString string

func (e errorString) Error() string { return string(e) }
