package main

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// User management for the web UI: users with a role log in with name + password and get a session
// cookie. As long as no user exists, the UI works as before (open, changes optionally guarded by
// ADMIN_TOKEN). The LLM API (/v1/...) is not affected – it uses the API tokens.

const (
	RoleViewer   = "viewer"   // sees everything, changes nothing
	RoleOperator = "operator" // + switch servers/groups on/off, stop requests
	RoleAdmin    = "admin"    // everything, incl. users and configuration
)

var roleRank = map[string]int{RoleViewer: 1, RoleOperator: 2, RoleAdmin: 3}

const (
	sessionCookie  = "llmb_session"
	sessionTTL     = 30 * 24 * time.Hour
	pbkdf2Iter     = 210000
	minPasswordLen = 8
)

type User struct {
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	Hash      string    `json:"hash,omitempty"` // pbkdf2-sha256$iter$salt$key
	Disabled  bool      `json:"disabled,omitempty"`
	Created   time.Time `json:"created"`
	LastLogin time.Time `json:"last_login,omitempty"`
}

type session struct {
	User    string    `json:"user"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
}

type loginFails struct {
	n     int
	first time.Time
	until time.Time
}

type Users struct {
	mu       sync.Mutex
	users    []*User
	sessions map[string]*session // sha256(cookie value) → session
	fails    map[string]*loginFails
	path     string
	spath    string
}

func NewUsers(dir string) *Users {
	u := &Users{sessions: map[string]*session{}, fails: map[string]*loginFails{}}
	if dir == "" {
		return u
	}
	u.path = filepath.Join(dir, "users.json")
	u.spath = filepath.Join(dir, "sessions.json")
	if data, err := os.ReadFile(u.path); err == nil {
		if err := json.Unmarshal(data, &u.users); err != nil {
			log.Printf("users: %s unreadable: %v", u.path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Printf("users: %v", err)
	}
	if data, err := os.ReadFile(u.spath); err == nil {
		_ = json.Unmarshal(data, &u.sessions)
		if u.sessions == nil {
			u.sessions = map[string]*session{}
		}
	}
	return u
}

// Active: login is required once at least one user exists.
func (u *Users) Active() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.users) > 0
}

// Bootstrap creates the first admin from ADMIN_USER / ADMIN_PASSWORD if there is no user yet.
func (u *Users) Bootstrap(name, password string) error {
	if name == "" || password == "" || u.Active() {
		return nil
	}
	if err := u.Add(name, password, RoleAdmin); err != nil {
		return err
	}
	log.Printf("users: created admin %q from ADMIN_USER/ADMIN_PASSWORD", name)
	return nil
}

func hashPassword(pw string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	key, _ := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iter, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

var dummyHash = hashPassword("dummy-password-for-timing")

func checkPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1000 || iter > 10_000_000 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

var userNameRe = regexp.MustCompile(`^[\p{L}\p{N}._@-]{1,64}$`)

func validRole(r string) bool { _, ok := roleRank[r]; return ok }

func (u *Users) findLocked(name string) *User {
	for _, x := range u.users {
		if strings.EqualFold(x.Name, name) {
			return x
		}
	}
	return nil
}

func (u *Users) adminsLocked(except *User) int {
	n := 0
	for _, x := range u.users {
		if x != except && x.Role == RoleAdmin && !x.Disabled {
			n++
		}
	}
	return n
}

func (u *Users) saveLocked() error {
	if u.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(u.path), 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(u.users, "", "  ")
	if err := writeFileAtomic(u.path, data, 0o600); err != nil {
		return fmt.Errorf("Benutzer nicht gespeichert: %v", err)
	}
	return nil
}

func (u *Users) saveSessionsLocked() {
	if u.spath == "" {
		return
	}
	now := time.Now()
	for k, s := range u.sessions {
		if now.After(s.Expires) {
			delete(u.sessions, k)
		}
	}
	data, _ := json.Marshal(u.sessions)
	if err := writeFileAtomic(u.spath, data, 0o600); err != nil {
		log.Printf("sessions: %v", err)
	}
}

func (u *Users) Add(name, password, role string) error {
	name = strings.TrimSpace(name)
	if !userNameRe.MatchString(name) {
		return httpErr{400, errors.New("Benutzername fehlt oder enthält ungültige Zeichen (erlaubt: Buchstaben, Ziffern, . _ @ -)")}
	}
	if !validRole(role) {
		return httpErr{400, fmt.Errorf("unbekannte Rolle %q", role)}
	}
	if len(password) < minPasswordLen {
		return httpErr{400, fmt.Errorf("Passwort zu kurz (mindestens %d Zeichen)", minPasswordLen)}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.findLocked(name) != nil {
		return httpErr{409, fmt.Errorf("Benutzer %q gibt es schon", name)}
	}
	u.users = append(u.users, &User{Name: name, Role: role, Hash: hashPassword(password), Created: time.Now()})
	return u.saveLocked()
}

type userUpdate struct {
	Name     *string `json:"name"`
	Role     *string `json:"role"`
	Password *string `json:"password"`
	Disabled *bool   `json:"disabled"`
}

func (u *Users) Update(name string, in userUpdate) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.findLocked(name)
	if x == nil {
		return httpErr{404, errors.New("unbekannter Benutzer")}
	}
	if in.Role != nil && !validRole(*in.Role) {
		return httpErr{400, fmt.Errorf("unbekannte Rolle %q", *in.Role)}
	}
	if in.Password != nil && len(*in.Password) < minPasswordLen {
		return httpErr{400, fmt.Errorf("Passwort zu kurz (mindestens %d Zeichen)", minPasswordLen)}
	}
	newName := x.Name
	if in.Name != nil {
		newName = strings.TrimSpace(*in.Name)
		if !userNameRe.MatchString(newName) {
			return httpErr{400, errors.New("ungültiger Benutzername")}
		}
		if o := u.findLocked(newName); o != nil && o != x {
			return httpErr{409, fmt.Errorf("Benutzer %q gibt es schon", newName)}
		}
	}
	losesAdmin := (in.Role != nil && *in.Role != RoleAdmin) || (in.Disabled != nil && *in.Disabled)
	if x.Role == RoleAdmin && !x.Disabled && losesAdmin && u.adminsLocked(x) == 0 {
		return httpErr{400, errors.New("der letzte aktive Administrator kann nicht herabgestuft oder gesperrt werden")}
	}
	old := x.Name
	x.Name = newName
	if in.Role != nil {
		x.Role = *in.Role
	}
	if in.Disabled != nil {
		x.Disabled = *in.Disabled
	}
	if in.Password != nil {
		x.Hash = hashPassword(*in.Password)
	}
	for k, s := range u.sessions {
		if s.User == old {
			if x.Disabled || in.Password != nil {
				delete(u.sessions, k) // log out everywhere
			} else {
				s.User = x.Name
			}
		}
	}
	u.saveSessionsLocked()
	return u.saveLocked()
}

func (u *Users) Delete(name string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i, x := range u.users {
		if strings.EqualFold(x.Name, name) {
			if x.Role == RoleAdmin && !x.Disabled && u.adminsLocked(x) == 0 {
				return httpErr{400, errors.New("der letzte aktive Administrator kann nicht gelöscht werden")}
			}
			u.users = append(u.users[:i:i], u.users[i+1:]...)
			for k, s := range u.sessions {
				if s.User == x.Name {
					delete(u.sessions, k)
				}
			}
			u.saveSessionsLocked()
			return u.saveLocked()
		}
	}
	return httpErr{404, errors.New("unbekannter Benutzer")}
}

// List returns the users without password hashes.
func (u *Users) List() []User {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]User, 0, len(u.users))
	for _, x := range u.users {
		c := *x
		c.Hash = ""
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// ---- login / sessions ----

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

var errLoginBlocked = errors.New("zu viele Fehlversuche – bitte in ein paar Minuten erneut versuchen")

// Login checks name + password and returns a new session token.
func (u *Users) Login(name, password, ip string) (string, *User, error) {
	u.mu.Lock()
	f := u.fails[ip]
	now := time.Now()
	if f != nil && now.Before(f.until) {
		u.mu.Unlock()
		return "", nil, httpErr{429, errLoginBlocked}
	}
	x := u.findLocked(strings.TrimSpace(name))
	hash := dummyHash
	if x != nil {
		hash = x.Hash
	}
	u.mu.Unlock()
	ok := checkPassword(hash, password) && x != nil // always hash, so unknown names take as long
	u.mu.Lock()
	defer u.mu.Unlock()
	if !ok || x.Disabled {
		if f == nil || now.Sub(f.first) > 10*time.Minute {
			f = &loginFails{first: now}
			u.fails[ip] = f
		}
		f.n++
		if f.n >= 5 {
			f.until = now.Add(5 * time.Minute)
			f.n = 0
			f.first = now
			log.Printf("users: login blocked for %s after repeated failures", ip)
		}
		if ok && x.Disabled {
			return "", nil, httpErr{403, errors.New("Benutzer ist gesperrt")}
		}
		return "", nil, httpErr{401, errors.New("Benutzername oder Passwort falsch")}
	}
	delete(u.fails, ip)
	tok := newSessionToken()
	u.sessions[sessionKey(tok)] = &session{User: x.Name, Created: now, Expires: now.Add(sessionTTL)}
	x.LastLogin = now
	u.saveSessionsLocked()
	_ = u.saveLocked()
	c := *x
	c.Hash = ""
	return tok, &c, nil
}

func newSessionToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func sessionKey(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// FromSession resolves a session cookie to the user (nil if invalid).
func (u *Users) FromSession(tok string) *User {
	if tok == "" {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	s := u.sessions[sessionKey(tok)]
	if s == nil || time.Now().After(s.Expires) {
		return nil
	}
	x := u.findLocked(s.User)
	if x == nil || x.Disabled {
		return nil
	}
	c := *x
	c.Hash = ""
	return &c
}

func (u *Users) Logout(tok string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.sessions, sessionKey(tok))
	u.saveSessionsLocked()
}

// ChangeOwnPassword checks the old password; other sessions of the user are ended.
func (u *Users) ChangeOwnPassword(name, oldPw, newPw, keepTok string) error {
	if len(newPw) < minPasswordLen {
		return httpErr{400, fmt.Errorf("Passwort zu kurz (mindestens %d Zeichen)", minPasswordLen)}
	}
	u.mu.Lock()
	x := u.findLocked(name)
	if x == nil {
		u.mu.Unlock()
		return httpErr{404, errors.New("unbekannter Benutzer")}
	}
	hash := x.Hash
	u.mu.Unlock()
	if !checkPassword(hash, oldPw) {
		return httpErr{400, errors.New("bisheriges Passwort ist falsch")}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	x.Hash = hashPassword(newPw)
	keep := sessionKey(keepTok)
	for k, s := range u.sessions {
		if s.User == x.Name && k != keep {
			delete(u.sessions, k)
		}
	}
	u.saveSessionsLocked()
	return u.saveLocked()
}

// ---- middleware ----

type authKey struct{}

// Identity of a UI/API request: a user, or "ADMIN_TOKEN" for scripts using the admin token.
type Identity struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

func identityFrom(r *http.Request) *Identity {
	id, _ := r.Context().Value(authKey{}).(*Identity)
	return id
}

var operatorPaths = []*regexp.Regexp{
	regexp.MustCompile(`^/api/backends/[^/]+/(enable|disable|stop)$`),
	regexp.MustCompile(`^/api/requests/[0-9]+/stop$`),
	regexp.MustCompile(`^/api/groups/[^/]+/(enable|disable)$`),
	regexp.MustCompile(`^/api/ollama/unload$`),
}

// requiredRole: what a request needs once users exist (path relative to the UI prefix).
func requiredRole(method, rel string) string {
	if strings.HasPrefix(rel, "/api/auth/") {
		return RoleViewer
	}
	if method == http.MethodGet || method == http.MethodHead {
		if rel == "/api/users" || strings.HasPrefix(rel, "/api/config/") {
			return RoleAdmin
		}
		return RoleViewer
	}
	if method == http.MethodPost {
		for _, re := range operatorPaths {
			if re.MatchString(rel) {
				return RoleOperator
			}
		}
	}
	return RoleAdmin
}

// publicPaths work without login: the page itself (it shows the login form), login, health, metrics.
func publicPath(rel string) bool {
	switch rel {
	case "", "/", "/healthz", "/metrics", "/api/auth/me", "/api/auth/login", "/api/auth/logout", "/api/auth/setup":
		return true
	}
	return false
}

func (u *Users) identify(r *http.Request, adminToken string) *Identity {
	if adminToken != "" {
		if t := r.Header.Get("X-Admin-Token"); t != "" && subtle.ConstantTimeCompare([]byte(t), []byte(adminToken)) == 1 {
			return &Identity{Name: "ADMIN_TOKEN", Role: RoleAdmin}
		}
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		if x := u.FromSession(c.Value); x != nil {
			return &Identity{Name: x.Name, Role: x.Role}
		}
	}
	return nil
}

// sameOrigin rejects cross-site form posts (in addition to the SameSite=Strict cookie).
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" || o == "null" {
		return o == ""
	}
	ou, err := url.Parse(o)
	return err == nil && strings.EqualFold(ou.Host, r.Host)
}

func (u *Users) middleware(pre, adminToken string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !u.Active() { // no users yet: open as before (ADMIN_TOKEN still guards changes)
			next.ServeHTTP(w, r)
			return
		}
		rel := strings.TrimPrefix(r.URL.Path, pre)
		id := u.identify(r, adminToken)
		if id != nil && r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
			writeAPIError(w, http.StatusForbidden, errors.New("Anfrage von fremder Seite abgelehnt"))
			return
		}
		if publicPath(rel) {
			if id != nil {
				r = r.WithContext(context.WithValue(r.Context(), authKey{}, id))
			}
			next.ServeHTTP(w, r)
			return
		}
		if id == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]any{"error": "Anmeldung erforderlich", "login": true})
			return
		}
		if need := requiredRole(r.Method, rel); roleRank[id.Role] < roleRank[need] {
			writeAPIError(w, http.StatusForbidden, fmt.Errorf("keine Berechtigung (Rolle %s, nötig: %s)", roleLabel[id.Role], roleLabel[need]))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authKey{}, id)))
	})
}

var roleLabel = map[string]string{RoleViewer: "Betrachter", RoleOperator: "Operator", RoleAdmin: "Administrator"}

// ---- HTTP API ----

func setSessionCookie(w http.ResponseWriter, r *http.Request, pre, tok string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: tok, Path: pre + "/", MaxAge: maxAge, HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")})
}

func registerUsers(mux *http.ServeMux, pre string, u *Users, adminToken string) {
	readJSON := func(r *http.Request, v any) error {
		dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			return httpErr{400, fmt.Errorf("ungültiges JSON: %v", err)}
		}
		return nil
	}
	sendJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET "+pre+"/api/auth/me", func(w http.ResponseWriter, r *http.Request) {
		active := u.Active()
		resp := map[string]any{"users": active, "admin_token": adminToken != ""}
		if id := identityFrom(r); id != nil {
			resp["user"] = id
		} else if !active {
			resp["user"] = nil
		}
		sendJSON(w, resp)
	})
	login := func(w http.ResponseWriter, r *http.Request, name, pw string) {
		tok, x, err := u.Login(name, pw, clientIP(r))
		if err != nil {
			writeResult(w, err)
			return
		}
		setSessionCookie(w, r, pre, tok, int(sessionTTL.Seconds()))
		log.Printf("users: %s logged in from %s", x.Name, clientIP(r))
		sendJSON(w, map[string]any{"user": Identity{Name: x.Name, Role: x.Role}})
	}
	mux.HandleFunc("POST "+pre+"/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Name, Password string }
		if err := readJSON(r, &in); err != nil {
			writeResult(w, err)
			return
		}
		login(w, r, in.Name, in.Password)
	})
	// first admin, only while there is no user (needs ADMIN_TOKEN if one is set)
	mux.HandleFunc("POST "+pre+"/api/auth/setup", adminAuth(adminToken, func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Name, Password string }
		if err := readJSON(r, &in); err != nil {
			writeResult(w, err)
			return
		}
		if u.Active() {
			writeAPIError(w, http.StatusConflict, errors.New("es gibt schon Benutzer"))
			return
		}
		if err := u.Add(in.Name, in.Password, RoleAdmin); err != nil {
			writeResult(w, err)
			return
		}
		log.Printf("users: first admin %q created via UI", in.Name)
		login(w, r, in.Name, in.Password)
	}))
	mux.HandleFunc("POST "+pre+"/api/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil {
			u.Logout(c.Value)
		}
		setSessionCookie(w, r, pre, "", -1)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST "+pre+"/api/auth/password", func(w http.ResponseWriter, r *http.Request) {
		id := identityFrom(r)
		if id == nil || id.Name == "ADMIN_TOKEN" {
			writeAPIError(w, http.StatusBadRequest, errors.New("nicht als Benutzer angemeldet"))
			return
		}
		var in struct {
			Old string `json:"old"`
			New string `json:"new"`
		}
		if err := readJSON(r, &in); err != nil {
			writeResult(w, err)
			return
		}
		tok := ""
		if c, err := r.Cookie(sessionCookie); err == nil {
			tok = c.Value
		}
		writeResult(w, u.ChangeOwnPassword(id.Name, in.Old, in.New, tok))
	})
	mux.HandleFunc("GET "+pre+"/api/users", adminAuth(adminToken, func(w http.ResponseWriter, r *http.Request) {
		sendJSON(w, u.List())
	}))
	mux.HandleFunc("POST "+pre+"/api/users", adminAuth(adminToken, func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Name, Role, Password string }
		if err := readJSON(r, &in); err != nil {
			writeResult(w, err)
			return
		}
		if !u.Active() && in.Role != RoleAdmin {
			writeAPIError(w, http.StatusBadRequest, errors.New("der erste Benutzer muss ein Administrator sein"))
			return
		}
		writeResult(w, u.Add(in.Name, in.Password, in.Role))
	}))
	mux.HandleFunc("PUT "+pre+"/api/users/{name}", adminAuth(adminToken, func(w http.ResponseWriter, r *http.Request) {
		var in userUpdate
		if err := readJSON(r, &in); err != nil {
			writeResult(w, err)
			return
		}
		writeResult(w, u.Update(r.PathValue("name"), in))
	}))
	mux.HandleFunc("DELETE "+pre+"/api/users/{name}", adminAuth(adminToken, func(w http.ResponseWriter, r *http.Request) {
		writeResult(w, u.Delete(r.PathValue("name")))
	}))
}
