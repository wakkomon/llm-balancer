package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // the scratch image has no zoneinfo; TZ=Europe/Berlin still works
)

//go:embed web/index.html
var indexHTML []byte

//go:embed VERSION
var versionFile string

var (
	commit  = "" // set at build time (-X main.commit=…), shown as detail only
	started = time.Now()
)

// versionString: the release number from the VERSION file, e.g. "1.0.1".
func versionString() string { return strings.TrimSpace(versionFile) }

func shortCommit() string {
	if len(commit) > 7 {
		return commit[:7]
	}
	if commit == "unknown" {
		return ""
	}
	return commit
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	pool, err := NewPool(cfg, cfg.Backends, NewStore(cfg.DataDir))
	if err != nil {
		log.Fatalf("saved backends: %v", err)
	}
	pool.stats = NewStats(cfg.DataDir, cfg.StatsDays)
	access, err := NewAccess(cfg.DataDir, cfg.APIKeys)
	if err != nil {
		log.Fatalf("access tokens: %v", err)
	}
	if err := pool.LoadCounters(cfg.DataDir); err != nil {
		log.Printf("counters: %v (starting from zero)", err)
	}
	px := NewProxy(cfg, pool)
	px.access = access
	pool.access = access
	px.users = NewUsers(cfg.DataDir)
	if err := px.users.Bootstrap(os.Getenv("ADMIN_USER"), os.Getenv("ADMIN_PASSWORD")); err != nil {
		log.Fatalf("ADMIN_USER/ADMIN_PASSWORD: %v", err)
	}
	access.onChange = pool.kickCaps
	pool.notifier = NewNotifier(cfg.DataDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go px.healthLoop(ctx)
	pool.checkCaps() // before serving: a cap reached before a restart must apply right away
	go pool.capsLoop(ctx.Done())
	go pool.notifier.mailLoop(ctx.Done())
	go pool.notifier.loop(pool, ctx.Done())
	statsDone := make(chan struct{})
	go func() { // statistics, token usage and counters: every minute (only what changed)
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				persistAll(pool, access)
			case <-ctx.Done():
				return
			}
		}
	}()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           newMux(cfg, pool, px),
		ReadHeaderTimeout: 30 * time.Second,
		// no Read/WriteTimeout: generations can stream for a long time
	}

	log.Printf("llm-balancer v%s (commit %s, started %s) listening on %s, UI at %s/", versionString(), shortCommit(), started.Format(time.RFC3339), cfg.Listen, cfg.UIPrefix)
	pool.mu.Lock()
	if pool.source == "saved" {
		log.Printf("using backends saved in %s (BACKENDS/config.json ignored until reset in the UI)", pool.store.path)
	}
	for _, b := range pool.configsLocked() {
		log.Printf("  backend %-20s %s  slots=%d prio=%d enabled=%v", b.Name, b.URL, b.MaxConcurrent, b.Priority, *b.Enabled)
	}
	if len(pool.backends) == 0 {
		log.Printf("  no backends configured yet – add them in the web UI")
	}
	pool.mu.Unlock()
	if len(cfg.dnsServers) > 0 {
		log.Printf("DNS servers for LLM server names: %s (fallback: container resolver)", strings.Join(cfg.dnsServers, ", "))
	}
	if access.Enabled() {
		log.Printf("API access requires a token (API_KEYS / tokens created in the UI)")
	} else {
		log.Printf("no API tokens configured: the LLM API is open to everyone who can reach it")
	}
	if cfg.AdminToken == "" {
		log.Printf("ADMIN_TOKEN not set: anyone who can reach the UI can change the backends")
	}

	go func() {
		<-ctx.Done()
		persistAll(pool, access) // right away: Docker may kill the container before running requests finish
		log.Printf("shutting down, waiting up to 30s for running requests")
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		persistAll(pool, access) // again, including requests that finished during shutdown
		close(statsDone)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	<-statsDone
}

func newMux(cfg Config, pool *Pool, px *Proxy) http.Handler {
	ui := http.NewServeMux()
	pre := cfg.UIPrefix

	ui.HandleFunc("GET "+pre+"/{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(indexHTML)
	})
	ui.HandleFunc("GET "+pre, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, pre+"/", http.StatusFound)
	})
	ui.HandleFunc("GET "+pre+"/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(pool.Status())
	})
	registerAdmin(ui, pre, pool, cfg.AdminToken)
	registerStats(ui, pre, pool.stats, cfg.AdminToken, pool.ResetCounters)
	registerDayView(ui, pre, pool)
	registerGroups(ui, pre, pool, cfg.AdminToken)
	if pool.notifier != nil {
		registerNotify(ui, pre, pool.notifier, cfg.AdminToken)
	}
	if px.access != nil {
		registerAccess(ui, pre, px.access, cfg.AdminToken)
	}
	users := px.users
	if users == nil {
		users = NewUsers("")
		px.users = users
	}
	registerUsers(ui, pre, users, cfg.AdminToken)
	registerConfigIO(ui, pre, pool, px.access, users, cfg.AdminToken)
	ui.HandleFunc("GET "+pre+"/api/recent", func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(pool.RecentAfter(after))
	})
	ui.HandleFunc("GET "+pre+"/metrics", func(w http.ResponseWriter, r *http.Request) {
		writeMetrics(w, pool.Status())
	})
	ui.HandleFunc("GET "+pre+"/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})

	guarded := users.middleware(pre, cfg.AdminToken, ui)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pre || strings.HasPrefix(r.URL.Path, pre+"/") {
			guarded.ServeHTTP(w, r)
			return
		}
		px.ServeHTTP(w, r)
	})
}

func writeMetrics(w http.ResponseWriter, st Status) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# TYPE llm_balancer_queue_length gauge\nllm_balancer_queue_length %d\n", len(st.Queue))
	fmt.Fprintf(w, "# TYPE llm_balancer_requests_total counter\nllm_balancer_requests_total %d\n", st.TotalReqs)
	fmt.Fprintf(w, "# TYPE llm_balancer_rejected_total counter\nllm_balancer_rejected_total %d\n", st.Rejected)
	b2i := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	fmt.Fprintln(w, "# TYPE llm_balancer_backend_active gauge")
	for _, b := range st.Backends {
		fmt.Fprintf(w, "llm_balancer_backend_active{backend=%q} %d\n", b.Name, b.Active)
	}
	fmt.Fprintln(w, "# TYPE llm_balancer_backend_up gauge")
	for _, b := range st.Backends {
		fmt.Fprintf(w, "llm_balancer_backend_up{backend=%q} %d\n", b.Name, b2i(b.Healthy && b.Enabled))
	}
	fmt.Fprintln(w, "# TYPE llm_balancer_backend_requests_total counter")
	for _, b := range st.Backends {
		fmt.Fprintf(w, "llm_balancer_backend_requests_total{backend=%q} %d\n", b.Name, b.Total)
	}
	fmt.Fprintln(w, "# TYPE llm_balancer_backend_errors_total counter")
	for _, b := range st.Backends {
		fmt.Fprintf(w, "llm_balancer_backend_errors_total{backend=%q} %d\n", b.Name, b.Errors)
	}
}
