package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// AccessToken is a client token for the LLM API of the balancer (what LiteLLM sends as api_key).
// Only a SHA-256 hash is stored; the token itself is shown once when it is created.
type AccessToken struct {
	Name     string    `json:"name"`
	Hash     string    `json:"hash"`
	Hint     string    `json:"hint"` // last 4 characters, for recognising it in the UI
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used,omitempty"`
	Requests int64     `json:"requests"`
	FromEnv  bool      `json:"env,omitempty"`
	// Prices per 1 million LLM tokens and what has been used so far.
	PriceIn   float64 `json:"price_in"`
	PriceOut  float64 `json:"price_out"`
	TokensIn  int64   `json:"tokens_in"`
	TokensOut int64   `json:"tokens_out"`
	Cost      float64 `json:"cost"`
	// Incoming cost cap: the token is blocked when its costs reach CostCap per CapPeriod.
	CostCap   float64 `json:"cost_cap,omitempty"`
	CapPeriod string  `json:"cap_period,omitempty"`
	Disabled  bool    `json:"disabled,omitempty"` // switched off by the admin
	capped    bool
	capSpent  float64
}

// Access holds the client tokens: from API_KEYS (always valid, read-only in the UI)
// plus tokens created in the UI (persisted in DATA_DIR/access.json).
type Access struct {
	onChange func() // called after settings changed (re-check caps)
	mu       sync.Mutex
	tokens   []*AccessToken
	path     string
	dirty    bool
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func hint(t string) string {
	if len(t) <= 4 {
		return strings.Repeat("•", len(t))
	}
	return t[len(t)-4:]
}

// parseAPIKeysEnv parses API_KEYS="litellm=sk-abc,hermes=xyz" or "sk-abc,xyz".
func parseAPIKeysEnv(v string) []*AccessToken {
	var out []*AccessToken
	for i, item := range strings.Split(v, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, tok := fmt.Sprintf("env-%d", i+1), item
		if k := strings.Index(item, "="); k > 0 {
			name, tok = item[:k], item[k+1:]
		}
		out = append(out, &AccessToken{Name: name, Hash: hashToken(tok), Hint: hint(tok), FromEnv: true})
	}
	return out
}

func NewAccess(dir, envKeys string) (*Access, error) {
	a := &Access{tokens: parseAPIKeysEnv(envKeys)}
	if dir == "" {
		return a, nil
	}
	a.path = filepath.Join(dir, "access.json")
	data, err := os.ReadFile(a.path)
	if errors.Is(err, os.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	var saved []*AccessToken
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, fmt.Errorf("%s: %w", a.path, err)
	}
	for _, t := range saved {
		if !t.FromEnv {
			a.tokens = append(a.tokens, t)
			continue
		}
		// prices and counters of tokens from API_KEYS are kept by name, as long as the token still exists
		for _, e := range a.tokens {
			if e.FromEnv && e.Name == t.Name {
				e.Created, e.LastUsed, e.Requests = t.Created, t.LastUsed, t.Requests
				e.PriceIn, e.PriceOut, e.TokensIn, e.TokensOut, e.Cost = t.PriceIn, t.PriceOut, t.TokensIn, t.TokensOut, t.Cost
				e.CostCap, e.CapPeriod, e.Disabled = t.CostCap, t.CapPeriod, t.Disabled
			}
		}
	}
	return a, nil
}

// Enabled reports whether clients must present a token.
func (a *Access) Enabled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.tokens) > 0
}

// tokenFromRequest reads the client token from the usual places.
func tokenFromRequest(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
			return strings.TrimSpace(h[7:])
		}
		return strings.TrimSpace(h)
	}
	if h := r.Header.Get("X-Api-Key"); h != "" { // Anthropic style
		return strings.TrimSpace(h)
	}
	return strings.TrimSpace(r.Header.Get("Api-Key")) // Azure style
}

// Check returns the token name for an allowed request; otherwise an HTTP status and message.
// If no tokens are configured at all, every request is allowed (name "").
func (a *Access) Check(r *http.Request) (name string, code int, msg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.tokens) == 0 {
		return "", 0, ""
	}
	tok := tokenFromRequest(r)
	if tok == "" {
		return "", http.StatusUnauthorized, "invalid or missing API key"
	}
	h := hashToken(tok)
	for _, t := range a.tokens {
		if subtle.ConstantTimeCompare([]byte(h), []byte(t.Hash)) == 1 {
			if t.Disabled {
				return t.Name, http.StatusForbidden, "API key " + t.Name + " is disabled"
			}
			if t.capped {
				return t.Name, http.StatusTooManyRequests, "cost limit of API key " + t.Name + " reached"
			}
			t.LastUsed = time.Now()
			t.Requests++
			a.dirty = true
			return t.Name, 0, ""
		}
	}
	return "", http.StatusUnauthorized, "invalid or missing API key"
}

func (a *Access) saveLocked() error {
	if a.path == "" {
		return nil
	}
	all := []AccessToken{}
	for _, t := range a.tokens {
		c := *t
		if c.FromEnv {
			c.Hash = "" // the env value is the source of truth; only metadata is stored
		}
		all = append(all, c)
	}
	data, _ := json.MarshalIndent(all, "", "  ")
	if err := os.MkdirAll(filepath.Dir(a.path), 0o755); err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, a.path); err != nil {
		return err
	}
	a.dirty = false
	return nil
}

// Flush persists usage counters (called periodically).
func (a *Access) Flush() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.dirty {
		if err := a.saveLocked(); err != nil {
			log.Printf("access tokens: %v", err)
		}
	}
}

// SetPrice changes the prices (per 1M tokens). Costs already counted are not recalculated.
type tokenUpdate struct {
	PriceIn   *float64 `json:"price_in"`
	PriceOut  *float64 `json:"price_out"`
	CostCap   *float64 `json:"cost_cap"`
	CapPeriod *string  `json:"cap_period"`
	Disabled  *bool    `json:"disabled"`
}

// Update changes prices, cost cap and on/off of a token (fields left out stay as they are).
func (a *Access) Update(name string, u tokenUpdate) error {
	if err := a.SetPrice(name, u.PriceIn, u.PriceOut); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, t := range a.tokens {
		if t.Name != name {
			continue
		}
		if u.CostCap != nil {
			if *u.CostCap < 0 || *u.CostCap > 1e9 {
				return httpErr{400, errors.New("Kostendeckel muss zwischen 0 und 1.000.000.000 liegen")}
			}
			t.CostCap = *u.CostCap
			if t.CostCap == 0 {
				t.capped = false
			}
		}
		if u.CapPeriod != nil {
			p, err := normalizePeriod(*u.CapPeriod)
			if err != nil {
				return httpErr{400, err}
			}
			t.CapPeriod = p
		}
		if t.CapPeriod == "" {
			t.CapPeriod = PeriodMonth
		}
		if u.Disabled != nil {
			t.Disabled = *u.Disabled
			log.Printf("access token %q %s via UI", name, map[bool]string{true: "disabled", false: "enabled"}[t.Disabled])
		}
		return a.saveLocked()
	}
	return httpErr{404, errors.New("unbekannter Token")}
}

func (a *Access) SetPrice(name string, in, out *float64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, t := range a.tokens {
		if t.Name != name {
			continue
		}
		for _, p := range []*float64{in, out} {
			if p != nil && (*p < 0 || *p > 1e6) {
				return httpErr{400, errors.New("Preis muss zwischen 0 und 1.000.000 liegen")}
			}
		}
		if in != nil {
			t.PriceIn = *in
		}
		if out != nil {
			t.PriceOut = *out
		}
		log.Printf("access token %q: price in=%g out=%g per 1M tokens", name, t.PriceIn, t.PriceOut)
		return a.saveLocked()
	}
	return httpErr{404, errors.New("unbekannter Token")}
}

// AddUsage books the LLM tokens of a finished request on the client token and returns its cost.
func (a *Access) AddUsage(name string, u Usage) float64 {
	if name == "" || !u.Found {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, t := range a.tokens {
		if t.Name == name {
			cost := (float64(u.In)*t.PriceIn + float64(u.Out)*t.PriceOut) / 1e6
			t.TokensIn += u.In
			t.TokensOut += u.Out
			t.Cost += cost
			a.dirty = true
			return cost
		}
	}
	return 0
}

// Create adds a token. If value is empty, a random one is generated. Returns the plain token.
func (a *Access) Create(name, value string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, "/?#") {
		return "", httpErr{400, errors.New("Name fehlt oder enthält / ? #")}
	}
	value = strings.TrimSpace(value)
	if value == "" {
		b := make([]byte, 24)
		rand.Read(b)
		value = "llmb-" + base64.RawURLEncoding.EncodeToString(b)
	} else if len(value) < 12 {
		return "", httpErr{400, errors.New("Token muss mindestens 12 Zeichen lang sein")}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	h := hashToken(value)
	for _, t := range a.tokens {
		if t.Name == name {
			return "", httpErr{409, fmt.Errorf("Name %q ist schon vergeben", name)}
		}
		if t.Hash == h {
			return "", httpErr{409, errors.New("Dieser Token existiert schon")}
		}
	}
	a.tokens = append(a.tokens, &AccessToken{Name: name, Hash: h, Hint: hint(value), Created: time.Now()})
	log.Printf("access token %q created via UI", name)
	if err := a.saveLocked(); err != nil {
		return value, fmt.Errorf("Token aktiv, aber nicht gespeichert (%v) – geht beim Neustart verloren", err)
	}
	return value, nil
}

func (a *Access) Delete(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, t := range a.tokens {
		if t.Name == name {
			if t.FromEnv {
				return httpErr{400, errors.New("Token aus API_KEYS kann nur in der Umgebungsvariable entfernt werden")}
			}
			a.tokens = append(a.tokens[:i:i], a.tokens[i+1:]...)
			log.Printf("access token %q deleted via UI", name)
			return a.saveLocked()
		}
	}
	return httpErr{404, errors.New("unbekannter Token")}
}

type tokenView struct {
	Name      string  `json:"name"`
	Hint      string  `json:"hint"`
	Source    string  `json:"source"` // env | ui
	Created   string  `json:"created,omitempty"`
	LastUsed  string  `json:"last_used,omitempty"`
	Requests  int64   `json:"requests"`
	PriceIn   float64 `json:"price_in"`
	PriceOut  float64 `json:"price_out"`
	TokensIn  int64   `json:"tokens_in"`
	TokensOut int64   `json:"tokens_out"`
	Cost      float64 `json:"cost"`
	CostCap   float64 `json:"cost_cap"`
	CapPeriod string  `json:"cap_period"`
	CapSpent  float64 `json:"cap_spent"`
	Capped    bool    `json:"capped"`
	Disabled  bool    `json:"disabled"`
}

func (a *Access) List() []tokenView {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []tokenView{}
	for _, t := range a.tokens {
		v := tokenView{Name: t.Name, Hint: t.Hint, Source: "ui", Created: fmtTime(t.Created), LastUsed: fmtTime(t.LastUsed), Requests: t.Requests,
			PriceIn: t.PriceIn, PriceOut: t.PriceOut, TokensIn: t.TokensIn, TokensOut: t.TokensOut, Cost: t.Cost,
			CostCap: t.CostCap, CapPeriod: t.CapPeriod, CapSpent: t.capSpent, Capped: t.capped, Disabled: t.Disabled}
		if v.CapPeriod == "" {
			v.CapPeriod = PeriodMonth
		}
		if t.FromEnv {
			v.Source = "env"
		}
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out
}

func registerAccess(mux *http.ServeMux, pre string, acc *Access, adminToken string) {
	mux.HandleFunc("GET "+pre+"/api/tokens", adminAuth(adminToken, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(acc.List())
	}))
	mux.HandleFunc("POST "+pre+"/api/tokens", adminAuth(adminToken, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name  string `json:"name"`
			Token string `json:"token"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
			writeAPIError(w, 400, fmt.Errorf("ungültiges JSON: %v", err))
			return
		}
		tok, err := acc.Create(req.Name, req.Token)
		var he httpErr
		if errors.As(err, &he) {
			writeAPIError(w, he.code, he.err)
			return
		}
		resp := map[string]string{"token": tok}
		if err != nil {
			resp["warning"] = err.Error()
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(resp)
	}))
	mux.HandleFunc("PUT "+pre+"/api/tokens/{name}", adminAuth(adminToken, func(w http.ResponseWriter, r *http.Request) {
		var req tokenUpdate
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
			writeAPIError(w, 400, fmt.Errorf("ungültiges JSON: %v", err))
			return
		}
		err := acc.Update(r.PathValue("name"), req)
		if err == nil && acc.onChange != nil {
			acc.onChange() // re-check the cost caps right away
		}
		writeResult(w, err)
	}))
	mux.HandleFunc("DELETE "+pre+"/api/tokens/{name}", adminAuth(adminToken, func(w http.ResponseWriter, r *http.Request) {
		writeResult(w, acc.Delete(r.PathValue("name")))
	}))
}
