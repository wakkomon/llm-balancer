package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Hop-by-hop headers are never forwarded (RFC 7230 §6.1).
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

type Proxy struct {
	cfg       Config
	pool      *Pool
	transport *http.Transport
}

func NewProxy(cfg Config, pool *Pool) *Proxy {
	return &Proxy{
		cfg:  cfg,
		pool: pool,
		transport: &http.Transport{
			Proxy:               nil, // talk to the LLM servers directly
			DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true, // pass Accept-Encoding / Content-Encoding through untouched
			// no ResponseHeaderTimeout: prompt processing on a local LLM can take minutes before the first byte
		},
	}
}

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func stripHop(h http.Header) {
	if c := h.Get("Connection"); c != "" {
		for _, f := range strings.Split(c, ",") {
			h.Del(strings.TrimSpace(f))
		}
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

func clientAddr(r *http.Request) string {
	if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
		return strings.TrimSpace(strings.Split(xf, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": "llm-balancer: " + msg, "type": "llm_balancer_error", "code": code},
	})
}

// upstream builds the outgoing request for backend b.
func (px *Proxy) upstream(ctx context.Context, r *http.Request, b *Backend, body []byte) (*http.Request, error) {
	target := px.pool.targetOf(b)
	target.Path = strings.TrimRight(target.Path, "/") + r.URL.Path
	target.RawQuery = r.URL.RawQuery
	out, err := http.NewRequestWithContext(ctx, r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyHeaders(out.Header, r.Header)
	stripHop(out.Header)
	out.Header.Del("Content-Length")
	out.ContentLength = int64(len(body))
	if body == nil {
		out.Body = http.NoBody
		out.ContentLength = 0
	}
	ip := clientAddr(r)
	if prior := r.Header.Get("X-Forwarded-For"); prior == "" {
		out.Header.Set("X-Forwarded-For", ip)
	}
	out.Header.Set("X-Forwarded-Host", r.Host)
	return out, nil
}

// ServeHTTP proxies every request that is not for the UI.
func (px *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil && r.Body != http.NoBody {
		var err error
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, int64(px.cfg.MaxBodyMB)<<20))
		if err != nil {
			jsonError(w, http.StatusRequestEntityTooLarge, "could not read request body: "+err.Error())
			return
		}
	}

	if !slices.Contains(px.cfg.SlotMethods, r.Method) {
		px.passthrough(w, r, body)
		return
	}

	req := px.pool.NewRequest(r.Method, r.URL.Path, clientAddr(r))
	// Model/stream are only extracted for display, the body is forwarded unchanged.
	var meta struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if len(body) > 0 && json.Unmarshal(body, &meta) == nil {
		req.Model, req.Stream = meta.Model, meta.Stream
	}

	ctx := r.Context()
	exclude := map[*Backend]bool{}
	var lastBackend *Backend
	maxAttempts := max(1, px.pool.count())

	for attempt := 1; ; attempt++ {
		b, err := px.pool.Acquire(ctx, req, exclude)
		if err != nil {
			code := http.StatusServiceUnavailable
			switch {
			case errors.Is(err, ErrQueueTimeout):
				code = http.StatusGatewayTimeout
			case errors.Is(err, ErrNoBackend):
				code = http.StatusBadGateway
			case ctx.Err() != nil:
				code = 499 // client went away while queued
			}
			if ctx.Err() == nil {
				jsonError(w, code, err.Error())
			}
			px.pool.Finish(req, lastBackend, code, err.Error())
			return
		}
		lastBackend = b

		out, err := px.upstream(ctx, r, b, body)
		if err != nil {
			px.pool.Release(b, req, err.Error())
			jsonError(w, http.StatusInternalServerError, err.Error())
			px.pool.Finish(req, b, 500, err.Error())
			return
		}
		resp, err := px.transport.RoundTrip(out)
		if err != nil {
			if ctx.Err() != nil { // client gone, not the backend's fault
				px.pool.Release(b, req, "")
				px.pool.Finish(req, b, 499, "client disconnected")
				return
			}
			msg := fmt.Sprintf("connect %s: %v", req.Backend, err)
			log.Printf("req %d: %s", req.ID, msg)
			px.pool.MarkUnhealthy(b, err.Error())
			px.pool.Release(b, req, msg)
			exclude[b] = true
			if attempt < maxAttempts {
				continue
			}
			jsonError(w, http.StatusBadGateway, msg)
			px.pool.Finish(req, b, http.StatusBadGateway, msg)
			return
		}

		if slices.Contains(px.cfg.RetryStatus, resp.StatusCode) && attempt < maxAttempts {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			msg := fmt.Sprintf("%s answered %d, trying next backend", req.Backend, resp.StatusCode)
			log.Printf("req %d: %s", req.ID, msg)
			px.pool.Release(b, req, msg)
			exclude[b] = true
			continue
		}

		n, copyErr := px.stream(w, resp, req)
		resp.Body.Close()
		errMsg := ""
		if copyErr != nil && ctx.Err() == nil {
			errMsg = "stream: " + copyErr.Error()
		}
		if resp.StatusCode >= 500 && errMsg == "" {
			errMsg = fmt.Sprintf("upstream status %d", resp.StatusCode)
		}
		px.pool.Release(b, req, errMsg)
		if ctx.Err() != nil && errMsg == "" {
			errMsg = "client disconnected"
		}
		px.pool.Finish(req, b, resp.StatusCode, errMsg)
		_ = n
		return
	}
}

// stream copies the upstream response to the client, flushing after every chunk (SSE friendly).
func (px *Proxy) stream(w http.ResponseWriter, resp *http.Response, req *ReqInfo) (int64, error) {
	stripHop(resp.Header)
	copyHeaders(w.Header(), resp.Header)
	w.Header().Set("X-LLM-Balancer-Backend", req.Backend)
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	_ = rc.Flush()

	buf := make([]byte, 32*1024)
	var total int64
	first := true
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if first {
				px.pool.setFirstByte(req)
				first = false
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
			req.bytes.Add(int64(n))
			_ = rc.Flush()
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

// passthrough forwards requests that don't need a slot (GET /v1/models, /health, ...).
func (px *Proxy) passthrough(w http.ResponseWriter, r *http.Request, body []byte) {
	b, name := px.pool.PickPassthrough()
	if b == nil {
		jsonError(w, http.StatusServiceUnavailable, "no healthy backend")
		return
	}
	out, err := px.upstream(r.Context(), r, b, body)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp, err := px.transport.RoundTrip(out)
	if err != nil {
		jsonError(w, http.StatusBadGateway, fmt.Sprintf("%s: %v", name, err))
		return
	}
	defer resp.Body.Close()
	stripHop(resp.Header)
	copyHeaders(w.Header(), resp.Header)
	w.Header().Set("X-LLM-Balancer-Backend", name)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// healthLoop actively probes idle backends.
func (px *Proxy) healthLoop(ctx context.Context) {
	if px.cfg.HealthPath == "" {
		return
	}
	client := &http.Client{Transport: px.transport, Timeout: 5 * time.Second}
	check := func() {
		for _, ht := range px.pool.idleTargets() {
			// busy servers are skipped: a single-slot server might not answer quickly, and it is obviously alive
			go func(ht healthTarget) {
				resp, err := client.Get(ht.url + px.cfg.HealthPath)
				if err != nil {
					px.pool.SetHealth(ht.b, false, err.Error(), ht.url)
					return
				}
				io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				if resp.StatusCode >= 500 {
					px.pool.SetHealth(ht.b, false, fmt.Sprintf("health %s → %d", px.cfg.HealthPath, resp.StatusCode), ht.url)
				} else {
					px.pool.SetHealth(ht.b, true, "", ht.url)
				}
			}(ht)
		}
	}
	check()
	t := time.NewTicker(time.Duration(px.cfg.HealthIntervalSec) * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		case <-px.pool.healthKick:
			check()
		}
	}
}
