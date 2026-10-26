package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestInspectSSE(t *testing.T) {
	cases := []struct {
		in   string
		eof  bool
		want peekVerdict
	}{
		{": keep-alive\n\n", false, peekMore},
		{"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n", false, peekOK},
		{"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{}}\n\n", false, peekOK},
		{"data: {\"error\":{\"message\":\"x\"}}\n\n", false, peekBad},
		{"event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"overloaded\"}}\n\n", false, peekBad},
		{"data: [DONE]\n\n", false, peekBad},
		{"", true, peekBad},
		{"data: {\"choices\":[]", false, peekMore},
	}
	for _, c := range cases {
		if v, msg := inspectSSE([]byte(c.in), c.eof); v != c.want {
			t.Errorf("%q: got %v (%s), want %v", c.in, v, msg, c.want)
		}
	}
	if v, _ := inspectJSON([]byte(`{"id":"x","choices":[],"error":null}`), true); v != peekOK {
		t.Error("error:null must be ok")
	}
	if v, _ := inspectJSON([]byte("{\n  \"choices\": []\n}"), true); v != peekOK {
		t.Error("pretty printed JSON must be ok")
	}
}

// The check must not delay streaming: the first token reaches the client as soon as the server sends it.
func TestPeekDoesNotDelayFirstToken(t *testing.T) {
	srv := newSSEServer(t, 300*time.Millisecond, 5, 200*time.Millisecond, "hello", 10, 5)
	bal, _ := setup(t, func(c *Config) { c.Backends = []BackendConfig{{Name: "s", URL: srv.URL}} })
	start := time.Now()
	resp, err := http.Post(bal.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	if el := time.Since(start); !strings.Contains(string(buf[:n]), "hello") || el > 450*time.Millisecond {
		t.Errorf("first token after %v: %q", el, buf[:n])
	}
}

// An error event after real content can't be redirected any more, but it is recorded.
func TestMidStreamErrorRecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"error\":{\"message\":\"out of memory\"}}\n\n")
	}))
	defer srv.Close()
	bal, _ := setup(t, func(c *Config) { c.Backends = []BackendConfig{{Name: "s", URL: srv.URL}} })
	streamReq(bal.URL)
	if r := lastRecent(t, bal.URL); !strings.Contains(r.Error, "out of memory") || !strings.Contains(r.Error, "nicht mehr umleitbar") {
		t.Errorf("error not recorded: %q", r.Error)
	}
}
