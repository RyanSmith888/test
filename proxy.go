package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

// ============================================================================
// Header Rules
// ============================================================================

var preserveHeaders = map[string]bool{
	"anthropic-version": true,
	"anthropic-beta":    true,
	"user-agent":        true,
	"content-type":      true,
	"content-length":    true,
	"accept":            true,
	"accept-encoding":   true,
}

var stripHeaders = map[string]bool{
	"x-api-key":       true,
	"x-forwarded-for": true,
	"x-real-ip":       true,
	"via":             true,
	"authorization":   true,
	"host":            true,
	"connection":      true,
	"session-id":      true,
}

// ============================================================================
// ProxyHandler: core reverse proxy with retry + failover
// ============================================================================

type ProxyHandler struct {
	store       *Store
	pool        *Pool
	cfg         *Config
	transports  sync.Map // proxyURL -> *http.Transport (cached)
}

func NewProxyHandler(store *Store, pool *Pool, cfg *Config) *ProxyHandler {
	return &ProxyHandler{
		store: store,
		pool:  pool,
		cfg:   cfg,
	}
}

func (ph *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	keyID := ctxAPIKeyID(ctx)
	startTime := ctxStartTime(ctx)
	as := ctxAccountState(ctx)

	if as == nil {
		writeError(w, 503, "no account assigned")
		return
	}

	// Read request body for potential retry
	var bodyBytes []byte
	if r.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			writeError(w, 400, "failed to read request body")
			return
		}
	}

	// Extract model from body for logging
	model := extractModel(bodyBytes)

	// Retry loop with silent failover
	excludeIDs := make(map[int64]bool)
	maxAttempts := ph.cfg.MaxRetryAttempts

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			// Pick a different account for retry
			ph.pool.Release(as)
			var err error
			as, err = ph.pool.PickExcluding(r.Header.Get("session-id"), excludeIDs)
			if err != nil {
				logWarn("retry %d: no more healthy accounts", attempt)
				break
			}
			ph.pool.Acquire(as)
			logInfo("retry %d: switching to account %d [%s]", attempt, as.Account.ID, as.Account.Name)
		}

		statusCode, done := ph.doUpstreamRequest(w, r, as, bodyBytes, keyID, model, startTime)
		if done {
			// Success (or client-visible response already written)
			ph.pool.Release(as)
			return
		}

		// Mark failed account
		excludeIDs[as.Account.ID] = true

		// Only retry on server errors or rate limits, not on 4xx client errors
		if statusCode > 0 && statusCode < 500 && statusCode != 429 {
			break
		}
	}

	// All retries exhausted
	ph.pool.Release(as)
	ph.pushLog(keyID, as.Account.ID, model, r.URL.Path, 502, startTime)
	writeError(w, 502, "all upstream accounts exhausted")
}

// doUpstreamRequest makes one attempt. Returns (statusCode, done).
// done=true means a response has been written to w (success or upstream error forwarded).
func (ph *ProxyHandler) doUpstreamRequest(
	w http.ResponseWriter, r *http.Request,
	as *AccountState, body []byte,
	keyID int64, model string, startTime time.Time,
) (int, bool) {

	accountID := as.Account.ID

	// Build upstream URL
	targetURL := ph.cfg.UpstreamURL + r.URL.Path
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	// Create upstream request
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bodyReader)
	if err != nil {
		logError("create upstream request: %v", err)
		return 0, false
	}

	// Clean & copy headers
	for key, vals := range r.Header {
		lower := strings.ToLower(key)
		if stripHeaders[lower] {
			continue
		}
		if preserveHeaders[lower] || strings.HasPrefix(lower, "x-stainless-") {
			for _, v := range vals {
				upReq.Header.Add(key, v)
			}
		}
	}

	// Set account authorization
	upReq.Header.Set("Authorization", "Bearer "+as.Account.Token)

	// 如果账号有录入指纹（原始设备 User-Agent），用它覆盖 UA
	if as.Account.Fingerprint != "" {
		upReq.Header.Set("User-Agent", as.Account.Fingerprint)
	}

	// Get or create transport for this proxy
	transport := ph.getTransport(as.ProxyURL)

	client := &http.Client{
		Transport: transport,
		Timeout:   ph.cfg.WriteTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Do(upReq)
	if err != nil {
		logError("upstream request to account %d failed: %v", accountID, err)
		ph.pool.MarkCooldown(as, 30*time.Second)
		return 0, false // retry
	}
	defer resp.Body.Close()

	// Handle rate limiting
	if resp.StatusCode == 429 {
		logWarn("account %d got 429, cooling down 60s", accountID)
		ph.pool.MarkCooldown(as, 60*time.Second)
		return 429, false // retry with different account
	}

	// Handle server errors (5xx) - retry
	if resp.StatusCode >= 500 {
		logWarn("account %d returned %d", accountID, resp.StatusCode)
		ph.pool.MarkCooldown(as, 15*time.Second)
		return resp.StatusCode, false
	}

	// Success or client error: forward response as-is
	ph.store.IncrementAccountReqs(accountID)
	ph.store.IncrementKeyReqs(keyID)

	// Copy response headers
	for key, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Stream the body
	ph.streamResponse(w, resp)

	// Log
	ph.pushLog(keyID, accountID, model, r.URL.Path, resp.StatusCode, startTime)

	return resp.StatusCode, true
}

// ============================================================================
// SSE Streaming (bufio, no fixed-size buffer truncation)
// ============================================================================

func (ph *ProxyHandler) streamResponse(w http.ResponseWriter, resp *http.Response) {
	flusher, canFlush := w.(http.Flusher)
	isSSE := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")

	if isSSE {
		reader := bufio.NewReaderSize(resp.Body, 64*1024)
		for {
			line, err := reader.ReadBytes('\n')
			if len(line) > 0 {
				if _, werr := w.Write(line); werr != nil {
					return
				}
				if canFlush {
					flusher.Flush()
				}
			}
			if err != nil {
				if err != io.EOF {
					logDebug("SSE stream ended: %v", err)
				}
				return
			}
		}
	} else {
		buf := make([]byte, 64*1024)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				if canFlush {
					flusher.Flush()
				}
			}
			if err != nil {
				return
			}
		}
	}
}

// ============================================================================
// Transport Management (cached per proxy URL)
// ============================================================================

func (ph *ProxyHandler) getTransport(proxyURL string) http.RoundTripper {
	key := proxyURL
	if key == "" {
		key = "__direct__"
	}

	if t, ok := ph.transports.Load(key); ok {
		return t.(*http.Transport)
	}

	var transport *http.Transport
	if proxyURL == "" {
		transport = &http.Transport{
			MaxIdleConns:        200,
			MaxIdleConnsPerHost: 50,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			ForceAttemptHTTP2:   true,
			DisableCompression:  false,
		}
	} else {
		u, err := url.Parse(proxyURL)
		if err != nil {
			logWarn("invalid proxy URL %q, falling back to direct", proxyURL)
			return ph.getTransport("")
		}

		switch u.Scheme {
		case "socks5", "socks5h":
			transport = &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					return dialViaSocks5(proxyURL, addr)
				},
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 15 * time.Second,
				ForceAttemptHTTP2:   true,
			}
		case "http", "https":
			transport = &http.Transport{
				Proxy:               http.ProxyURL(u),
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 15 * time.Second,
				ForceAttemptHTTP2:   false, // HTTP 代理不支持 H2
			}
		default:
			logWarn("unsupported proxy scheme %q, falling back to direct", u.Scheme)
			return ph.getTransport("")
		}
	}

	ph.transports.Store(key, transport)
	return transport
}

func dialViaSocks5(proxyAddr, target string) (net.Conn, error) {
	u, err := url.Parse(proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("parse proxy url: %w", err)
	}

	var auth *proxy.Auth
	if u.User != nil {
		pass, _ := u.User.Password()
		auth = &proxy.Auth{
			User:     u.User.Username(),
			Password: pass,
		}
	}

	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":1080"
	}

	// SOCKS5 with remote DNS resolution
	dialer, err := proxy.SOCKS5("tcp", host, auth, &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("socks5 dialer: %w", err)
	}

	return dialer.Dial("tcp", target)
}

// ============================================================================
// Helpers
// ============================================================================

func extractModel(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var req struct {
		Model string `json:"model"`
	}
	json.Unmarshal(body, &req)
	return req.Model
}

func (ph *ProxyHandler) pushLog(keyID, accountID int64, model, path string, status int, startTime time.Time) {
	ph.store.PushLog(LogEntry{
		APIKeyID:  keyID,
		AccountID: accountID,
		Model:     model,
		Path:      path,
		Status:    status,
		Latency:   time.Since(startTime).Milliseconds(),
		CreatedAt: time.Now().UTC(),
	})
}
