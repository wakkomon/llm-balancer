package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// Configuration export/import: servers + groups, API tokens, e-mail settings and users in one JSON file.
// Secrets (bearer tokens of the servers, SMTP password, token and password hashes) are only included on
// request. On import, a missing secret keeps the current value of the entry with the same name.

const configFormat = "llm-balancer-config"

var configSections = []string{"servers", "tokens", "notify", "users"}

type tokenExport struct {
	Name      string  `json:"name"`
	Hash      string  `json:"hash,omitempty"`
	Hint      string  `json:"hint,omitempty"`
	FromEnv   bool    `json:"env,omitempty"` // from API_KEYS: only prices/cap/lock are applied on import
	PriceIn   float64 `json:"price_in"`
	PriceOut  float64 `json:"price_out"`
	CostCap   float64 `json:"cost_cap,omitempty"`
	CapPeriod string  `json:"cap_period,omitempty"`
	Disabled  bool    `json:"disabled,omitempty"`
}

type ConfigFile struct {
	Format        string           `json:"format"`
	FormatVersion int              `json:"format_version"`
	Version       string           `json:"balancer_version,omitempty"`
	ExportedAt    time.Time        `json:"exported_at"`
	WithSecrets   bool             `json:"with_secrets"`
	Backends      *[]BackendConfig `json:"backends,omitempty"`
	Groups        *[]GroupConfig   `json:"groups,omitempty"`
	Routers       *[]RouterConfig  `json:"routers,omitempty"`
	Tokens        *[]tokenExport   `json:"tokens,omitempty"`
	Notify        *NotifyConfig    `json:"notify,omitempty"`
	Users         *[]User          `json:"users,omitempty"`
}

func parseSections(v string) (map[string]bool, error) {
	out := map[string]bool{}
	if strings.TrimSpace(v) == "" {
		for _, s := range configSections {
			out[s] = true
		}
		return out, nil
	}
	for _, s := range strings.Split(v, ",") {
		s = strings.TrimSpace(s)
		ok := false
		for _, k := range configSections {
			ok = ok || k == s
		}
		if !ok {
			return nil, fmt.Errorf("unbekannter Bereich %q (möglich: %s)", s, strings.Join(configSections, ", "))
		}
		out[s] = true
	}
	return out, nil
}

func exportConfig(pool *Pool, acc *Access, users *Users, sec map[string]bool, secrets bool) ConfigFile {
	f := ConfigFile{Format: configFormat, FormatVersion: 1, Version: versionString(), ExportedAt: time.Now(), WithSecrets: secrets}
	if sec["servers"] {
		pool.mu.Lock()
		bs := pool.configsLocked()
		gs := append([]GroupConfig{}, pool.groups...)
		pool.mu.Unlock()
		if !secrets {
			for i := range bs {
				bs[i].APIKey = ""
			}
		}
		rs := append([]RouterConfig{}, pool.Routers()...)
		f.Backends, f.Groups, f.Routers = &bs, &gs, &rs
	}
	if sec["tokens"] && acc != nil {
		acc.mu.Lock()
		ts := []tokenExport{}
		for _, t := range acc.tokens {
			e := tokenExport{Name: t.Name, Hint: t.Hint, FromEnv: t.FromEnv, PriceIn: t.PriceIn, PriceOut: t.PriceOut,
				CostCap: t.CostCap, CapPeriod: t.CapPeriod, Disabled: t.Disabled}
			if secrets && !t.FromEnv {
				e.Hash = t.Hash
			}
			ts = append(ts, e)
		}
		acc.mu.Unlock()
		f.Tokens = &ts
	}
	if sec["notify"] && pool.notifier != nil {
		pool.notifier.mu.Lock()
		c := pool.notifier.cfg
		pool.notifier.mu.Unlock()
		if !secrets {
			c.Password = ""
		}
		f.Notify = &c
	}
	if sec["users"] && users != nil {
		users.mu.Lock()
		us := []User{}
		for _, x := range users.users {
			c := *x
			if !secrets {
				c.Hash = ""
			}
			us = append(us, c)
		}
		users.mu.Unlock()
		f.Users = &us
	}
	return f
}

type importResult struct {
	Applied  []string `json:"applied"`
	Warnings []string `json:"warnings"`
}

func importConfig(pool *Pool, acc *Access, users *Users, f ConfigFile, sec map[string]bool) (importResult, error) {
	res := importResult{Applied: []string{}, Warnings: []string{}}
	if f.Format != configFormat {
		return res, httpErr{400, errors.New("keine Konfigurationsdatei des LLM-Balancers")}
	}
	if f.FormatVersion > 1 {
		return res, httpErr{400, fmt.Errorf("Dateiformat %d ist neuer als diese Version – bitte den Balancer aktualisieren", f.FormatVersion)}
	}
	// validate every chosen section before changing anything
	var bcs []BackendConfig
	var gs []GroupConfig
	if sec["servers"] && f.Backends != nil {
		var err error
		if bcs, gs, err = pool.prepareServers(*f.Backends, f.Groups, &res); err != nil {
			return res, httpErr{400, fmt.Errorf("Server/Gruppen: %v", err)}
		}
	}
	var newUsers []*User
	if sec["users"] && f.Users != nil && users != nil {
		var err error
		if newUsers, err = users.prepareImport(*f.Users, &res); err != nil {
			return res, httpErr{400, fmt.Errorf("Benutzer: %v", err)}
		}
	}
	if sec["notify"] && f.Notify != nil {
		c := *f.Notify
		if err := c.validate(); err != nil {
			return res, httpErr{400, fmt.Errorf("E-Mail: %v", err)}
		}
	}
	if sec["tokens"] && f.Tokens != nil && acc != nil {
		for _, t := range *f.Tokens {
			if t.CapPeriod != "" {
				if _, err := normalizePeriod(t.CapPeriod); err != nil {
					return res, httpErr{400, fmt.Errorf("API-Token %q: %v", t.Name, err)}
				}
			}
		}
	}

	if sec["servers"] && f.Routers != nil {
		for i := range *f.Routers {
			if err := normalizeRouter(&(*f.Routers)[i]); err != nil {
				return res, httpErr{400, fmt.Errorf("Router %q: %v", (*f.Routers)[i].Name, err)}
			}
		}
	}
	if bcs != nil {
		pool.mu.Lock()
		pool.replaceLocked(bcs, gs)
		err := pool.persistLocked()
		pool.mu.Unlock()
		if err != nil {
			res.Warnings = append(res.Warnings, err.Error())
		}
		res.Applied = append(res.Applied, fmt.Sprintf("%d Server, %d Gruppen", len(bcs), len(gs)))
		if f.Routers != nil {
			if err := pool.SetRouters(*f.Routers); err != nil {
				res.Warnings = append(res.Warnings, err.Error())
			} else if len(*f.Routers) > 0 {
				res.Applied = append(res.Applied, fmt.Sprintf("%d Router", len(*f.Routers)))
			}
		}
	}
	if sec["tokens"] && f.Tokens != nil && acc != nil {
		n := acc.importTokens(*f.Tokens, &res)
		res.Applied = append(res.Applied, fmt.Sprintf("%d API-Tokens", n))
	}
	if sec["notify"] && f.Notify != nil && pool.notifier != nil {
		c := *f.Notify
		if err := pool.notifier.SetConfig(c, c.Password == ""); err != nil {
			res.Warnings = append(res.Warnings, err.Error())
		}
		res.Applied = append(res.Applied, "E-Mail-Einstellungen")
	}
	if newUsers != nil {
		if err := users.applyImport(newUsers); err != nil {
			res.Warnings = append(res.Warnings, err.Error())
		}
		res.Applied = append(res.Applied, fmt.Sprintf("%d Benutzer", len(newUsers)))
	}
	log.Printf("configuration imported: %s", strings.Join(res.Applied, ", "))
	return res, nil
}

// prepareServers validates an imported server list + groups; missing bearer tokens are taken from the
// current server with the same name.
func (p *Pool) prepareServers(in []BackendConfig, groupsIn *[]GroupConfig, res *importResult) ([]BackendConfig, []GroupConfig, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var gs []GroupConfig
	if groupsIn != nil {
		gnames := map[string]bool{}
		for _, g := range *groupsIn {
			g.keepDisabled = false
			if err := normalizeGroup(&g); err != nil {
				return nil, nil, err
			}
			if gnames[g.Name] {
				return nil, nil, fmt.Errorf("Gruppe %q doppelt", g.Name)
			}
			gnames[g.Name] = true
			gs = append(gs, g)
		}
		for _, g := range gs {
			if g.Fallback != "" && !gnames[g.Fallback] {
				return nil, nil, fmt.Errorf("Gruppe %q: unbekannte Fallback-Gruppe %q", g.Name, g.Fallback)
			}
		}
	} else {
		gs = append([]GroupConfig{}, p.groups...)
	}
	has := map[string]bool{}
	for _, g := range gs {
		has[g.Name] = true
	}
	names := map[string]bool{}
	out := make([]BackendConfig, 0, len(in))
	kept := 0
	for _, bc := range in {
		if err := normalizeBackend(&bc); err != nil {
			return nil, nil, fmt.Errorf("Server %q: %v", bc.Name, err)
		}
		if names[bc.Name] {
			return nil, nil, fmt.Errorf("Server %q doppelt", bc.Name)
		}
		names[bc.Name] = true
		if bc.Group != "" && !has[bc.Group] {
			return nil, nil, fmt.Errorf("Server %q: unbekannte Gruppe %q", bc.Name, bc.Group)
		}
		if bc.APIKey == "" {
			if _, cur := p.findLocked(bc.Name); cur != nil && cur.apiKey != "" {
				bc.APIKey = cur.apiKey
				kept++
			}
		}
		out = append(out, bc)
	}
	if kept > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d Bearer-Token(s) nicht in der Datei – bisherige beibehalten", kept))
	}
	return out, gs, nil
}

// importTokens replaces the tokens created in the UI; tokens from API_KEYS only get their settings.
func (a *Access) importTokens(in []tokenExport, res *importResult) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	cur := map[string]*AccessToken{}
	var env []*AccessToken
	for _, t := range a.tokens {
		cur[t.Name] = t
		if t.FromEnv {
			env = append(env, t)
		}
	}
	apply := func(t *AccessToken, e tokenExport) {
		t.PriceIn, t.PriceOut, t.CostCap, t.Disabled = e.PriceIn, e.PriceOut, e.CostCap, e.Disabled
		t.CapPeriod, _ = normalizePeriod(e.CapPeriod)
		if t.CapPeriod == "" {
			t.CapPeriod = PeriodMonth
		}
		if t.CostCap == 0 {
			t.capped = false
		}
	}
	next := append([]*AccessToken{}, env...)
	n, skipped := 0, 0
	for _, e := range in {
		t := cur[e.Name]
		switch {
		case t != nil && t.FromEnv:
			apply(t, e)
			n++
		case e.FromEnv:
			res.Warnings = append(res.Warnings, fmt.Sprintf("API-Token %q stammt aus API_KEYS und fehlt hier – übersprungen", e.Name))
		case t != nil:
			if e.Hash != "" {
				t.Hash, t.Hint = e.Hash, e.Hint
			}
			apply(t, e)
			next = append(next, t)
			n++
		case e.Hash != "":
			nt := &AccessToken{Name: e.Name, Hash: e.Hash, Hint: e.Hint, Created: time.Now()}
			apply(nt, e)
			next = append(next, nt)
			n++
		default:
			skipped++
		}
	}
	if skipped > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d API-Token(s) ohne Hash (Export ohne Geheimnisse) und hier unbekannt – übersprungen", skipped))
	}
	a.tokens = next
	if err := a.saveLocked(); err != nil {
		res.Warnings = append(res.Warnings, err.Error())
	}
	if a.onChange != nil {
		go a.onChange()
	}
	return n
}

func (u *Users) prepareImport(in []User, res *importResult) ([]*User, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []*User
	names := map[string]bool{}
	admins, skipped := 0, 0
	for _, x := range in {
		x.Name = strings.TrimSpace(x.Name)
		if !userNameRe.MatchString(x.Name) {
			return nil, fmt.Errorf("ungültiger Benutzername %q", x.Name)
		}
		if !validRole(x.Role) {
			return nil, fmt.Errorf("Benutzer %q: unbekannte Rolle %q", x.Name, x.Role)
		}
		if names[strings.ToLower(x.Name)] {
			return nil, fmt.Errorf("Benutzer %q doppelt", x.Name)
		}
		names[strings.ToLower(x.Name)] = true
		if x.Hash == "" {
			if cur := u.findLocked(x.Name); cur != nil {
				x.Hash = cur.Hash
			}
		}
		if x.Hash == "" {
			skipped++
			continue
		}
		if !strings.HasPrefix(x.Hash, "pbkdf2-sha256$") {
			return nil, fmt.Errorf("Benutzer %q: unbekanntes Passwort-Format", x.Name)
		}
		if x.Role == RoleAdmin && !x.Disabled {
			admins++
		}
		c := x
		out = append(out, &c)
	}
	if skipped > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d Benutzer ohne Passwort (Export ohne Geheimnisse) und hier unbekannt – übersprungen", skipped))
	}
	if admins == 0 {
		return nil, errors.New("nach dem Import gäbe es keinen aktiven Administrator – abgebrochen")
	}
	return out, nil
}

func (u *Users) applyImport(list []*User) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.users = list
	for k, s := range u.sessions { // sessions of removed or locked users end
		if x := u.findLocked(s.User); x == nil || x.Disabled {
			delete(u.sessions, k)
		}
	}
	u.saveSessionsLocked()
	return u.saveLocked()
}

func registerConfigIO(mux *http.ServeMux, pre string, pool *Pool, acc *Access, users *Users, token string) {
	mux.HandleFunc("GET "+pre+"/api/config/export", adminAuth(token, func(w http.ResponseWriter, r *http.Request) {
		sec, err := parseSections(r.URL.Query().Get("sections"))
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		secrets := r.URL.Query().Get("secrets") == "1" || r.URL.Query().Get("secrets") == "true"
		f := exportConfig(pool, acc, users, sec, secrets)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="llm-balancer-config_%s.json"`, time.Now().Format("2006-01-02")))
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(f)
		who := "?"
		if id := identityFrom(r); id != nil {
			who = id.Name
		}
		log.Printf("configuration exported by %s (secrets=%v)", who, secrets)
	}))
	mux.HandleFunc("POST "+pre+"/api/config/import", adminAuth(token, func(w http.ResponseWriter, r *http.Request) {
		sec, err := parseSections(r.URL.Query().Get("sections"))
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		var f ConfigFile
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8<<20)).Decode(&f); err != nil {
			writeAPIError(w, http.StatusBadRequest, fmt.Errorf("Datei nicht lesbar: %v", err))
			return
		}
		res, err := importConfig(pool, acc, users, f, sec)
		if err != nil {
			writeResult(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(res)
	}))
}
