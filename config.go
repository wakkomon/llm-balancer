package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// BackendConfig describes one upstream LLM server.
type BackendConfig struct {
	Name          string `json:"name"`
	URL           string `json:"url"`
	MaxConcurrent int    `json:"max_concurrent"`            // parallel requests the server can handle (default 1)
	Priority      int    `json:"priority"`                  // lower = preferred when several are free (default 0)
	Enabled       *bool  `json:"enabled"`                   // default true
	APIKey        string `json:"api_key,omitempty"`         // sent as "Authorization: Bearer <key>" to this server
	Group         string `json:"group,omitempty"`           // name of the server group (see Config.Groups)
	TLSSkipVerify bool   `json:"tls_skip_verify,omitempty"` // https with a self-signed certificate: don't verify it
	// Outgoing prices: what this server costs per 1M input/output tokens (for the cost statistics)
	PriceIn  float64 `json:"price_in,omitempty"`
	PriceOut float64 `json:"price_out,omitempty"`
	// Outgoing cost cap: the server is taken out of rotation when its costs reach CostCap per CapPeriod
	CostCap   float64 `json:"cost_cap,omitempty"`
	CapPeriod string  `json:"cap_period,omitempty"` // day | month (default) | total
	// Model filter: names/patterns this server serves (in addition to the models it reports in
	// /v1/models, unless AutoModels is false). Used to pick the right server inside a group.
	Models     []string `json:"models,omitempty"`
	AutoModels *bool    `json:"auto_models,omitempty"` // default true
	keep       int      // API update: fields not sent (keep current value)
}

// Config is the full balancer configuration.
type Config struct {
	Listen               string          `json:"listen"`    // default ":8080"
	UIPrefix             string          `json:"ui_prefix"` // default "/balancer"
	Backends             []BackendConfig `json:"backends"`
	Groups               []GroupConfig   `json:"groups"`              // server groups, routed by model name
	UnknownModel         string          `json:"unknown_model"`       // "all" (default): models no group/server knows go to every server; "reject": 404
	QueueTimeoutSec      int             `json:"queue_timeout_sec"`   // max wait in queue, 0 = unlimited (default 900)
	MaxQueue             int             `json:"max_queue"`           // max queued requests, 0 = unlimited
	HealthPath           string          `json:"health_path"`         // default "/v1/models", "" disables active checks
	HealthIntervalSec    int             `json:"health_interval_sec"` // default 15
	SlotMethods          []string        `json:"slot_methods"`        // methods that occupy a slot (default ["POST"])
	RetryAll             bool            `json:"retry_all"`           // every error answer (>= 400) makes the balancer try another server (default true)
	RetryStatus          []int           `json:"retry_status"`        // besides every 5xx: status codes that make the balancer try another server (default 401,403,404,408,429)
	MaxBodyMB            int             `json:"max_body_mb"`         // default 64
	RecentRequests       int             `json:"recent_requests"`     // how many finished requests the UI keeps (default 300)
	AdminToken           string          `json:"admin_token"`         // if set, changes in the UI/API need this token
	StatsDays            int             `json:"stats_days"`          // how many days of statistics to keep (default 400)
	APIKeys              string          `json:"api_keys"`            // client tokens for the LLM API, "name=token,..." (env API_KEYS)
	TagCheck             string          `json:"tag_check"`           // tags to check in generated text, "*" = all, "off" (default: DefaultTagCheck)
	StreamUsage          bool            `json:"stream_usage"`        // add stream_options.include_usage to streaming requests (default true)
	Currency             string          `json:"currency"`            // shown next to prices (default "€")
	DNSServers           string          `json:"dns_servers"`         // DNS servers for the LLM server names, e.g. "192.168.1.1,1.1.1.1"
	dnsServers           []string
	DataDir              string `json:"data_dir"`               // where backends.json (UI changes) is stored (default "data", Docker: /data)
	UnhealthyCooldownSec int    `json:"unhealthy_cooldown_sec"` // used when health checks are disabled (default 10)
}

func defaultConfig() Config {
	return Config{
		Listen:               ":8080",
		UIPrefix:             "/balancer",
		QueueTimeoutSec:      900,
		HealthPath:           "/v1/models",
		HealthIntervalSec:    15,
		SlotMethods:          []string{"POST"},
		RetryAll:             true,
		RetryStatus:          []int{401, 403, 404, 408, 429},
		MaxBodyMB:            64,
		RecentRequests:       300,
		UnhealthyCooldownSec: 10,
		DataDir:              "data",
		StatsDays:            400,
		StreamUsage:          true,
		TagCheck:             DefaultTagCheck,
		Currency:             "€",
	}
}

// loadConfig reads an optional JSON file (CONFIG env, default ./config.json or /config/config.json)
// and then applies environment overrides.
func loadConfig() (Config, error) {
	cfg := defaultConfig()

	path := os.Getenv("CONFIG")
	if path == "" {
		for _, p := range []string{"config.json", "/config/config.json"} {
			if _, err := os.Stat(p); err == nil {
				path = p
				break
			}
		}
	}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config %s: %w", path, err)
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
	}

	// Environment overrides (handy for docker-compose / Portainer).
	if v := os.Getenv("LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("BACKENDS"); v != "" {
		bs, err := parseBackendsEnv(v)
		if err != nil {
			return cfg, err
		}
		cfg.Backends = bs
	}
	envInt := func(name string, dst *int) error {
		if v := os.Getenv(name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			*dst = n
		}
		return nil
	}
	for name, dst := range map[string]*int{
		"QUEUE_TIMEOUT_SEC":   &cfg.QueueTimeoutSec,
		"MAX_QUEUE":           &cfg.MaxQueue,
		"HEALTH_INTERVAL_SEC": &cfg.HealthIntervalSec,
		"STATS_DAYS":          &cfg.StatsDays,
	} {
		if err := envInt(name, dst); err != nil {
			return cfg, err
		}
	}
	if v := os.Getenv("UNKNOWN_MODEL"); v != "" {
		cfg.UnknownModel = strings.ToLower(v)
	}
	if v, ok := os.LookupEnv("TAG_CHECK"); ok {
		cfg.TagCheck = v
	}
	if v, ok := os.LookupEnv("RETRY_STATUS"); ok && strings.EqualFold(strings.TrimSpace(v), "all") {
		cfg.RetryAll = true
	} else if ok {
		cfg.RetryAll = false
		cfg.RetryStatus = nil
		for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' }) {
			n, err := strconv.Atoi(f)
			if err != nil || n < 400 || n > 599 {
				return cfg, fmt.Errorf("RETRY_STATUS: ungültiger Statuscode %q", f)
			}
			cfg.RetryStatus = append(cfg.RetryStatus, n)
		}
	}
	if v := os.Getenv("STREAM_USAGE"); v != "" {
		cfg.StreamUsage = v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
	}
	if v := os.Getenv("CURRENCY"); v != "" {
		cfg.Currency = v
	}
	if v := os.Getenv("API_KEYS"); v != "" {
		cfg.APIKeys = v
	}
	if v := os.Getenv("BACKEND_KEYS"); v != "" {
		if err := applyBackendKeys(cfg.Backends, v); err != nil {
			return cfg, err
		}
	}
	if v := os.Getenv("ADMIN_TOKEN"); v != "" {
		cfg.AdminToken = v
	}
	if v := os.Getenv("DNS_SERVERS"); v != "" {
		cfg.DNSServers = v
	}
	if v := os.Getenv("DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if v, ok := os.LookupEnv("HEALTH_PATH"); ok {
		cfg.HealthPath = v
	}

	return cfg, cfg.validate()
}

// parseBackendsEnv parses BACKENDS="name=http://host:port*2@1,http://other:8080"
//
//	name=   optional display name
//	*N      optional max concurrent requests (default 1)
//	@N      optional priority (lower = preferred)
func parseBackendsEnv(v string) ([]BackendConfig, error) {
	var out []BackendConfig
	for _, item := range strings.Split(v, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		var b BackendConfig
		if i := strings.Index(item, "="); i > 0 && !strings.Contains(item[:i], "://") {
			b.Name, item = item[:i], item[i+1:]
		}
		if i := strings.LastIndex(item, "@"); i > strings.Index(item, "://")+3 {
			p, err := strconv.Atoi(item[i+1:])
			if err != nil {
				return nil, fmt.Errorf("BACKENDS priority in %q: %w", item, err)
			}
			b.Priority, item = p, item[:i]
		}
		if i := strings.LastIndex(item, "*"); i > 0 {
			n, err := strconv.Atoi(item[i+1:])
			if err != nil {
				return nil, fmt.Errorf("BACKENDS max_concurrent in %q: %w", item, err)
			}
			b.MaxConcurrent, item = n, item[:i]
		}
		b.URL = item
		out = append(out, b)
	}
	return out, nil
}

func (c *Config) validate() error {
	c.UIPrefix = "/" + strings.Trim(c.UIPrefix, "/")
	if err := validateSetup(c.Backends, c.Groups); err != nil {
		return err
	}
	ds, err := parseDNSServers(c.DNSServers)
	if err != nil {
		return err
	}
	c.dnsServers = ds
	if c.UnknownModel != "reject" {
		c.UnknownModel = "all"
	}
	if c.HealthIntervalSec <= 0 {
		c.HealthIntervalSec = 15
	}
	if c.MaxBodyMB <= 0 {
		c.MaxBodyMB = 64
	}
	if c.RecentRequests <= 0 {
		c.RecentRequests = 300
	}
	if c.UnhealthyCooldownSec <= 0 {
		c.UnhealthyCooldownSec = 10
	}
	for i, m := range c.SlotMethods {
		c.SlotMethods[i] = strings.ToUpper(m)
	}
	return nil
}

// normalizeBackend checks and fills defaults for a single backend entry.
func normalizeBackend(b *BackendConfig) error {
	b.Name = strings.TrimSpace(b.Name)
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(b.URL), "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("ungültige URL %q (erwartet z. B. http://192.168.1.50:8080)", b.URL)
	}
	b.URL = u.String()
	if b.Name == "" {
		b.Name = u.Host
	}
	if strings.ContainsAny(b.Name, "/?#") {
		return fmt.Errorf("Name %q darf kein / ? # enthalten", b.Name)
	}
	if b.MaxConcurrent <= 0 {
		b.MaxConcurrent = 1
	}
	var ms []string
	seen := map[string]bool{}
	for _, m := range b.Models {
		if m = strings.TrimSpace(m); m != "" && !seen[strings.ToLower(m)] {
			seen[strings.ToLower(m)] = true
			ms = append(ms, m)
		}
	}
	b.Models = ms
	if b.PriceIn < 0 || b.PriceOut < 0 || b.PriceIn > 1e6 || b.PriceOut > 1e6 {
		return fmt.Errorf("Preis muss zwischen 0 und 1.000.000 liegen")
	}
	if b.CostCap < 0 || b.CostCap > 1e9 {
		return fmt.Errorf("Kostendeckel muss zwischen 0 und 1.000.000.000 liegen")
	}
	per, err := normalizePeriod(b.CapPeriod)
	if err != nil {
		return err
	}
	b.CapPeriod = per
	return nil
}

func validateBackends(bs []BackendConfig) error {
	seen := map[string]bool{}
	for i := range bs {
		if err := normalizeBackend(&bs[i]); err != nil {
			return fmt.Errorf("backend %d: %w", i+1, err)
		}
		if seen[bs[i].Name] {
			return fmt.Errorf("doppelter Backend-Name %q", bs[i].Name)
		}
		seen[bs[i].Name] = true
	}
	return nil
}

func (c *Config) queueTimeout() time.Duration {
	return time.Duration(c.QueueTimeoutSec) * time.Second
}

// applyBackendKeys parses BACKEND_KEYS="gpu=sk-abc,amd=xyz" and sets the bearer tokens by backend name.
// Names must match BACKENDS (a name defaults to host:port when none was given).
func applyBackendKeys(bs []BackendConfig, v string) error {
	names := map[string]int{}
	for i := range bs {
		n := bs[i].Name
		if n == "" {
			if u, err := url.Parse(bs[i].URL); err == nil {
				n = u.Host
			}
		}
		names[n] = i
	}
	for _, item := range strings.Split(v, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		k := strings.Index(item, "=")
		if k <= 0 {
			return fmt.Errorf("BACKEND_KEYS: erwartet name=token, nicht %q", item)
		}
		i, ok := names[item[:k]]
		if !ok {
			return fmt.Errorf("BACKEND_KEYS: kein Backend namens %q in BACKENDS", item[:k])
		}
		bs[i].APIKey = item[k+1:]
	}
	return nil
}

// validateSetup checks backends and groups together (group references must exist).
func validateSetup(bs []BackendConfig, gs []GroupConfig) error {
	if err := validateBackends(bs); err != nil {
		return err
	}
	names := map[string]bool{}
	for i := range gs {
		if err := normalizeGroup(&gs[i]); err != nil {
			return fmt.Errorf("Gruppe %d: %w", i+1, err)
		}
		if names[gs[i].Name] {
			return fmt.Errorf("doppelte Gruppe %q", gs[i].Name)
		}
		names[gs[i].Name] = true
	}
	for _, g := range gs {
		if g.Fallback != "" && !names[g.Fallback] {
			return fmt.Errorf("Gruppe %q: unbekannte Fallback-Gruppe %q", g.Name, g.Fallback)
		}
	}
	for _, b := range bs {
		if b.Group != "" && !names[b.Group] {
			return fmt.Errorf("Server %q: unbekannte Gruppe %q", b.Name, b.Group)
		}
	}
	return nil
}
