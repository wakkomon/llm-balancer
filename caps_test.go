package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP is a tiny SMTP server (no TLS) that records the received mails.
type fakeSMTP struct {
	addr  string
	mu    sync.Mutex
	mails []string
	auth  []string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeSMTP{addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	say := func(s string) { fmt.Fprintf(c, "%s\r\n", s) }
	say("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			say("250-fake")
			say("250 AUTH PLAIN")
		case strings.HasPrefix(cmd, "AUTH PLAIN"):
			parts := strings.Fields(strings.TrimSpace(line))
			dec, _ := base64.StdEncoding.DecodeString(parts[len(parts)-1])
			f.mu.Lock()
			f.auth = append(f.auth, strings.ReplaceAll(string(dec), "\x00", "|"))
			f.mu.Unlock()
			say("235 ok")
		case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"), strings.HasPrefix(cmd, "RSET"), strings.HasPrefix(cmd, "NOOP"):
			say("250 ok")
		case cmd == "DATA":
			say("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil || l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			f.mu.Lock()
			f.mails = append(f.mails, b.String())
			f.mu.Unlock()
			say("250 queued")
		case cmd == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func (f *fakeSMTP) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.mails...)
}

func smtpConfigJSON(f *fakeSMTP, events string) string {
	host, port, _ := net.SplitHostPort(f.addr)
	return fmt.Sprintf(`{"enabled":true,"smtp_host":%q,"smtp_port":%s,"smtp_security":"none","smtp_username":"bal","smtp_password":"pw",
		"from":"balancer@example.lan","to":["admin@example.lan"],"down_delay_sec":0,"events":%s}`, host, port, events)
}

func startNotifier(t *testing.T, pool *Pool) {
	stop := make(chan struct{})
	go pool.notifier.mailLoop(stop)
	go pool.notifier.loop(pool, stop)
	t.Cleanup(func() { close(stop) })
}

func getEvents(t *testing.T, url string) []Event {
	resp, err := http.Get(url + "/balancer/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var ev []Event
	json.NewDecoder(resp.Body).Decode(&ev)
	return ev
}

func TestTokenCostCapBlocksAndMails(t *testing.T) {
	smtp := newFakeSMTP(t)
	a := newModelLLM(t, "a", 10*time.Millisecond, "m") // 1000 in / 500 out per request
	srv, pool := setup(t, func(c *Config) {
		c.APIKeys = "litellm=sk-litellm-0123456789"
		c.Backends = []BackendConfig{{Name: "a", URL: a.srv.URL}}
	})
	startNotifier(t, pool)
	if c := api(t, "PUT", srv.URL+"/balancer/api/notify", smtpConfigJSON(smtp, `{"token_capped":true,"token_uncapped":true}`), ""); c != 204 {
		t.Fatalf("notify config: %d", c)
	}
	// 1 € in / 2 € out per 1M → 0.002 per request; cap 0.005 → blocked after the 3rd request
	if c := api(t, "PUT", srv.URL+"/balancer/api/tokens/litellm", `{"price_in":1,"price_out":2,"cost_cap":0.005,"cap_period":"month"}`, ""); c != 204 {
		t.Fatalf("set cap: %d", c)
	}
	auth := map[string]string{"Authorization": "Bearer sk-litellm-0123456789"}
	for i := 0; i < 3; i++ {
		if code, _ := chatModel(t, srv.URL, "m", auth); code != 200 {
			t.Fatalf("request %d: %d", i, code)
		}
	}
	waitFor(t, func() bool {
		code, _ := chatModel(t, srv.URL, "m", auth)
		return code == 429
	})
	_, list := call(t, "GET", srv.URL+"/balancer/api/tokens", nil, "")
	if !strings.Contains(list, `"capped":true`) {
		t.Errorf("token list: %s", list)
	}
	waitFor(t, func() bool { return len(smtp.all()) == 1 })
	m := smtp.all()[0]
	if !strings.Contains(m, "Subject: [LLM-Balancer] Kostendeckel API-Token erreicht: litellm") ||
		!strings.Contains(m, "To: admin@example.lan") || !strings.Contains(m, "0,0060 € von 0,0050 € pro Monat") {
		t.Errorf("mail:\n%s", m)
	}
	if len(smtp.auth) != 1 || smtp.auth[0] != "|bal|pw" {
		t.Errorf("auth: %v", smtp.auth)
	}
	// raising the cap unblocks the token immediately (and mails "freigegeben")
	api(t, "PUT", srv.URL+"/balancer/api/tokens/litellm", `{"cost_cap":1}`, "")
	waitFor(t, func() bool { code, _ := chatModel(t, srv.URL, "m", auth); return code == 200 })
	waitFor(t, func() bool { return len(smtp.all()) == 2 })
	ev := getEvents(t, srv.URL)
	if len(ev) < 2 || ev[0].Kind != EvTokenUncapped || ev[1].Kind != EvTokenCapped || ev[1].Mail != "gesendet" {
		t.Errorf("events: %+v", ev)
	}
	// switched off by the admin → 403
	api(t, "PUT", srv.URL+"/balancer/api/tokens/litellm", `{"disabled":true}`, "")
	if code, _ := chatModel(t, srv.URL, "m", auth); code != 403 {
		t.Errorf("disabled token: %d", code)
	}
}

func TestServerCostCapTakesServerOutOfRotation(t *testing.T) {
	a := newModelLLM(t, "a", 10*time.Millisecond, "m")
	f := newModelLLM(t, "f", 10*time.Millisecond, "m")
	srv, _ := setup(t, func(c *Config) {
		c.Groups = []GroupConfig{{Name: "g", Models: []string{"m"}, Fallback: "fb", FallbackAfterSec: 60}, {Name: "fb"}}
		c.Backends = []BackendConfig{
			{Name: "a", URL: a.srv.URL, Group: "g", PriceIn: 1, PriceOut: 2, CostCap: 0.003, CapPeriod: "day"},
			{Name: "f", URL: f.srv.URL, Group: "fb"},
		}
	})
	be1 := func() string { _, be := chatModel(t, srv.URL, "m", nil); return be }
	if b := be1(); b != "a" {
		t.Fatalf("first: %s", b)
	}
	be1() // 0.004 ≥ 0.003 → capped
	waitFor(t, func() bool { return status(t, srv.URL).Backends[0].State == "capped" })
	if b := be1(); b != "f" {
		t.Errorf("capped server still used / no fallback: %s", b)
	}
	st := status(t, srv.URL).Backends[0]
	if !st.Capped || st.CapSpent < 0.0039 || st.CostCap != 0.003 || st.CapPeriod != "day" {
		t.Errorf("status: %+v", st)
	}
	// cap removed → back in rotation
	api(t, "PUT", srv.URL+"/balancer/api/backends/a", `{"name":"a","url":"`+a.srv.URL+`","group":"g","cost_cap":0}`, "")
	waitFor(t, func() bool { return status(t, srv.URL).Backends[0].State == "idle" })
	if b := be1(); b != "a" {
		t.Errorf("after removing the cap: %s", b)
	}
}

func TestServerDownMailAfterDelay(t *testing.T) {
	smtp := newFakeSMTP(t)
	a := newModelLLM(t, "a", 10*time.Millisecond, "m")
	srv, pool := setup(t, func(c *Config) { c.Backends = []BackendConfig{{Name: "a", URL: a.srv.URL}} })
	startNotifier(t, pool)
	api(t, "PUT", srv.URL+"/balancer/api/notify", smtpConfigJSON(smtp, `{"server_down":true,"server_up":true}`), "")
	pool.MarkUnhealthy(pool.backends[0], "dial tcp: connection refused")
	pool.notifier.watch(pool) // the loop does this every 5 s
	waitFor(t, func() bool { return len(smtp.all()) == 1 })
	if m := smtp.all()[0]; !strings.Contains(m, "Server nicht erreichbar: a") || !strings.Contains(m, "connection refused") {
		t.Errorf("down mail:\n%s", m)
	}
	pool.SetHealth(pool.backends[0], true, "", pool.backends[0].URL)
	pool.notifier.watch(pool)
	waitFor(t, func() bool { return len(smtp.all()) == 2 })
	if m := smtp.all()[1]; !strings.Contains(m, "Server wieder erreichbar: a") {
		t.Errorf("up mail:\n%s", m)
	}
}

func TestNotifyConfigAPI(t *testing.T) {
	smtp := newFakeSMTP(t)
	dir := t.TempDir()
	srv, _ := setup(t, func(c *Config) { c.DataDir = dir; c.AdminToken = "s3cret" })
	if c := api(t, "PUT", srv.URL+"/balancer/api/notify", smtpConfigJSON(smtp, `{}`), ""); c != 401 {
		t.Errorf("without admin token: %d", c)
	}
	if c := api(t, "PUT", srv.URL+"/balancer/api/notify", `{"enabled":true,"smtp_host":"x"}`, "s3cret"); c != 400 {
		t.Errorf("incomplete config accepted: %d", c)
	}
	api(t, "PUT", srv.URL+"/balancer/api/notify", smtpConfigJSON(smtp, `{}`), "s3cret")
	// password is never sent back, but kept when the field is left out
	_, body := call(t, "GET", srv.URL+"/balancer/api/notify", map[string]string{"X-Admin-Token": "s3cret"}, "")
	if strings.Contains(body, `"pw"`) || !strings.Contains(body, `"has_password":true`) {
		t.Errorf("config view: %s", body)
	}
	host, port, _ := net.SplitHostPort(smtp.addr)
	api(t, "PUT", srv.URL+"/balancer/api/notify", fmt.Sprintf(`{"enabled":true,"smtp_host":%q,"smtp_port":%s,"smtp_security":"none","smtp_username":"bal","from":"a@b.c","to":["x@y.z, z@y.z"]}`, host, port), "s3cret")
	if c := api(t, "POST", srv.URL+"/balancer/api/notify/test", "", "s3cret"); c != 204 {
		t.Fatalf("test mail: %d", c)
	}
	if len(smtp.auth) != 1 || smtp.auth[0] != "|bal|pw" {
		t.Errorf("password not kept: %v", smtp.auth)
	}
	if m := smtp.all()[0]; !strings.Contains(m, "To: x@y.z, z@y.z") {
		t.Errorf("recipients: %s", m)
	}
}

func TestCapAppliesRightAfterRestart(t *testing.T) {
	a := newModelLLM(t, "a", 10*time.Millisecond, "m")
	dir := t.TempDir()
	cfg := func(c *Config) {
		c.DataDir = dir
		c.APIKeys = "litellm=sk-litellm-0123456789"
		c.Backends = []BackendConfig{{Name: "a", URL: a.srv.URL}}
	}
	srv, pool := setup(t, cfg)
	api(t, "PUT", srv.URL+"/balancer/api/tokens/litellm", `{"price_in":1,"price_out":2,"cost_cap":0.001}`, "")
	auth := map[string]string{"Authorization": "Bearer sk-litellm-0123456789"}
	chatModel(t, srv.URL, "m", auth)
	waitFor(t, func() bool { c, _ := chatModel(t, srv.URL, "m", auth); return c == 429 })
	pool.stats.Flush()
	pool.access.Flush()
	// "restart" – the first request must already be blocked
	srv2, pool2 := setup(t, cfg)
	pool2.checkCaps() // what main() does before serving
	if c, _ := chatModel(t, srv2.URL, "m", auth); c != 429 {
		t.Errorf("cap not applied right after restart: %d", c)
	}
}
