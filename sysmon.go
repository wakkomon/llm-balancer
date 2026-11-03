package main

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Load of the machine the balancer runs on – to spot an overloaded balancer host (all those streams
// and the UI polling cost CPU too). Read from /proc every 2 s; inside a container /proc/stat and
// /proc/loadavg describe the whole host, /proc/self the balancer process.

type SysStatus struct {
	Available  bool       `json:"available"`           // false: no /proc (not Linux)
	ProcCPU    float64    `json:"proc_cpu"`            // balancer process, % of one core
	ProcShare  float64    `json:"proc_cpu_share"`      // balancer process, % of the CPUs it may use
	HostCPU    float64    `json:"host_cpu"`            // whole host, % of all cores
	Cores      int        `json:"cores"`               // cores of the host
	CPULimit   float64    `json:"cpu_limit,omitempty"` // container CPU limit in cores (cgroup), 0 = none
	Load       [3]float64 `json:"load"`                // load average 1/5/15 min
	ProcRSS    int64      `json:"proc_rss"`            // bytes
	MemTotal   int64      `json:"mem_total"`           // host, bytes
	MemAvail   int64      `json:"mem_available"`       // host, bytes
	MemLimit   int64      `json:"mem_limit,omitempty"` // container memory limit (cgroup), bytes
	Goroutines int        `json:"goroutines"`
	PeakProc   float64    `json:"proc_cpu_peak"` // highest ProcShare of the last 5 minutes
	PeakHost   float64    `json:"host_cpu_peak"` // highest HostCPU of the last 5 minutes
}

type sysSample struct {
	at                  time.Time
	procTicks           uint64
	hostBusy, hostTotal uint64
	procShare, hostCPU  float64
}

type sysMonitor struct {
	once sync.Once
	mu   sync.Mutex
	last sysSample
	hist []sysSample // last 5 minutes
	cur  SysStatus
	proc string // /proc, replaceable in tests
	cg   string // /sys/fs/cgroup
}

var sysMon = &sysMonitor{proc: "/proc", cg: "/sys/fs/cgroup"}

const clockTicks = 100 // USER_HZ, 100 on every Linux the image runs on

func (m *sysMonitor) get() SysStatus {
	m.once.Do(func() {
		m.sample()
		go func() {
			for range time.Tick(2 * time.Second) {
				m.sample()
			}
		}()
	})
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.cur
	c.Goroutines = runtime.NumGoroutine()
	return c
}

func (m *sysMonitor) read(name string) string {
	b, err := os.ReadFile(name)
	if err != nil {
		return ""
	}
	return string(b)
}

func (m *sysMonitor) sample() {
	now := time.Now()
	st := SysStatus{}
	s := sysSample{at: now}

	stat := m.read(m.proc + "/stat")
	for _, line := range strings.Split(stat, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		if f[0] == "cpu" {
			var total, idle uint64
			for i, v := range f[1:] {
				n, _ := strconv.ParseUint(v, 10, 64)
				if i >= 8 { // guest times are already part of user/nice
					break
				}
				total += n
				if i == 3 || i == 4 { // idle, iowait
					idle += n
				}
			}
			s.hostTotal, s.hostBusy = total, total-idle
		} else if strings.HasPrefix(f[0], "cpu") {
			st.Cores++
		}
	}
	st.Available = stat != ""
	if st.Cores == 0 {
		st.Cores = runtime.NumCPU()
	}
	// utime + stime; the command name (field 2) may contain spaces – count from the closing parenthesis
	if raw := m.read(m.proc + "/self/stat"); strings.LastIndexByte(raw, ')') > 0 {
		if g := strings.Fields(raw[strings.LastIndexByte(raw, ')')+1:]); len(g) > 12 {
			ut, _ := strconv.ParseUint(g[11], 10, 64)
			stt, _ := strconv.ParseUint(g[12], 10, 64)
			s.procTicks = ut + stt
		}
	}
	if f := strings.Fields(m.read(m.proc + "/loadavg")); len(f) >= 3 {
		for i := 0; i < 3; i++ {
			st.Load[i], _ = strconv.ParseFloat(f[i], 64)
		}
	}
	sc := bufio.NewScanner(strings.NewReader(m.read(m.proc + "/self/status")))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "VmRSS:"); ok {
			st.ProcRSS = kbField(v)
		}
	}
	sc = bufio.NewScanner(strings.NewReader(m.read(m.proc + "/meminfo")))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "MemTotal:"); ok {
			st.MemTotal = kbField(v)
		} else if v, ok := strings.CutPrefix(sc.Text(), "MemAvailable:"); ok {
			st.MemAvail = kbField(v)
		}
	}
	// container limits (cgroup v2)
	if f := strings.Fields(m.read(m.cg + "/cpu.max")); len(f) == 2 && f[0] != "max" {
		q, _ := strconv.ParseFloat(f[0], 64)
		per, _ := strconv.ParseFloat(f[1], 64)
		if per > 0 {
			st.CPULimit = q / per
		}
	}
	if v := strings.TrimSpace(m.read(m.cg + "/memory.max")); v != "" && v != "max" {
		st.MemLimit, _ = strconv.ParseInt(v, 10, 64)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if prev := m.last; !prev.at.IsZero() {
		if secs := now.Sub(prev.at).Seconds(); secs > 0 && s.procTicks >= prev.procTicks {
			st.ProcCPU = float64(s.procTicks-prev.procTicks) / clockTicks / secs * 100
		}
		if dt := s.hostTotal - prev.hostTotal; s.hostTotal > prev.hostTotal && s.hostBusy >= prev.hostBusy {
			st.HostCPU = float64(s.hostBusy-prev.hostBusy) / float64(dt) * 100
		}
	}
	usable := float64(min(st.Cores, runtime.NumCPU()))
	if st.CPULimit > 0 && st.CPULimit < usable {
		usable = st.CPULimit
	}
	if usable > 0 {
		st.ProcShare = st.ProcCPU / usable
	}
	s.procShare, s.hostCPU = st.ProcShare, st.HostCPU
	m.last = s
	m.hist = append(m.hist, s)
	for len(m.hist) > 0 && now.Sub(m.hist[0].at) > 5*time.Minute {
		m.hist = m.hist[1:]
	}
	for _, h := range m.hist {
		st.PeakProc = max(st.PeakProc, h.procShare)
		st.PeakHost = max(st.PeakHost, h.hostCPU)
	}
	m.cur = st
}

func kbField(v string) int64 {
	f := strings.Fields(v)
	if len(f) == 0 {
		return 0
	}
	n, _ := strconv.ParseInt(f[0], 10, 64)
	return n * 1024
}
