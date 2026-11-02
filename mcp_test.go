package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func mcpCall(t *testing.T, base, token, body string) (int, map[string]any, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	json.Unmarshal(b, &m)
	return resp.StatusCode, m, string(b)
}

func toolCall(t *testing.T, base, token, name, args string) (bool, string) {
	t.Helper()
	code, m, raw := mcpCall(t, base, token, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`)
	if code != 200 || m["result"] == nil {
		t.Fatalf("%s: %d %s", name, code, raw)
	}
	res := m["result"].(map[string]any)
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	return res["isError"] == true, text
}

func TestMCP(t *testing.T) {
	a := newFakeLLM(t, "a", 0)
	srv, pool := setup(t, func(c *Config) { c.DataDir = t.TempDir() }, a)
	admin := newWeb(t, srv.URL)

	// no token → 401; GET → 405
	if c, _, _ := mcpCall(t, srv.URL, "", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`); c != 401 {
		t.Errorf("without token: %d", c)
	}
	if resp, err := http.Get(srv.URL + "/mcp"); err != nil || resp.StatusCode != 405 {
		t.Errorf("GET: %v %v", err, resp.StatusCode)
	}

	toks := map[string]string{}
	for _, r := range []string{"viewer", "operator", "admin"} {
		c, b := admin.do("POST", "/api/mcp-tokens", `{"name":"mcp-`+r+`","role":"`+r+`"}`)
		var out map[string]string
		json.Unmarshal([]byte(b), &out)
		if c != 200 || !strings.HasPrefix(out["token"], "mcp-") {
			t.Fatalf("create: %d %s", c, b)
		}
		toks[r] = out["token"]
	}
	if c, _ := admin.do("POST", "/api/mcp-tokens", `{"name":"MCP-viewer","role":"viewer"}`); c != 409 {
		t.Errorf("duplicate name: %d", c)
	}
	if _, b := admin.do("GET", "/api/mcp-tokens", ""); strings.Contains(b, `"hash"`) || !strings.Contains(b, `"role":"operator"`) {
		t.Errorf("list: %s", b)
	}
	// MCP tokens are no LLM API tokens and vice versa
	if c, _, _ := mcpCall(t, srv.URL, "sk-something-else-123", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); c != 401 {
		t.Errorf("wrong token: %d", c)
	}

	c, m, raw := mcpCall(t, srv.URL, toks["viewer"], `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	if c != 200 || m["result"].(map[string]any)["protocolVersion"] != "2025-06-18" || !strings.Contains(raw, `"tools"`) {
		t.Fatalf("initialize: %d %s", c, raw)
	}
	if c, _, _ := mcpCall(t, srv.URL, toks["viewer"], `{"jsonrpc":"2.0","method":"notifications/initialized"}`); c != 202 {
		t.Errorf("notification: %d", c)
	}
	count := func(tok string) int {
		_, m, _ := mcpCall(t, srv.URL, tok, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
		return len(m["result"].(map[string]any)["tools"].([]any))
	}
	nv, no, na := count(toks["viewer"]), count(toks["operator"]), count(toks["admin"])
	if !(nv < no && no < na && na == len(mcpTools)) {
		t.Errorf("tools per role: %d %d %d", nv, no, na)
	}
	if _, m, _ := mcpCall(t, srv.URL, toks["viewer"], `{"jsonrpc":"2.0","id":3,"method":"nope"}`); m["error"] == nil {
		t.Error("unknown method must be an error")
	}

	// read tools
	post(t, srv.URL)
	if isErr, txt := toolCall(t, srv.URL, toks["viewer"], "get_status", `{}`); isErr || !strings.Contains(txt, `"free_slots"`) || !strings.Contains(txt, `"name": "a"`) {
		t.Errorf("get_status: %s", txt)
	}
	if isErr, txt := toolCall(t, srv.URL, toks["viewer"], "list_requests", `{"state":"done","limit":5}`); isErr || !strings.Contains(txt, `"server": "a"`) {
		t.Errorf("list_requests: %s", txt)
	}
	pool.stats.Flush()
	if isErr, txt := toolCall(t, srv.URL, toks["viewer"], "get_statistics", `{"days":3}`); isErr || !strings.Contains(txt, `"by_server"`) || !strings.Contains(txt, `"by_group"`) {
		t.Errorf("get_statistics: %s", txt)
	}
	if isErr, txt := toolCall(t, srv.URL, toks["viewer"], "get_day_view", `{}`); isErr || !strings.Contains(txt, `"utilization_pct"`) {
		t.Errorf("get_day_view: %s", txt)
	}
	for _, name := range []string{"list_sessions", "list_models", "list_events"} {
		if isErr, txt := toolCall(t, srv.URL, toks["viewer"], name, `{}`); isErr {
			t.Errorf("%s: %s", name, txt)
		}
	}
	if isErr, txt := toolCall(t, srv.URL, toks["viewer"], "test_route", `{"model":"x","message":"hi"}`); isErr || !strings.Contains(txt, "kein Router") {
		t.Errorf("test_route: %s", txt)
	}

	// control: viewer may not, operator may
	if isErr, txt := toolCall(t, srv.URL, toks["viewer"], "set_server_enabled", `{"server":"a","enabled":false}`); !isErr || !strings.Contains(txt, "Berechtigung") {
		t.Errorf("viewer switched a server: %s", txt)
	}
	if isErr, txt := toolCall(t, srv.URL, toks["operator"], "set_server_enabled", `{"server":"a","enabled":false}`); isErr {
		t.Errorf("operator: %s", txt)
	}
	pool.mu.Lock()
	en := pool.backends[0].enabled
	pool.mu.Unlock()
	if en {
		t.Error("server still enabled")
	}
	if isErr, _ := toolCall(t, srv.URL, toks["operator"], "set_server_enabled", `{"server":"zzz","enabled":true}`); !isErr {
		t.Error("unknown server must be an error")
	}
	toolCall(t, srv.URL, toks["operator"], "set_server_enabled", `{"server":"a","enabled":"true"}`)
	if isErr, _ := toolCall(t, srv.URL, toks["operator"], "pull_model", `{"server":"a","model":"x"}`); !isErr {
		t.Error("operator must not pull models")
	}
	if isErr, txt := toolCall(t, srv.URL, toks["operator"], "stop_request", `{"id":99999}`); !isErr || !strings.Contains(txt, "läuft nicht") {
		t.Errorf("stop_request: %s", txt)
	}

	// batch
	if c, _, raw := mcpCall(t, srv.URL, toks["viewer"], `[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","id":2,"method":"ping"}]`); c != 200 || !strings.HasPrefix(raw, "[") {
		t.Errorf("batch: %d %s", c, raw)
	}

	// disable, renew, delete
	admin.do("PUT", "/api/mcp-tokens/mcp-viewer", `{"disabled":true}`)
	if c, _, _ := mcpCall(t, srv.URL, toks["viewer"], `{"jsonrpc":"2.0","id":1,"method":"ping"}`); c != 401 {
		t.Errorf("disabled token: %d", c)
	}
	_, b := admin.do("PUT", "/api/mcp-tokens/mcp-operator", `{"renew":true,"role":"viewer"}`)
	var nt map[string]string
	json.Unmarshal([]byte(b), &nt)
	if c, _, _ := mcpCall(t, srv.URL, toks["operator"], `{"jsonrpc":"2.0","id":1,"method":"ping"}`); c != 401 {
		t.Errorf("old value after renew: %d", c)
	}
	if count(nt["token"]) != nv {
		t.Error("role change not applied")
	}
	admin.do("DELETE", "/api/mcp-tokens/mcp-admin", "")
	if c, _, _ := mcpCall(t, srv.URL, toks["admin"], `{"jsonrpc":"2.0","id":1,"method":"ping"}`); c != 401 {
		t.Errorf("deleted token: %d", c)
	}

	// export with secrets contains the hashes, without not
	_, plain := admin.do("GET", "/api/config/export?sections=tokens", "")
	_, full := admin.do("GET", "/api/config/export?sections=tokens&secrets=1", "")
	if !strings.Contains(plain, `"mcp_tokens"`) || strings.Contains(plain, `"hash"`) || !strings.Contains(full, hashToken(nt["token"])) {
		t.Errorf("export: %s", plain)
	}
}

func TestMCPTokensPersist(t *testing.T) {
	dir := t.TempDir()
	m := NewMCPTokens(dir)
	v, err := m.Create("bot", "operator", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create("x", "root", ""); err == nil {
		t.Error("invalid role accepted")
	}
	if _, err := m.Create("y", "viewer", "short"); err == nil {
		t.Error("short value accepted")
	}
	m2 := NewMCPTokens(dir)
	if tok := m2.Check(v); tok == nil || tok.Role != "operator" || tok.Calls != 1 {
		t.Errorf("reloaded: %+v", tok)
	}
}
