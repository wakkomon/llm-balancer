package main

import (
	"encoding/binary"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// miniDNS answers A queries for one name with 127.0.0.1 and NXDOMAIN for everything else.
func miniDNS(t *testing.T, name string) (addr string, queries *atomic.Int32) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	queries = &atomic.Int32{}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			queries.Add(1)
			q := buf[:n]
			// question name starts at byte 12
			var labels []string
			i := 12
			for i < n && q[i] != 0 {
				l := int(q[i])
				labels = append(labels, string(q[i+1:i+1+l]))
				i += 1 + l
			}
			qEnd := i + 5 // zero byte + type + class
			qtype := binary.BigEndian.Uint16(q[i+1 : i+3])
			resp := make([]byte, 0, 512)
			resp = append(resp, q[0], q[1]) // id
			match := strings.EqualFold(strings.Join(labels, "."), name)
			rcode := byte(0)
			if !match {
				rcode = 3
			}
			ans := 0
			if match && qtype == 1 {
				ans = 1
			}
			resp = append(resp, 0x81, 0x80|rcode, 0, 1, 0, byte(ans), 0, 0, 0, 0)
			resp = append(resp, q[12:qEnd]...)
			if ans == 1 {
				resp = append(resp, 0xc0, 12, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, 127, 0, 0, 1)
			}
			pc.WriteTo(resp, from)
		}
	}()
	return pc.LocalAddr().String(), queries
}

func TestCustomDNSServers(t *testing.T) {
	dnsAddr, queries := miniDNS(t, "llm.intern.test")
	backend := newModelLLM(t, "b", 10*time.Millisecond, "m")
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(backend.srv.URL, "http://"))
	srv, _ := setup(t, func(c *Config) {
		c.DNSServers = dnsAddr
		c.Backends = []BackendConfig{{Name: "intern", URL: "http://llm.intern.test:" + port}}
	})
	if code, be := chatModel(t, srv.URL, "m", nil); code != 200 || be != "intern" {
		t.Fatalf("name via DNS_SERVERS: %d %q", code, be)
	}
	if queries.Load() == 0 {
		t.Error("custom DNS server was not asked")
	}
	// "localhost" is unknown to the custom server → falls back to the container resolver
	if c := api(t, "PUT", srv.URL+"/balancer/api/backends/intern", `{"name":"intern","url":"http://localhost:`+port+`"}`, ""); c != 204 {
		t.Fatal(c)
	}
	if code, _ := chatModel(t, srv.URL, "m", nil); code != 200 {
		t.Errorf("fallback to system resolver: %d", code)
	}
}

func TestParseDNSServers(t *testing.T) {
	got, err := parseDNSServers(" 192.168.1.1 , 1.1.1.1:5353,[fd00::1]")
	if err != nil || strings.Join(got, ",") != "192.168.1.1:53,1.1.1.1:5353,[fd00::1]:53" {
		t.Errorf("%v %v", got, err)
	}
	if _, err := parseDNSServers("router.lan"); err == nil {
		t.Error("hostname accepted")
	}
	_ = http.StatusOK
}
