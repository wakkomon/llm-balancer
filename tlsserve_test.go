package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestHTTPSPortSelfSigned(t *testing.T) {
	a := newFakeLLM(t, "a", 0)
	dir := t.TempDir()
	srv, pool := setup(t, func(c *Config) { c.DataDir = dir }, a)
	cfg := pool.cfg
	cfg.DataDir, cfg.TLSHosts = dir, []string{"balancer.lan", "192.168.1.20"}
	m, err := newTLSManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	in := m.info()
	for _, h := range []string{"localhost", "balancer.lan", "192.168.1.20", "127.0.0.1"} {
		if !slices.Contains(in.Hosts, h) {
			t.Errorf("certificate lacks %s: %v", h, in.Hosts)
		}
	}
	if d := time.Until(in.NotAfter); d < 800*24*time.Hour || d > 826*24*time.Hour {
		t.Errorf("validity %v", d)
	}
	if st, err := os.Stat(filepath.Join(dir, "tls", "key.pem")); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("key file: %v %v", err, st)
	}
	// restart: same certificate
	m2, _ := newTLSManager(cfg)
	if m2.info().Fingerprint != in.Fingerprint {
		t.Error("certificate not reused after restart")
	}
	// a new TLS_HOSTS entry → new certificate
	cfg.TLSHosts = append(cfg.TLSHosts, "llm.example")
	m3, _ := newTLSManager(cfg)
	if m3.info().Fingerprint == in.Fingerprint || !slices.Contains(m3.info().Hosts, "llm.example") {
		t.Error("certificate not renewed for a new host")
	}

	// the https port serves the same handler; a client trusting the certificate gets through
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := newHTTPSServer("", srv.Config.Handler, m3)
	go hs.ServeTLS(ln, "", "")
	defer hs.Close()
	pool2 := x509.NewCertPool()
	pool2.AppendCertsFromPEM(m3.pemCert)
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool2}}}
	base := "https://" + ln.Addr().String()
	resp, err := c.Post(base+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("X-Llm-Balancer-Backend") != "a" {
		t.Errorf("via https: %d %s %v", resp.StatusCode, body, resp.Header)
	}
	if _, err := http.Get(base + "/balancer/healthz"); err == nil {
		t.Error("an untrusting client must fail the handshake")
	}
}

func TestTLSRegenerateAndAPI(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.DataDir = dir
	m, _ := newTLSManager(cfg)
	if err := m.regenerate([]string{"nas.lan", "10.0.0.7"}); err != nil {
		t.Fatal(err)
	}
	if err := m.regenerate([]string{"bad name"}); err == nil {
		t.Error("invalid name accepted")
	}
	m2, _ := newTLSManager(cfg) // extra names persist
	if !slices.Contains(m2.info().Hosts, "nas.lan") || !slices.Contains(m2.info().Extra, "10.0.0.7") {
		t.Errorf("extra hosts: %+v", m2.info())
	}
	a := newFakeLLM(t, "a", 0)
	srv, _ := setup(t, func(c *Config) { c.DataDir = t.TempDir() }, a)
	resp, _ := http.Get(srv.URL + "/balancer/api/tls")
	var in TLSInfo
	json.NewDecoder(resp.Body).Decode(&in)
	resp.Body.Close()
	if in.Enabled { // setup() does not start the https port
		t.Errorf("tls info: %+v", in)
	}
}

func TestTLSFromFiles(t *testing.T) {
	dir := t.TempDir()
	gen := defaultConfig()
	gen.DataDir = dir
	g, _ := newTLSManager(gen) // use a generated pair as "own" files
	cfg := defaultConfig()
	cfg.TLSCert, cfg.TLSKey = filepath.Join(dir, "tls", "cert.pem"), filepath.Join(dir, "tls", "key.pem")
	m, err := newTLSManager(cfg)
	if err != nil || m.info().Source != "files" || m.info().Fingerprint != g.info().Fingerprint {
		t.Fatalf("files: %v %+v", err, m.info())
	}
	if m.regenerate(nil) == nil {
		t.Error("regenerate must refuse own certificate files")
	}
	cfg.TLSKey = ""
	if _, err := newTLSManager(cfg); err == nil {
		t.Error("cert without key accepted")
	}
}
