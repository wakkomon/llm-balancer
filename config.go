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
	MaxConcurrent int    `json:"max_concurrent"` // parallel requests the server can handle (default 1)
	Priority      int    `json:"priority"`       // lower = preferred when several are free (default 0)
	Enabled       *bool  `json:"enabled"`        // default true
}

// Config is the full balancer configuration.
type Config struct {
	Listen               string          `json:"listen"`    // default ":8080"
	UIPrefix             string          `json:"ui_prefix"` // default "/balancer"
	Backends             []BackendConfig `json:"backends"`
	QueueTimeoutSec      int             `json:"queue_timeout_sec"`      // max wait in queue, 0 = unlimited (default 900)
	MaxQueue             int             `json:"max_queue"`              // max queued requests, 0 = unlimited
	HealthPath           string          `json:"health_path"`            // default "/v1/models", "" disables active checks
	HealthIntervalSec    int             `json:"health_interval_sec"`    // default 15
	SlotMethods          []string        `json:"slot_methods"`           // methods that occupy a slot (default ["POST"])
	RetryStatus          []int           `json:"retry_status"`           // upstream status codes that trigger a retry on another backend (default [503])
	MaxBodyMB            int             `json:"max_body_mb"`            // default 64
	RecentRequests       int             `json:"recent_requests"`        // how many finished requests the UI keeps (default 100)
	UnhealthyCooldownSec int             `json:"unhealthy_cooldown_sec"` // used when health checks are disabled (default 10)
}

func defaultConfig() Config {
	return Config{
		Listen:               ":8080",
		UIPrefix:             "/balancer",
		QueueTimeoutSec:      900,
		HealthPath:           "/v1/models",
		HealthIntervalSec:    15,
		SlotMethods:          []string{"POST"},
		RetryStatus:          []int{503},
		MaxBodyMB:            64,
		RecentRequests:       100,
		UnhealthyCooldownSec: 10,
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
	} {
		if err := envInt(name, dst); err != nil {
			return cfg, err
		}
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
	if len(c.Backends) == 0 {
		return fmt.Errorf("no backends configured (config.json or BACKENDS env)")
	}
	c.UIPrefix = "/" + strings.Trim(c.UIPrefix, "/")
	seen := map[string]bool{}
	for i := range c.Backends {
		b := &c.Backends[i]
		u, err := url.Parse(strings.TrimRight(b.URL, "/"))
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("backend %d: invalid url %q", i, b.URL)
		}
		b.URL = u.String()
		if b.Name == "" {
			b.Name = u.Host
		}
		if seen[b.Name] {
			return fmt.Errorf("duplicate backend name %q", b.Name)
		}
		seen[b.Name] = true
		if b.MaxConcurrent <= 0 {
			b.MaxConcurrent = 1
		}
	}
	if c.HealthIntervalSec <= 0 {
		c.HealthIntervalSec = 15
	}
	if c.MaxBodyMB <= 0 {
		c.MaxBodyMB = 64
	}
	if c.RecentRequests <= 0 {
		c.RecentRequests = 100
	}
	if c.UnhealthyCooldownSec <= 0 {
		c.UnhealthyCooldownSec = 10
	}
	for i, m := range c.SlotMethods {
		c.SlotMethods[i] = strings.ToUpper(m)
	}
	return nil
}

func (c *Config) queueTimeout() time.Duration {
	return time.Duration(c.QueueTimeoutSec) * time.Second
}
