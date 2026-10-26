package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"
)

// Counters of the server cards and the tiles at the top, persisted in DATA_DIR/counters.json
// so they survive restarts and image updates (written every minute and on shutdown).

type backendCounters struct {
	Total        int64     `json:"requests"`
	Errors       int64     `json:"errors"`
	Finished     int64     `json:"finished"`
	SumDurMs     int64     `json:"sum_duration_ms"`
	SumTTFBMs    int64     `json:"sum_ttfb_ms"`
	GenTokens    int64     `json:"gen_tokens"`
	GenMs        int64     `json:"gen_ms"`
	PromptTokens int64     `json:"prompt_tokens"`
	PromptMs     int64     `json:"prompt_ms"`
	LastTPS      float64   `json:"last_tps"`
	TagErrors    int64     `json:"tag_errors"`
	LastErr      string    `json:"last_error,omitempty"`
	LastErrAt    time.Time `json:"last_error_at,omitempty"`
	LastOkAt     time.Time `json:"last_ok_at,omitempty"`
}

type countersFile struct {
	Requests int64                      `json:"requests"`
	Rejected int64                      `json:"rejected"`
	MaxQueue int                        `json:"max_queue"`
	Backends map[string]backendCounters `json:"backends"` // by server name
	SavedAt  time.Time                  `json:"saved_at"`
}

func countersPath(dir string) string { return filepath.Join(dir, "counters.json") }

// LoadCounters restores saved counters (servers that no longer exist are skipped).
func (p *Pool) LoadCounters(dir string) error {
	p.countersPath = countersPath(dir)
	data, err := os.ReadFile(p.countersPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var f countersFile
	if err := json.Unmarshal(data, &f); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.totalReqs, p.totalRejected, p.maxQueueSeen = f.Requests, f.Rejected, f.MaxQueue
	for _, b := range p.backends {
		c, ok := f.Backends[b.Name]
		if !ok {
			continue
		}
		b.total, b.errors, b.finished = c.Total, c.Errors, c.Finished
		b.sumDur = time.Duration(c.SumDurMs) * time.Millisecond
		b.sumTTFB = time.Duration(c.SumTTFBMs) * time.Millisecond
		b.genTokens, b.genTime = c.GenTokens, time.Duration(c.GenMs)*time.Millisecond
		b.promptTokens, b.promptTime = c.PromptTokens, time.Duration(c.PromptMs)*time.Millisecond
		b.lastTPS, b.tagErrors = c.LastTPS, c.TagErrors
		b.lastErr, b.lastErrAt, b.lastOkAt = c.LastErr, c.LastErrAt, c.LastOkAt
	}
	p.countersSaved = countersSnapshot(f)
	return nil
}

func countersSnapshot(f countersFile) []byte {
	f.SavedAt = time.Time{}
	b, _ := json.Marshal(f)
	return b
}

// SaveCounters writes the counters if they changed since the last write.
func (p *Pool) SaveCounters() {
	if p.countersPath == "" {
		return
	}
	p.mu.Lock()
	f := countersFile{Requests: p.totalReqs, Rejected: p.totalRejected, MaxQueue: p.maxQueueSeen,
		Backends: map[string]backendCounters{}}
	for _, b := range p.backends {
		// requests still running are counted in total but not yet finished; that's what the card shows too
		f.Backends[b.Name] = backendCounters{
			Total: b.total, Errors: b.errors, Finished: b.finished,
			SumDurMs: b.sumDur.Milliseconds(), SumTTFBMs: b.sumTTFB.Milliseconds(),
			GenTokens: b.genTokens, GenMs: b.genTime.Milliseconds(),
			PromptTokens: b.promptTokens, PromptMs: b.promptTime.Milliseconds(),
			LastTPS: b.lastTPS, TagErrors: b.tagErrors,
			LastErr: b.lastErr, LastErrAt: b.lastErrAt, LastOkAt: b.lastOkAt,
		}
	}
	snap := countersSnapshot(f)
	if bytes.Equal(snap, p.countersSaved) {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	f.SavedAt = time.Now()
	data, _ := json.MarshalIndent(f, "", "  ")
	if err := writeFileAtomic(p.countersPath, data, 0o644); err != nil {
		log.Printf("counters: %v", err)
		return
	}
	p.mu.Lock()
	p.countersSaved = snap
	p.mu.Unlock()
}

// ResetCounters clears the card counters and tiles (together with "Statistik löschen").
func (p *Pool) ResetCounters() {
	p.mu.Lock()
	p.totalReqs, p.totalRejected, p.maxQueueSeen = 0, 0, len(p.queue)
	for _, b := range p.backends {
		b.total, b.errors, b.finished = int64(b.active), 0, 0
		b.sumDur, b.sumTTFB = 0, 0
		b.genTokens, b.genTime, b.promptTokens, b.promptTime = 0, 0, 0, 0
		b.lastTPS, b.tagErrors = 0, 0
		b.lastErr, b.lastErrAt = "", time.Time{}
	}
	p.mu.Unlock()
	p.SaveCounters()
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// persistAll writes statistics, token usage and counters (each only if changed).
func persistAll(pool *Pool, access *Access) {
	if pool.stats != nil {
		pool.stats.Flush()
	}
	if access != nil {
		access.Flush()
	}
	pool.SaveCounters()
}
