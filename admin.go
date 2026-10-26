package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---- persistence of the backend list ----

// Store keeps the backend list that was edited in the web UI. If the file exists it wins over
// BACKENDS / config.json, which are only the initial setup.
type Store struct {
	path string
}

type storedState struct {
	Backends []BackendConfig `json:"backends"`
	Groups   []GroupConfig   `json:"groups"`
	SavedAt  time.Time       `json:"saved_at"`
}

func NewStore(dir string) *Store {
	return &Store{path: filepath.Join(dir, "backends.json")}
}

// Load returns the saved state (ok=true) if one exists.
func (s *Store) Load() (st storedState, ok bool, err error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return st, false, nil
	}
	if err != nil {
		return st, false, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, false, fmt.Errorf("%s: %w", s.path, err)
	}
	if err := validateSetup(st.Backends, st.Groups); err != nil {
		return st, false, fmt.Errorf("%s: %w", s.path, err)
	}
	return st, true, nil
}

func (s *Store) Save(bs []BackendConfig, gs []GroupConfig) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(storedState{Backends: bs, Groups: gs, SavedAt: time.Now()}, "", "  ")
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil { // contains bearer tokens
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) Delete() error {
	err := os.Remove(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ---- runtime changes to the pool ----

func newBackend(bc BackendConfig) *Backend {
	u, _ := url.Parse(bc.URL)
	return &Backend{
		Name: bc.Name, URL: bc.URL, target: u, Max: bc.MaxConcurrent, Priority: bc.Priority,
		apiKey:   bc.APIKey,
		group:    bc.Group,
		insecure: bc.TLSSkipVerify,
		enabled:  bc.Enabled == nil || *bc.Enabled,
		healthy:  true, // optimistic until the first health check says otherwise
	}
}

func (b *Backend) config() BackendConfig {
	en := b.enabled
	return BackendConfig{Name: b.Name, URL: b.URL, MaxConcurrent: b.Max, Priority: b.Priority, Enabled: &en, APIKey: b.apiKey, Group: b.group, TLSSkipVerify: b.insecure}
}

// configsLocked returns the current list as config entries. Caller holds p.mu.
func (p *Pool) configsLocked() []BackendConfig {
	out := make([]BackendConfig, 0, len(p.backends))
	for _, b := range p.backends {
		out = append(out, b.config())
	}
	return out
}

// persistLocked writes the current list to disk. Caller holds p.mu.
func (p *Pool) persistLocked() error {
	if p.store == nil {
		return nil
	}
	if err := p.store.Save(p.configsLocked(), p.groups); err != nil {
		log.Printf("could not save backends: %v", err)
		return fmt.Errorf("Änderung aktiv, aber nicht gespeichert (%v) – geht beim Neustart verloren", err)
	}
	p.source = "saved"
	return nil
}

func (p *Pool) kickHealth() {
	select {
	case p.healthKick <- struct{}{}:
	default:
	}
}

func (p *Pool) findLocked(name string) (int, *Backend) {
	for i, b := range p.backends {
		if b.Name == name {
			return i, b
		}
	}
	return -1, nil
}

var errNotFound = errors.New("unbekannter Server")

type httpErr struct {
	code int
	err  error
}

func (e httpErr) Error() string { return e.err.Error() }

func (p *Pool) AddBackend(bc BackendConfig) error {
	if err := normalizeBackend(&bc); err != nil {
		return httpErr{400, err}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, b := p.findLocked(bc.Name); b != nil {
		return httpErr{409, fmt.Errorf("Name %q ist schon vergeben", bc.Name)}
	}
	if bc.Group != "" && p.groupLocked(bc.Group) == nil {
		return httpErr{400, fmt.Errorf("unbekannte Gruppe %q", bc.Group)}
	}
	p.backends = append(p.backends, newBackend(bc))
	log.Printf("backend %s (%s) added via UI", bc.Name, bc.URL)
	defer p.kickHealth()
	p.dispatchLocked()
	return p.persistLocked()
}

// UpdateBackend changes a backend in place; running requests keep running on it.
func (p *Pool) UpdateBackend(name string, bc BackendConfig, key, group *string) error {
	if err := normalizeBackend(&bc); err != nil {
		return httpErr{400, err}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, b := p.findLocked(name)
	if b == nil {
		return httpErr{404, errNotFound}
	}
	if bc.Name != name {
		if _, other := p.findLocked(bc.Name); other != nil {
			return httpErr{409, fmt.Errorf("Name %q ist schon vergeben", bc.Name)}
		}
		for _, r := range p.active {
			if r.Backend == name {
				r.Backend = bc.Name
			}
		}
	}
	if bc.URL != b.URL {
		u, _ := url.Parse(bc.URL)
		b.URL, b.target = bc.URL, u
		b.healthy, b.healthMsg, b.unhealthyTil = true, "", time.Time{}
		defer p.kickHealth()
	}
	if b.insecure != bc.TLSSkipVerify || (key != nil && strings.TrimSpace(*key) != b.apiKey) {
		// connection settings changed: give the server a fresh chance and check it right away
		b.healthy, b.healthMsg, b.unhealthyTil = true, "", time.Time{}
		defer p.kickHealth()
	}
	b.Name, b.Max, b.Priority, b.insecure = bc.Name, bc.MaxConcurrent, bc.Priority, bc.TLSSkipVerify
	if key != nil { // nil = keep the stored token
		b.apiKey = strings.TrimSpace(*key)
	}
	if group != nil { // nil = keep the group
		if bc.Group != "" && p.groupLocked(bc.Group) == nil {
			return httpErr{400, fmt.Errorf("unbekannte Gruppe %q", bc.Group)}
		}
		b.group = bc.Group
	}
	if bc.Enabled != nil {
		b.enabled = *bc.Enabled
	}
	log.Printf("backend %s updated via UI: %s slots=%d prio=%d enabled=%v token=%v", b.Name, b.URL, b.Max, b.Priority, b.enabled, b.apiKey != "")
	p.dispatchLocked()
	return p.persistLocked()
}

// RemoveBackend takes a backend out of the list. Requests already running on it finish normally.
func (p *Pool) RemoveBackend(name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	i, b := p.findLocked(name)
	if b == nil {
		return httpErr{404, errNotFound}
	}
	p.backends = append(p.backends[:i:i], p.backends[i+1:]...)
	log.Printf("backend %s removed via UI (%d running request(s) will finish)", name, b.active)
	return p.persistLocked()
}

func (p *Pool) SetEnabled(name string, en bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, b := p.findLocked(name)
	if b == nil {
		return httpErr{404, errNotFound}
	}
	b.enabled = en
	if en {
		p.dispatchLocked()
	}
	log.Printf("backend %s %s via UI", name, map[bool]string{true: "enabled", false: "disabled"}[en])
	return p.persistLocked()
}

// ResetBackends replaces the list with the initial one from BACKENDS / config.json and deletes the saved file.
func (p *Pool) ResetBackends() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	old := map[string]*Backend{}
	for _, b := range p.backends {
		old[b.Name] = b
	}
	var next []*Backend
	for _, bc := range p.initial {
		if b, ok := old[bc.Name]; ok && b.URL == bc.URL {
			// keep runtime state (active slots, stats) of unchanged servers
			b.Max, b.Priority, b.enabled, b.apiKey, b.group, b.insecure = bc.MaxConcurrent, bc.Priority, bc.Enabled == nil || *bc.Enabled, bc.APIKey, bc.Group, bc.TLSSkipVerify
			next = append(next, b)
		} else {
			next = append(next, newBackend(bc))
		}
	}
	p.backends = next
	p.groups = append([]GroupConfig{}, p.initialGroups...)
	p.source = "env"
	log.Printf("backends reset to initial configuration via UI")
	defer p.kickHealth()
	p.dispatchLocked()
	if p.store != nil {
		if err := p.store.Delete(); err != nil {
			return fmt.Errorf("zurückgesetzt, aber %s konnte nicht gelöscht werden: %v", p.store.path, err)
		}
	}
	return nil
}

// ---- HTTP API ----

func adminAuth(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Admin-Token")), []byte(token)) != 1 {
			writeAPIError(w, http.StatusUnauthorized, errors.New("Admin-Token fehlt oder ist falsch"))
			return
		}
		next(w, r)
	}
}

func writeAPIError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func writeResult(w http.ResponseWriter, err error) {
	var he httpErr
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.As(err, &he):
		writeAPIError(w, he.code, he.err)
	default: // applied, but e.g. not persisted
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"warning": err.Error()})
	}
}

// backendInput is the API body; api_key absent = keep the stored token, "" = remove it.
type backendInput struct {
	Name          string  `json:"name"`
	URL           string  `json:"url"`
	MaxConcurrent int     `json:"max_concurrent"`
	Priority      int     `json:"priority"`
	Enabled       *bool   `json:"enabled"`
	APIKey        *string `json:"api_key"`
	Group         *string `json:"group"`
	TLSSkipVerify *bool   `json:"tls_skip_verify"`
}

func decodeBackend(r *http.Request) (BackendConfig, *string, *string, error) {
	var in backendInput
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return BackendConfig{}, nil, nil, httpErr{400, fmt.Errorf("ungültiges JSON: %v", err)}
	}
	bc := BackendConfig{Name: in.Name, URL: in.URL, MaxConcurrent: in.MaxConcurrent, Priority: in.Priority, Enabled: in.Enabled}
	if in.TLSSkipVerify != nil {
		bc.TLSSkipVerify = *in.TLSSkipVerify
	}
	if in.APIKey != nil {
		bc.APIKey = strings.TrimSpace(*in.APIKey)
	}
	if in.Group != nil {
		bc.Group = strings.TrimSpace(*in.Group)
	}
	return bc, in.APIKey, in.Group, nil
}

// backendView is a BackendConfig without the secret.
type backendView struct {
	Name          string `json:"name"`
	URL           string `json:"url"`
	MaxConcurrent int    `json:"max_concurrent"`
	Priority      int    `json:"priority"`
	Enabled       bool   `json:"enabled"`
	HasAPIKey     bool   `json:"has_api_key"`
	Group         string `json:"group"`
	TLSSkipVerify bool   `json:"tls_skip_verify"`
}

func registerAdmin(mux *http.ServeMux, pre string, pool *Pool, token string) {
	mux.HandleFunc("GET "+pre+"/api/backends", func(w http.ResponseWriter, r *http.Request) {
		pool.mu.Lock()
		list := []backendView{}
		for _, bc := range pool.configsLocked() {
			list = append(list, backendView{bc.Name, bc.URL, bc.MaxConcurrent, bc.Priority, *bc.Enabled, bc.APIKey != "", bc.Group, bc.TLSSkipVerify})
		}
		pool.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(list)
	})
	mux.HandleFunc("POST "+pre+"/api/backends", adminAuth(token, func(w http.ResponseWriter, r *http.Request) {
		bc, _, _, err := decodeBackend(r)
		if err == nil {
			err = pool.AddBackend(bc)
		}
		writeResult(w, err)
	}))
	mux.HandleFunc("PUT "+pre+"/api/backends/{name}", adminAuth(token, func(w http.ResponseWriter, r *http.Request) {
		bc, key, group, err := decodeBackend(r)
		if err == nil {
			err = pool.UpdateBackend(r.PathValue("name"), bc, key, group)
		}
		writeResult(w, err)
	}))
	mux.HandleFunc("DELETE "+pre+"/api/backends/{name}", adminAuth(token, func(w http.ResponseWriter, r *http.Request) {
		writeResult(w, pool.RemoveBackend(r.PathValue("name")))
	}))
	mux.HandleFunc("POST "+pre+"/api/backends/{name}/{action}", adminAuth(token, func(w http.ResponseWriter, r *http.Request) {
		action := r.PathValue("action")
		if action != "enable" && action != "disable" {
			writeAPIError(w, http.StatusBadRequest, errors.New("action must be enable or disable"))
			return
		}
		writeResult(w, pool.SetEnabled(r.PathValue("name"), action == "enable"))
	}))
	mux.HandleFunc("POST "+pre+"/api/reset", adminAuth(token, func(w http.ResponseWriter, r *http.Request) {
		writeResult(w, pool.ResetBackends())
	}))
	mux.HandleFunc("POST "+pre+"/api/auth-check", adminAuth(token, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
}
