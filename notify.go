package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---- events ----

const (
	EvServerDown     = "server_down"
	EvServerUp       = "server_up"
	EvServerError    = "server_error"
	EvServerCapped   = "server_capped"
	EvServerUncapped = "server_uncapped"
	EvTokenCapped    = "token_capped"
	EvTokenUncapped  = "token_uncapped"
	EvTest           = "test"
)

var eventTitles = map[string]string{
	EvServerDown:     "Server nicht erreichbar",
	EvServerUp:       "Server wieder erreichbar",
	EvServerError:    "Server liefert Fehler",
	EvServerCapped:   "Kostendeckel Server erreicht",
	EvServerUncapped: "Server wieder freigegeben",
	EvTokenCapped:    "Kostendeckel API-Token erreicht",
	EvTokenUncapped:  "API-Token wieder freigegeben",
	EvTest:           "Testnachricht",
}

type Event struct {
	ID     int64     `json:"id"`
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"`
	Title  string    `json:"title"`
	Object string    `json:"object"` // server or token name
	Text   string    `json:"text"`
	Mail   string    `json:"mail"` // "" (not sent: notifications off for this kind), "gesendet", "Fehler: …", "wird gesendet"
}

// ---- configuration (DATA_DIR/notify.json) ----

type NotifyConfig struct {
	Enabled       bool            `json:"enabled"`
	Host          string          `json:"smtp_host"`
	Port          int             `json:"smtp_port"`
	Security      string          `json:"smtp_security"` // starttls | tls | none
	SkipVerify    bool            `json:"smtp_skip_verify"`
	Username      string          `json:"smtp_username"`
	Password      string          `json:"smtp_password,omitempty"`
	From          string          `json:"from"`
	To            []string        `json:"to"`
	Events        map[string]bool `json:"events"`         // kind → send mail
	DownDelaySec  int             `json:"down_delay_sec"` // a server must be down/faulty this long before a mail goes out
	PublicURL     string          `json:"public_url"`     // link to the web UI in mails
	SubjectPrefix string          `json:"subject_prefix"`
}

func defaultNotifyConfig() NotifyConfig {
	return NotifyConfig{Port: 587, Security: "starttls", DownDelaySec: 120, SubjectPrefix: "[LLM-Balancer]",
		Events: map[string]bool{EvServerDown: true, EvServerUp: true, EvServerError: false,
			EvServerCapped: true, EvServerUncapped: false, EvTokenCapped: true, EvTokenUncapped: false}}
}

func (c *NotifyConfig) validate() error {
	c.Host = strings.TrimSpace(c.Host)
	c.From = strings.TrimSpace(c.From)
	var to []string
	for _, a := range c.To {
		for _, x := range strings.FieldsFunc(a, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\n' }) {
			to = append(to, x)
		}
	}
	c.To = to
	switch c.Security {
	case "starttls", "tls", "none":
	case "":
		c.Security = "starttls"
	default:
		return errors.New("Verschlüsselung: starttls, tls oder none")
	}
	if c.Port == 0 {
		c.Port = map[string]int{"starttls": 587, "tls": 465, "none": 25}[c.Security]
	}
	if c.DownDelaySec < 0 {
		c.DownDelaySec = 0
	}
	if c.Enabled {
		if c.Host == "" || c.From == "" || len(c.To) == 0 {
			return errors.New("für E-Mails werden SMTP-Server, Absender und mindestens ein Empfänger gebraucht")
		}
		for _, a := range append([]string{c.From}, c.To...) {
			if !strings.Contains(a, "@") {
				return fmt.Errorf("ungültige E-Mail-Adresse %q", a)
			}
		}
	}
	if c.Events == nil {
		c.Events = map[string]bool{}
	}
	if strings.TrimSpace(c.SubjectPrefix) == "" {
		c.SubjectPrefix = "[LLM-Balancer]"
	}
	return nil
}

// ---- notifier ----

type Notifier struct {
	mu       sync.Mutex
	cfg      NotifyConfig
	events   []Event
	nextID   int64
	cfgPath  string
	evPath   string
	dirty    bool
	queue    chan int64 // event ids to mail
	send     func(cfg NotifyConfig, subject, body string) error
	downAt   map[string]time.Time // server → since when down/faulty (not yet reported)
	reported map[string]string    // server → reported state ("down" / "error")
}

const maxEvents = 300

func NewNotifier(dir string) *Notifier {
	n := &Notifier{cfg: defaultNotifyConfig(), queue: make(chan int64, 100), send: sendMail,
		downAt: map[string]time.Time{}, reported: map[string]string{}}
	if dir == "" {
		return n
	}
	n.cfgPath, n.evPath = filepath.Join(dir, "notify.json"), filepath.Join(dir, "events.json")
	if data, err := os.ReadFile(n.cfgPath); err == nil {
		c := defaultNotifyConfig()
		if json.Unmarshal(data, &c) == nil {
			n.cfg = c
		}
	}
	if data, err := os.ReadFile(n.evPath); err == nil {
		json.Unmarshal(data, &n.events)
		for _, e := range n.events {
			n.nextID = max(n.nextID, e.ID)
		}
	}
	return n
}

// Emit records an event and queues a mail if notifications are on for its kind.
func (n *Notifier) Emit(e Event) {
	n.mu.Lock()
	n.nextID++
	e.ID, e.Time = n.nextID, time.Now()
	if e.Title == "" {
		e.Title = eventTitles[e.Kind]
	}
	mail := n.cfg.Enabled && (n.cfg.Events[e.Kind] || e.Kind == EvTest)
	if mail {
		e.Mail = "wird gesendet"
	}
	n.events = append(n.events, e)
	if len(n.events) > maxEvents {
		n.events = n.events[len(n.events)-maxEvents:]
	}
	n.dirty = true
	n.mu.Unlock()
	log.Printf("event %s %s: %s", e.Kind, e.Object, e.Text)
	if mail {
		select {
		case n.queue <- e.ID:
		default:
			n.setMail(e.ID, "Fehler: zu viele Nachrichten in der Warteschlange")
		}
	}
}

func (n *Notifier) setMail(id int64, status string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for i := range n.events {
		if n.events[i].ID == id {
			n.events[i].Mail = status
			n.dirty = true
		}
	}
}

// mailLoop sends queued mails one after another.
func (n *Notifier) mailLoop(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case id := <-n.queue:
			n.mu.Lock()
			cfg := n.cfg
			var ev *Event
			for i := range n.events {
				if n.events[i].ID == id {
					e := n.events[i]
					ev = &e
				}
			}
			n.mu.Unlock()
			if ev == nil {
				continue
			}
			subject, body := mailText(cfg, *ev)
			if err := n.send(cfg, subject, body); err != nil {
				log.Printf("mail for event %d failed: %v", id, err)
				n.setMail(id, "Fehler: "+err.Error())
			} else {
				n.setMail(id, "gesendet")
			}
		}
	}
}

func mailText(cfg NotifyConfig, e Event) (string, string) {
	subject := strings.TrimSpace(cfg.SubjectPrefix + " " + e.Title)
	if e.Object != "" {
		subject += ": " + e.Object
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n%s\n\nZeit: %s\n", e.Title, e.Text, e.Time.In(time.Local).Format("02.01.2006 15:04:05"))
	if cfg.PublicURL != "" {
		fmt.Fprintf(&b, "Web-UI: %s\n", strings.TrimRight(cfg.PublicURL, "/"))
	}
	fmt.Fprintf(&b, "\n-- \nllm-balancer v%s\n", versionString())
	return subject, b.String()
}

// watch turns server states into events: down (health check / connection failed) or faulty
// (requests fail) for longer than the configured delay, and recovered again.
func (n *Notifier) watch(p *Pool) {
	n.mu.Lock()
	delay := time.Duration(n.cfg.DownDelaySec) * time.Second
	n.mu.Unlock()
	now := time.Now()
	p.mu.Lock()
	type sv struct{ name, state, msg string }
	var list []sv
	for _, b := range p.backends {
		st := backendState(b, now)
		msg := b.healthMsg
		if st == "error" {
			msg = b.lastErr
		}
		list = append(list, sv{b.Name, st, msg})
	}
	p.mu.Unlock()

	var emit []Event
	n.mu.Lock()
	seen := map[string]bool{}
	for _, s := range list {
		seen[s.name] = true
		bad := s.state == "down" || s.state == "error"
		rep := n.reported[s.name]
		if bad {
			since, ok := n.downAt[s.name]
			if !ok {
				n.downAt[s.name] = now
				since = now
			}
			if rep == "" && now.Sub(since) >= delay {
				n.reported[s.name] = s.state
				kind, what := EvServerDown, "ist nicht erreichbar"
				if s.state == "error" {
					kind, what = EvServerError, "liefert Fehler"
				}
				emit = append(emit, Event{Kind: kind, Object: s.name,
					Text: fmt.Sprintf("Server %s %s (seit %s).\n\nMeldung: %s", s.name, what, since.In(time.Local).Format("15:04:05"), s.msg)})
			}
			continue
		}
		delete(n.downAt, s.name)
		if rep != "" {
			delete(n.reported, s.name)
			emit = append(emit, Event{Kind: EvServerUp, Object: s.name,
				Text: fmt.Sprintf("Server %s arbeitet wieder normal (Zustand: %s).", s.name, s.state)})
		}
	}
	for name := range n.downAt { // removed servers
		if !seen[name] {
			delete(n.downAt, name)
			delete(n.reported, name)
		}
	}
	n.mu.Unlock()
	for _, e := range emit {
		n.Emit(e)
	}
}

func (n *Notifier) loop(p *Pool, stop <-chan struct{}) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			n.flush()
			return
		case <-t.C:
			n.watch(p)
			n.flush()
		}
	}
}

func (n *Notifier) flush() {
	n.mu.Lock()
	if !n.dirty || n.evPath == "" {
		n.mu.Unlock()
		return
	}
	data, _ := json.Marshal(n.events)
	n.dirty = false
	n.mu.Unlock()
	if err := writeFileAtomic(n.evPath, data, 0o644); err != nil {
		log.Printf("events: %v", err)
	}
}

func (n *Notifier) SetConfig(c NotifyConfig, keepPassword bool) error {
	if err := c.validate(); err != nil {
		return httpErr{400, err}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if keepPassword {
		c.Password = n.cfg.Password
	}
	n.cfg = c
	if n.cfgPath != "" {
		data, _ := json.MarshalIndent(c, "", "  ")
		if err := writeFileAtomic(n.cfgPath, data, 0o600); err != nil { // contains the SMTP password
			return fmt.Errorf("Einstellungen aktiv, aber nicht gespeichert: %v", err)
		}
	}
	log.Printf("notification settings changed via UI (enabled=%v, to=%v)", c.Enabled, c.To)
	return nil
}

// ---- SMTP ----

// plainAuth is smtp.PlainAuth without Go's refusal to send credentials over an unencrypted connection
// (only used when the admin explicitly chose "none").
type plainAuth struct{ user, pass string }

func (a plainAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.user + "\x00" + a.pass), nil
}
func (a plainAuth) Next([]byte, bool) ([]byte, error) { return nil, nil }

func sendMail(cfg NotifyConfig, subject, body string) error {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	tlsCfg := &tls.Config{ServerName: cfg.Host, InsecureSkipVerify: cfg.SkipVerify}
	var conn net.Conn
	var err error
	d := &net.Dialer{Timeout: 15 * time.Second}
	if cfg.Security == "tls" {
		conn, err = tls.DialWithDialer(d, "tcp", addr, tlsCfg)
	} else {
		conn, err = d.Dial("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("Verbindung zu %s: %w", addr, err)
	}
	conn.SetDeadline(time.Now().Add(60 * time.Second))
	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if cfg.Security == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("Server bietet kein STARTTLS an – Verschlüsselung auf „tls“ oder „none“ stellen")
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("STARTTLS: %w", err)
		}
	}
	if cfg.Username != "" {
		var auth smtp.Auth = plainAuth{cfg.Username, cfg.Password}
		if cfg.Security != "none" {
			auth = smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
		}
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("Anmeldung: %w", err)
		}
	}
	if err := c.Mail(cfg.From); err != nil {
		return fmt.Errorf("Absender: %w", err)
	}
	for _, to := range cfg.To {
		if err := c.Rcpt(to); err != nil {
			return fmt.Errorf("Empfänger %s: %w", to, err)
		}
	}
	wc, err := c.Data()
	if err != nil {
		return err
	}
	msg := "From: " + cfg.From + "\r\n" +
		"To: " + strings.Join(cfg.To, ", ") + "\r\n" +
		"Subject: " + mime.QEncoding.Encode("utf-8", subject) + "\r\n" +
		"Date: " + time.Now().Format(time.RFC1123Z) + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: 8bit\r\n\r\n" +
		strings.ReplaceAll(body, "\n", "\r\n")
	if _, err := wc.Write([]byte(msg)); err != nil {
		return err
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("Versand: %w", err)
	}
	return c.Quit()
}

// ---- HTTP API ----

type notifyView struct {
	NotifyConfig
	HasPassword bool              `json:"has_password"`
	Titles      map[string]string `json:"titles"`
}

func registerNotify(mux *http.ServeMux, pre string, n *Notifier, token string) {
	mux.HandleFunc("GET "+pre+"/api/notify", adminAuth(token, func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		v := notifyView{NotifyConfig: n.cfg, HasPassword: n.cfg.Password != "", Titles: eventTitles}
		n.mu.Unlock()
		v.Password = ""
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}))
	mux.HandleFunc("PUT "+pre+"/api/notify", adminAuth(token, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			NotifyConfig
			Password *string `json:"smtp_password"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
			writeAPIError(w, 400, fmt.Errorf("ungültiges JSON: %v", err))
			return
		}
		c := in.NotifyConfig
		keep := in.Password == nil // field left out: keep the stored password
		if !keep {
			c.Password = *in.Password
		}
		writeResult(w, n.SetConfig(c, keep))
	}))
	mux.HandleFunc("POST "+pre+"/api/notify/test", adminAuth(token, func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		cfg := n.cfg
		n.mu.Unlock()
		if cfg.Host == "" || cfg.From == "" || len(cfg.To) == 0 {
			writeAPIError(w, 400, errors.New("erst SMTP-Server, Absender und Empfänger speichern"))
			return
		}
		e := Event{Kind: EvTest, Title: eventTitles[EvTest], Text: "Diese Testnachricht zeigt, dass der Versand funktioniert.", Time: time.Now()}
		subject, body := mailText(cfg, e)
		if err := n.send(cfg, subject, body); err != nil {
			writeAPIError(w, 502, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET "+pre+"/api/events", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		out := make([]Event, len(n.events))
		for i, e := range n.events { // newest first
			out[len(n.events)-1-i] = e
		}
		n.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("POST "+pre+"/api/events/clear", adminAuth(token, func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		n.events, n.dirty = nil, true
		n.mu.Unlock()
		n.flush()
		w.WriteHeader(http.StatusNoContent)
	}))
}
