package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// HTTPS: an additional port (HTTPS_LISTEN, default :8443) serves exactly the same as the http port.
// Certificate: TLS_CERT + TLS_KEY (files, reloaded when they change) – or a self-signed one that the
// balancer creates in DATA_DIR/tls/ and renews before it expires. Names/IPs for it: localhost, the
// container's hostname and addresses plus TLS_HOSTS and the extra names entered in the UI.

const certValidity = 825 * 24 * time.Hour // Apple clients refuse longer-lived TLS server certificates

type TLSInfo struct {
	Enabled     bool      `json:"enabled"`
	Listen      string    `json:"listen,omitempty"`
	Source      string    `json:"source"` // "self-signed" | "files"
	Subject     string    `json:"subject,omitempty"`
	Hosts       []string  `json:"hosts,omitempty"` // SANs of the certificate
	Extra       []string  `json:"extra_hosts"`     // names added in the UI (self-signed only)
	EnvHosts    []string  `json:"env_hosts,omitempty"`
	NotBefore   time.Time `json:"not_before"`
	NotAfter    time.Time `json:"not_after"`
	Fingerprint string    `json:"sha256"`
	Error       string    `json:"error,omitempty"`
}

type tlsManager struct {
	mu      sync.Mutex
	cfg     Config
	dir     string // DATA_DIR/tls ("" = keep in memory only)
	cert    *tls.Certificate
	leaf    *x509.Certificate
	pemCert []byte
	files   bool
	modTime time.Time
	checked time.Time
	extra   []string
	lastErr string
}

func newTLSManager(cfg Config) (*tlsManager, error) {
	m := &tlsManager{cfg: cfg, files: cfg.TLSCert != "" || cfg.TLSKey != ""}
	if m.files {
		if cfg.TLSCert == "" || cfg.TLSKey == "" {
			return nil, errors.New("TLS_CERT und TLS_KEY müssen beide gesetzt sein")
		}
		return m, m.loadFiles()
	}
	if cfg.DataDir != "" {
		m.dir = filepath.Join(cfg.DataDir, "tls")
		if data, err := os.ReadFile(filepath.Join(m.dir, "hosts.json")); err == nil {
			json.Unmarshal(data, &m.extra)
		}
	}
	return m, m.ensureSelfSigned(false)
}

func (m *tlsManager) loadFiles() error {
	st, err := os.Stat(m.cfg.TLSCert)
	if err != nil {
		return err
	}
	c, err := tls.LoadX509KeyPair(m.cfg.TLSCert, m.cfg.TLSKey)
	if err != nil {
		return fmt.Errorf("TLS-Zertifikat: %w", err)
	}
	pemData, _ := os.ReadFile(m.cfg.TLSCert)
	m.set(&c, pemData)
	m.modTime = st.ModTime()
	return nil
}

func (m *tlsManager) set(c *tls.Certificate, pemData []byte) {
	if len(c.Certificate) > 0 {
		c.Leaf, _ = x509.ParseCertificate(c.Certificate[0])
	}
	m.cert, m.leaf, m.pemCert = c, c.Leaf, pemData
}

// wantedHosts: names the self-signed certificate must contain.
func (m *tlsManager) wantedHosts() []string {
	hs := []string{"localhost"}
	hs = append(hs, m.cfg.TLSHosts...)
	hs = append(hs, m.extra...)
	return uniqHosts(hs)
}

func uniqHosts(in []string) []string {
	out := []string{}
	for _, h := range in {
		h = strings.ToLower(strings.TrimSpace(h))
		h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
		if h != "" && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

func certHosts(c *x509.Certificate) []string {
	out := slices.Clone(c.DNSNames)
	for _, ip := range c.IPAddresses {
		out = append(out, ip.String())
	}
	return out
}

func (m *tlsManager) covers(c *x509.Certificate) bool {
	have := certHosts(c)
	for _, h := range m.wantedHosts() {
		if !slices.Contains(have, h) {
			return false
		}
	}
	return true
}

// ensureSelfSigned loads the stored certificate or creates a new one (missing, expiring within 30 days,
// a wanted name missing, or force).
func (m *tlsManager) ensureSelfSigned(force bool) error {
	if !force && m.dir != "" {
		cp, kp := filepath.Join(m.dir, "cert.pem"), filepath.Join(m.dir, "key.pem")
		if c, err := tls.LoadX509KeyPair(cp, kp); err == nil {
			pemData, _ := os.ReadFile(cp)
			m.set(&c, pemData)
			if m.leaf != nil && time.Until(m.leaf.NotAfter) > 30*24*time.Hour && m.covers(m.leaf) {
				return nil
			}
			log.Printf("TLS: stored certificate expires soon or lacks a configured name – creating a new one")
		}
	}
	return m.generate()
}

func (m *tlsManager) generate() error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	hosts := m.wantedHosts()
	if hn, err := os.Hostname(); err == nil {
		hosts = append(hosts, hn)
	}
	hosts = append(hosts, "127.0.0.1", "::1")
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLinkLocalUnicast() {
				hosts = append(hosts, ipn.IP.String())
			}
		}
	}
	hosts = uniqHosts(hosts)
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	now := time.Now()
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "llm-balancer", Organization: []string{"llm-balancer (selbst signiert)"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else {
			tpl.DNSNames = append(tpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return err
	}
	m.set(&c, certPEM)
	if m.dir != "" {
		if err := os.MkdirAll(m.dir, 0o700); err != nil {
			return err
		}
		if err := writeFileAtomic(filepath.Join(m.dir, "key.pem"), keyPEM, 0o600); err != nil {
			return err
		}
		if err := writeFileAtomic(filepath.Join(m.dir, "cert.pem"), certPEM, 0o644); err != nil {
			return err
		}
	}
	log.Printf("TLS: self-signed certificate created for %s (valid until %s, SHA-256 %s)", strings.Join(hosts, ", "),
		m.leaf.NotAfter.Format("2006-01-02"), fingerprint(m.leaf))
	return nil
}

func fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	var parts []string
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

// getCertificate is called for every TLS handshake: reloads changed files, renews the self-signed
// certificate in time (checked at most once an hour).
func (m *tlsManager) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Since(m.checked) > time.Minute {
		m.checked = time.Now()
		if m.files {
			if st, err := os.Stat(m.cfg.TLSCert); err == nil && !st.ModTime().Equal(m.modTime) {
				if err := m.loadFiles(); err != nil {
					m.lastErr = err.Error()
					log.Printf("TLS: reload failed, keeping the old certificate: %v", err)
				} else {
					m.lastErr = ""
					log.Printf("TLS: certificate reloaded from %s", m.cfg.TLSCert)
				}
			}
		} else if m.leaf != nil && time.Until(m.leaf.NotAfter) < 30*24*time.Hour {
			if err := m.generate(); err != nil {
				m.lastErr = err.Error()
			}
		}
	}
	return m.cert, nil
}

func (m *tlsManager) info() TLSInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	in := TLSInfo{Enabled: true, Listen: m.cfg.HTTPSListen, Source: "self-signed", Extra: append([]string{}, m.extra...), EnvHosts: m.cfg.TLSHosts, Error: m.lastErr}
	if m.files {
		in.Source = "files"
	}
	if m.leaf != nil {
		in.Subject = m.leaf.Subject.CommonName
		in.Hosts = certHosts(m.leaf)
		in.NotBefore, in.NotAfter = m.leaf.NotBefore, m.leaf.NotAfter
		in.Fingerprint = fingerprint(m.leaf)
	}
	return in
}

// regenerate creates a new self-signed certificate with these extra names.
func (m *tlsManager) regenerate(extra []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.files {
		return httpErr{400, errors.New("das Zertifikat kommt aus TLS_CERT/TLS_KEY – dort austauschen")}
	}
	for _, h := range uniqHosts(extra) {
		if strings.ContainsAny(h, " /\\,;\"'<>") || len(h) > 253 {
			return httpErr{400, fmt.Errorf("ungültiger Name: %q", h)}
		}
	}
	m.extra = uniqHosts(extra)
	if m.dir != "" {
		os.MkdirAll(m.dir, 0o700)
		data, _ := json.Marshal(m.extra)
		writeFileAtomic(filepath.Join(m.dir, "hosts.json"), data, 0o644)
	}
	if err := m.generate(); err != nil {
		return err
	}
	m.lastErr = ""
	return nil
}

func registerTLS(mux *http.ServeMux, pre string, m *tlsManager, cfg Config, adminToken string) {
	mux.HandleFunc("GET "+pre+"/api/tls", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if m == nil {
			in := TLSInfo{Enabled: false}
			if cfg.httpsErr != "" {
				in.Error = cfg.httpsErr
			}
			json.NewEncoder(w).Encode(in)
			return
		}
		json.NewEncoder(w).Encode(m.info())
	})
	mux.HandleFunc("GET "+pre+"/api/tls/cert.pem", func(w http.ResponseWriter, r *http.Request) {
		if m == nil {
			http.NotFound(w, r)
			return
		}
		m.mu.Lock()
		data := m.pemCert
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Header().Set("Content-Disposition", `attachment; filename="llm-balancer.crt"`)
		w.Write(data)
	})
	mux.HandleFunc("POST "+pre+"/api/tls/regenerate", adminAuth(adminToken, func(w http.ResponseWriter, r *http.Request) {
		if m == nil {
			writeAPIError(w, 400, errors.New("HTTPS ist nicht aktiv"))
			return
		}
		var in struct {
			Hosts []string `json:"extra_hosts"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
			writeAPIError(w, 400, err)
			return
		}
		if err := m.regenerate(in.Hosts); err != nil {
			writeResult(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(m.info())
	}))
}

func newHTTPSServer(addr string, h http.Handler, m *tlsManager) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: m.getCertificate},
		ErrorLog:          log.New(tlsErrFilter{}, "", 0),
	}
}

// tlsErrFilter drops the handshake errors of clients that don't trust the self-signed certificate
// (browsers before the exception is accepted) – they would flood the log.
type tlsErrFilter struct{}

func (tlsErrFilter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "TLS handshake error") {
		return len(p), nil
	}
	log.Print(string(p))
	return len(p), nil
}
