package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Model router: sits between the request and the group/server choice. A router catches requests by
// a virtual model name (e.g. "auto") and/or patterns of real model names, checks its rules from top to
// bottom and replaces the model; the server choice then works as usual by the (new) model name.

type RouterRule struct {
	Name     string `json:"name"`
	Disabled bool   `json:"disabled,omitempty"`
	// target: a model, or (if empty) the best model with the required capabilities
	Model string   `json:"model,omitempty"`
	Tier  string   `json:"tier,omitempty"`  // or a tier of the router (its model pool)
	Needs []string `json:"needs,omitempty"` // vision | tools | reasoning | context
	// conditions – every condition that is set must hold
	MinTokens int      `json:"min_tokens,omitempty"` // estimated prompt tokens
	MaxTokens int      `json:"max_tokens,omitempty"`
	Images    *bool    `json:"images,omitempty"`
	Tools     *bool    `json:"tools,omitempty"`
	JSON      *bool    `json:"json,omitempty"`
	Reasoning *bool    `json:"reasoning,omitempty"`
	Pattern   string   `json:"pattern,omitempty"`    // regular expression, case-insensitive
	PatternIn string   `json:"pattern_in,omitempty"` // last (default) | system | all
	Tokens    []string `json:"tokens,omitempty"`     // API token names
	Clients   []string `json:"clients,omitempty"`    // IP, CIDR or prefix
	Header    string   `json:"header,omitempty"`     // "X-Name: regex"
	QueueMin  int      `json:"queue_min,omitempty"`  // queue length ≥ N
	WhenBusy  string   `json:"when_busy,omitempty"`  // model whose servers have no free slot
	Hours     string   `json:"hours,omitempty"`      // "22-6"
	re        *regexp.Regexp
	hre       *regexp.Regexp
}

type RouterConfig struct {
	Name     string       `json:"name"`             // also the virtual model name clients can ask for
	Models   []string     `json:"models,omitempty"` // real model names/patterns the router also takes over
	Default  string       `json:"default,omitempty"`
	AutoCaps *bool        `json:"auto_caps,omitempty"` // switch to a capable model if the chosen one lacks a needed feature (default on)
	Sticky   bool         `json:"sticky,omitempty"`    // keep the model per session
	Disabled bool         `json:"disabled,omitempty"`
	Rules    []RouterRule `json:"rules"`
	// decision when no rule matches (LiteLLM-like)
	Classifier     string              `json:"classifier,omitempty"` // "" | heuristic | llm | arch
	Tiers          map[string][]string `json:"tiers,omitempty"`      // SIMPLE/MEDIUM/COMPLEX/REASONING → models
	Routes         []RouteDef          `json:"routes,omitempty"`     // Arch-Router routes
	ClassModel     string              `json:"class_model,omitempty"`
	ClassServer    string              `json:"class_server,omitempty"`
	ClassTimeoutMs int                 `json:"class_timeout_ms,omitempty"`
	ClassTurns     *int                `json:"class_turns,omitempty"`
	ClassPrompt    string              `json:"class_prompt,omitempty"`
	ClassFallback  string              `json:"class_fallback,omitempty"` // default | heuristic
	Thresholds     []float64           `json:"thresholds,omitempty"`
}

func (rc RouterConfig) autoCaps() bool { return rc.AutoCaps == nil || *rc.AutoCaps }

var capNames = map[string]string{"vision": "Bilder", "tools": "Tools", "reasoning": "Denken", "context": "Kontext"}

func normalizeRouter(rc *RouterConfig) error {
	rc.Name = strings.TrimSpace(rc.Name)
	if rc.Name == "" || strings.ContainsAny(rc.Name, " /?#,") {
		return errors.New("Name fehlt oder enthält Leerzeichen / ? # ,")
	}
	rc.Default = strings.TrimSpace(rc.Default)
	var ms []string
	for _, m := range rc.Models {
		if m = strings.TrimSpace(m); m != "" {
			ms = append(ms, m)
		}
	}
	rc.Models = ms
	if rc.Rules == nil {
		rc.Rules = []RouterRule{}
	}
	if err := normalizeClassifier(rc); err != nil {
		return err
	}
	for i := range rc.Rules {
		r := &rc.Rules[i]
		r.Name = strings.TrimSpace(r.Name)
		if r.Name == "" {
			r.Name = "Regel " + strconv.Itoa(i+1)
		}
		r.Model = strings.TrimSpace(r.Model)
		for _, n := range r.Needs {
			if capNames[n] == "" {
				return fmt.Errorf("%s: unbekannte Fähigkeit %q", r.Name, n)
			}
		}
		r.Tier = strings.ToUpper(strings.TrimSpace(r.Tier))
		if r.Tier != "" && !validTier(r.Tier) {
			return fmt.Errorf("%s: unbekannte Stufe %q", r.Name, r.Tier)
		}
		if r.Model == "" && len(r.Needs) == 0 && r.Tier == "" {
			return fmt.Errorf("%s: Zielmodell, Stufe oder benötigte Fähigkeit fehlt", r.Name)
		}
		if r.Pattern != "" {
			re, err := regexp.Compile("(?is)" + r.Pattern)
			if err != nil {
				return fmt.Errorf("%s: ungültiges Muster: %v", r.Name, err)
			}
			r.re = re
		}
		switch r.PatternIn {
		case "", "last", "system", "all":
		default:
			return fmt.Errorf("%s: Textquelle %q unbekannt", r.Name, r.PatternIn)
		}
		if r.Header != "" {
			name, val, ok := strings.Cut(r.Header, ":")
			if !ok || strings.TrimSpace(name) == "" {
				return fmt.Errorf("%s: Header als „Name: Muster“ angeben", r.Name)
			}
			re, err := regexp.Compile("(?i)" + strings.TrimSpace(val))
			if err != nil {
				return fmt.Errorf("%s: ungültiges Header-Muster: %v", r.Name, err)
			}
			r.hre = re
		}
		if r.Hours != "" {
			if _, _, err := parseHours(r.Hours); err != nil {
				return fmt.Errorf("%s: %v", r.Name, err)
			}
		}
		if r.MinTokens < 0 || r.MaxTokens < 0 || r.QueueMin < 0 {
			return fmt.Errorf("%s: negative Werte sind nicht erlaubt", r.Name)
		}
	}
	return nil
}

func parseHours(s string) (int, int, error) {
	a, b, ok := strings.Cut(strings.ReplaceAll(s, " ", ""), "-")
	from, err1 := strconv.Atoi(a)
	to, err2 := strconv.Atoi(b)
	if !ok || err1 != nil || err2 != nil || from < 0 || from > 24 || to < 0 || to > 24 {
		return 0, 0, errors.New("Uhrzeit als „von-bis“ in vollen Stunden, z. B. 22-6")
	}
	return from, to, nil
}

func inHours(s string, now time.Time) bool {
	from, to, err := parseHours(s)
	if err != nil {
		return false
	}
	h := now.Hour()
	if from <= to {
		return h >= from && h < to
	}
	return h >= from || h < to // over midnight
}

// ---- what a request contains ----

type reqFeatures struct {
	Tokens    int    `json:"tokens"` // estimated prompt tokens
	Images    bool   `json:"images"`
	Tools     bool   `json:"tools"`
	JSON      bool   `json:"json"`
	Reasoning bool   `json:"reasoning"`
	system    string // texts for pattern rules
	last      string
	all       string
}

func analyzeRequest(body []byte) reqFeatures {
	var f reqFeatures
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		return f
	}
	var sys, all strings.Builder
	addText := func(sb *strings.Builder, v any) string {
		t := contentText(v)
		sb.WriteString(t)
		sb.WriteByte('\n')
		return t
	}
	hasImage := func(v any) bool {
		parts, ok := v.([]any)
		if !ok {
			return false
		}
		for _, p := range parts {
			if m, ok := p.(map[string]any); ok {
				switch m["type"] {
				case "image_url", "input_image", "image":
					return true
				}
			}
		}
		return false
	}
	addText(&sys, doc["system"])
	if s, ok := doc["instructions"].(string); ok {
		sys.WriteString(s)
	}
	msgs, _ := doc["messages"].([]any)
	if in, ok := doc["input"].([]any); ok && msgs == nil {
		msgs = in
	}
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := mm["role"].(string)
		if role == "system" || role == "developer" {
			addText(&sys, mm["content"])
			continue
		}
		t := addText(&all, mm["content"])
		if role == "user" {
			f.last = t
		}
		if hasImage(mm["content"]) {
			f.Images = true
		}
		if imgs, ok := mm["images"].([]any); ok && len(imgs) > 0 { // Ollama
			f.Images = true
		}
		if tc, ok := mm["tool_calls"].([]any); ok && len(tc) > 0 {
			all.WriteString(fmt.Sprint(tc))
		}
	}
	if p, ok := doc["prompt"].(string); ok { // completions / Ollama generate
		all.WriteString(p)
		f.last = p
	}
	if imgs, ok := doc["images"].([]any); ok && len(imgs) > 0 {
		f.Images = true
	}
	if t, ok := doc["tools"].([]any); ok && len(t) > 0 {
		f.Tools = true
		b, _ := json.Marshal(t)
		all.Write(b)
	}
	if t, ok := doc["functions"].([]any); ok && len(t) > 0 {
		f.Tools = true
	}
	if rf, ok := doc["response_format"].(map[string]any); ok {
		if t, _ := rf["type"].(string); t == "json_object" || t == "json_schema" {
			f.JSON = true
		}
	}
	if fm, ok := doc["format"]; ok && fm != nil && fm != "" { // Ollama: "json" or a schema
		f.JSON = true
	}
	if _, ok := doc["reasoning_effort"]; ok {
		f.Reasoning = true
	}
	if _, ok := doc["reasoning"].(map[string]any); ok {
		f.Reasoning = true
	}
	if th, ok := doc["think"]; ok && th != false && th != nil { // Ollama
		f.Reasoning = true
	}
	if th, ok := doc["thinking"].(map[string]any); ok && th["type"] == "enabled" { // Anthropic
		f.Reasoning = true
	}
	if k, ok := doc["chat_template_kwargs"].(map[string]any); ok && k["enable_thinking"] == true {
		f.Reasoning = true
	}
	f.system, f.all = sys.String(), all.String()
	// ~3.5 characters per token for mixed German/English text and code
	f.Tokens = int(float64(len(f.system)+len(f.all)) / 3.5)
	return f
}

// ---- model capabilities ----

// ModelFeature describes what a model can do; nil = unknown.
type ModelFeature struct {
	Model     string `json:"model"` // name or pattern (manual entries)
	Vision    *bool  `json:"vision,omitempty"`
	Tools     *bool  `json:"tools,omitempty"`
	Reasoning *bool  `json:"reasoning,omitempty"`
	Context   int    `json:"context,omitempty"` // tokens
}

// featuresOf: manual entries of the server (first matching pattern per field) over detected ones.
func (b *Backend) featuresOf(model string) ModelFeature {
	f := ModelFeature{Model: model}
	if d, ok := b.detectedCaps[strings.ToLower(model)]; ok {
		f.Vision, f.Tools, f.Reasoning, f.Context = d.Vision, d.Tools, d.Reasoning, d.Context
	}
	for _, m := range b.features {
		if !modelMatch(m.Model, model) {
			continue
		}
		if m.Vision != nil {
			f.Vision = m.Vision
		}
		if m.Tools != nil {
			f.Tools = m.Tools
		}
		if m.Reasoning != nil {
			f.Reasoning = m.Reasoning
		}
		if m.Context > 0 {
			f.Context = m.Context
		}
		break
	}
	return f
}

// lacks: the model is known NOT to have what is needed (unknown counts as "has").
func (f ModelFeature) lacks(need string, tokens int) bool {
	switch need {
	case "vision":
		return f.Vision != nil && !*f.Vision
	case "tools":
		return f.Tools != nil && !*f.Tools
	case "reasoning":
		return f.Reasoning != nil && !*f.Reasoning
	case "context":
		return f.Context > 0 && f.Context < tokens
	}
	return false
}

// has: the model is known to have it (for choosing a target by capability).
func (f ModelFeature) has(need string, tokens int) bool {
	switch need {
	case "vision":
		return f.Vision != nil && *f.Vision
	case "tools":
		return f.Tools != nil && *f.Tools
	case "reasoning":
		return f.Reasoning != nil && *f.Reasoning
	case "context":
		return f.Context >= tokens && f.Context > 0
	}
	return false
}

// servedModels: concrete model names a server takes (reported + filter entries without *).
func (b *Backend) servedModels() []string {
	var out []string
	seen := map[string]bool{}
	add := func(m string) {
		if !seen[strings.ToLower(m)] && !strings.Contains(m, "*") {
			seen[strings.ToLower(m)] = true
			out = append(out, m)
		}
	}
	for _, m := range b.manual {
		add(m)
	}
	if b.autoModels {
		for _, m := range b.models {
			add(m)
		}
	}
	return out
}

// modelLacksLocked: on every usable server that serves the model, it is known to lack a needed feature.
func (p *Pool) modelLacksLocked(model string, needs []string, tokens int) bool {
	now := time.Now()
	known := false
	for _, b := range p.backends {
		if !b.usable(now) || !b.serves(model) {
			continue
		}
		f := b.featuresOf(model)
		ok := true
		for _, n := range needs {
			if f.lacks(n, tokens) {
				ok = false
			}
		}
		if ok {
			return false
		}
		known = true
	}
	return known
}

// pickCapableLocked chooses a model that has every needed feature: prefer "prefer", then one with a
// free server, then by server priority. "" if none is known.
func (p *Pool) pickCapableLocked(needs []string, tokens int, prefer string) string {
	type cand struct {
		model string
		free  bool
		prio  int
	}
	now := time.Now()
	best := map[string]*cand{}
	for _, b := range p.backends {
		if !b.usable(now) {
			continue
		}
		for _, m := range b.servedModels() {
			f := b.featuresOf(m)
			ok := true
			for _, n := range needs {
				if !f.has(n, tokens) {
					ok = false
				}
			}
			if !ok {
				continue
			}
			if strings.EqualFold(m, prefer) {
				return m
			}
			c := best[strings.ToLower(m)]
			if c == nil {
				c = &cand{model: m, prio: b.Priority}
				best[strings.ToLower(m)] = c
			}
			c.free = c.free || b.active < b.Max
			c.prio = min(c.prio, b.Priority)
		}
	}
	var list []*cand
	for _, c := range best {
		list = append(list, c)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].free != list[j].free {
			return list[i].free
		}
		if list[i].prio != list[j].prio {
			return list[i].prio < list[j].prio
		}
		return list[i].model < list[j].model
	})
	if len(list) == 0 {
		return ""
	}
	return list[0].model
}

// modelBusyLocked: no usable server for the model has a free slot.
func (p *Pool) modelBusyLocked(model string) bool {
	rt := p.routeLocked(model)
	return p.pickFree(nil, rt) == nil
}

// ---- decision ----

type RouteDecision struct {
	Router     string       `json:"router"`
	Rule       string       `json:"rule,omitempty"` // "" = default / unchanged
	Model      string       `json:"model"`          // target
	Reason     string       `json:"reason"`
	Sticky     bool         `json:"sticky,omitempty"`
	Request    reqFeatures  `json:"request"`
	Checks     []string     `json:"checks,omitempty"` // test tool: why rules did not match
	Classifier *classResult `json:"classifier,omitempty"`
}

type routeCtx struct {
	model   string
	token   string
	client  string
	header  http.Header
	session string
	body    []byte
	explain bool
}

func clientMatch(list []string, ip string) bool {
	addr := net.ParseIP(ip)
	for _, c := range list {
		c = strings.TrimSpace(c)
		if strings.Contains(c, "/") {
			if _, n, err := net.ParseCIDR(c); err == nil && addr != nil && n.Contains(addr) {
				return true
			}
		} else if c != "" && strings.HasPrefix(ip, c) {
			return true
		}
	}
	return false
}

func boolCond(want *bool, got bool) bool { return want == nil || *want == got }

// ruleMatchLocked returns "" if the rule matches, otherwise why not (for the test tool).
func (p *Pool) ruleMatchLocked(r *RouterRule, f reqFeatures, c routeCtx, now time.Time) string {
	switch {
	case r.Disabled:
		return "deaktiviert"
	case r.MinTokens > 0 && f.Tokens < r.MinTokens:
		return fmt.Sprintf("≈%d Tokens < %d", f.Tokens, r.MinTokens)
	case r.MaxTokens > 0 && f.Tokens > r.MaxTokens:
		return fmt.Sprintf("≈%d Tokens > %d", f.Tokens, r.MaxTokens)
	case !boolCond(r.Images, f.Images):
		return "Bilder: " + jaNein(f.Images)
	case !boolCond(r.Tools, f.Tools):
		return "Tools: " + jaNein(f.Tools)
	case !boolCond(r.JSON, f.JSON):
		return "JSON-Antwort: " + jaNein(f.JSON)
	case !boolCond(r.Reasoning, f.Reasoning):
		return "Denken angefordert: " + jaNein(f.Reasoning)
	case len(r.Tokens) > 0 && !containsFold(r.Tokens, c.token):
		return "API-Token " + cmp.Or(c.token, "(keiner)")
	case len(r.Clients) > 0 && !clientMatch(r.Clients, c.client):
		return "Client " + c.client
	case r.Hours != "" && !inHours(r.Hours, now):
		return "Uhrzeit " + now.Format("15:04")
	case r.QueueMin > 0 && len(p.queue) < r.QueueMin:
		return fmt.Sprintf("Warteschlange %d < %d", len(p.queue), r.QueueMin)
	case r.WhenBusy != "" && !p.modelBusyLocked(r.WhenBusy):
		return r.WhenBusy + " hat einen freien Server"
	}
	if r.hre != nil {
		name, _, _ := strings.Cut(r.Header, ":")
		if !r.hre.MatchString(c.header.Get(strings.TrimSpace(name))) {
			return "Header " + strings.TrimSpace(name)
		}
	}
	if r.re != nil {
		text := f.last
		switch r.PatternIn {
		case "system":
			text = f.system
		case "all":
			text = f.system + "\n" + f.all
		}
		if !r.re.MatchString(text) {
			return "Muster passt nicht"
		}
	}
	return ""
}

func jaNein(b bool) string {
	if b {
		return "ja"
	}
	return "nein"
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(strings.TrimSpace(x), s) {
			return true
		}
	}
	return false
}

// routerForLocked: a router whose name is the requested model, else the first whose patterns match.
func (p *Pool) routerForLocked(model string) *RouterConfig {
	if model == "" {
		return nil
	}
	for i := range p.routers {
		if !p.routers[i].Disabled && strings.EqualFold(p.routers[i].Name, model) {
			return &p.routers[i]
		}
	}
	for i := range p.routers {
		if !p.routers[i].Disabled && matchesAny(p.routers[i].Models, model) {
			return &p.routers[i]
		}
	}
	return nil
}

type stickyEntry struct {
	model, rule string
	until       time.Time
}

const stickyTTL = 2 * time.Hour

// decide runs the router for a request; nil if no router is responsible. Explicit rules first; if none
// matches, the classifier (heuristic / LLM / Arch-Router) chooses a tier or route; then the default.
// The pool lock is not held while a classifier model answers.
func (p *Pool) decide(c routeCtx) *RouteDecision {
	p.mu.Lock()
	rcp := p.routerForLocked(c.model)
	if rcp == nil {
		p.mu.Unlock()
		return nil
	}
	rc := *rcp
	now := time.Now()
	f := analyzeRequest(c.body)
	d := &RouteDecision{Router: rc.Name, Request: f}
	key := rc.Name + "\x00" + c.session
	if rc.Sticky && c.session != "" && !c.explain {
		if e, ok := p.sticky[key]; ok && now.Before(e.until) {
			e.until = now.Add(stickyTTL)
			d.Model, d.Rule, d.Sticky, d.Reason = e.model, e.rule, true, "Session "+c.session
			p.routerHit(rc.Name, e.rule)
			p.mu.Unlock()
			return d
		}
	}
	virtual := strings.EqualFold(rc.Name, c.model)
	for i := range rc.Rules {
		r := &rc.Rules[i]
		if why := p.ruleMatchLocked(r, f, c, now); why != "" {
			if c.explain {
				d.Checks = append(d.Checks, r.Name+": "+why)
			}
			continue
		}
		target := r.Model
		reason := "Regel " + r.Name
		if target == "" && r.Tier != "" {
			var used string
			if target, used = p.tierModelLocked(&rc, r.Tier); used != "" && used != r.Tier {
				reason += fmt.Sprintf(" → Stufe %s (leer) → %s", r.Tier, used)
			} else if target != "" {
				reason += " → Stufe " + r.Tier
			}
		}
		if target == "" && len(r.Needs) > 0 {
			prefer := ""
			if !virtual {
				prefer = c.model
			}
			target = p.pickCapableLocked(r.Needs, f.Tokens, prefer)
		}
		if target == "" {
			if c.explain {
				d.Checks = append(d.Checks, r.Name+": kein passendes Modell bekannt")
			}
			continue
		}
		d.Rule, d.Model, d.Reason = r.Name, target, reason
		break
	}
	if d.Model == "" && rc.Classifier != "" {
		p.mu.Unlock()
		cr := p.classify(&rc, c.body) // may call a model
		p.mu.Lock()
		d.Classifier = &cr
		switch {
		case cr.Route != "":
			for _, rt := range rc.Routes {
				if rt.Name == cr.Route {
					d.Model, d.Rule = p.pickPoolLocked(rt.Models), "route:"+rt.Name
				}
			}
			if d.Model == "" && cr.Route != "other" {
				cr.Detail += " (unbekannte Route)"
			}
		case cr.Tier != "":
			m, used := p.tierModelLocked(&rc, cr.Tier)
			if m != "" {
				d.Model, d.Rule = m, "tier:"+used
				if used != cr.Tier {
					cr.Detail += fmt.Sprintf(" → Stufe %s ohne Modell → %s", cr.Tier, used)
				}
			}
		}
		d.Reason = cr.Detail
		if d.Model == "" {
			d.Rule = ""
		}
	}
	if d.Model == "" {
		reason := ""
		if d.Reason != "" {
			reason = d.Reason + " → "
		}
		switch {
		case rc.Default != "":
			d.Model, d.Reason = rc.Default, reason+"Standard"
		case !virtual:
			d.Model, d.Reason = c.model, reason+"unverändert"
		default:
			d.Reason = reason + "keine Regel passt und kein Standardmodell"
			p.mu.Unlock()
			return d
		}
	}
	// the chosen model lacks something the request needs → a capable one
	if rc.autoCaps() {
		var needs []string
		if f.Images {
			needs = append(needs, "vision")
		}
		if f.Tools {
			needs = append(needs, "tools")
		}
		if p.modelLacksLocked(d.Model, []string{"context"}, f.Tokens) {
			needs = append(needs, "context")
		}
		if len(needs) > 0 && p.modelLacksLocked(d.Model, needs, f.Tokens) {
			if alt := p.pickCapableLocked(needs, f.Tokens, ""); alt != "" && !strings.EqualFold(alt, d.Model) {
				d.Reason += fmt.Sprintf(" → %s fehlt %s → %s", d.Model, needsText(needs), alt)
				d.Model = alt
			}
		}
	}
	if !c.explain {
		p.routerHit(rc.Name, d.Rule)
		if rc.Sticky && c.session != "" {
			p.sticky[key] = &stickyEntry{model: d.Model, rule: d.Rule, until: now.Add(stickyTTL)}
		}
	}
	p.mu.Unlock()
	return d
}

func needsText(n []string) string {
	var out []string
	for _, x := range n {
		out = append(out, capNames[x])
	}
	return strings.Join(out, "+")
}

func (p *Pool) routerHit(router, rule string) {
	if p.routerHits == nil {
		p.routerHits = map[string]*atomic.Int64{}
	}
	k := router + "\x00" + rule
	if p.routerHits[k] == nil {
		p.routerHits[k] = &atomic.Int64{}
	}
	p.routerHits[k].Add(1)
}

func (p *Pool) pruneStickyLocked(now time.Time) {
	for k, e := range p.sticky {
		if now.After(e.until) {
			delete(p.sticky, k)
		}
	}
}

// applyRouter is called by the proxy before the slot is requested.
func (p *Pool) applyRouter(req *ReqInfo, r *http.Request, body []byte) {
	p.mu.Lock()
	n := len(p.routers)
	p.mu.Unlock()
	if n == 0 {
		return
	}
	d := p.decide(routeCtx{model: req.Model, token: req.Token, client: req.Client, header: r.Header, session: req.Session, body: body})
	if d == nil || d.Model == "" {
		return
	}
	req.Router = d.Router + ": " + d.Reason
	if !strings.EqualFold(d.Model, req.Model) {
		req.Requested, req.Model = req.Model, d.Model
	}
}

// ---- persistence and API ----

type RouterStatus struct {
	RouterConfig
	Hits  map[string]int64 `json:"hits"` // rule name ("" = default) → requests since start
	Bound int              `json:"sessions"`
}

func (p *Pool) routerStatusLocked() []RouterStatus {
	out := []RouterStatus{}
	for _, rc := range p.routers {
		st := RouterStatus{RouterConfig: rc, Hits: map[string]int64{}}
		for k, v := range p.routerHits {
			if r, rule, _ := strings.Cut(k, "\x00"); r == rc.Name {
				st.Hits[rule] = v.Load()
			}
		}
		for k := range p.sticky {
			if strings.HasPrefix(k, rc.Name+"\x00") {
				st.Bound++
			}
		}
		out = append(out, st)
	}
	return out
}

func (p *Pool) SetRouters(list []RouterConfig) error {
	names := map[string]bool{}
	for i := range list {
		if err := normalizeRouter(&list[i]); err != nil {
			return httpErr{400, fmt.Errorf("Router %q: %v", list[i].Name, err)}
		}
		if names[strings.ToLower(list[i].Name)] {
			return httpErr{400, fmt.Errorf("Router %q doppelt", list[i].Name)}
		}
		names[strings.ToLower(list[i].Name)] = true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.routers = list
	for k := range p.sticky { // a changed configuration starts fresh
		delete(p.sticky, k)
	}
	log.Printf("routers changed via UI (%d)", len(list))
	return p.persistLocked()
}

func (p *Pool) Routers() []RouterConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]RouterConfig{}, p.routers...)
}

func registerRouters(mux *http.ServeMux, pre string, pool *Pool, token string) {
	readOne := func(r *http.Request) (RouterConfig, error) {
		var rc RouterConfig
		dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 256<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&rc); err != nil {
			return rc, httpErr{400, fmt.Errorf("ungültiges JSON: %v", err)}
		}
		return rc, nil
	}
	mux.HandleFunc("GET "+pre+"/api/routers", func(w http.ResponseWriter, r *http.Request) {
		pool.mu.Lock()
		st := pool.routerStatusLocked()
		pool.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc("POST "+pre+"/api/routers", adminAuth(token, func(w http.ResponseWriter, r *http.Request) {
		rc, err := readOne(r)
		if err != nil {
			writeResult(w, err)
			return
		}
		list := pool.Routers()
		for _, x := range list {
			if strings.EqualFold(x.Name, strings.TrimSpace(rc.Name)) {
				writeAPIError(w, http.StatusConflict, fmt.Errorf("Router %q gibt es schon", rc.Name))
				return
			}
		}
		writeResult(w, pool.SetRouters(append(list, rc)))
	}))
	mux.HandleFunc("PUT "+pre+"/api/routers/{name}", adminAuth(token, func(w http.ResponseWriter, r *http.Request) {
		rc, err := readOne(r)
		if err != nil {
			writeResult(w, err)
			return
		}
		list := pool.Routers()
		for i := range list {
			if list[i].Name == r.PathValue("name") {
				list[i] = rc
				writeResult(w, pool.SetRouters(list))
				return
			}
		}
		writeAPIError(w, http.StatusNotFound, errors.New("unbekannter Router"))
	}))
	mux.HandleFunc("PUT "+pre+"/api/routers", adminAuth(token, func(w http.ResponseWriter, r *http.Request) { // order / whole list
		var list []RouterConfig
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&list); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		writeResult(w, pool.SetRouters(list))
	}))
	mux.HandleFunc("DELETE "+pre+"/api/routers/{name}", adminAuth(token, func(w http.ResponseWriter, r *http.Request) {
		list := pool.Routers()
		for i := range list {
			if list[i].Name == r.PathValue("name") {
				writeResult(w, pool.SetRouters(append(list[:i:i], list[i+1:]...)))
				return
			}
		}
		writeAPIError(w, http.StatusNotFound, errors.New("unbekannter Router"))
	}))
	// Test: what would the routers do with this request? (no side effects)
	mux.HandleFunc("POST "+pre+"/api/routers/test", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Request json.RawMessage `json:"request"`
			Token   string          `json:"token"`
			Client  string          `json:"client"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 16<<20)).Decode(&in); err != nil {
			writeAPIError(w, http.StatusBadRequest, fmt.Errorf("ungültiges JSON: %v", err))
			return
		}
		var meta struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(in.Request, &meta); err != nil {
			writeAPIError(w, http.StatusBadRequest, errors.New("Anfrage ist kein JSON-Objekt"))
			return
		}
		d := pool.decide(routeCtx{model: meta.Model, token: in.Token, client: in.Client, header: http.Header{}, body: in.Request, explain: true})
		w.Header().Set("Content-Type", "application/json")
		if d == nil {
			json.NewEncoder(w).Encode(map[string]any{"router": "", "model": meta.Model, "reason": "kein Router zuständig – Modell bleibt", "request": analyzeRequest(in.Request)})
			return
		}
		json.NewEncoder(w).Encode(d)
	})
}

// ---- Ollama: detect capabilities via /api/show ----

func (px *Proxy) detectOllamaCaps(client *http.Client, ht healthTarget, names []string) {
	p := px.pool
	p.mu.Lock()
	var todo []string
	for _, n := range names {
		if _, ok := ht.b.detectedCaps[strings.ToLower(n)]; !ok {
			todo = append(todo, n)
		}
	}
	p.mu.Unlock()
	if len(todo) > 3 {
		todo = todo[:3] // a few per health check
	}
	for _, n := range todo {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		data, _ := json.Marshal(map[string]string{"model": n, "name": n})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ht.url+"/api/show", bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		if ht.key != "" {
			req.Header.Set("Authorization", "Bearer "+ht.key)
		}
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			return
		}
		var show struct {
			Capabilities []string       `json:"capabilities"`
			ModelInfo    map[string]any `json:"model_info"`
		}
		ok := resp.StatusCode == 200 && json.NewDecoder(resp.Body).Decode(&show) == nil
		resp.Body.Close()
		cancel()
		f := ModelFeature{Model: n}
		if ok {
			if show.Capabilities != nil {
				has := func(c string) *bool { v := containsFold(show.Capabilities, c); return &v }
				f.Vision, f.Tools, f.Reasoning = has("vision"), has("tools"), has("thinking")
			}
			for k, v := range show.ModelInfo {
				if strings.HasSuffix(k, ".context_length") {
					if n, ok := v.(float64); ok {
						f.Context = int(n)
					}
				}
			}
		}
		p.mu.Lock()
		if ht.b.detectedCaps == nil {
			ht.b.detectedCaps = map[string]ModelFeature{}
		}
		ht.b.detectedCaps[strings.ToLower(n)] = f
		p.mu.Unlock()
	}
}
