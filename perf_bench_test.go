package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime/pprof"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestPerfHarness(t *testing.T) {
	if os.Getenv("PERF") == "" {
		t.Skip()
	}
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Write([]byte(`{"data":[{"id":"m"}]}`))
			return
		}
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 60; i++ { // 40 tok/s for 1.5 s (agent turn)
			fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-abc\",\"object\":\"chat.completion.chunk\",\"created\":1730000000,\"model\":\"m\",\"system_fingerprint\":\"b1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" wort%d\"},\"logprobs\":null,\"finish_reason\":null}],\"timings\":{\"prompt_n\":1,\"predicted_n\":%d}}\n\n", i, i)
			w.(http.Flusher).Flush()
			time.Sleep(25 * time.Millisecond)
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":90000,\"completion_tokens\":300}}\n\ndata: [DONE]\n\n")
	}))
	defer llm.Close()
	srv, _ := setup(t, func(c *Config) {
		c.Backends = []BackendConfig{{Name: "a", URL: llm.URL}, {Name: "b", URL: llm.URL}, {Name: "c", URL: llm.URL}}
	})
	big := agentBody(250) // ~1 MB agent history
	var tmp map[string]any
	json.Unmarshal(big, &tmp)
	msgs, _ := tmp["messages"].([]any)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for s := 0; s < 3; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			for k := 0; ; k++ {
				select {
				case <-stop:
					return
				default:
				}
				m := append(append([]any{}, msgs...), map[string]any{"role": "user", "content": fmt.Sprintf("Schritt %d/%d", s, k)})
				body, _ := json.Marshal(map[string]any{"model": "m", "stream": true, "messages": m})
				resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(string(body)))
				if err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}
		}(s)
	}
	poll := func(path string, every time.Duration) {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(every):
			}
			if resp, err := http.Get(srv.URL + "/balancer" + path); err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}
	}
	for tab := 0; tab < 2; tab++ {
		wg.Add(2)
		go poll("/api/status", time.Second)
		go poll("/api/recent?after=0", 2*time.Second)
	}
	time.Sleep(2 * time.Second)
	f, _ := os.Create(os.Getenv("PERF"))
	var ru0, ru1 syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru0)
	pprof.StartCPUProfile(f)
	time.Sleep(20 * time.Second)
	pprof.StopCPUProfile()
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru1)
	cpu := time.Duration(ru1.Utime.Nano() + ru1.Stime.Nano() - ru0.Utime.Nano() - ru0.Stime.Nano())
	t.Logf("CPU %v in 20 s = %.1f %% of one core (includes the fake server and clients)", cpu, cpu.Seconds()/20*100)
	close(stop)
	wg.Wait()
}
