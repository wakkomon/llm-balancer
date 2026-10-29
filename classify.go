package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Classifiers decide when no explicit rule matched:
//   heuristic – LiteLLM-style complexity score without any model call → tier
//   llm       – a small model on one of the servers assigns a tier
//   arch      – Arch-Router (katanemo/Arch-Router-1.5B) picks one of the described routes
// Tiers (SIMPLE < MEDIUM < COMPLEX < REASONING) and routes map to model pools.

var tierOrder = []string{"SIMPLE", "MEDIUM", "COMPLEX", "REASONING"}

func validTier(t string) bool {
	for _, x := range tierOrder {
		if x == t {
			return true
		}
	}
	return false
}

type RouteDef struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Models      []string `json:"models"`
}

var defaultThresholds = []float64{0.15, 0.35, 0.60}

// ---- conversation extraction ----

var systemReminderRe = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)

type turn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// conversationTurns: user/assistant text turns (tool output and system prompts left out), cleaned.
func conversationTurns(body []byte) []turn {
	var doc struct {
		Messages []map[string]any `json:"messages"`
		Input    []map[string]any `json:"input"`
		Prompt   string           `json:"prompt"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return nil
	}
	msgs := doc.Messages
	if msgs == nil {
		msgs = doc.Input
	}
	var out []turn
	for _, m := range msgs {
		role, _ := m["role"].(string)
		if role != "user" && role != "assistant" {
			continue
		}
		text := strings.TrimSpace(systemReminderRe.ReplaceAllString(contentText(m["content"]), ""))
		if text == "" {
			continue // e.g. a turn that only carries tool results
		}
		out = append(out, turn{role, text})
	}
	if len(out) == 0 && doc.Prompt != "" {
		out = append(out, turn{"user", doc.Prompt})
	}
	return out
}

func lastUserText(turns []turn) string {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == "user" {
			return turns[i].Content
		}
	}
	return ""
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---- heuristic scorer ----

type heuristicResult struct {
	Tier   string             `json:"tier"`
	Score  float64            `json:"score"`
	Parts  map[string]float64 `json:"parts"`
	Forced string             `json:"forced,omitempty"`
}

var (
	codeWords      = []string{"```", "function", "funktion", "class ", "klasse", "api", "database", "datenbank", "sql", "def ", "import ", "regex", "script", "skript", "code", "programm", "bug", "stack trace", "exception", "compile", "kompil", "json", "yaml", "dockerfile", "refactor", "variable", "methode", "method"}
	reasoningWords = []string{"step by step", "schritt für schritt", "think through", "denk nach", "analyze", "analysiere", "analyse", "reason", "begründe", "prove", "beweise", "herleiten", "abwägen", "trade-off", "tradeoff", "vor- und nachteile", "pros and cons", "evaluate", "bewerte", "vergleiche", "compare", "warum genau", "explain why", "erkläre warum"}
	technicalWords = []string{"architecture", "architektur", "distributed", "verteilt", "encryption", "verschlüssel", "kubernetes", "k8s", "docker", "latency", "latenz", "algorithm", "algorithmus", "protocol", "protokoll", "concurrency", "nebenläufig", "scalab", "skalier", "performance", "optimi", "security", "sicherheit", "network", "netzwerk", "kernel", "compiler", "infrastructure", "infrastruktur", "microservice", "cluster", "proxmox", "datenmodell", "schema"}
	simpleWords    = []string{"what is", "was ist", "who is", "wer ist", "define", "definiere", "hello", "hallo", "hi ", "danke", "thanks", "thank you", "übersetze", "translate", "wie spät", "guten morgen", "ok", "ja", "nein"}
	multiStepRe    = regexp.MustCompile(`(?is)(first|zuerst|erstens).{1,200}(then|dann|danach|anschließend|zweitens)|(?m)^\s*(\d+[.)]|[-*])\s+\S.*\n\s*(\d+[.)]|[-*])\s+\S`)
)

func countAny(text string, words []string) int {
	n := 0
	for _, w := range words {
		if strings.Contains(text, w) {
			n++
		}
	}
	return n
}

func heuristicTier(text string, thresholds []float64) heuristicResult {
	t := " " + strings.ToLower(text) + " "
	tokens := float64(len(text)) / 4
	parts := map[string]float64{}
	switch {
	case tokens < 15:
		parts["tokenCount"] = -1
	case tokens > 400:
		parts["tokenCount"] = 1
	default:
		parts["tokenCount"] = (tokens - 15) / (400 - 15)
	}
	parts["codePresence"] = min(float64(countAny(t, codeWords))/2, 1)
	reasoning := countAny(t, reasoningWords)
	parts["reasoningMarkers"] = min(float64(reasoning)/2, 1)
	parts["technicalTerms"] = min(float64(countAny(t, technicalWords))/3, 1)
	if countAny(t, simpleWords) > 0 {
		parts["simpleIndicators"] = -1
	}
	if multiStepRe.MatchString(text) {
		parts["multiStepPatterns"] = 1
	}
	if q := strings.Count(text, "?"); q > 1 {
		parts["questionComplexity"] = min(float64(q-1)/3, 1)
	}
	weights := map[string]float64{"tokenCount": 0.10, "codePresence": 0.30, "reasoningMarkers": 0.25, "technicalTerms": 0.25,
		"simpleIndicators": 0.05, "multiStepPatterns": 0.03, "questionComplexity": 0.02}
	score := 0.0
	for k, v := range parts {
		score += weights[k] * v
	}
	if len(thresholds) != 3 {
		thresholds = defaultThresholds
	}
	r := heuristicResult{Score: score, Parts: parts}
	switch {
	case reasoning >= 2:
		r.Tier, r.Forced = "REASONING", "2+ Reasoning-Marker"
	case score < thresholds[0]:
		r.Tier = "SIMPLE"
	case score < thresholds[1]:
		r.Tier = "MEDIUM"
	case score < thresholds[2]:
		r.Tier = "COMPLEX"
	default:
		r.Tier = "REASONING"
	}
	return r
}

// ---- LLM classifiers ----

const llmTierPrompt = `You classify a user request for an AI assistant by how capable a model must be to answer it well.
Tiers:
SIMPLE    – greetings, small talk, short factual questions, simple translations or rewording
MEDIUM    – normal questions, explanations, short texts, everyday tasks
COMPLEX   – programming, technical analysis, long or structured documents, multi-step tasks, tool use
REASONING – hard problems needing careful step-by-step thinking: math, proofs, debugging difficult issues, planning, trade-off analysis
Judge the latest request; earlier turns are only context. Answer with JSON only: {"tier": "SIMPLE|MEDIUM|COMPLEX|REASONING"}`

const archTaskInstruction = `
You are a helpful assistant designed to find the best suited route.
You are provided with route description within <routes></routes> XML tags:
<routes>

%s

</routes>

<conversation>

%s

</conversation>
`

const archFormatPrompt = `
Your task is to decide which route is best suit with user intent on the conversation in <conversation></conversation> XML tags.  Follow the instruction:
1. If the latest intent from user is irrelevant or user intent is full filled, response with other route {"route": "other"}.
2. You must analyze the route descriptions and find the best match route for user latest intent.
3. You only response the name of the route that best matches the user's request, use the exact name in the <routes></routes>.

Based on your analysis, provide your response in the following JSON formats if you decide to match any route:
{"route": "route_name"}
`

func archPrompt(routes []RouteDef, turns []turn) string {
	type r struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	var rs []r
	for _, x := range routes {
		rs = append(rs, r{x.Name, x.Description})
	}
	rj, _ := json.Marshal(rs)
	cj, _ := json.Marshal(turns)
	return fmt.Sprintf(archTaskInstruction, rj, cj) + archFormatPrompt
}

func llmTierMessages(rc *RouterConfig, turns []turn) []turn {
	sys := llmTierPrompt
	if strings.TrimSpace(rc.ClassPrompt) != "" {
		sys = rc.ClassPrompt
	}
	cur := lastUserText(turns)
	var sb strings.Builder
	prior := 0
	n := rc.classTurns()
	for i := len(turns) - 2; i >= 0 && prior < n; i-- { // earlier user turns, newest first
		if turns[i].Role == "user" && turns[i].Content != cur {
			sb.WriteString(fmt.Sprintf("[earlier] %s\n", clip(turns[i].Content, 1500)))
			prior++
		}
	}
	user := ""
	if sb.Len() > 0 {
		user = "Earlier requests (context only):\n" + sb.String() + "\n"
	}
	user += "Latest request:\n" + clip(cur, 6000)
	return []turn{{"system", sys}, {"user", user}}
}

func (rc *RouterConfig) classTurns() int {
	if rc.ClassTurns == nil {
		return 3
	}
	return *rc.ClassTurns
}

// recentTurns: the last N user turns with the assistant answers in between (for Arch-Router).
func recentTurns(turns []turn, n int) []turn {
	if n <= 0 {
		n = 1
	}
	users, start := 0, 0
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == "user" {
			users++
			if users == n {
				start = i
				break
			}
		}
	}
	out := make([]turn, 0, len(turns)-start)
	for _, t := range turns[start:] {
		out = append(out, turn{t.Role, clip(t.Content, 2000)})
	}
	return out
}

var (
	tierOutRe  = regexp.MustCompile(`(?i)"tier"\s*:\s*"(SIMPLE|MEDIUM|COMPLEX|REASONING)"|\b(SIMPLE|MEDIUM|COMPLEX|REASONING)\b`)
	routeOutRe = regexp.MustCompile(`"route"\s*:\s*"([^"]+)"`)
)

// classifierTarget picks the server that runs the classifier model (outside the slot logic: the
// classification must not wait for the slot of the model that will answer).
func (p *Pool) classifierTargetLocked(rc *RouterConfig) (healthTarget, bool, error) {
	now := time.Now()
	var best *Backend
	for _, b := range p.backends {
		if rc.ClassServer != "" && b.Name != rc.ClassServer {
			continue
		}
		if !b.usable(now) {
			continue
		}
		if rc.ClassServer == "" && !b.serves(rc.ClassModel) {
			continue
		}
		if best == nil || (b.active < b.Max && best.active >= best.Max) || (b.active < b.Max) == (best.active < best.Max) && b.Priority < best.Priority {
			best = b
		}
	}
	if best == nil {
		return healthTarget{}, false, fmt.Errorf("kein erreichbarer Server mit %s", rc.ClassModel)
	}
	return healthTarget{best, best.URL, best.apiKey, best.insecure}, best.isOllama(), nil
}

// runClassifier sends the prompt to the classifier model and returns its text answer.
func (p *Pool) runClassifier(ctx context.Context, rc *RouterConfig, msgs []turn, jsonMode bool) (string, string, error) {
	p.mu.Lock()
	ht, ollama, err := p.classifierTargetLocked(rc)
	tf := p.transport
	p.mu.Unlock()
	if err != nil {
		return "", "", err
	}
	var path string
	var body map[string]any
	if ollama {
		path = "/api/chat"
		body = map[string]any{"model": rc.ClassModel, "messages": msgs, "stream": false, "keep_alive": "30m",
			"options": map[string]any{"temperature": 0, "num_predict": 64}}
		if jsonMode {
			body["format"] = "json"
		}
	} else {
		path = "/v1/chat/completions"
		body = map[string]any{"model": rc.ClassModel, "messages": msgs, "stream": false, "temperature": 0, "max_tokens": 64}
	}
	data, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ht.url+path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	if ht.key != "" {
		req.Header.Set("Authorization", "Bearer "+ht.key)
	}
	var tr http.RoundTripper = http.DefaultTransport
	if tf != nil {
		tr = tf(ht.insecure)
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return "", ht.b.Name, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", ht.b.Name, fmt.Errorf("%s antwortet %d: %s", ht.b.Name, resp.StatusCode, clip(strings.TrimSpace(string(raw)), 200))
	}
	var out struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", ht.b.Name, err
	}
	text := out.Message.Content
	if len(out.Choices) > 0 {
		text = out.Choices[0].Message.Content
	}
	return strings.TrimSpace(text), ht.b.Name, nil
}

type classResult struct {
	Tier    string `json:"tier,omitempty"`
	Route   string `json:"route,omitempty"`
	Detail  string `json:"detail"`
	Failed  bool   `json:"failed,omitempty"`
	Elapsed int64  `json:"ms"`
}

// classify runs the router's classifier (no lock held during model calls).
func (p *Pool) classify(rc *RouterConfig, body []byte) classResult {
	start := time.Now()
	turns := conversationTurns(body)
	text := lastUserText(turns)
	heur := func(prefix string) classResult {
		h := heuristicTier(text, rc.Thresholds)
		d := fmt.Sprintf("%sHeuristik: %s (Score %.2f%s)", prefix, h.Tier, h.Score, map[bool]string{true: ", " + h.Forced}[h.Forced != ""])
		return classResult{Tier: h.Tier, Detail: d}
	}
	finish := func(r classResult) classResult { r.Elapsed = time.Since(start).Milliseconds(); return r }
	switch rc.Classifier {
	case "heuristic":
		return finish(heur(""))
	case "llm", "arch":
		timeout := time.Duration(rc.ClassTimeoutMs) * time.Millisecond
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		var msgs []turn
		if rc.Classifier == "arch" {
			msgs = []turn{{"user", archPrompt(rc.Routes, recentTurns(turns, rc.classTurns()))}}
		} else {
			msgs = llmTierMessages(rc, turns)
		}
		out, server, err := p.runClassifier(ctx, rc, msgs, rc.Classifier == "llm")
		if err == nil {
			if rc.Classifier == "arch" {
				if m := routeOutRe.FindStringSubmatch(out); m != nil {
					return finish(classResult{Route: m[1], Detail: fmt.Sprintf("%s auf %s: %s", rc.ClassModel, server, m[1])})
				}
				err = fmt.Errorf("unerwartete Antwort %q", clip(out, 80))
			} else if m := tierOutRe.FindStringSubmatch(out); m != nil {
				return finish(classResult{Tier: strings.ToUpper(m[1] + m[2]), Detail: fmt.Sprintf("%s auf %s: %s", rc.ClassModel, server, strings.ToUpper(m[1]+m[2]))})
			} else {
				err = fmt.Errorf("unerwartete Antwort %q", clip(out, 80))
			}
		}
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			err = fmt.Errorf("Zeitüberschreitung nach %v", timeout)
		}
		msg := fmt.Sprintf("%s fehlgeschlagen (%v)", rc.ClassModel, err)
		if rc.ClassFallback == "heuristic" && len(rc.Tiers) > 0 {
			r := heur(msg + " → ")
			r.Failed = true
			return finish(r)
		}
		return finish(classResult{Detail: msg, Failed: true})
	}
	return classResult{}
}

// tierModelLocked: models of a tier; an empty tier escalates to the next higher one with models.
func (p *Pool) tierModelLocked(rc *RouterConfig, tier string) (string, string) {
	start := 0
	for i, t := range tierOrder {
		if t == tier {
			start = i
		}
	}
	for i := start; i < len(tierOrder); i++ {
		if ms := rc.Tiers[tierOrder[i]]; len(ms) > 0 {
			return strings.Join(ms, ", "), tierOrder[i]
		}
	}
	for i := start - 1; i >= 0; i-- { // nothing higher: the next lower one
		if ms := rc.Tiers[tierOrder[i]]; len(ms) > 0 {
			return strings.Join(ms, ", "), tierOrder[i]
		}
	}
	return "", ""
}

func normalizeClassifier(rc *RouterConfig) error {
	rc.ClassModel = strings.TrimSpace(rc.ClassModel)
	clean := func(ms []string) []string {
		var out []string
		for _, m := range ms {
			if m = strings.TrimSpace(m); m != "" {
				out = append(out, m)
			}
		}
		return out
	}
	tiers := map[string][]string{}
	for k, v := range rc.Tiers {
		k = strings.ToUpper(strings.TrimSpace(k))
		if !validTier(k) {
			return fmt.Errorf("unbekannte Stufe %q", k)
		}
		if v = clean(v); len(v) > 0 {
			tiers[k] = v
		}
	}
	rc.Tiers = tiers
	if len(rc.Tiers) == 0 {
		rc.Tiers = nil
	}
	seen := map[string]bool{}
	for i := range rc.Routes {
		r := &rc.Routes[i]
		r.Name, r.Description, r.Models = strings.TrimSpace(r.Name), strings.TrimSpace(r.Description), clean(r.Models)
		if r.Name == "" || strings.ContainsAny(r.Name, `"`) || r.Name == "other" {
			return fmt.Errorf("Route %d: Name fehlt oder ist ungültig („other“ ist reserviert)", i+1)
		}
		if seen[r.Name] {
			return fmt.Errorf("Route %q doppelt", r.Name)
		}
		seen[r.Name] = true
		if r.Description == "" || len(r.Models) == 0 {
			return fmt.Errorf("Route %q: Beschreibung und Modell nötig", r.Name)
		}
	}
	if len(rc.Thresholds) != 0 {
		t := rc.Thresholds
		if len(t) != 3 || !(t[0] < t[1] && t[1] < t[2]) {
			return errors.New("Schwellen: drei aufsteigende Werte, z. B. 0.15 0.35 0.60")
		}
	}
	switch rc.ClassFallback {
	case "", "default", "heuristic":
	default:
		return fmt.Errorf("unbekanntes Verhalten bei Fehler %q", rc.ClassFallback)
	}
	if rc.ClassTimeoutMs < 0 || rc.ClassTimeoutMs > 60000 {
		return errors.New("Timeout zwischen 0 und 60000 ms")
	}
	switch rc.Classifier {
	case "", "none":
		rc.Classifier = ""
	case "heuristic":
		if len(rc.Tiers) == 0 {
			return errors.New("Heuristik: mindestens einer Stufe Modelle zuordnen")
		}
	case "llm":
		if rc.ClassModel == "" || len(rc.Tiers) == 0 {
			return errors.New("LLM-Klassifizierer: Modell und Stufen nötig")
		}
	case "arch":
		if rc.ClassModel == "" || len(rc.Routes) == 0 {
			return errors.New("Arch-Router: Modell (z. B. arch-router:1.5b) und Routen nötig")
		}
	default:
		return fmt.Errorf("unbekannter Klassifizierer %q", rc.Classifier)
	}
	return nil
}
