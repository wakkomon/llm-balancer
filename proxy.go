package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"slices"
	"strconv"
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
	access    *Access
	users     *Users
	transport *http.Transport
	insecure  *http.Transport // for servers with "tls_skip_verify" (self-signed certificates)
}

func newTransport(skipVerify bool, dns []string) *http.Transport {
	return &http.Transport{
		Proxy:               nil, // talk to the LLM servers directly
		DialContext:         newDialer(dns),
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: skipVerify},
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true, // pass Accept-Encoding / Content-Encoding through untouched
		// no ResponseHeaderTimeout: prompt processing on a local LLM can take minutes before the first byte
	}
}

func NewProxy(cfg Config, pool *Pool) *Proxy {
	return &Proxy{cfg: cfg, pool: pool, transport: newTransport(false, cfg.dnsServers), insecure: newTransport(true, cfg.dnsServers)}
}

func (px *Proxy) transportFor(skipVerify bool) *http.Transport {
	if skipVerify {
		return px.insecure
	}
	return px.transport
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
	target, key := px.pool.upstreamOf(b)
	target.Path = strings.TrimRight(target.Path, "/") + r.URL.Path
	target.RawQuery = r.URL.RawQuery
	out, err := http.NewRequestWithContext(ctx, r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyHeaders(out.Header, r.Header)
	stripHop(out.Header)
	// The client's credentials are meant for the balancer, not for the LLM servers:
	// drop them when the balancer checks tokens or the server has its own token.
	if key != "" || (px.access != nil && px.access.Enabled()) {
		out.Header.Del("Authorization")
		out.Header.Del("X-Api-Key")
		out.Header.Del("Api-Key")
	}
	if key != "" {
		out.Header.Set("Authorization", "Bearer "+key)
	}
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
	tokenName := ""
	if px.access != nil {
		name, code, msg := px.access.Check(r)
		if code != 0 {
			typ, ecode := "invalid_request_error", "invalid_api_key"
			switch code {
			case http.StatusUnauthorized:
				w.Header().Set("WWW-Authenticate", `Bearer realm="llm-balancer"`)
			case http.StatusForbidden:
				ecode = "api_key_disabled"
			case http.StatusTooManyRequests:
				typ, ecode = "insufficient_quota", "insufficient_quota" // same as OpenAI for an exhausted budget
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"message": "llm-balancer: " + msg, "type": typ, "code": ecode}})
			return
		}
		tokenName = name
	}
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
	req.Token = tokenName
	// Model/stream are only extracted for display, the body is forwarded unchanged.
	var meta struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if len(body) > 0 && json.Unmarshal(body, &meta) == nil {
		req.Model, req.Stream = meta.Model, meta.Stream
	}
	req.Session, req.SessionSrc = detectSession(r, body, tokenName)
	// Ask OpenAI-compatible servers to report token usage at the end of a stream (needed for prices/stats).
	if px.cfg.StreamUsage && meta.Stream && strings.HasSuffix(r.URL.Path, "/completions") {
		if nb, ok := withStreamUsage(body); ok {
			body = nb
		}
	}

	// own context: lets the admin stop this request (STOP button) – cancelling it closes the upstream connection
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	px.pool.setCancel(req, cancel)
	exclude := map[*Backend]bool{}
	var lastBackend *Backend
	var lastFail *upstreamFail         // last error answer of a server – passed on if every server fails
	var failMsgs []string              // connection errors, for the final message
	maxAttempts := px.pool.count() + 2 // every server once, plus re-trying a "busy" one

	for attempt := 1; ; attempt++ {
		b, err := px.pool.Acquire(ctx, req, exclude)
		if err != nil {
			if errors.Is(err, ErrNoBackend) && (lastFail != nil || len(failMsgs) > 0) && !req.stopped.Load() {
				px.passFailure(w, req, lastBackend, lastFail, failMsgs)
				return
			}
			code := http.StatusServiceUnavailable
			switch {
			case errors.Is(err, ErrQueueTimeout):
				code = http.StatusGatewayTimeout
			case errors.Is(err, ErrNoBackend):
				code = http.StatusBadGateway
			case errors.Is(err, ErrNoRoute):
				code = http.StatusNotFound
				err = fmt.Errorf("model %q is not served by any server group", req.Model)
			case req.stopped.Load():
				code, err = 499, errStopped
			case ctx.Err() != nil:
				code = 499 // client went away while queued
			}
			if ctx.Err() == nil || req.stopped.Load() {
				jsonError(w, code, err.Error())
			}
			px.pool.Finish(req, lastBackend, code, err.Error())
			return
		}
		lastBackend = b
		label := req.Backend
		if strings.Contains(req.Group, " → ") {
			label += " (Fallback)"
		}

		sendBody := body
		if req.rewrite != "" {
			sendBody = withModel(body, req.rewrite)
		}
		out, err := px.upstream(ctx, r, b, sendBody)
		if err != nil {
			px.pool.Release(b, req, err.Error())
			jsonError(w, http.StatusInternalServerError, err.Error())
			px.pool.Finish(req, b, 500, err.Error())
			return
		}
		resp, err := px.transportFor(px.pool.skipVerify(b)).RoundTrip(out)
		if err != nil {
			if req.stopped.Load() { // STOP button while the server was processing the prompt
				px.pool.Release(b, req, "")
				jsonError(w, 499, errStopped.Error())
				px.pool.Finish(req, b, 499, stoppedMsg)
				return
			}
			if ctx.Err() != nil { // client gone, not the backend's fault
				px.pool.Release(b, req, "")
				px.pool.Finish(req, b, 499, "client disconnected")
				return
			}
			msg := fmt.Sprintf("connect %s: %s", req.Backend, explainNetErr(err))
			log.Printf("req %d: %s – trying another server", req.ID, msg)
			px.pool.MarkUnhealthy(b, err.Error())
			px.pool.Release(b, req, msg)
			px.pool.addTrail(req, label+": keine Verbindung")
			failMsgs = append(failMsgs, msg)
			px.pool.failedAttempt(req.Backend)
			exclude[b] = true
			if attempt < maxAttempts {
				continue
			}
			px.passFailure(w, req, b, lastFail, failMsgs)
			return
		}

		if px.retryable(resp.StatusCode) && attempt < maxAttempts {
			// read the error answer (bounded) so it can be passed on if no other server succeeds
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			lastFail = &upstreamFail{status: resp.StatusCode, header: resp.Header.Clone(), body: errBody, backend: req.Backend}
			msg := fmt.Sprintf("%s answered %d: %s", req.Backend, resp.StatusCode, shortBody(errBody))
			log.Printf("req %d: %s – trying another server", req.ID, msg)
			soft := resp.StatusCode == 503 || resp.StatusCode == 429 // "busy": not counted as server error
			if soft {
				px.pool.Release(b, req, "")
			} else {
				px.pool.Release(b, req, msg)
				px.pool.failedAttempt(req.Backend)
			}
			px.pool.setSoftRetry(req, b, soft)
			px.pool.addTrail(req, fmt.Sprintf("%s: %d", label, resp.StatusCode))
			exclude[b] = true
			continue
		}
		// HTTP 200 doesn't mean success yet: check the start of the answer before sending anything
		var prefix []byte
		if resp.StatusCode < 300 && attempt < maxAttempts {
			var bad string
			var perr error
			prefix, bad, perr = peekStart(resp, func() { px.pool.setFirstByte(req) })
			if perr != nil || bad != "" {
				resp.Body.Close()
				if req.stopped.Load() {
					px.pool.Release(b, req, "")
					jsonError(w, 499, errStopped.Error())
					px.pool.Finish(req, b, 499, stoppedMsg)
					return
				}
				if ctx.Err() != nil {
					px.pool.Release(b, req, "")
					px.pool.Finish(req, b, 499, "client disconnected")
					return
				}
				if perr != nil {
					bad = "Verbindung vor der ersten Antwort abgebrochen: " + perr.Error()
				}
				lastFail = &upstreamFail{status: resp.StatusCode, header: resp.Header.Clone(), body: prefix, backend: req.Backend}
				msg := fmt.Sprintf("%s: %s", req.Backend, bad)
				log.Printf("req %d: %s – trying another server", req.ID, msg)
				px.pool.Release(b, req, msg)
				px.pool.failedAttempt(req.Backend)
				px.pool.setSoftRetry(req, b, false)
				px.pool.addTrail(req, fmt.Sprintf("%s: %d, %s", label, resp.StatusCode, shortText(bad, 60)))
				exclude[b] = true
				continue
			}
		}
		if req.Attempts > 1 {
			px.pool.addTrail(req, fmt.Sprintf("%s: %d", label, resp.StatusCode))
		}

		scan := newUsageScanner(resp.Header.Get("Content-Type"))
		if resp.StatusCode < 300 {
			scan.tags = newTagChecker(px.cfg.TagCheck)
			scan.onContent = func() {
				req.chunks.Add(1)
				req.lastChunk.Store(time.Now().UnixNano())
			}
		}
		n, copyErr := px.stream(w, resp, req, scan, prefix)
		if req.stopped.Load() {
			// tell a streaming client why the answer ends here (OpenAI clients raise this as an API error)
			if strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
				fmt.Fprintf(w, "data: {\"error\":{\"message\":%q,\"type\":\"llm_balancer_stopped\"}}\n\n", errStopped.Error())
				http.NewResponseController(w).Flush()
			}
			px.pool.setResult(req, b, scan.Finish(), nil, px.access)
			resp.Body.Close()
			px.pool.Release(b, req, "") // not the server's fault
			px.pool.Finish(req, b, 499, stoppedMsg)
			return
		}
		u := scan.Finish()
		px.pool.setResult(req, b, u, scan.TagIssues(copyErr == nil && ctx.Err() == nil), px.access)
		resp.Body.Close()
		errMsg := ""
		if copyErr != nil && ctx.Err() == nil {
			errMsg = "stream: " + copyErr.Error()
		}
		if resp.StatusCode >= 500 && errMsg == "" {
			errMsg = fmt.Sprintf("upstream status %d", resp.StatusCode)
		}
		if errMsg == "" && scan.streamErr != "" {
			errMsg = "Fehler im Stream nach Beginn der Antwort (nicht mehr umleitbar): " + shortText(scan.streamErr, 160)
		}
		if errMsg == "" && resp.StatusCode >= 400 {
			errMsg = fmt.Sprintf("Fehler an den Client weitergegeben: %d", resp.StatusCode)
		}
		if (resp.StatusCode == 401 || resp.StatusCode == 403) && errMsg == "" {
			errMsg = fmt.Sprintf("Server lehnt den Bearer-Token ab (%d)", resp.StatusCode)
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
func (px *Proxy) stream(w http.ResponseWriter, resp *http.Response, req *ReqInfo, scan *usageScanner, prefix []byte) (int64, error) {
	stripHop(resp.Header)
	copyHeaders(w.Header(), resp.Header)
	w.Header().Set("X-LLM-Balancer-Backend", req.Backend)
	if req.Attempts > 1 {
		w.Header().Set("X-LLM-Balancer-Attempts", strconv.Itoa(req.Attempts))
	}
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	var total int64
	if len(prefix) > 0 { // the part already read while checking the start of the answer
		if _, werr := w.Write(prefix); werr != nil {
			return 0, werr
		}
		total = int64(len(prefix))
		req.bytes.Add(total)
		scan.Write(prefix)
		_ = rc.Flush()
	}

	buf := make([]byte, 32*1024)
	first := len(prefix) == 0
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
			scan.Write(buf[:n])
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
	path := strings.TrimRight(r.URL.Path, "/")
	if r.Method == http.MethodGet && path == "/v1/models" && px.serveModelList(w) {
		return // models of all servers, so clients see everything the balancer can route
	}
	if r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/models/") && px.serveModel(w, strings.TrimPrefix(path, "/v1/models/")) {
		return // a single model the balancer knows (OpenAI "retrieve model")
	}
	// Requests that don't need a slot (GET …) go to one server; on an error answer the next one is tried.
	tried := map[*Backend]bool{}
	var last *upstreamFail
	var connErrs []string
	for {
		b, name := px.pool.PickPassthroughEx(tried)
		if b == nil {
			break
		}
		tried[b] = true
		out, err := px.upstream(r.Context(), r, b, body)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp, err := px.transportFor(px.pool.skipVerify(b)).RoundTrip(out)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			connErrs = append(connErrs, fmt.Sprintf("%s: %s", name, explainNetErr(err)))
			continue
		}
		if px.retryable(resp.StatusCode) {
			eb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			last = &upstreamFail{status: resp.StatusCode, header: resp.Header.Clone(), body: eb, backend: name}
			continue
		}
		defer resp.Body.Close()
		stripHop(resp.Header)
		copyHeaders(w.Header(), resp.Header)
		w.Header().Set("X-LLM-Balancer-Backend", name)
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return
	}
	switch {
	case last != nil:
		stripHop(last.header)
		last.header.Del("Content-Length")
		copyHeaders(w.Header(), last.header)
		w.Header().Set("X-LLM-Balancer-Backend", last.backend)
		w.WriteHeader(last.status)
		w.Write(last.body)
	case len(connErrs) > 0:
		jsonError(w, http.StatusBadGateway, "all servers failed: "+strings.Join(connErrs, "; "))
	default:
		jsonError(w, http.StatusServiceUnavailable, "no healthy backend")
	}
}

// healthLoop actively probes idle backends.
func (px *Proxy) healthLoop(ctx context.Context) {
	if px.cfg.HealthPath == "" {
		return
	}
	check := func() {
		for _, ht := range px.pool.idleTargets() {
			// busy servers are skipped: a single-slot server might not answer quickly, and it is obviously alive
			go func(ht healthTarget) {
				client := &http.Client{Transport: px.transportFor(ht.insecure), Timeout: 5 * time.Second}
				hreq, _ := http.NewRequest(http.MethodGet, ht.url+px.cfg.HealthPath, nil)
				if ht.key != "" {
					hreq.Header.Set("Authorization", "Bearer "+ht.key)
				}
				resp, err := client.Do(hreq)
				if err != nil {
					px.pool.SetHealth(ht.b, false, explainNetErr(err), ht.url)
					return
				}
				hbody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
				resp.Body.Close()
				if resp.StatusCode == 401 || resp.StatusCode == 403 {
					px.pool.SetHealth(ht.b, false, fmt.Sprintf("Server lehnt den Bearer-Token ab (%d) – Token prüfen", resp.StatusCode), ht.url)
				} else if resp.StatusCode >= 500 {
					px.pool.SetHealth(ht.b, false, fmt.Sprintf("health %s → %d", px.cfg.HealthPath, resp.StatusCode), ht.url)
				} else {
					px.pool.SetHealth(ht.b, true, "", ht.url)
					if px.cfg.HealthPath == "/v1/models" && resp.StatusCode == 200 {
						px.discoverModels(client, ht, hbody)
					} else {
						px.discoverModels(client, ht, nil)
					}
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

// explainNetErr adds a hint in plain words to typical connection errors (shown in the UI).
func explainNetErr(err error) string {
	msg := err.Error()
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	switch {
	case errors.As(err, &dnsErr):
		return msg + " – DNS-Auflösung im Container fehlgeschlagen. " +
			"Abhilfe: DNS_SERVERS (z. B. die IP des Routers) setzen, oder im Stack „dns:“ bzw. „extra_hosts: name:IP“ eintragen."
	case errors.As(err, &certErr):
		return msg + " – Zertifikat nicht vertrauenswürdig. Bei selbstsignierten Zertifikaten beim Server „Zertifikat nicht prüfen“ aktivieren."
	case strings.Contains(msg, "server gave HTTP response to HTTPS client"):
		return msg + " – Der Server spricht http, nicht https: URL mit http:// eintragen."
	case strings.Contains(msg, "first record does not look like a TLS handshake"):
		return msg + " – Der Server spricht http, nicht https: URL mit http:// eintragen."
	}
	return msg
}

var errStopped = errors.New("llm-balancer: request stopped by administrator")

const stoppedMsg = "vom Admin gestoppt"

// upstreamFail is an error answer of a server, kept to be passed on if no other server succeeds.
type upstreamFail struct {
	status  int
	header  http.Header
	body    []byte
	backend string
}

// retryable: which answers make the balancer try another server (and then the fallback group).
// Every 5xx, plus RETRY_STATUS (default 401, 403, 404, 408, 429: wrong server token, model not
// loaded there, timeout, overloaded). Other 4xx (400, 413, 422 …) are problems of the request itself
// and are passed on right away.
func (px *Proxy) retryable(code int) bool {
	if px.cfg.RetryAll {
		return code >= 400
	}
	return code >= 500 || slices.Contains(px.cfg.RetryStatus, code)
}

func shortText(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func shortBody(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	if s == "" {
		s = "(leer)"
	}
	return s
}

// passFailure answers the client after every possible server (incl. the fallback group) failed:
// with the last error answer of a server, or 502 listing the connection errors.
func (px *Proxy) passFailure(w http.ResponseWriter, req *ReqInfo, b *Backend, f *upstreamFail, conn []string) {
	w.Header().Set("X-LLM-Balancer-Attempts", strconv.Itoa(req.Attempts))
	if f == nil {
		msg := "all servers failed: " + strings.Join(conn, "; ")
		jsonError(w, http.StatusBadGateway, msg)
		px.pool.Finish(req, b, http.StatusBadGateway, msg)
		return
	}
	stripHop(f.header)
	f.header.Del("Content-Length")
	copyHeaders(w.Header(), f.header)
	w.Header().Set("X-LLM-Balancer-Backend", f.backend)
	w.WriteHeader(f.status)
	w.Write(f.body)
	msg := fmt.Sprintf("alle Server fehlgeschlagen, zuletzt %s: %d %s", f.backend, f.status, shortBody(f.body))
	px.pool.Finish(req, b, f.status, msg)
}
