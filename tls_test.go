package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPSBackendSelfSigned(t *testing.T) {
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "m"}}})
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer tlsSrv.Close()
	srv, pool := setup(t, func(c *Config) {
		c.HealthPath = "/v1/models"
		c.Backends = []BackendConfig{{Name: "secure", URL: tlsSrv.URL}}
	})
	// certificate is self-signed → request fails with a readable hint
	code, _ := chatModel(t, srv.URL, "m", nil)
	st := status(t, srv.URL)
	if code != 502 || !strings.Contains(st.Backends[0].LastError, "Zertifikat nicht prüfen") {
		t.Fatalf("expected certificate error: %d %q", code, st.Backends[0].LastError)
	}
	// enable "don't verify" → works, including health check and model discovery
	if c := api(t, "PUT", srv.URL+"/balancer/api/backends/secure", `{"name":"secure","url":"`+tlsSrv.URL+`","tls_skip_verify":true}`, ""); c != 204 {
		t.Fatalf("update: %d", c)
	}
	if code, be := chatModel(t, srv.URL, "m", nil); code != 200 || be != "secure" {
		t.Fatalf("with skip verify: %d %s", code, be)
	}
	// the response reaches the client a moment before the slot is released; discovery skips busy servers
	waitFor(t, func() bool { return status(t, srv.URL).Backends[0].Active == 0 })
	px := NewProxy(pool.cfg, pool)
	for _, ht := range pool.idleTargets() {
		px.discoverModels(nil, ht, nil)
	}
	if st := status(t, srv.URL); !st.Backends[0].TLSSkip || len(st.Backends[0].Models) != 1 {
		t.Errorf("status: %+v", st.Backends[0])
	}
}

func TestDNSErrorHint(t *testing.T) {
	srv, pool := setup(t, func(c *Config) {
		c.HealthPath = "/v1/models"
		c.Backends = []BackendConfig{{Name: "x", URL: "https://does-not-exist.invalid:8443"}}
	})
	px := NewProxy(pool.cfg, pool)
	ctx, cancel := contextWithCancel()
	defer cancel()
	go px.healthLoop(ctx)
	waitFor(t, func() bool { return status(t, srv.URL).Backends[0].State == "down" })
	if msg := status(t, srv.URL).Backends[0].HealthMsg; !strings.Contains(msg, "DNS-Auflösung") {
		t.Errorf("hint missing: %q", msg)
	}
	_ = time.Second
}
