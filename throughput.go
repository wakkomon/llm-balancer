package main

import "time"

// Live throughput: generated tokens per second over the last few seconds, in total and per API token
// (and per server). Streamed chunks count while a request runs (≈ one token each); when it finishes,
// the reported output tokens replace the estimate.

const tpsWindow = 10 // seconds

type tpsSample struct {
	at  time.Time
	cum map[string]int64 // key → cumulative output tokens
}

type LiveLoad struct {
	Running int     `json:"running"`
	Queued  int     `json:"queued"`
	TPS     float64 `json:"tps"` // generated tokens/s, average over the last seconds
}

type LiveStatus struct {
	LiveLoad
	Tokens   map[string]*LiveLoad `json:"tokens"`   // API token name ("" = without token)
	Backends map[string]*LiveLoad `json:"backends"` // server name
}

func tpsKeys(r *ReqInfo) []string { return []string{"t:" + r.Token, "b:" + r.Backend} }

func reqOut(r *ReqInfo) int64 { return max(r.chunks.Load(), r.TokensOut) }

// finishedOutLocked books a finished request (caller holds p.mu, r is still in p.active).
func (p *Pool) finishedOutLocked(r *ReqInfo) {
	if p.doneOut == nil {
		p.doneOut = map[string]int64{}
	}
	n := reqOut(r)
	for _, k := range tpsKeys(r) {
		p.doneOut[k] += n
	}
	p.doneOut["all"] += n
}

// sampleTPSLocked is called once a second.
func (p *Pool) sampleTPSLocked(now time.Time) {
	cum := map[string]int64{}
	for k, v := range p.doneOut {
		cum[k] = v
	}
	for _, r := range p.active {
		if r.State != "running" {
			continue
		}
		n := r.chunks.Load()
		for _, k := range tpsKeys(r) {
			cum[k] += n
		}
		cum["all"] += n
	}
	p.tpsHist = append(p.tpsHist, tpsSample{now, cum})
	if len(p.tpsHist) > tpsWindow+1 {
		p.tpsHist = p.tpsHist[len(p.tpsHist)-tpsWindow-1:]
	}
}

func (p *Pool) tpsLocked(key string) float64 {
	if len(p.tpsHist) < 2 {
		return 0
	}
	a, b := p.tpsHist[0], p.tpsHist[len(p.tpsHist)-1]
	sec := b.at.Sub(a.at).Seconds()
	if sec <= 0 {
		return 0
	}
	d := float64(b.cum[key] - a.cum[key])
	if d < 0 {
		d = 0
	}
	return d / sec
}

func (p *Pool) liveLocked() LiveStatus {
	ls := LiveStatus{Tokens: map[string]*LiveLoad{}, Backends: map[string]*LiveLoad{}}
	get := func(m map[string]*LiveLoad, k string) *LiveLoad {
		if m[k] == nil {
			m[k] = &LiveLoad{}
		}
		return m[k]
	}
	for _, r := range p.active {
		t := get(ls.Tokens, r.Token)
		switch r.State {
		case "running":
			t.Running++
			ls.Running++
			if r.Backend != "" {
				get(ls.Backends, r.Backend).Running++
			}
		case "queued":
			t.Queued++
			ls.Queued++
		}
	}
	ls.TPS = p.tpsLocked("all")
	if len(p.tpsHist) > 0 { // tokens/servers that produced output recently, even if idle now
		last := p.tpsHist[len(p.tpsHist)-1].cum
		for k := range last {
			if v := p.tpsLocked(k); v > 0 {
				switch k[:2] {
				case "t:":
					get(ls.Tokens, k[2:]).TPS = v
				case "b:":
					get(ls.Backends, k[2:]).TPS = v
				}
			}
		}
	}
	return ls
}
