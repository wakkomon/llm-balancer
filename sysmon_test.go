package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSysMonitorParses(t *testing.T) {
	dir := t.TempDir()
	write := func(name, s string) {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
		os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644)
	}
	write("proc/stat", "cpu  100 0 100 800 0 0 0 0 0 0\ncpu0 50 0 50 400 0 0 0 0 0 0\ncpu1 50 0 50 400 0 0 0 0 0 0\nintr 1\n")
	write("proc/self/stat", "42 (llm balancer) S 1 1 1 0 -1 0 0 0 0 0 100 50 0 0 20 0 9 0")
	write("proc/loadavg", "0.50 0.40 0.30 1/100 42\n")
	write("proc/self/status", "Name:\tllmb\nVmRSS:\t   20480 kB\n")
	write("proc/meminfo", "MemTotal:       8000000 kB\nMemFree: 1 kB\nMemAvailable:   6000000 kB\n")
	write("cg/cpu.max", "100000 100000\n")
	write("cg/memory.max", "max\n")
	m := &sysMonitor{proc: filepath.Join(dir, "proc"), cg: filepath.Join(dir, "cg")}
	m.sample()
	m.last.at = m.last.at.Add(-2 * time.Second) // pretend 2 s passed
	write("proc/stat", "cpu  200 0 200 1400 0 0 0 0 0 0\ncpu0 1 0 1 1 0 0 0 0 0 0\ncpu1 1 0 1 1 0 0 0 0 0 0\n")
	write("proc/self/stat", "42 (llm balancer) S 1 1 1 0 -1 0 0 0 0 0 150 100 0 0 20 0 9 0")
	m.sample()
	st := m.cur
	if !st.Available || st.Cores != 2 || st.Load[0] != 0.5 || st.ProcRSS != 20480*1024 || st.MemAvail != 6000000*1024 || st.CPULimit != 1 || st.MemLimit != 0 {
		t.Fatalf("parsed: %+v", st)
	}
	// host: busy +200 of +800 ticks = 25 %; process: 100 ticks in ~2 s = ~50 % of one core, limit 1 core
	if st.HostCPU != 25 || st.ProcCPU < 45 || st.ProcCPU > 55 || st.ProcShare != st.ProcCPU || st.PeakHost != 25 {
		t.Errorf("cpu: %+v", st)
	}
}

func TestTotalSlotsOnlyEnabled(t *testing.T) {
	a, b := newFakeLLM(t, "a", 0), newFakeLLM(t, "b", 0)
	_, pool := setup(t, func(c *Config) { c.Backends[1].MaxConcurrent = 2 }, a, b)
	if st := pool.Status(); st.TotalSlots != 3 || st.FreeSlots != 3 {
		t.Fatalf("before: %d/%d", st.FreeSlots, st.TotalSlots)
	}
	pool.SetEnabled("b", false)
	if st := pool.Status(); st.TotalSlots != 1 || st.FreeSlots != 1 {
		t.Errorf("disabled server still counted: %d/%d", st.FreeSlots, st.TotalSlots)
	}
}
