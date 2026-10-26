package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCountersSurviveRestart(t *testing.T) {
	a := newModelLLM(t, "a", 20*time.Millisecond, "m")
	dir := t.TempDir()
	srv, pool := setup(t, func(c *Config) { c.Backends = []BackendConfig{{Name: "a", URL: a.srv.URL}} })
	if err := pool.LoadCounters(dir); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		chatModel(t, srv.URL, "m", nil)
	}
	waitFor(t, func() bool { return status(t, srv.URL).Backends[0].Total == 3 && len(status(t, srv.URL).Recent) == 3 })
	pool.SaveCounters()
	fi, err := os.Stat(filepath.Join(dir, "counters.json"))
	if err != nil {
		t.Fatal(err)
	}
	// unchanged → not rewritten
	time.Sleep(20 * time.Millisecond)
	pool.SaveCounters()
	if fi2, _ := os.Stat(filepath.Join(dir, "counters.json")); !fi2.ModTime().Equal(fi.ModTime()) {
		t.Error("file rewritten although nothing changed")
	}

	// "restart": a fresh pool with the same data dir
	srv2, pool2 := setup(t, func(c *Config) { c.Backends = []BackendConfig{{Name: "a", URL: a.srv.URL}} })
	if err := pool2.LoadCounters(dir); err != nil {
		t.Fatal(err)
	}
	st := status(t, srv2.URL)
	b := st.Backends[0]
	if st.TotalReqs != 3 || b.Total != 3 || b.AvgDurMs == 0 || b.AvgTPS != 0 && b.AvgTPS < 1 {
		t.Errorf("restored: total=%d card=%+v", st.TotalReqs, b)
	}
	// stats reset clears the counters as well
	if c := api(t, "POST", srv2.URL+"/balancer/api/stats/reset", "", ""); c != 204 {
		t.Fatalf("reset: %d", c)
	}
	if st := status(t, srv2.URL); st.TotalReqs != 0 || st.Backends[0].Total != 0 {
		t.Errorf("after reset: %d %d", st.TotalReqs, st.Backends[0].Total)
	}
}
