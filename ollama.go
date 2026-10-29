package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Ollama support:
//   - server type auto/openai/ollama; "auto" detects Ollama via GET /api/version during the health check
//   - the native Ollama API at the balancer: /api/chat, /api/generate, /api/embed(dings) go through the
//     slot logic to Ollama servers only; /api/tags, /api/ps, /api/version are answered from all Ollama servers
//   - model management (list, pull with progress, delete, unload) in the web UI

func (b *Backend) isOllama() bool {
	return b.kind == "ollama" || (b.kind == "" && b.ollamaVer != "")
}

// ---- detection and model discovery (health check) ----

type ollamaTag struct {
	Name       string `json:"name"`
	Model      string `json:"model"`
	ModifiedAt string `json:"modified_at"`
	Size       int64  `json:"size"`
	Digest     string `json:"digest"`
	Details    struct {
		Format            string   `json:"format"`
		Family            string   `json:"family"`
		Families          []string `json:"families"`
		ParameterSize     string   `json:"parameter_size"`
		QuantizationLevel string   `json:"quantization_level"`
	} `json:"details"`
}

type ollamaPS struct {
	Name      string `json:"name"`
	Model     string `json:"model"`
	Size      int64  `json:"size"`
	SizeVRAM  int64  `json:"size_vram"`
	ExpiresAt string `json:"expires_at"`
	Context   int    `json:"context_length,omitempty"`
}

func (p *Pool) kindOf(b *Backend) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return b.kind
}

func ollamaGet(ctx context.Context, client *http.Client, base, key, path string, v any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s → %d %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, v)
}

// probeOllama runs after a successful health check (servers of type auto or ollama). It returns true if the
// server is an Ollama server; its model list then comes from /api/tags.
func (px *Proxy) probeOllama(client *http.Client, ht healthTarget) bool {
	kind := px.pool.kindOf(ht.b)
	if kind == "openai" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var ver struct {
		Version string `json:"version"`
	}
	if err := ollamaGet(ctx, client, ht.url, ht.key, "/api/version", &ver); err != nil || ver.Version == "" {
		if kind != "ollama" {
			px.pool.setOllama(ht.b, ht.url, "", nil, nil)
			return false
		}
	}
	var tags struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := ollamaGet(ctx, client, ht.url, ht.key, "/api/tags", &tags); err != nil {
		return false
	}
	var ps struct {
		Models []json.RawMessage `json:"models"`
	}
	_ = ollamaGet(ctx, client, ht.url, ht.key, "/api/ps", &ps)
	px.pool.setOllama(ht.b, ht.url, cmp.Or(ver.Version, "?"), tags.Models, ps.Models)
	return true
}

func (p *Pool) setOllama(b *Backend, checkedURL, ver string, tags, ps []json.RawMessage) {
	var names, loaded []string
	for _, raw := range tags {
		var t ollamaTag
		if json.Unmarshal(raw, &t) == nil && t.Name != "" {
			names = append(names, t.Name)
		}
	}
	for _, raw := range ps {
		var t ollamaPS
		if json.Unmarshal(raw, &t) == nil && t.Name != "" {
			loaded = append(loaded, t.Name)
		}
	}
	p.mu.Lock()
	if b.URL != checkedURL {
		p.mu.Unlock()
		return
	}
	if b.ollamaVer == "" && ver != "" {
		log.Printf("backend %s is an Ollama server (version %s)", b.Name, ver)
	}
	b.ollamaVer, b.ollamaTags, b.ollamaPS, b.loaded = ver, tags, ps, loaded
	p.mu.Unlock()
	if ver != "" {
		p.SetModels(b, names, checkedURL)
	}
}

// ---- native Ollama API at the balancer ----

// ollamaManagePaths change the model store of a server: only through the balancer UI.
var ollamaManagePaths = map[string]bool{"/api/pull": true, "/api/push": true, "/api/create": true, "/api/copy": true, "/api/delete": true}

func isOllamaPath(path string) bool { return strings.HasPrefix(path, "/api/") }

func (p *Pool) hasOllama() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, b := range p.backends {
		if b.isOllama() {
			return true
		}
	}
	return false
}

// nonOllama returns the servers to skip for a native Ollama request.
func (p *Pool) nonOllama() map[*Backend]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[*Backend]bool{}
	for _, b := range p.backends {
		if !b.isOllama() {
			out[b] = true
		}
	}
	return out
}

func ollamaError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": "llm-balancer: " + msg})
}

// serveOllamaInfo answers /api/tags, /api/ps and /api/version from the Ollama servers (true = handled).
func (px *Proxy) serveOllamaInfo(w http.ResponseWriter, r *http.Request, path string) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	p := px.pool
	switch path {
	case "/api/tags", "/api/ps":
		p.mu.Lock()
		now := time.Now()
		seen := map[string]bool{}
		list := []json.RawMessage{}
		for _, b := range p.backends {
			if !b.isOllama() || !b.usable(now) {
				continue
			}
			src := b.ollamaTags
			if path == "/api/ps" {
				src = b.ollamaPS
			}
			for _, raw := range src {
				var t struct{ Name string }
				if json.Unmarshal(raw, &t) == nil && !seen[strings.ToLower(t.Name)] {
					seen[strings.ToLower(t.Name)] = true
					list = append(list, raw)
				}
			}
		}
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"models": list})
		return true
	case "/api/version":
		p.mu.Lock()
		ver := ""
		for _, b := range p.backends {
			if b.isOllama() && b.ollamaVer != "" && b.ollamaVer != "?" {
				ver = b.ollamaVer
				break
			}
		}
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"version": cmp.Or(ver, "0.0.0")})
		return true
	}
	return false
}

// ---- model management ----

type pullJob struct {
	ID        int       `json:"id"`
	Server    string    `json:"server"`
	Model     string    `json:"model"`
	State     string    `json:"state"`  // running | done | failed | cancelled
	Status    string    `json:"status"` // last status line of Ollama
	Completed int64     `json:"completed"`
	Total     int64     `json:"total"`
	Error     string    `json:"error,omitempty"`
	Started   time.Time `json:"started"`
	Ended     time.Time `json:"ended,omitempty"`
	By        string    `json:"by,omitempty"`
	cancel    context.CancelFunc
	layers    map[string][2]int64
}

type ollamaManager struct {
	px     *Proxy
	mu     sync.Mutex
	jobs   []*pullJob
	nextID int
}

func (m *ollamaManager) target(name string) (healthTarget, error) {
	p := m.px.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	_, b := p.findLocked(name)
	if b == nil {
		return healthTarget{}, httpErr{404, fmt.Errorf("unbekannter Server %q", name)}
	}
	if !b.isOllama() {
		return healthTarget{}, httpErr{400, fmt.Errorf("%s ist kein Ollama-Server", name)}
	}
	return healthTarget{b, b.URL, b.apiKey, b.insecure}, nil
}

func (m *ollamaManager) client(ht healthTarget, timeout time.Duration) *http.Client {
	return &http.Client{Transport: m.px.transportFor(ht.insecure), Timeout: timeout}
}

func ollamaSend(ctx context.Context, c *http.Client, ht healthTarget, method, path string, body any) (*http.Response, error) {
	data, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, method, ht.url+path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	if ht.key != "" {
		req.Header.Set("Authorization", "Bearer "+ht.key)
	}
	return c.Do(req)
}

func readOllamaErr(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return fmt.Errorf("%s", e.Error)
	}
	return fmt.Errorf("HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
}

type serverModels struct {
	Server  string      `json:"server"`
	URL     string      `json:"url"`
	Version string      `json:"version,omitempty"`
	Error   string      `json:"error,omitempty"`
	Models  []ollamaTag `json:"models"`
	Loaded  []ollamaPS  `json:"loaded"`
}

// List asks every Ollama server live for its models and the models in memory.
func (m *ollamaManager) List(ctx context.Context) []serverModels {
	p := m.px.pool
	p.mu.Lock()
	var hts []healthTarget
	vers := map[*Backend]string{}
	for _, b := range p.backends {
		if b.isOllama() {
			hts = append(hts, healthTarget{b, b.URL, b.apiKey, b.insecure})
			vers[b] = b.ollamaVer
		}
	}
	p.mu.Unlock()
	out := make([]serverModels, len(hts))
	var wg sync.WaitGroup
	for i, ht := range hts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sm := serverModels{Server: ht.b.Name, URL: ht.url, Version: vers[ht.b], Models: []ollamaTag{}, Loaded: []ollamaPS{}}
			c := m.client(ht, 10*time.Second)
			var tags struct{ Models []ollamaTag }
			if err := ollamaGet(ctx, c, ht.url, ht.key, "/api/tags", &tags); err != nil {
				sm.Error = explainNetErr(err)
			} else {
				sm.Models = tags.Models
				var ps struct{ Models []ollamaPS }
				if ollamaGet(ctx, c, ht.url, ht.key, "/api/ps", &ps) == nil {
					sm.Loaded = ps.Models
				}
			}
			out[i] = sm
		}()
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].Server < out[j].Server })
	return out
}

func validModelName(s string) error {
	if s == "" || len(s) > 300 || strings.ContainsAny(s, " \t\n\"'\\") {
		return errors.New("ungültiger Modellname")
	}
	return nil
}

// Pull starts a download on a server; the progress is kept in the job.
func (m *ollamaManager) Pull(server, model, by string) (*pullJob, error) {
	model = strings.TrimSpace(model)
	if err := validModelName(model); err != nil {
		return nil, httpErr{400, err}
	}
	ht, err := m.target(server)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	for _, j := range m.jobs {
		if j.State == "running" && j.Server == server && strings.EqualFold(j.Model, model) {
			m.mu.Unlock()
			return nil, httpErr{409, fmt.Errorf("%s wird auf %s schon heruntergeladen", model, server)}
		}
	}
	m.nextID++
	ctx, cancel := context.WithCancel(context.Background())
	j := &pullJob{ID: m.nextID, Server: server, Model: model, State: "running", Status: "starte …", Started: time.Now(), By: by,
		cancel: cancel, layers: map[string][2]int64{}}
	m.jobs = append(m.jobs, j)
	m.pruneLocked()
	m.mu.Unlock()
	log.Printf("ollama: pull %s on %s started by %s", model, server, cmp.Or(by, "?"))
	go m.runPull(ctx, ht, j)
	return j, nil
}

func (m *ollamaManager) runPull(ctx context.Context, ht healthTarget, j *pullJob) {
	finish := func(state, msg string) {
		m.mu.Lock()
		j.State, j.Ended = state, time.Now()
		if state == "failed" {
			j.Error = msg
		} else if msg != "" {
			j.Status = msg
		}
		m.mu.Unlock()
		log.Printf("ollama: pull %s on %s: %s %s", j.Model, j.Server, state, msg)
		m.px.pool.kickHealth() // refresh the model list
	}
	resp, err := ollamaSend(ctx, m.client(ht, 0), ht, http.MethodPost, "/api/pull", map[string]any{"model": j.Model, "name": j.Model, "stream": true})
	if err != nil {
		if ctx.Err() != nil {
			finish("cancelled", "abgebrochen")
		} else {
			finish("failed", explainNetErr(err))
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		finish("failed", readOllamaErr(resp).Error())
		return
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var ev struct {
			Status    string `json:"status"`
			Digest    string `json:"digest"`
			Total     int64  `json:"total"`
			Completed int64  `json:"completed"`
			Error     string `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev.Error != "" {
			finish("failed", ev.Error)
			return
		}
		m.mu.Lock()
		j.Status = ev.Status
		if ev.Digest != "" && ev.Total > 0 {
			j.layers[ev.Digest] = [2]int64{ev.Completed, ev.Total}
			var c, t int64
			for _, l := range j.layers {
				c, t = c+l[0], t+l[1]
			}
			j.Completed, j.Total = c, t
		}
		m.mu.Unlock()
		if ev.Status == "success" {
			m.mu.Lock()
			j.Completed = j.Total
			m.mu.Unlock()
			finish("done", "fertig")
			return
		}
	}
	switch {
	case ctx.Err() != nil:
		finish("cancelled", "abgebrochen")
	case sc.Err() != nil:
		finish("failed", sc.Err().Error())
	default:
		finish("failed", "Verbindung beendet, ohne dass Ollama „success“ gemeldet hat")
	}
}

func (m *ollamaManager) pruneLocked() {
	cut := time.Now().Add(-6 * time.Hour)
	keep := m.jobs[:0]
	for _, j := range m.jobs {
		if j.State == "running" || j.Ended.After(cut) {
			keep = append(keep, j)
		}
	}
	m.jobs = keep
	if len(m.jobs) > 50 {
		m.jobs = m.jobs[len(m.jobs)-50:]
	}
}

func (m *ollamaManager) Jobs() []pullJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]pullJob, 0, len(m.jobs))
	for i := len(m.jobs) - 1; i >= 0; i-- {
		c := *m.jobs[i]
		c.cancel, c.layers = nil, nil
		out = append(out, c)
	}
	return out
}

func (m *ollamaManager) Cancel(id int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.jobs {
		if j.ID == id {
			if j.State == "running" {
				j.cancel()
			}
			return nil
		}
	}
	return httpErr{404, errors.New("unbekannter Download")}
}

func (m *ollamaManager) ClearFinished() {
	m.mu.Lock()
	defer m.mu.Unlock()
	keep := m.jobs[:0]
	for _, j := range m.jobs {
		if j.State == "running" {
			keep = append(keep, j)
		}
	}
	m.jobs = keep
}

func (m *ollamaManager) Delete(server, model string) error {
	ht, err := m.target(server)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := ollamaSend(ctx, m.client(ht, 0), ht, http.MethodDelete, "/api/delete", map[string]string{"model": model, "name": model})
	if err != nil {
		return httpErr{502, fmt.Errorf("%s: %s", server, explainNetErr(err))}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return httpErr{502, fmt.Errorf("%s: %v", server, readOllamaErr(resp))}
	}
	m.px.pool.kickHealth()
	return nil
}

// Unload removes a model from the memory of a server (keep_alive 0).
func (m *ollamaManager) Unload(server, model string) error {
	ht, err := m.target(server)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := m.client(ht, 0)
	var last error
	for _, try := range []struct {
		path string
		body map[string]any
	}{
		{"/api/generate", map[string]any{"model": model, "keep_alive": 0}},
		{"/api/embed", map[string]any{"model": model, "input": "", "keep_alive": 0}}, // embedding models can't generate
	} {
		resp, err := ollamaSend(ctx, c, ht, http.MethodPost, try.path, try.body)
		if err != nil {
			return httpErr{502, fmt.Errorf("%s: %s", server, explainNetErr(err))}
		}
		if resp.StatusCode == 200 {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			m.px.pool.kickHealth()
			return nil
		}
		last = readOllamaErr(resp)
		resp.Body.Close()
	}
	return httpErr{502, fmt.Errorf("%s: %v", server, last)}
}

func registerOllama(mux *http.ServeMux, pre string, px *Proxy, token string) *ollamaManager {
	m := &ollamaManager{px: px}
	send := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(v)
	}
	who := func(r *http.Request) string {
		if id := identityFrom(r); id != nil {
			return id.Name
		}
		return ""
	}
	type req struct {
		Model   string   `json:"model"`
		Servers []string `json:"servers"`
	}
	read := func(r *http.Request) (req, error) {
		var in req
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64<<10)).Decode(&in); err != nil {
			return in, httpErr{400, fmt.Errorf("ungültiges JSON: %v", err)}
		}
		in.Model = strings.TrimSpace(in.Model)
		if err := validModelName(in.Model); err != nil {
			return in, httpErr{400, err}
		}
		if len(in.Servers) == 0 {
			return in, httpErr{400, errors.New("kein Server gewählt")}
		}
		return in, nil
	}
	mux.HandleFunc("GET "+pre+"/api/ollama/models", func(w http.ResponseWriter, r *http.Request) {
		send(w, m.List(r.Context()))
	})
	mux.HandleFunc("GET "+pre+"/api/ollama/jobs", func(w http.ResponseWriter, r *http.Request) {
		send(w, m.Jobs())
	})
	mux.HandleFunc("POST "+pre+"/api/ollama/pull", adminAuth(token, func(w http.ResponseWriter, r *http.Request) {
		in, err := read(r)
		if err != nil {
			writeResult(w, err)
			return
		}
		var started []pullJob
		var errs []string
		for _, s := range in.Servers {
			j, err := m.Pull(s, in.Model, who(r))
			if err != nil {
				errs = append(errs, err.Error())
				continue
			}
			started = append(started, *j)
		}
		if len(started) == 0 {
			writeAPIError(w, http.StatusBadRequest, errors.New(strings.Join(errs, "; ")))
			return
		}
		send(w, map[string]any{"started": len(started), "errors": errs})
	}))
	mux.HandleFunc("POST "+pre+"/api/ollama/jobs/{id}/cancel", adminAuth(token, func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.PathValue("id"))
		writeResult(w, m.Cancel(id))
	}))
	mux.HandleFunc("POST "+pre+"/api/ollama/jobs/clear", adminAuth(token, func(w http.ResponseWriter, r *http.Request) {
		m.ClearFinished()
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST "+pre+"/api/ollama/delete", adminAuth(token, func(w http.ResponseWriter, r *http.Request) {
		in, err := read(r)
		if err != nil {
			writeResult(w, err)
			return
		}
		var errs []string
		for _, s := range in.Servers {
			if err := m.Delete(s, in.Model); err != nil {
				errs = append(errs, err.Error())
			} else {
				log.Printf("ollama: %s deleted on %s by %s", in.Model, s, cmp.Or(who(r), "?"))
			}
		}
		if len(errs) > 0 {
			writeAPIError(w, http.StatusBadGateway, errors.New(strings.Join(errs, "; ")))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST "+pre+"/api/ollama/unload", adminAuth(token, func(w http.ResponseWriter, r *http.Request) {
		in, err := read(r)
		if err != nil {
			writeResult(w, err)
			return
		}
		var errs []string
		for _, s := range in.Servers {
			if err := m.Unload(s, in.Model); err != nil {
				errs = append(errs, err.Error())
			}
		}
		if len(errs) > 0 {
			writeAPIError(w, http.StatusBadGateway, errors.New(strings.Join(errs, "; ")))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	return m
}
