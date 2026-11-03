package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// MCP interface (Model Context Protocol, "Streamable HTTP" transport, JSON responses).
//
//   POST /mcp   JSON-RPC 2.0 – initialize, ping, tools/list, tools/call (+ empty resources/prompts lists)
//
// Access only with an MCP token (Authorization: Bearer …). MCP tokens are separate from the API tokens for
// the LLM API; each has a role like the web users: viewer (read), operator (+ switch servers/groups on/off,
// stop requests, unload models), admin (+ download / delete Ollama models).

const mcpPath = "/mcp"

var mcpVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// ---- tokens (DATA_DIR/mcp.json, only SHA-256 hashes) ----

type MCPToken struct {
	Name     string    `json:"name"`
	Hash     string    `json:"hash,omitempty"`
	Hint     string    `json:"hint"`
	Role     string    `json:"role"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used,omitempty"`
	Calls    int64     `json:"calls"`
	Disabled bool      `json:"disabled,omitempty"`
}

type MCPTokens struct {
	mu    sync.Mutex
	path  string
	list  []*MCPToken
	saved time.Time
}

func NewMCPTokens(dir string) *MCPTokens {
	m := &MCPTokens{}
	if dir == "" {
		return m
	}
	m.path = filepath.Join(dir, "mcp.json")
	if data, err := os.ReadFile(m.path); err == nil {
		if err := json.Unmarshal(data, &m.list); err != nil {
			log.Printf("mcp: %s unreadable: %v", m.path, err)
			m.list = nil
		}
	}
	return m
}

func (m *MCPTokens) saveLocked() error {
	m.saved = time.Now()
	if m.path == "" {
		return nil
	}
	if m.list == nil {
		m.list = []*MCPToken{}
	}
	data, _ := json.MarshalIndent(m.list, "", "  ")
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(m.path, data, 0o600)
}

func newMCPTokenValue() string {
	b := make([]byte, 24)
	rand.Read(b)
	return "mcp-" + hex.EncodeToString(b)
}

func (m *MCPTokens) findLocked(name string) *MCPToken {
	for _, t := range m.list {
		if strings.EqualFold(t.Name, name) {
			return t
		}
	}
	return nil
}

// Check returns a copy of the token for a valid, enabled value.
func (m *MCPTokens) Check(value string) *MCPToken {
	if value == "" {
		return nil
	}
	h := hashToken(value)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.list {
		if subtle.ConstantTimeCompare([]byte(t.Hash), []byte(h)) == 1 {
			if t.Disabled {
				return nil
			}
			t.Calls++
			t.LastUsed = time.Now()
			if time.Since(m.saved) > time.Minute {
				m.saveLocked()
			}
			c := *t
			return &c
		}
	}
	return nil
}

func validMCPName(n string) error {
	if n == "" || len(n) > 64 || strings.ContainsAny(n, "/\\?#%\"<>") {
		return errors.New("Name fehlt oder enthält ungültige Zeichen")
	}
	return nil
}

func (m *MCPTokens) Create(name, role, value string) (string, error) {
	name = strings.TrimSpace(name)
	if err := validMCPName(name); err != nil {
		return "", httpErr{400, err}
	}
	if role == "" {
		role = RoleViewer
	}
	if !validRole(role) {
		return "", httpErr{400, errors.New("unbekannte Rolle")}
	}
	if value == "" {
		value = newMCPTokenValue()
	} else if len(value) < 16 {
		return "", httpErr{400, errors.New("Token muss mindestens 16 Zeichen haben")}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.findLocked(name) != nil {
		return "", httpErr{409, errors.New("Name existiert bereits")}
	}
	h := hashToken(value)
	for _, t := range m.list {
		if t.Hash == h {
			return "", httpErr{409, errors.New("Token-Wert wird schon verwendet")}
		}
	}
	m.list = append(m.list, &MCPToken{Name: name, Hash: h, Hint: hint(value), Role: role, Created: time.Now()})
	log.Printf("mcp token %s created (role %s)", name, role)
	return value, m.saveLocked()
}

type mcpTokenUpdate struct {
	Name     *string `json:"name"`
	Role     *string `json:"role"`
	Disabled *bool   `json:"disabled"`
	Renew    bool    `json:"renew"`
}

// Update returns the new token value if it was renewed.
func (m *MCPTokens) Update(name string, u mcpTokenUpdate) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.findLocked(name)
	if t == nil {
		return "", httpErr{404, errors.New("unbekanntes MCP-Token")}
	}
	if u.Name != nil {
		n := strings.TrimSpace(*u.Name)
		if err := validMCPName(n); err != nil {
			return "", httpErr{400, err}
		}
		if o := m.findLocked(n); o != nil && o != t {
			return "", httpErr{409, errors.New("Name existiert bereits")}
		}
		t.Name = n
	}
	if u.Role != nil {
		if !validRole(*u.Role) {
			return "", httpErr{400, errors.New("unbekannte Rolle")}
		}
		t.Role = *u.Role
	}
	if u.Disabled != nil {
		t.Disabled = *u.Disabled
	}
	val := ""
	if u.Renew {
		val = newMCPTokenValue()
		t.Hash, t.Hint = hashToken(val), hint(val)
	}
	return val, m.saveLocked()
}

func (m *MCPTokens) Delete(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, t := range m.list {
		if strings.EqualFold(t.Name, name) {
			m.list = append(m.list[:i], m.list[i+1:]...)
			log.Printf("mcp token %s deleted", t.Name)
			return m.saveLocked()
		}
	}
	return httpErr{404, errors.New("unbekanntes MCP-Token")}
}

func (m *MCPTokens) List(withHash bool) []MCPToken {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []MCPToken{}
	for _, t := range m.list {
		c := *t
		if !withHash {
			c.Hash = ""
		}
		out = append(out, c)
	}
	return out
}

func (m *MCPTokens) Flush() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.saved.IsZero() || len(m.list) > 0 {
		m.saveLocked()
	}
}

// importTokens replaces the MCP tokens with the imported ones; entries without hash keep their old value.
func (m *MCPTokens) importTokens(in []MCPToken, res *importResult) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []*MCPToken{}
	for _, t := range in {
		t := t
		if validMCPName(t.Name) != nil || !validRole(t.Role) {
			res.Warnings = append(res.Warnings, fmt.Sprintf("MCP-Token %q übersprungen (Name/Rolle ungültig)", t.Name))
			continue
		}
		if t.Hash == "" {
			if old := m.findLocked(t.Name); old != nil {
				t.Hash, t.Hint = old.Hash, old.Hint
			} else {
				res.Warnings = append(res.Warnings, fmt.Sprintf("MCP-Token %q ohne Geheimnis und noch nicht vorhanden – übersprungen", t.Name))
				continue
			}
		}
		out = append(out, &t)
	}
	m.list = out
	m.saveLocked()
	return len(out)
}

func registerMCPTokens(mux *http.ServeMux, pre string, m *MCPTokens, adminToken string) {
	send := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET "+pre+"/api/mcp-tokens", func(w http.ResponseWriter, r *http.Request) {
		send(w, 200, m.List(false))
	})
	mux.HandleFunc("POST "+pre+"/api/mcp-tokens", adminAuth(adminToken, func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Name, Role, Token string }
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<16)).Decode(&in); err != nil {
			writeAPIError(w, 400, err)
			return
		}
		val, err := m.Create(in.Name, in.Role, in.Token)
		if err != nil {
			writeResult(w, err)
			return
		}
		send(w, 200, map[string]string{"name": strings.TrimSpace(in.Name), "token": val})
	}))
	mux.HandleFunc("PUT "+pre+"/api/mcp-tokens/{name}", adminAuth(adminToken, func(w http.ResponseWriter, r *http.Request) {
		var u mcpTokenUpdate
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<16)).Decode(&u); err != nil {
			writeAPIError(w, 400, err)
			return
		}
		val, err := m.Update(r.PathValue("name"), u)
		if err != nil {
			writeResult(w, err)
			return
		}
		if val != "" {
			send(w, 200, map[string]string{"token": val})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("DELETE "+pre+"/api/mcp-tokens/{name}", adminAuth(adminToken, func(w http.ResponseWriter, r *http.Request) {
		writeResult(w, m.Delete(r.PathValue("name")))
	}))
}

// ---- JSON-RPC ----

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

func rpcError(id json.RawMessage, code int, msg string) rpcResp {
	if id == nil {
		id = json.RawMessage("null")
	}
	return rpcResp{JSONRPC: "2.0", ID: id, Error: &rpcErr{code, msg}}
}

func (px *Proxy) serveMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeAPIError(w, http.StatusMethodNotAllowed, errors.New("MCP: nur POST (Streamable HTTP ohne Server-Stream)"))
		return
	}
	tok := px.mcp.Check(tokenFromRequest(r))
	if tok == nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="llm-balancer-mcp"`)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(rpcError(nil, -32001, "MCP-Token fehlt, ist falsch oder gesperrt"))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeAPIError(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	body = []byte(strings.TrimSpace(string(body)))
	var msgs []rpcMsg
	batch := len(body) > 0 && body[0] == '['
	if batch {
		err = json.Unmarshal(body, &msgs)
	} else {
		var m rpcMsg
		err = json.Unmarshal(body, &m)
		msgs = []rpcMsg{m}
	}
	w.Header().Set("Content-Type", "application/json")
	if err != nil || len(msgs) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(rpcError(nil, -32700, "Parse error"))
		return
	}
	out := []rpcResp{}
	for _, m := range msgs {
		if m.Method == "" { // a response from the client (we never send requests) – ignore
			continue
		}
		res := px.mcpHandle(r.Context(), tok, m)
		if len(m.ID) == 0 || string(m.ID) == "null" { // notification: no answer
			continue
		}
		out = append(out, res)
	}
	if len(out) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if batch {
		json.NewEncoder(w).Encode(out)
		return
	}
	json.NewEncoder(w).Encode(out[0])
}

func (px *Proxy) mcpHandle(ctx context.Context, tok *MCPToken, m rpcMsg) rpcResp {
	ok := func(v any) rpcResp { return rpcResp{JSONRPC: "2.0", ID: m.ID, Result: v} }
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(m.Params, &p)
		ver := mcpVersions[0]
		for _, v := range mcpVersions {
			if v == p.ProtocolVersion {
				ver = v
			}
		}
		return ok(map[string]any{
			"protocolVersion": ver,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "llm-balancer", "title": "LLM Balancer", "version": versionString()},
			"instructions": "Steuert und überwacht den LLM Balancer vor lokalen LLM-Servern. Rolle dieses Tokens: " + roleLabel[tok.Role] +
				". get_status liefert den Überblick; Statistik über get_statistics/get_day_view; Anfragen über list_requests.",
		})
	case "ping":
		return ok(map[string]any{})
	case "tools/list":
		list := []map[string]any{}
		for _, t := range mcpTools {
			if roleRank[tok.Role] >= roleRank[t.role] {
				d := map[string]any{"name": t.name, "description": t.desc, "inputSchema": t.schema()}
				d["annotations"] = map[string]any{"readOnlyHint": t.role == RoleViewer, "destructiveHint": t.destructive, "openWorldHint": false}
				list = append(list, d)
			}
		}
		return ok(map[string]any{"tools": list})
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return rpcError(m.ID, -32602, "ungültige Parameter")
		}
		var tool *mcpTool
		for i := range mcpTools {
			if mcpTools[i].name == p.Name {
				tool = &mcpTools[i]
			}
		}
		if tool == nil {
			return rpcError(m.ID, -32602, "unbekanntes Tool: "+p.Name)
		}
		if roleRank[tok.Role] < roleRank[tool.role] {
			return ok(toolText(fmt.Sprintf("keine Berechtigung: %s braucht Rolle %s, dieses Token hat %s", p.Name, roleLabel[tool.role], roleLabel[tok.Role]), true))
		}
		if p.Arguments == nil {
			p.Arguments = map[string]any{}
		}
		if tool.role != RoleViewer {
			log.Printf("MCP %s (%s) calls %s %v", tok.Name, tok.Role, p.Name, p.Arguments)
		}
		v, err := tool.run(ctx, px, mcpArgs(p.Arguments), tok)
		if err != nil {
			return ok(toolText(err.Error(), true))
		}
		if s, isStr := v.(string); isStr {
			return ok(toolText(s, false))
		}
		data, _ := json.MarshalIndent(v, "", " ")
		res := toolText(string(data), false)
		if _, isObj := v.(map[string]any); isObj {
			res["structuredContent"] = v
		}
		return ok(res)
	case "resources/list":
		return ok(map[string]any{"resources": []any{}})
	case "resources/templates/list":
		return ok(map[string]any{"resourceTemplates": []any{}})
	case "prompts/list":
		return ok(map[string]any{"prompts": []any{}})
	case "logging/setLevel":
		return ok(map[string]any{})
	}
	if strings.HasPrefix(m.Method, "notifications/") {
		return ok(nil)
	}
	return rpcError(m.ID, -32601, "Methode nicht unterstützt: "+m.Method)
}

func toolText(s string, isErr bool) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": s}}, "isError": isErr}
}

// ---- tools ----

type mcpArgs map[string]any

func (a mcpArgs) str(k string) string {
	switch v := a[k].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return jsonNum(v)
	}
	return ""
}

func (a mcpArgs) num(k string, def, lo, hi int) int {
	n := def
	switch v := a[k].(type) {
	case float64:
		n = int(v)
	case string:
		fmt.Sscan(v, &n)
	}
	return min(max(n, lo), hi)
}

func (a mcpArgs) boolean(k string) (bool, bool) {
	switch v := a[k].(type) {
	case bool:
		return v, true
	case string:
		switch strings.ToLower(v) {
		case "true", "1", "ja", "yes", "on":
			return true, true
		case "false", "0", "nein", "no", "off":
			return false, true
		}
	}
	return false, false
}

type mcpParam struct {
	name, typ, desc string
	required        bool
	enum            []string
}

type mcpTool struct {
	name, desc  string
	role        string
	destructive bool
	params      []mcpParam
	run         func(ctx context.Context, px *Proxy, a mcpArgs, tok *MCPToken) (any, error)
}

func (t mcpTool) schema() map[string]any {
	props, req := map[string]any{}, []string{}
	for _, p := range t.params {
		d := map[string]any{"type": p.typ, "description": p.desc}
		if len(p.enum) > 0 {
			d["enum"] = p.enum
		}
		props[p.name] = d
		if p.required {
			req = append(req, p.name)
		}
	}
	s := map[string]any{"type": "object", "properties": props}
	if len(req) > 0 {
		s["required"] = req
	}
	return s
}

var mcpTools []mcpTool

func init() {
	mcpTools = []mcpTool{
		{name: "get_status", role: RoleViewer,
			desc: "Überblick: freie Slots, Warteschlange, Durchsatz (tok/s, Anfragen/s), Zustand jedes Servers mit laufenden Anfragen, Gruppen und Router.",
			run:  mcpStatus},
		{name: "list_requests", role: RoleViewer,
			desc: "Laufende, wartende und zuletzt abgeschlossene Anfragen (neueste zuerst) mit Modell, Server, Token, Session, Dauer, Tokens und Fehler.",
			params: []mcpParam{
				{name: "state", typ: "string", desc: "Filter nach Zustand", enum: []string{"all", "running", "queued", "done"}},
				{name: "limit", typ: "integer", desc: "max. Anzahl (Standard 30, max. 500)"},
				{name: "errors_only", typ: "boolean", desc: "nur fehlgeschlagene Anfragen"},
				{name: "server", typ: "string", desc: "nur dieser Server"},
				{name: "token", typ: "string", desc: "nur dieses API-Token"},
				{name: "model", typ: "string", desc: "nur dieses Modell (angefragt oder verwendet)"},
				{name: "session", typ: "string", desc: "nur diese Session-ID"},
			}, run: mcpRequests},
		{name: "list_sessions", role: RoleViewer,
			desc:   "Zuletzt aktive Sessions (Gespräche / Agent-Läufe) mit Titel, Anfragen, Token, Client und Modellen.",
			params: []mcpParam{{name: "limit", typ: "integer", desc: "max. Anzahl (Standard 20, max. 500)"}},
			run:    mcpSessions},
		{name: "get_session", role: RoleViewer,
			desc:   "Details einer Session: Herkunft (IPs, User-Agent, Header), System-Prompt, erste und letzte Nachricht, letzte Antwort.",
			params: []mcpParam{{name: "id", typ: "string", desc: "Session-ID (8 Hex-Zeichen, aus list_requests/list_sessions)", required: true}},
			run:    mcpSession},
		{name: "get_statistics", role: RoleViewer,
			desc: "Statistik über einen Zeitraum: Summen nach Server, Gruppe, Modell und API-Token sowie pro Tag (Anfragen, Fehler, Dauer, Tokens, tok/s, Kosten) und Warteschlange.",
			params: []mcpParam{
				{name: "days", typ: "integer", desc: "Zeitraum in Tagen bis heute (Standard 7, max. 400)"},
				{name: "by", typ: "string", desc: "nur eine Aufteilung liefern", enum: []string{"all", "server", "group", "model", "token", "day"}},
			}, run: mcpStatistics},
		{name: "get_day_view", role: RoleViewer,
			desc: "Tagesverlauf pro Stunde: Anfragen und Auslastung der Slots (in %) für einen Tag im Vergleich zum Durchschnitt der Vortage, plus Warteschlange.",
			params: []mcpParam{
				{name: "date", typ: "string", desc: "Tag im Format JJJJ-MM-TT (Standard heute)"},
				{name: "avg_days", typ: "integer", desc: "Vortage für den Durchschnitt (Standard 7)"},
				{name: "server", typ: "string", desc: "nur dieser Server"},
			}, run: mcpDayView},
		{name: "list_models", role: RoleViewer,
			desc: "Alle Modellnamen, die der Balancer über die API anbietet (inkl. Router), und welche Server sie bedienen.",
			run:  mcpModels},
		{name: "list_ollama_models", role: RoleViewer,
			desc: "Fragt alle Ollama-Server live nach installierten und geladenen Modellen (Größe, Parameter, Quantisierung).",
			run:  mcpOllamaModels},
		{name: "test_route", role: RoleViewer,
			desc: "Simuliert die Router-Entscheidung für ein Modell und eine Nachricht, ohne etwas zu senden.",
			params: []mcpParam{
				{name: "model", typ: "string", desc: "angefragtes Modell, z. B. auto", required: true},
				{name: "message", typ: "string", desc: "Text der Nutzer-Nachricht"},
				{name: "system", typ: "string", desc: "System-Prompt (optional)"},
				{name: "token", typ: "string", desc: "Name des API-Tokens (für Token-Regeln)"},
			}, run: mcpTestRoute},
		{name: "list_events", role: RoleViewer,
			desc:   "Ereignisse (Server ausgefallen / wieder da, Fehler, Kostendeckel …), neueste zuerst.",
			params: []mcpParam{{name: "limit", typ: "integer", desc: "max. Anzahl (Standard 30, max. 300)"}},
			run:    mcpEvents},

		{name: "set_server_enabled", role: RoleOperator,
			desc: "Server ein- oder ausschalten. Ausgeschaltete Server bekommen keine neuen Anfragen; laufende laufen zu Ende.",
			params: []mcpParam{
				{name: "server", typ: "string", desc: "Servername", required: true},
				{name: "enabled", typ: "boolean", desc: "true = einschalten, false = ausschalten", required: true},
			}, run: mcpSetServer},
		{name: "set_group_enabled", role: RoleOperator,
			desc: "Gruppe ein- oder ausschalten (alle Server der Gruppe nehmen dann keine Anfragen an).",
			params: []mcpParam{
				{name: "group", typ: "string", desc: "Gruppenname", required: true},
				{name: "enabled", typ: "boolean", desc: "true = einschalten, false = ausschalten", required: true},
			}, run: mcpSetGroup},
		{name: "stop_request", role: RoleOperator, destructive: true,
			desc:   "Bricht eine laufende oder wartende Anfrage ab (der Client bekommt einen Fehler).",
			params: []mcpParam{{name: "id", typ: "integer", desc: "Anfrage-ID aus list_requests", required: true}},
			run:    mcpStopRequest},
		{name: "stop_server_requests", role: RoleOperator, destructive: true,
			desc:   "Bricht alle laufenden Anfragen eines Servers ab.",
			params: []mcpParam{{name: "server", typ: "string", desc: "Servername", required: true}},
			run:    mcpStopServer},
		{name: "unload_model", role: RoleOperator,
			desc: "Entlädt ein Modell auf einem Ollama-Server aus dem (V)RAM.",
			params: []mcpParam{
				{name: "server", typ: "string", desc: "Ollama-Server", required: true},
				{name: "model", typ: "string", desc: "Modellname", required: true},
			}, run: mcpUnload},

		{name: "pull_model", role: RoleAdmin,
			desc: "Lädt ein Modell auf einen Ollama-Server herunter (läuft im Hintergrund, Fortschritt über get_model_jobs).",
			params: []mcpParam{
				{name: "server", typ: "string", desc: "Ollama-Server", required: true},
				{name: "model", typ: "string", desc: "Modellname, z. B. qwen3:8b", required: true},
			}, run: mcpPull},
		{name: "get_model_jobs", role: RoleAdmin,
			desc: "Fortschritt der Modell-Downloads.",
			run:  mcpJobs},
		{name: "delete_model", role: RoleAdmin, destructive: true,
			desc: "Löscht ein Modell von einem Ollama-Server.",
			params: []mcpParam{
				{name: "server", typ: "string", desc: "Ollama-Server", required: true},
				{name: "model", typ: "string", desc: "Modellname", required: true},
			}, run: mcpDelete},
	}
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

func mcpStatus(_ context.Context, px *Proxy, _ mcpArgs, _ *MCPToken) (any, error) {
	st := px.pool.Status()
	type srv struct {
		Name      string   `json:"name"`
		State     string   `json:"state"`
		Enabled   bool     `json:"enabled"`
		Group     string   `json:"group,omitempty"`
		Slots     string   `json:"slots"`
		TPS       float64  `json:"tok_per_s,omitempty"`
		Models    []string `json:"models"`
		Loaded    []string `json:"loaded,omitempty"`
		Ollama    bool     `json:"ollama,omitempty"`
		Health    string   `json:"health,omitempty"`
		LastError string   `json:"last_error,omitempty"`
		Requests  int64    `json:"requests_since_start"`
		Errors    int64    `json:"errors_since_start"`
		Running   []mcpReq `json:"running,omitempty"`
	}
	servers := []srv{}
	for _, b := range st.Backends {
		s := srv{Name: b.Name, State: b.State, Enabled: b.Enabled, Group: b.Group, Slots: fmt.Sprintf("%d/%d belegt", b.Active, b.Max),
			Models: b.Models, Loaded: b.Loaded, Ollama: b.IsOllama, LastError: b.LastError, Requests: b.Total, Errors: b.Errors}
		if len(b.Manual) > 0 {
			s.Models = append(append([]string{}, b.Manual...), b.Models...)
		}
		if l := st.Live.Backends[b.Name]; l != nil {
			s.TPS = round1(l.TPS)
		}
		if !b.Healthy {
			s.Health = b.HealthMsg
		}
		for _, r := range b.Running {
			s.Running = append(s.Running, summarizeReq(r))
		}
		servers = append(servers, s)
	}
	groups := []map[string]any{}
	for _, g := range st.Groups {
		groups = append(groups, map[string]any{"name": g.Name, "disabled": g.Disabled, "members": g.Members, "models": g.Available, "queued": g.Queued, "fallback": g.Fallback})
	}
	routers := []map[string]any{}
	for _, r := range st.Routers {
		routers = append(routers, map[string]any{"name": r.Name, "hits": r.Hits, "sticky_sessions": r.Bound})
	}
	queue := []mcpReq{}
	for _, r := range st.Queue {
		queue = append(queue, summarizeReq(r))
	}
	return map[string]any{
		"version": st.Version, "uptime": (time.Duration(st.UptimeSec) * time.Second).String(),
		"free_slots": st.FreeSlots, "total_slots": st.TotalSlots, "running": st.Live.Running, "queued": len(st.Queue),
		"tok_per_s": round1(st.Live.TPS), "requests_per_s": round1(st.Live.RPS), "server_states": st.States,
		"requests_since_start": st.TotalReqs, "rejected_since_start": st.Rejected,
		"servers": servers, "groups": groups, "routers": routers, "queue": queue,
		"balancer_host": map[string]any{"balancer_cpu_pct_of_one_core": round1(st.System.ProcCPU), "balancer_cpu_pct": round1(st.System.ProcShare),
			"host_cpu_pct": round1(st.System.HostCPU), "cores": st.System.Cores, "load": st.System.Load,
			"balancer_ram_mb": st.System.ProcRSS >> 20, "host_ram_free_mb": st.System.MemAvail >> 20, "peak_5min_balancer_cpu_pct": round1(st.System.PeakProc), "peak_5min_host_cpu_pct": round1(st.System.PeakHost)},
	}, nil
}

type mcpReq struct {
	ID        uint64  `json:"id"`
	State     string  `json:"state"`
	Model     string  `json:"model,omitempty"`
	Requested string  `json:"requested_model,omitempty"`
	Effective string  `json:"effective_model,omitempty"`
	Router    string  `json:"router,omitempty"`
	Server    string  `json:"server,omitempty"`
	Group     string  `json:"group,omitempty"`
	Token     string  `json:"token,omitempty"`
	Session   string  `json:"session,omitempty"`
	Title     string  `json:"session_title,omitempty"`
	Client    string  `json:"client,omitempty"`
	Path      string  `json:"path"`
	Queued    string  `json:"queued_at"`
	WaitMs    int64   `json:"wait_ms"`
	DurMs     int64   `json:"duration_ms,omitempty"`
	TTFBMs    int64   `json:"ttfb_ms,omitempty"`
	In        int64   `json:"tokens_in,omitempty"`
	Out       int64   `json:"tokens_out,omitempty"`
	TPS       float64 `json:"tok_per_s,omitempty"`
	Status    int     `json:"http_status,omitempty"`
	Error     string  `json:"error,omitempty"`
	Trail     string  `json:"trail,omitempty"`
}

func summarizeReq(r *ReqInfo) mcpReq {
	o := mcpReq{ID: r.ID, State: r.State, Model: r.Model, Requested: r.Requested, Router: r.Router, Server: r.Backend, Group: r.Group,
		Token: r.Token, Session: r.Session, Title: r.SessTitle, Client: r.Client, Path: r.Path, Queued: r.QueuedAt.Format(time.RFC3339),
		WaitMs: r.QueueMs, DurMs: r.DurMs, TTFBMs: r.TTFBMs, In: r.TokensIn, Out: r.TokensOut, Status: r.Status, Error: r.Error, Trail: r.Trail}
	if r.Effective != r.Model {
		o.Effective = r.Effective
	}
	o.TPS = round1(r.GenTPS)
	if r.State == "running" {
		o.TPS = round1(r.LiveTPS)
	}
	return o
}

func mcpRequests(_ context.Context, px *Proxy, a mcpArgs, _ *MCPToken) (any, error) {
	st := px.pool.Status()
	all := []*ReqInfo{}
	state := a.str("state")
	if state == "" {
		state = "all"
	}
	if state == "all" || state == "queued" {
		all = append(all, st.Queue...)
	}
	if state == "all" || state == "running" {
		for _, b := range st.Backends {
			all = append(all, b.Running...)
		}
	}
	if state == "all" || state == "done" {
		all = append(all, px.pool.RecentAfter(0)...)
	}
	limit := a.num("limit", 30, 1, 500)
	errOnly, _ := a.boolean("errors_only")
	server, token, model, sess := a.str("server"), a.str("token"), a.str("model"), a.str("session")
	out := []mcpReq{}
	for _, r := range all {
		switch {
		case errOnly && r.Error == "" && r.Status < 400,
			server != "" && !strings.EqualFold(r.Backend, server),
			token != "" && !strings.EqualFold(r.Token, token),
			model != "" && !strings.EqualFold(r.Model, model) && !strings.EqualFold(r.Requested, model) && !strings.EqualFold(r.Effective, model),
			sess != "" && r.Session != sess:
			continue
		}
		out = append(out, summarizeReq(r))
		if len(out) >= limit {
			break
		}
	}
	return map[string]any{"count": len(out), "requests": out}, nil
}

func mcpSessions(_ context.Context, px *Proxy, a mcpArgs, _ *MCPToken) (any, error) {
	return map[string]any{"sessions": px.sessions.list(a.num("limit", 20, 1, 500))}, nil
}

func mcpSession(_ context.Context, px *Proxy, a mcpArgs, _ *MCPToken) (any, error) {
	si := px.sessions.get(a.str("id"))
	if si == nil {
		return nil, errSessionGone
	}
	return si, nil
}

type statSum struct {
	Requests  int64   `json:"requests"`
	Errors    int64   `json:"errors,omitempty"`
	Cancelled int64   `json:"cancelled,omitempty"`
	AvgDurMs  int64   `json:"avg_duration_ms,omitempty"`
	MaxDurMs  int64   `json:"max_duration_ms,omitempty"`
	AvgTTFBMs int64   `json:"avg_ttfb_ms,omitempty"`
	AvgWaitMs int64   `json:"avg_wait_ms,omitempty"`
	TokensIn  int64   `json:"tokens_in,omitempty"`
	TokensOut int64   `json:"tokens_out,omitempty"`
	TPS       float64 `json:"tok_per_s,omitempty"`
	Cost      float64 `json:"cost,omitempty"`
	Revenue   float64 `json:"revenue,omitempty"`
}

func sumOf(d *DayStat) statSum {
	s := statSum{Requests: d.Requests, Errors: d.Errors, Cancelled: d.Cancelled, MaxDurMs: d.MaxDurMs, TokensIn: d.TokensIn,
		TokensOut: d.TokensOut, Cost: d.Cost, Revenue: d.Revenue}
	if d.Requests > 0 {
		s.AvgDurMs, s.AvgWaitMs = d.DurMs/d.Requests, d.QueueMs/d.Requests
	}
	if d.TTFBCount > 0 {
		s.AvgTTFBMs = d.TTFBMs / d.TTFBCount
	}
	if d.GenMs > 0 {
		s.TPS = round1(float64(d.GenTokens) * 1000 / float64(d.GenMs))
	}
	return s
}

func mcpStatistics(_ context.Context, px *Proxy, a mcpArgs, _ *MCPToken) (any, error) {
	days := a.num("days", 7, 1, 400)
	by := a.str("by")
	if by == "" {
		by = "all"
	}
	rep := px.pool.stats.Report(days, time.Now())
	out := map[string]any{"from": rep.From, "to": rep.To, "currency": px.cfg.Currency}
	want := func(k string) bool { return by == "all" || by == k }
	if want("server") {
		m := map[string]statSum{}
		for k, v := range rep.Totals {
			m[k] = sumOf(v)
		}
		out["by_server"] = m
	}
	if want("group") {
		groupOf := map[string]string{}
		for _, b := range px.pool.Status().Backends {
			groupOf[b.Name] = b.Group
		}
		agg := map[string]*DayStat{}
		for k, v := range rep.Totals {
			g := cmpOr(groupOf[k], "(ohne Gruppe)")
			if agg[g] == nil {
				agg[g] = &DayStat{}
			}
			agg[g].add(v)
		}
		m := map[string]statSum{}
		for k, v := range agg {
			m[k] = sumOf(v)
		}
		out["by_group"] = m
	}
	if want("model") {
		m := map[string]statSum{}
		for k, v := range rep.ModelTotals {
			m[k] = sumOf(v)
		}
		out["by_model"] = m
	}
	if want("token") {
		out["by_token"] = rep.TokenTotals
	}
	if want("day") {
		type day struct {
			Date string `json:"date"`
			statSum
			Queue *QueueStat `json:"queue,omitempty"`
		}
		list := []day{}
		for _, d := range rep.Days {
			t := &DayStat{}
			for _, v := range rep.Data[d] {
				t.add(v)
			}
			list = append(list, day{Date: d, statSum: sumOf(t), Queue: rep.QueueData[d]})
		}
		out["by_day"] = list
	}
	if by == "all" {
		t := &DayStat{}
		for _, v := range rep.Totals {
			t.add(v)
		}
		out["total"] = sumOf(t)
		out["queue"] = rep.QueueTotal
	}
	return out, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func mcpDayView(_ context.Context, px *Proxy, a mcpArgs, _ *MCPToken) (any, error) {
	date := time.Now()
	if d := a.str("date"); d != "" {
		t, err := time.ParseInLocation("2006-01-02", d, time.Local)
		if err != nil {
			return nil, errors.New("Datum bitte als JJJJ-MM-TT")
		}
		date = t.Add(23*time.Hour + 59*time.Minute)
	}
	slots := map[string]int{}
	for _, b := range px.pool.Status().Backends {
		slots[b.Name] = b.Max
	}
	dv := px.pool.stats.Day(date, a.num("avg_days", 7, 1, hourKeepDays), slots)
	server := a.str("server")
	type hour struct {
		Hour        int     `json:"hour"`
		Requests    int64   `json:"requests"`
		Utilization float64 `json:"utilization_pct"`
		AvgRequests float64 `json:"avg_requests"`
		AvgUtil     float64 `json:"avg_utilization_pct"`
		QueueWaited int64   `json:"queue_waited,omitempty"`
		QueueMaxLen int     `json:"queue_max_len,omitempty"`
	}
	hours := make([]hour, 24)
	capMs := 0.0
	for _, b := range dv.Backends {
		if server != "" && !strings.EqualFold(b, server) {
			continue
		}
		capMs += float64(max(1, dv.Slots[b])) * 3600000
		for h := 0; h < 24; h++ {
			if h < len(dv.Today[b]) {
				hours[h].Requests += dv.Today[b][h].Requests
				hours[h].Utilization += float64(dv.Today[b][h].BusyMs)
			}
			if h < len(dv.Avg[b]) {
				hours[h].AvgRequests += dv.Avg[b][h].Requests
				hours[h].AvgUtil += dv.Avg[b][h].BusyMs
			}
		}
	}
	for h := range hours {
		hours[h].Hour = h
		if capMs > 0 {
			hours[h].Utilization = round1(hours[h].Utilization * 100 / capMs)
			hours[h].AvgUtil = round1(hours[h].AvgUtil * 100 / capMs)
		}
		hours[h].AvgRequests = round1(hours[h].AvgRequests)
		if server == "" && h < len(dv.QueueToday) {
			hours[h].QueueWaited, hours[h].QueueMaxLen = dv.QueueToday[h].Waited, dv.QueueToday[h].MaxLen
		}
	}
	return map[string]any{"date": dv.Date, "avg_of_days": dv.AvgDays, "servers": dv.Backends, "hours": hours}, nil
}

func mcpModels(_ context.Context, px *Proxy, _ mcpArgs, _ *MCPToken) (any, error) {
	st := px.pool.Status()
	servedBy := map[string][]string{}
	for _, b := range st.Backends {
		for m := range b.Caps {
			servedBy[m] = append(servedBy[m], b.Name)
		}
	}
	type model struct {
		Name    string   `json:"name"`
		Servers []string `json:"servers,omitempty"`
		Router  bool     `json:"router,omitempty"`
	}
	routers := map[string]bool{}
	for _, r := range st.Routers {
		routers[strings.ToLower(r.Name)] = true
	}
	out := []model{}
	for _, m := range px.pool.AllModels() {
		s := servedBy[m]
		sort.Strings(s)
		out = append(out, model{Name: m, Servers: s, Router: routers[strings.ToLower(m)]})
	}
	return map[string]any{"models": out}, nil
}

func (px *Proxy) ollamaMgr() (*ollamaManager, error) {
	if px.ollama == nil {
		return nil, errors.New("Ollama-Verwaltung nicht verfügbar")
	}
	return px.ollama, nil
}

func mcpOllamaModels(ctx context.Context, px *Proxy, _ mcpArgs, _ *MCPToken) (any, error) {
	m, err := px.ollamaMgr()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	type mdl struct {
		Name   string `json:"name"`
		SizeGB string `json:"size"`
		Params string `json:"parameters,omitempty"`
		Quant  string `json:"quantization,omitempty"`
		Family string `json:"family,omitempty"`
		Loaded bool   `json:"loaded,omitempty"`
	}
	out := []map[string]any{}
	for _, s := range m.List(ctx) {
		loaded := map[string]bool{}
		for _, l := range s.Loaded {
			loaded[l.Name] = true
		}
		ms := []mdl{}
		for _, t := range s.Models {
			ms = append(ms, mdl{Name: t.Name, SizeGB: fmt.Sprintf("%.1f GB", float64(t.Size)/1e9), Params: t.Details.ParameterSize,
				Quant: t.Details.QuantizationLevel, Family: t.Details.Family, Loaded: loaded[t.Name]})
		}
		e := map[string]any{"server": s.Server, "version": s.Version, "models": ms}
		if s.Error != "" {
			e["error"] = s.Error
		}
		out = append(out, e)
	}
	return map[string]any{"servers": out}, nil
}

func mcpTestRoute(_ context.Context, px *Proxy, a mcpArgs, _ *MCPToken) (any, error) {
	model := a.str("model")
	if model == "" {
		return nil, errors.New("model fehlt")
	}
	msgs := []map[string]string{}
	if s := a.str("system"); s != "" {
		msgs = append(msgs, map[string]string{"role": "system", "content": s})
	}
	msgs = append(msgs, map[string]string{"role": "user", "content": a.str("message")})
	body, _ := json.Marshal(map[string]any{"model": model, "messages": msgs})
	d := px.pool.decide(routeCtx{model: model, token: a.str("token"), header: http.Header{}, body: body, explain: true})
	if d == nil {
		return map[string]any{"router": "", "model": model, "reason": "kein Router zuständig – Modell bleibt"}, nil
	}
	return d, nil
}

func mcpEvents(_ context.Context, px *Proxy, a mcpArgs, _ *MCPToken) (any, error) {
	n := px.pool.notifier
	if n == nil {
		return map[string]any{"events": []Event{}}, nil
	}
	limit := a.num("limit", 30, 1, maxEvents)
	n.mu.Lock()
	out := []Event{}
	for i := len(n.events) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, n.events[i])
	}
	n.mu.Unlock()
	return map[string]any{"events": out}, nil
}

func needBool(a mcpArgs, k string) (bool, error) {
	v, ok := a.boolean(k)
	if !ok {
		return false, fmt.Errorf("%s (true/false) fehlt", k)
	}
	return v, nil
}

func mcpErr(err error) error {
	var he httpErr
	if errors.As(err, &he) {
		return he.err
	}
	return err
}

func mcpSetServer(_ context.Context, px *Proxy, a mcpArgs, tok *MCPToken) (any, error) {
	en, err := needBool(a, "enabled")
	if err != nil {
		return nil, err
	}
	name := a.str("server")
	if err := px.pool.SetEnabled(name, en); err != nil {
		return nil, fmt.Errorf("Server %q: %v", name, mcpErr(err))
	}
	return fmt.Sprintf("Server %s %s.", name, map[bool]string{true: "eingeschaltet", false: "ausgeschaltet"}[en]), nil
}

func mcpSetGroup(_ context.Context, px *Proxy, a mcpArgs, tok *MCPToken) (any, error) {
	en, err := needBool(a, "enabled")
	if err != nil {
		return nil, err
	}
	name := a.str("group")
	if err := px.pool.SetGroupDisabled(name, !en); err != nil {
		return nil, fmt.Errorf("Gruppe %q: %v", name, mcpErr(err))
	}
	return fmt.Sprintf("Gruppe %s %s.", name, map[bool]string{true: "eingeschaltet", false: "ausgeschaltet"}[en]), nil
}

func mcpStopRequest(_ context.Context, px *Proxy, a mcpArgs, _ *MCPToken) (any, error) {
	id := a.num("id", 0, 0, math.MaxInt32)
	if id <= 0 {
		return nil, errors.New("id fehlt")
	}
	if px.pool.Stop("", uint64(id)) == 0 {
		return nil, fmt.Errorf("Anfrage %d läuft nicht (mehr)", id)
	}
	return fmt.Sprintf("Anfrage %d abgebrochen.", id), nil
}

func mcpStopServer(_ context.Context, px *Proxy, a mcpArgs, _ *MCPToken) (any, error) {
	name := a.str("server")
	if name == "" {
		return nil, errors.New("server fehlt")
	}
	n := px.pool.Stop(name, 0)
	return fmt.Sprintf("%d laufende Anfrage(n) auf %s abgebrochen.", n, name), nil
}

func mcpUnload(_ context.Context, px *Proxy, a mcpArgs, _ *MCPToken) (any, error) {
	m, err := px.ollamaMgr()
	if err != nil {
		return nil, err
	}
	if err := m.Unload(a.str("server"), a.str("model")); err != nil {
		return nil, mcpErr(err)
	}
	return fmt.Sprintf("Modell %s auf %s entladen.", a.str("model"), a.str("server")), nil
}

func mcpPull(_ context.Context, px *Proxy, a mcpArgs, tok *MCPToken) (any, error) {
	m, err := px.ollamaMgr()
	if err != nil {
		return nil, err
	}
	j, err := m.Pull(a.str("server"), a.str("model"), "MCP "+tok.Name)
	if err != nil {
		return nil, mcpErr(err)
	}
	return map[string]any{"job": j.ID, "server": j.Server, "model": j.Model, "state": j.State, "hint": "Fortschritt mit get_model_jobs abfragen"}, nil
}

func mcpJobs(_ context.Context, px *Proxy, _ mcpArgs, _ *MCPToken) (any, error) {
	m, err := px.ollamaMgr()
	if err != nil {
		return nil, err
	}
	type job struct {
		pullJob
		Percent float64 `json:"percent,omitempty"`
	}
	out := []job{}
	for _, j := range m.Jobs() {
		x := job{pullJob: j}
		if j.Total > 0 {
			x.Percent = round1(float64(j.Completed) * 100 / float64(j.Total))
		}
		out = append(out, x)
	}
	return map[string]any{"jobs": out}, nil
}

func mcpDelete(_ context.Context, px *Proxy, a mcpArgs, _ *MCPToken) (any, error) {
	m, err := px.ollamaMgr()
	if err != nil {
		return nil, err
	}
	if err := m.Delete(a.str("server"), a.str("model")); err != nil {
		return nil, mcpErr(err)
	}
	return fmt.Sprintf("Modell %s auf %s gelöscht.", a.str("model"), a.str("server")), nil
}
