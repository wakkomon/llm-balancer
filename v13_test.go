package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
)

type webClient struct {
	t    *testing.T
	base string
	c    *http.Client
	hdr  map[string]string
}

func newWeb(t *testing.T, base string) *webClient {
	jar, _ := cookiejar.New(nil)
	return &webClient{t: t, base: base, c: &http.Client{Jar: jar}, hdr: map[string]string{}}
}

func (w *webClient) do(method, path, body string) (int, string) {
	w.t.Helper()
	req, _ := http.NewRequest(method, w.base+"/balancer"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range w.hdr {
		req.Header.Set(k, v)
	}
	resp, err := w.c.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestUsersAndRoles(t *testing.T) {
	a := newFakeLLM(t, "a", 10*0)
	srv, _ := setup(t, nil, a)
	anon := newWeb(t, srv.URL)
	if c, _ := anon.do("GET", "/api/status", ""); c != 200 {
		t.Fatalf("legacy mode should be open: %d", c)
	}
	if c, b := anon.do("POST", "/api/users", `{"name":"v","role":"viewer","password":"12345678"}`); c != 400 {
		t.Errorf("first user must be admin: %d %s", c, b)
	}
	admin := newWeb(t, srv.URL)
	if c, b := admin.do("POST", "/api/auth/setup", `{"name":"anna","password":"short"}`); c != 400 {
		t.Errorf("short password: %d %s", c, b)
	}
	if c, b := admin.do("POST", "/api/auth/setup", `{"name":"anna","password":"geheim123"}`); c != 200 {
		t.Fatalf("setup: %d %s", c, b)
	}
	if c, _ := admin.do("POST", "/api/auth/setup", `{"name":"x","password":"geheim123"}`); c != 409 {
		t.Errorf("second setup: %d", c)
	}
	// login is required now – except for the page, health and the LLM API
	if c, _ := anon.do("GET", "/api/status", ""); c != 401 {
		t.Errorf("anonymous status: %d", c)
	}
	if c, _ := anon.do("GET", "/", ""); c != 200 {
		t.Errorf("page: %d", c)
	}
	if be, _ := post(t, srv.URL); be != "a" {
		t.Errorf("LLM API must not need a login: %q", be)
	}
	if c, b := admin.do("GET", "/api/auth/me", ""); c != 200 || !strings.Contains(b, `"role":"admin"`) {
		t.Errorf("me: %d %s", c, b)
	}
	for _, u := range []string{`{"name":"op","role":"operator","password":"operator1"}`, `{"name":"view","role":"viewer","password":"viewer12"}`} {
		if c, b := admin.do("POST", "/api/users", u); c != 204 {
			t.Fatalf("add user: %d %s", c, b)
		}
	}
	op, view := newWeb(t, srv.URL), newWeb(t, srv.URL)
	if c, _ := op.do("POST", "/api/auth/login", `{"name":"op","password":"falsch"}`); c != 401 {
		t.Errorf("wrong password: %d", c)
	}
	op.do("POST", "/api/auth/login", `{"name":"OP","password":"operator1"}`) // names are case-insensitive
	view.do("POST", "/api/auth/login", `{"name":"view","password":"viewer12"}`)
	cases := []struct {
		who          *webClient
		method, path string
		body         string
		want         int
	}{
		{view, "GET", "/api/status", "", 200},
		{view, "GET", "/api/tokens", "", 200},
		{view, "POST", "/api/backends/a/disable", "", 403},
		{view, "GET", "/api/users", "", 403},
		{op, "POST", "/api/backends/a/disable", "", 204},
		{op, "POST", "/api/backends/a/enable", "", 204},
		{op, "PUT", "/api/backends/a", `{"name":"a","url":"` + a.srv.URL + `","max_concurrent":2}`, 403},
		{op, "GET", "/api/config/export", "", 403},
		{admin, "PUT", "/api/backends/a", `{"name":"a","url":"` + a.srv.URL + `","max_concurrent":2}`, 204},
		{admin, "GET", "/api/users", "", 200},
	}
	for _, c := range cases {
		if got, b := c.who.do(c.method, c.path, c.body); got != c.want {
			t.Errorf("%s %s: %d, want %d (%s)", c.method, c.path, got, c.want, b)
		}
	}
	// own password change; the last admin cannot be removed
	if c, b := view.do("POST", "/api/auth/password", `{"old":"viewer12","new":"neuesPasswort"}`); c != 204 {
		t.Errorf("own password: %d %s", c, b)
	}
	if c, _ := admin.do("PUT", "/api/users/anna", `{"role":"viewer"}`); c != 400 {
		t.Errorf("demote last admin: %d", c)
	}
	if c, _ := admin.do("DELETE", "/api/users/anna", ""); c != 400 {
		t.Errorf("delete last admin: %d", c)
	}
	// locked user loses the session right away
	admin.do("PUT", "/api/users/op", `{"disabled":true}`)
	if c, _ := op.do("GET", "/api/status", ""); c != 401 {
		t.Errorf("locked user still logged in: %d", c)
	}
	// cross-site POST with a valid session is refused
	admin.hdr["Origin"] = "http://evil.example"
	if c, _ := admin.do("POST", "/api/backends/a/disable", ""); c != 403 {
		t.Errorf("cross-origin post: %d", c)
	}
	delete(admin.hdr, "Origin")
	// logout
	admin.do("POST", "/api/auth/logout", "")
	if c, _ := admin.do("GET", "/api/status", ""); c != 401 {
		t.Errorf("after logout: %d", c)
	}
}

func TestLoginLockout(t *testing.T) {
	srv, _ := setup(t, nil)
	w := newWeb(t, srv.URL)
	w.do("POST", "/api/auth/setup", `{"name":"admin","password":"richtig123"}`)
	x := newWeb(t, srv.URL)
	for i := 0; i < 5; i++ {
		x.do("POST", "/api/auth/login", `{"name":"admin","password":"falsch"}`)
	}
	if c, _ := x.do("POST", "/api/auth/login", `{"name":"admin","password":"richtig123"}`); c != 429 {
		t.Errorf("expected lockout, got %d", c)
	}
}

func TestAdminTokenWithUsers(t *testing.T) {
	a := newFakeLLM(t, "a", 0)
	srv, _ := setup(t, func(c *Config) { c.AdminToken = "tok" }, a)
	w := newWeb(t, srv.URL)
	if c, _ := w.do("POST", "/api/auth/setup", `{"name":"admin","password":"richtig123"}`); c != 401 {
		t.Errorf("setup must need ADMIN_TOKEN when set: %d", c)
	}
	w.hdr["X-Admin-Token"] = "tok"
	if c, _ := w.do("POST", "/api/auth/setup", `{"name":"admin","password":"richtig123"}`); c != 200 {
		t.Fatalf("setup with token: %d", c)
	}
	s := newWeb(t, srv.URL)
	s.hdr["X-Admin-Token"] = "tok"
	if c, _ := s.do("POST", "/api/backends/a/disable", ""); c != 204 {
		t.Errorf("scripts with ADMIN_TOKEN: %d", c)
	}
}

func TestConfigExportImport(t *testing.T) {
	a := newFakeLLM(t, "a", 0)
	srv, _ := setup(t, func(c *Config) {
		c.DataDir = t.TempDir()
		c.Backends[0].APIKey = "sk-backend-secret"
		c.Groups = []GroupConfig{{Name: "g1", Disabled: true}}
		c.Backends[0].Group = "g1"
	}, a)
	w := newWeb(t, srv.URL)
	w.do("POST", "/api/auth/setup", `{"name":"admin","password":"richtig123"}`)
	_, tokBody := w.do("POST", "/api/tokens", `{"name":"hermes","token":"sk-client-0001"}`)
	if !strings.Contains(tokBody, "sk-client-0001") {
		t.Fatalf("token: %s", tokBody)
	}
	w.do("PUT", "/api/tokens/hermes", `{"price_in":1.5,"price_out":3}`)
	w.do("PUT", "/api/notify", `{"enabled":false,"smtp_host":"mail.example","smtp_port":587,"smtp_security":"starttls","smtp_username":"u","smtp_password":"smtp-secret","from":"a@b.c","to":["x@y.z"]}`)

	_, plain := w.do("GET", "/api/config/export", "")
	for _, secret := range []string{"sk-backend-secret", "smtp-secret", "pbkdf2", `"hash"`} {
		if strings.Contains(plain, secret) {
			t.Errorf("export without secrets contains %s", secret)
		}
	}
	_, full := w.do("GET", "/api/config/export?secrets=1", "")
	for _, secret := range []string{"sk-backend-secret", "smtp-secret", "pbkdf2-sha256$", `"hash"`, `"disabled": true`} {
		if !strings.Contains(full, secret) {
			t.Errorf("full export misses %s", secret)
		}
	}

	// import the full export into a fresh balancer
	b := newFakeLLM(t, "b", 0)
	srv2, pool2 := setup(t, func(c *Config) { c.DataDir = t.TempDir() }, b)
	w2 := newWeb(t, srv2.URL)
	// the file still points to a's URL – fine for the test
	code, body := w2.do("POST", "/api/config/import", full)
	if code != 200 {
		t.Fatalf("import: %d %s", code, body)
	}
	var res importResult
	json.Unmarshal([]byte(body), &res)
	if len(res.Applied) != 4 {
		t.Errorf("applied: %+v", res)
	}
	pool2.mu.Lock()
	if len(pool2.backends) != 1 || pool2.backends[0].Name != "a" || pool2.backends[0].apiKey != "sk-backend-secret" || !pool2.groups[0].Disabled {
		t.Errorf("servers not imported: %+v %+v", pool2.backends[0], pool2.groups)
	}
	pool2.mu.Unlock()
	// imported users: login required now, and the old password works
	if c, _ := w2.do("GET", "/api/status", ""); c != 401 {
		t.Errorf("users not active after import: %d", c)
	}
	if c, _ := w2.do("POST", "/api/auth/login", `{"name":"admin","password":"richtig123"}`); c != 200 {
		t.Errorf("imported login: %d", c)
	}
	// imported API token works for the LLM API
	req, _ := http.NewRequest("POST", srv2.URL+"/v1/chat/completions", bytes.NewReader([]byte(`{"model":"x"}`)))
	req.Header.Set("Authorization", "Bearer sk-client-0001")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode == 401 {
		t.Errorf("imported API token rejected: %v %v", err, resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if c, b := w2.do("GET", "/api/notify", ""); c != 200 || !strings.Contains(b, `"has_password":true`) {
		t.Errorf("notify: %d %s", c, b)
	}

	// re-import without secrets keeps the bearer token and passwords
	code, body = w2.do("POST", "/api/config/import?sections=servers,users", plain)
	if code != 200 || !strings.Contains(body, "beibehalten") {
		t.Errorf("plain import: %d %s", code, body)
	}
	pool2.mu.Lock()
	if pool2.backends[0].apiKey != "sk-backend-secret" {
		t.Error("bearer token lost on import without secrets")
	}
	pool2.mu.Unlock()
	if c, _ := newWeb(t, srv2.URL).do("POST", "/api/auth/login", `{"name":"admin","password":"richtig123"}`); c != 200 {
		t.Errorf("password lost on import without secrets: %d", c)
	}

	// a file without an active admin is refused
	bad := strings.Replace(full, `"role": "admin"`, `"role": "viewer"`, 1)
	if c, _ := w2.do("POST", "/api/config/import?sections=users", bad); c != 400 {
		t.Errorf("import without admin: %d", c)
	}
	if c, _ := w2.do("POST", "/api/config/import", `{"format":"something"}`); c != 400 {
		t.Errorf("wrong format: %d", c)
	}
}
