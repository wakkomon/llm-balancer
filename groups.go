package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// GroupConfig is a named set of servers that serve certain models.
// Servers join a group via their "group" field.
type GroupConfig struct {
	Name       string   `json:"name"`
	Models     []string `json:"models"`      // model names or patterns with *, e.g. "qwen3*"
	AutoModels *bool    `json:"auto_models"` // also route models the members report via /v1/models (default true)
	// Fallback: if a request for this group waits longer than FallbackAfterSec in the queue (or every
	// server of the group is down), the servers of the Fallback group may take it as well.
	Fallback         string `json:"fallback,omitempty"`
	FallbackAfterSec int    `json:"fallback_after_sec,omitempty"`
	FallbackModel    string `json:"fallback_model,omitempty"` // optional: model name to send to the fallback servers
	// ForceModel: requests handled by a server of this group are sent with this model name
	// (the routing still uses the requested name).
	ForceModel string `json:"force_model,omitempty"`
	// Disabled: the servers of the group take no requests; requests for its models go straight to
	// the fallback group (or other servers that have the model).
	Disabled     bool `json:"disabled,omitempty"`
	keepDisabled bool // update without "disabled": keep the current value
}

// syncGroupsLocked updates Backend.groupOff from the group settings. Caller holds p.mu.
func (p *Pool) syncGroupsLocked() {
	off := map[string]bool{}
	for _, g := range p.groups {
		if g.Disabled {
			off[g.Name] = true
		}
	}
	for _, b := range p.backends {
		b.groupOff = b.group != "" && off[b.group]
	}
}

// SetGroupDisabled switches a group off or on.
func (p *Pool) SetGroupDisabled(name string, disabled bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	g := p.groupLocked(name)
	if g == nil {
		return httpErr{404, errors.New("unbekannte Gruppe")}
	}
	g.Disabled = disabled
	log.Printf("group %s %s via UI", name, map[bool]string{true: "disabled", false: "enabled"}[disabled])
	p.dispatchLocked()
	return p.persistLocked()
}

func (g GroupConfig) auto() bool { return g.AutoModels == nil || *g.AutoModels }

var ErrNoRoute = errors.New("no group serves this model")

func normalizeGroup(g *GroupConfig) error {
	g.Name = strings.TrimSpace(g.Name)
	if g.Name == "" || strings.ContainsAny(g.Name, "/?#,") {
		return errors.New("Gruppenname fehlt oder enthält / ? # ,")
	}
	var ms []string
	seen := map[string]bool{}
	for _, m := range g.Models {
		m = strings.TrimSpace(m)
		if m != "" && !seen[strings.ToLower(m)] {
			seen[strings.ToLower(m)] = true
			ms = append(ms, m)
		}
	}
	g.Models = ms
	if g.Models == nil {
		g.Models = []string{}
	}
	g.Fallback = strings.TrimSpace(g.Fallback)
	g.FallbackModel = strings.TrimSpace(g.FallbackModel)
	g.ForceModel = strings.TrimSpace(g.ForceModel)
	if g.FallbackAfterSec < 0 {
		return errors.New("Fallback-Wartezeit darf nicht negativ sein")
	}
	if g.Fallback == g.Name {
		return errors.New("eine Gruppe kann nicht ihr eigener Fallback sein")
	}
	return nil
}

// modelMatch compares case-insensitively; "*" matches any sequence (also "/" and ":").
func modelMatch(pattern, model string) bool {
	p, m := strings.ToLower(pattern), strings.ToLower(model)
	if !strings.Contains(p, "*") {
		return p == m
	}
	parts := strings.Split(p, "*")
	if !strings.HasPrefix(m, parts[0]) {
		return false
	}
	m = m[len(parts[0]):]
	for i := 1; i < len(parts)-1; i++ {
		k := strings.Index(m, parts[i])
		if k < 0 {
			return false
		}
		m = m[k+len(parts[i]):]
	}
	return strings.HasSuffix(m, parts[len(parts)-1])
}

func (b *Backend) reports(model string) bool {
	for _, x := range b.models {
		if strings.EqualFold(x, model) {
			return true
		}
	}
	return false
}

// serves: the server takes this model – by its manual filter, or because it reports it (unless the
// admin switched reported models off for this server).
func (b *Backend) serves(model string) bool { return b.serveLevel(model) > 0 }

// match quality: an exactly known model beats a wildcard rule
const (
	matchNone    = 0
	matchPattern = 1 // only via a filter with *
	matchExact   = 2 // reported by the server, or listed by name in the filter
)

func (b *Backend) serveLevel(model string) int {
	if model == "" {
		return matchNone
	}
	if b.autoModels && b.reports(model) {
		return matchExact
	}
	return patternLevel(b.manual, model)
}

// patternLevel: how a list of names/patterns matches a model.
func patternLevel(list []string, model string) int {
	lvl := matchNone
	for _, p := range list {
		if !strings.Contains(p, "*") {
			if strings.EqualFold(p, model) {
				return matchExact
			}
		} else if modelMatch(p, model) {
			lvl = matchPattern
		}
	}
	return lvl
}

// servingMembers narrows a group to the members that serve the model (or the model the group forces).
// If no member is known to serve it (pattern match, servers without a model list), the whole group
// may take it, as before.
// Members that know the model by name are preferred over members that only match a wildcard.
func servingMembers(g *GroupConfig, members []*Backend, model string) []*Backend {
	best, out := matchNone, []*Backend(nil)
	for _, b := range members {
		lvl := b.serveLevel(model)
		if g != nil && g.ForceModel != "" {
			lvl = max(lvl, b.serveLevel(g.ForceModel))
		}
		switch {
		case lvl == matchNone || lvl < best:
		case lvl > best:
			best, out = lvl, []*Backend{b}
		default:
			out = append(out, b)
		}
	}
	if len(out) == 0 {
		return members
	}
	return out
}

func (p *Pool) groupLocked(name string) *GroupConfig {
	for i := range p.groups {
		if p.groups[i].Name == name {
			return &p.groups[i]
		}
	}
	return nil
}

func (p *Pool) membersLocked(group string) []*Backend {
	var out []*Backend
	for _, b := range p.backends {
		if b.group == group {
			out = append(out, b)
		}
	}
	return out
}

// Route describes which servers may handle a model.
type Route struct {
	allow    map[*Backend]bool // nil = every server
	Group    string            // matching group(s), comma separated
	Mode     string            // group | model | all | reject
	groups   []string
	fallback map[*Backend]string // servers added as fallback → model to send instead ("" = unchanged)
	pref     map[*Backend]int    // several target models: index of the first one the server serves (lower = preferred)
}

// routeReqLocked: like routeWaitLocked, but a request with several alternative models (router target
// "a, b") may take a server of any of them – it waits for whichever becomes free first.
func (p *Pool) routeReqLocked(model string, alts []string, waited time.Duration, level int, ollamaOnly bool) Route {
	if len(alts) <= 1 {
		return p.routeWaitLocked(model, waited, level, ollamaOnly)
	}
	var out Route
	allow, fallback, pref := map[*Backend]bool{}, map[*Backend]string{}, map[*Backend]int{}
	all := false
	seenG := map[string]bool{}
	for i, m := range alts {
		rt := p.routeWaitLocked(m, waited, level, ollamaOnly)
		if rt.Mode == "reject" {
			continue
		}
		if out.Mode == "" || rt.Mode == "group" {
			out.Mode = rt.Mode
		}
		for _, g := range rt.groups {
			if !seenG[g] {
				seenG[g] = true
				out.groups = append(out.groups, g)
			}
		}
		if rt.allow == nil {
			all = true
		}
		for _, b := range p.backends {
			if !rt.allows(b) {
				continue
			}
			fm, isFb := rt.fallback[b]
			if _, ok := pref[b]; !ok {
				pref[b] = i
			}
			if !allow[b] {
				allow[b] = true
				if isFb {
					fallback[b] = fm
				}
			} else if !isFb {
				delete(fallback, b) // allowed directly for another model: not a fallback
			}
		}
	}
	if out.Mode == "" {
		return Route{allow: map[*Backend]bool{}, Mode: "reject"}
	}
	if !all {
		out.allow = allow
	}
	out.Group = strings.Join(out.groups, ",")
	if len(fallback) > 0 {
		out.fallback = fallback
	}
	out.pref = pref
	return out
}

// splitModels: "a, b" → [a b]
func splitModels(s string) []string {
	var out []string
	for _, m := range strings.Split(s, ",") {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	return out
}

// routeWaitLocked is routeLocked plus fallback groups: their servers are added once the request has
// waited long enough (or force, when every server of the group is down). Caller holds p.mu.
// level 0: normal; 1: fallback group without waiting (group exhausted or down);
// 2: additionally every other server that reports the model itself (last resort).
func (p *Pool) routeWaitLocked(model string, waited time.Duration, level int, ollamaOnly bool) Route {
	rt := p.routeWaitBase(model, waited, level)
	if !ollamaOnly {
		return rt
	}
	// native Ollama API (/api/chat …): only Ollama servers understand it
	allow := map[*Backend]bool{}
	for _, b := range p.backends {
		if b.isOllama() && rt.allows(b) {
			allow[b] = true
		}
	}
	rt.allow = allow
	return rt
}

func (p *Pool) routeWaitBase(model string, waited time.Duration, level int) Route {
	force := level >= 1
	rt := p.routeLocked(model)
	if rt.Mode != "group" {
		return rt
	}
	defer func() {
		if level < 2 || model == "" {
			return
		}
		for _, b := range p.backends {
			if rt.allow[b] || !b.serves(model) {
				continue
			}
			if rt.fallback == nil {
				rt.fallback = map[*Backend]string{}
			}
			rt.allow[b] = true
			rt.fallback[b] = "" // the server has the model: send it unchanged
		}
	}()
	for _, name := range rt.groups {
		g := p.groupLocked(name)
		if g == nil || g.Fallback == "" || p.groupLocked(g.Fallback) == nil {
			continue
		}
		if !force && waited < time.Duration(g.FallbackAfterSec)*time.Second {
			continue
		}
		target := model
		if g.FallbackModel != "" {
			target = g.FallbackModel
		}
		for _, b := range servingMembers(p.groupLocked(g.Fallback), p.membersLocked(g.Fallback), target) {
			if rt.allow[b] {
				continue
			}
			if rt.fallback == nil {
				rt.fallback = map[*Backend]string{}
			}
			rt.allow[b] = true
			rt.fallback[b] = g.FallbackModel
		}
	}
	return rt
}

// routeLocked resolves a model name to the servers that may handle it. Caller holds p.mu.
//  1. groups whose model list matches (pattern, or a member reports the model when auto_models is on)
//  2. otherwise: any server that reports the model in /v1/models
//  3. otherwise: every server (or reject, if unknown_model = "reject" and groups exist)
func (p *Pool) routeLocked(model string) Route {
	if model == "" {
		return Route{Mode: "all"}
	}
	allow := map[*Backend]bool{}
	var names []string
	// groups that know the model by name win over groups that only match a wildcard
	best := matchNone
	for i := range p.groups {
		g := &p.groups[i]
		members := p.membersLocked(g.Name)
		lvl := patternLevel(g.Models, model)
		if g.auto() {
			for _, b := range members {
				lvl = max(lvl, b.serveLevel(model))
			}
		}
		if lvl == matchNone || lvl < best {
			continue
		}
		if lvl > best {
			best, names, allow = lvl, nil, map[*Backend]bool{}
		}
		names = append(names, g.Name)
		for _, b := range servingMembers(g, members, model) { // only the members that have the model
			allow[b] = true
		}
	}
	if len(names) > 0 {
		return Route{allow: allow, Group: strings.Join(names, ","), Mode: "group", groups: names}
	}
	best = matchNone
	for _, b := range p.backends {
		lvl := b.serveLevel(model)
		if lvl == matchNone || lvl < best {
			continue
		}
		if lvl > best {
			best, allow = lvl, map[*Backend]bool{}
		}
		allow[b] = true
	}
	if len(allow) > 0 {
		return Route{allow: allow, Mode: "model"}
	}
	if p.cfg.UnknownModel == "reject" && len(p.groups) > 0 {
		return Route{allow: map[*Backend]bool{}, Mode: "reject"}
	}
	return Route{Mode: "all"}
}

func (r Route) allows(b *Backend) bool { return r.allow == nil || r.allow[b] }

// ---- model discovery ----

// parseModelList understands OpenAI /v1/models ({"data":[{"id":..}]}) and Ollama /api/tags ({"models":[{"name":..}]}).
func parseModelList(body []byte) ([]string, bool) {
	var m struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if json.Unmarshal(body, &m) != nil {
		return nil, false
	}
	var out []string
	for _, d := range m.Data {
		if d.ID != "" {
			out = append(out, d.ID)
		}
	}
	for _, d := range m.Models {
		if d.Name != "" {
			out = append(out, d.Name)
		} else if d.Model != "" {
			out = append(out, d.Model)
		}
	}
	sort.Strings(out)
	return out, m.Data != nil || m.Models != nil
}

func (p *Pool) SetModels(b *Backend, models []string, checkedURL string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if b.URL != checkedURL {
		return
	}
	changed := strings.Join(b.models, "\n") != strings.Join(models, "\n")
	b.models, b.modelsAt = models, time.Now()
	if changed {
		log.Printf("backend %s serves %d model(s): %s", b.Name, len(models), strings.Join(models, ", "))
		p.dispatchLocked() // a waiting request may now have a server
	}
}

// discoverModels fetches /v1/models from a server (reusing the health check response when possible).
func (px *Proxy) discoverModels(client *http.Client, ht healthTarget, body []byte) {
	if client == nil {
		client = &http.Client{Transport: px.transportFor(ht.insecure), Timeout: 5 * time.Second}
	}
	if body == nil {
		req, _ := http.NewRequest(http.MethodGet, ht.url+"/v1/models", nil)
		if ht.key != "" {
			req.Header.Set("Authorization", "Bearer "+ht.key)
		}
		resp, err := client.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return
		}
		body, _ = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	}
	if models, ok := parseModelList(body); ok {
		px.pool.SetModels(ht.b, models, ht.url)
	}
}

// AllModels is the union of discovered models plus exact (non-pattern) group models.
func (p *Pool) AllModels() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	add := func(m string) {
		if k := strings.ToLower(m); !seen[k] {
			seen[k] = true
			out = append(out, m)
		}
	}
	for _, b := range p.backends {
		if b.enabled {
			if b.autoModels {
				for _, m := range b.models {
					add(m)
				}
			}
			for _, m := range b.manual {
				if !strings.Contains(m, "*") {
					add(m)
				}
			}
		}
	}
	for _, g := range p.groups {
		for _, m := range g.Models {
			if !strings.Contains(m, "*") {
				add(m)
			}
		}
	}
	for _, rc := range p.routers { // virtual models of the routers, e.g. "auto"
		if !rc.Disabled {
			add(rc.Name)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}

// modelsFor: the models a client token may use (all if it has no list).
func (px *Proxy) modelsFor(token string) []string {
	all := px.pool.AllModels()
	if px.access == nil || len(px.access.ModelsOf(token)) == 0 {
		return all
	}
	out := []string{}
	for _, m := range all {
		if px.access.Allowed(token, m) {
			out = append(out, m)
		}
	}
	return out
}

// serveModel answers GET /v1/models/{id} for a model the balancer knows; false = unknown here.
func (px *Proxy) serveModel(w http.ResponseWriter, id, token string) bool {
	if u, err := url.PathUnescape(id); err == nil {
		id = u
	}
	for _, m := range px.modelsFor(token) {
		if strings.EqualFold(m, id) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"id": m, "object": "model", "created": px.pool.started.Unix(), "owned_by": "llm-balancer"})
			return true
		}
	}
	return false
}

// serveModelList answers GET /v1/models with the models of all servers.
func (px *Proxy) serveModelList(w http.ResponseWriter, token string) bool {
	models := px.modelsFor(token)
	if len(px.pool.AllModels()) == 0 {
		return false
	}
	type entry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	list := struct {
		Object string  `json:"object"`
		Data   []entry `json:"data"`
	}{Object: "list"}
	for _, m := range models {
		list.Data = append(list.Data, entry{m, "model", px.pool.started.Unix(), "llm-balancer"})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
	return true
}

// ---- group administration ----

func (p *Pool) setMembersLocked(group string, members []string) error {
	want := map[string]bool{}
	for _, m := range members {
		_, b := p.findLocked(m)
		if b == nil {
			return httpErr{400, fmt.Errorf("unbekannter Server %q", m)}
		}
		want[m] = true
	}
	for _, b := range p.backends {
		if want[b.Name] {
			b.group = group
		} else if b.group == group {
			b.group = ""
		}
	}
	return nil
}

func (p *Pool) AddGroup(g GroupConfig, members []string) error {
	if err := normalizeGroup(&g); err != nil {
		return httpErr{400, err}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.groupLocked(g.Name) != nil {
		return httpErr{409, fmt.Errorf("Gruppe %q gibt es schon", g.Name)}
	}
	if g.Fallback != "" && p.groupLocked(g.Fallback) == nil {
		return httpErr{400, fmt.Errorf("unbekannte Fallback-Gruppe %q", g.Fallback)}
	}
	p.groups = append(p.groups, g)
	if err := p.setMembersLocked(g.Name, members); err != nil {
		p.groups = p.groups[:len(p.groups)-1]
		return err
	}
	log.Printf("group %s added via UI (models %v, members %v)", g.Name, g.Models, members)
	p.dispatchLocked()
	return p.persistLocked()
}

// UpdateGroup changes a group; members nil = keep membership.
func (p *Pool) UpdateGroup(name string, g GroupConfig, members []string) error {
	if err := normalizeGroup(&g); err != nil {
		return httpErr{400, err}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.groupLocked(name)
	if cur == nil {
		return httpErr{404, errors.New("unbekannte Gruppe")}
	}
	if g.Name != name && p.groupLocked(g.Name) != nil {
		return httpErr{409, fmt.Errorf("Gruppe %q gibt es schon", g.Name)}
	}
	if g.Fallback != "" && g.Fallback != name && p.groupLocked(g.Fallback) == nil {
		return httpErr{400, fmt.Errorf("unbekannte Fallback-Gruppe %q", g.Fallback)}
	}
	if g.Fallback == name { // renamed group pointing to its old name
		return httpErr{400, errors.New("eine Gruppe kann nicht ihr eigener Fallback sein")}
	}
	if members != nil {
		for _, m := range members {
			if _, b := p.findLocked(m); b == nil {
				return httpErr{400, fmt.Errorf("unbekannter Server %q", m)}
			}
		}
	}
	if g.keepDisabled {
		g.Disabled = cur.Disabled
	}
	g.keepDisabled = false
	*cur = g
	for _, b := range p.backends {
		if b.group == name {
			b.group = g.Name
		}
	}
	for i := range p.groups { // other groups that use this one as fallback follow the rename
		if p.groups[i].Fallback == name {
			p.groups[i].Fallback = g.Name
		}
	}
	if members != nil {
		p.setMembersLocked(g.Name, members)
	}
	log.Printf("group %s updated via UI (models %v)", g.Name, g.Models)
	p.dispatchLocked()
	return p.persistLocked()
}

func (p *Pool) DeleteGroup(name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.groups {
		if p.groups[i].Name == name {
			p.groups = append(p.groups[:i:i], p.groups[i+1:]...)
			for _, b := range p.backends {
				if b.group == name {
					b.group = ""
				}
			}
			for k := range p.groups {
				if p.groups[k].Fallback == name {
					p.groups[k].Fallback = ""
				}
			}
			log.Printf("group %s deleted via UI", name)
			p.dispatchLocked()
			return p.persistLocked()
		}
	}
	return httpErr{404, errors.New("unbekannte Gruppe")}
}

type groupInput struct {
	Name             string    `json:"name"`
	Models           []string  `json:"models"`
	AutoModels       *bool     `json:"auto_models"`
	Members          *[]string `json:"members"`
	Fallback         string    `json:"fallback"`
	FallbackAfterSec int       `json:"fallback_after_sec"`
	FallbackModel    string    `json:"fallback_model"`
	ForceModel       string    `json:"force_model"`
	Disabled         *bool     `json:"disabled"`
}

func registerGroups(mux *http.ServeMux, pre string, pool *Pool, token string) {
	decode := func(r *http.Request) (GroupConfig, []string, error) {
		var in groupInput
		dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			return GroupConfig{}, nil, httpErr{400, fmt.Errorf("ungültiges JSON: %v", err)}
		}
		var members []string
		if in.Members != nil {
			members = append([]string{}, (*in.Members)...)
		}
		return GroupConfig{Name: in.Name, Models: in.Models, AutoModels: in.AutoModels,
			Fallback: in.Fallback, FallbackAfterSec: in.FallbackAfterSec, FallbackModel: in.FallbackModel,
			ForceModel: in.ForceModel, Disabled: in.Disabled != nil && *in.Disabled, keepDisabled: in.Disabled == nil}, members, nil
	}
	mux.HandleFunc("POST "+pre+"/api/groups", adminAuth(token, func(w http.ResponseWriter, r *http.Request) {
		g, members, err := decode(r)
		if err == nil {
			err = pool.AddGroup(g, members)
		}
		writeResult(w, err)
	}))
	mux.HandleFunc("PUT "+pre+"/api/groups/{name}", adminAuth(token, func(w http.ResponseWriter, r *http.Request) {
		g, members, err := decode(r)
		if err == nil {
			err = pool.UpdateGroup(r.PathValue("name"), g, members)
		}
		writeResult(w, err)
	}))
	for _, act := range []string{"enable", "disable"} {
		mux.HandleFunc("POST "+pre+"/api/groups/{name}/"+act, adminAuth(token, func(w http.ResponseWriter, r *http.Request) {
			writeResult(w, pool.SetGroupDisabled(r.PathValue("name"), act == "disable"))
		}))
	}
	mux.HandleFunc("DELETE "+pre+"/api/groups/{name}", adminAuth(token, func(w http.ResponseWriter, r *http.Request) {
		writeResult(w, pool.DeleteGroup(r.PathValue("name")))
	}))
	// Routing preview: which servers would get a request for this model?
	mux.HandleFunc("GET "+pre+"/api/route", func(w http.ResponseWriter, r *http.Request) {
		model := r.URL.Query().Get("model")
		pool.mu.Lock()
		rt := pool.routeLocked(model)
		names := []string{}
		for _, b := range pool.backends {
			if rt.allows(b) {
				names = append(names, b.Name)
			}
		}
		pool.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"model": model, "mode": rt.Mode, "group": rt.Group, "backends": names})
	})
}
