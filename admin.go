package main

import (
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

type AdminHandler struct {
	store *Store
	pool  *Pool
	cfg   *Config
}

func NewAdminHandler(store *Store, pool *Pool, cfg *Config) *AdminHandler {
	return &AdminHandler{store: store, pool: pool, cfg: cfg}
}

func (ah *AdminHandler) RegisterRoutes(mux *http.ServeMux, adminKey string) {
	admin := http.NewServeMux()

	// Overview
	admin.HandleFunc("GET /overview", ah.overview)

	// Accounts
	admin.HandleFunc("GET /accounts", ah.listAccounts)
	admin.HandleFunc("POST /accounts", ah.createAccount)
	admin.HandleFunc("PUT /accounts/{id}", ah.updateAccount)
	admin.HandleFunc("PUT /accounts/{id}/status", ah.updateAccountStatus)
	admin.HandleFunc("PUT /accounts/{id}/token", ah.updateAccountToken)
	admin.HandleFunc("DELETE /accounts/{id}", ah.deleteAccount)
	admin.HandleFunc("POST /accounts/verify", ah.verifyToken)

	// API Keys
	admin.HandleFunc("GET /keys", ah.listKeys)
	admin.HandleFunc("POST /keys", ah.createKey)
	admin.HandleFunc("PUT /keys/{id}", ah.updateKey)
	admin.HandleFunc("DELETE /keys/{id}", ah.deleteKey)

	// Proxies
	admin.HandleFunc("GET /proxies", ah.listProxies)
	admin.HandleFunc("POST /proxies", ah.createProxy)
	admin.HandleFunc("POST /proxies/batch", ah.createProxiesBatch)
	admin.HandleFunc("DELETE /proxies/{id}", ah.deleteProxy)

	// Stats
	admin.HandleFunc("GET /stats/daily", ah.dailyStats)
	admin.HandleFunc("GET /stats/hourly", ah.hourlyStats)
	admin.HandleFunc("GET /stats/keys", ah.keyStats)
	admin.HandleFunc("GET /stats/models", ah.modelStats)
	admin.HandleFunc("GET /stats/pool", ah.poolStatus)
	admin.HandleFunc("GET /stats/logs", ah.recentLogs)

	// Ops
	admin.HandleFunc("POST /ops/reload", ah.reloadPool)
	admin.HandleFunc("POST /ops/purge-logs", ah.purgeLogs)

	mux.Handle("/admin/", AdminAuthMiddleware(adminKey)(http.StripPrefix("/admin", admin)))

	// Embedded web UI
	webSub, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServer(http.FS(webSub)))
}

// ============================================================================
// Overview
// ============================================================================

func (ah *AdminHandler) overview(w http.ResponseWriter, r *http.Request) {
	stats, err := ah.store.GetOverviewStats()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, stats)
}

// ============================================================================
// Accounts
// ============================================================================

func (ah *AdminHandler) listAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := ah.store.ListAccounts()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	for i := range accounts {
		accounts[i].Token = maskToken(accounts[i].Token)
	}
	if accounts == nil {
		accounts = []Account{}
	}
	writeJSON(w, 200, accounts)
}

func (ah *AdminHandler) createAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string `json:"name"`
		Token        string `json:"token"`
		Fingerprint  string `json:"fingerprint"`
		RefreshToken string `json:"refresh_token"`
		RPM          int    `json:"rpm"`
		MaxConcur    int    `json:"max_concur"`
		TokenExpiry  int64  `json:"token_expiry"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "invalid json body")
		return
	}
	if req.Name == "" || req.Token == "" {
		writeError(w, 400, "name and token are required")
		return
	}
	if req.RPM <= 0 {
		req.RPM = ah.cfg.DefaultRPM
	}
	if req.MaxConcur <= 0 {
		req.MaxConcur = ah.cfg.DefaultMaxConcur
	}

	id, err := ah.store.CreateAccount(req.Name, req.Token, req.Fingerprint, req.RefreshToken, req.RPM, req.MaxConcur, req.TokenExpiry)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	ah.pool.Reload()
	logInfo("created account %d [%s]", id, req.Name)
	writeJSON(w, 201, map[string]int64{"id": id})
}

func (ah *AdminHandler) updateAccount(w http.ResponseWriter, r *http.Request) {
	id := pathInt64(r, "id")
	if id == 0 {
		writeError(w, 400, "invalid id")
		return
	}
	var req struct {
		Name         *string `json:"name"`
		Fingerprint  *string `json:"fingerprint"`
		RefreshToken *string `json:"refresh_token"`
		RPM          *int    `json:"rpm"`
		MaxConcur    *int    `json:"max_concur"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "invalid json")
		return
	}

	acct, err := ah.store.GetAccount(id)
	if err != nil {
		writeError(w, 404, "account not found")
		return
	}

	name := acct.Name
	fp := acct.Fingerprint
	rpm := acct.RPM
	mc := acct.MaxConcur
	if req.Name != nil {
		name = *req.Name
	}
	if req.Fingerprint != nil {
		fp = *req.Fingerprint
	}
	if req.RPM != nil {
		rpm = *req.RPM
	}
	if req.MaxConcur != nil {
		mc = *req.MaxConcur
	}

	if err := ah.store.UpdateAccount(id, name, fp, rpm, mc); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	// Update refresh_token if provided
	if req.RefreshToken != nil {
		ah.store.UpdateAccountRefreshToken(id, *req.RefreshToken)
	}
	ah.pool.Reload()
	writeJSON(w, 200, map[string]string{"status": "updated"})
}

func (ah *AdminHandler) updateAccountStatus(w http.ResponseWriter, r *http.Request) {
	id := pathInt64(r, "id")
	var req struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Status == "" {
		writeError(w, 400, "status is required")
		return
	}
	if err := ah.store.UpdateAccountStatus(id, req.Status); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	ah.pool.Reload()
	writeJSON(w, 200, map[string]string{"status": "updated"})
}

func (ah *AdminHandler) updateAccountToken(w http.ResponseWriter, r *http.Request) {
	id := pathInt64(r, "id")
	var req struct {
		Token       string `json:"token"`
		TokenExpiry int64  `json:"token_expiry"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Token == "" {
		writeError(w, 400, "token is required")
		return
	}
	if err := ah.store.UpdateAccountToken(id, req.Token, req.TokenExpiry); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	ah.pool.Reload()
	logInfo("token updated for account %d", id)
	writeJSON(w, 200, map[string]string{"status": "updated"})
}

func (ah *AdminHandler) deleteAccount(w http.ResponseWriter, r *http.Request) {
	id := pathInt64(r, "id")
	if err := ah.store.DeleteAccount(id); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	ah.pool.Reload()
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

func (ah *AdminHandler) verifyToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		writeJSON(w, 400, map[string]any{"error": "token required"})
		return
	}

	testBody := `{"model":"claude-haiku-4-5-20251001","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequest("POST", ah.cfg.UpstreamURL+"/v1/messages", strings.NewReader(testBody))
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")

	if isOAuthToken(body.Token) {
		req.Header.Set("Authorization", "Bearer "+body.Token)
		req.Header.Set("anthropic-beta", "claude-code-20250219,oauth-2025-04-20")
	} else {
		req.Header.Set("x-api-key", body.Token)
		req.Header.Set("Authorization", "Bearer "+body.Token)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, 200, map[string]any{"valid": false, "error": "连接失败: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(resp.Body)

	valid := resp.StatusCode == 200 || resp.StatusCode == 400 || resp.StatusCode == 529

	tokenType := "API Key"
	if strings.HasPrefix(body.Token, "sk-ant-sid02-") {
		tokenType = "Session Token (sid02)"
	} else if strings.HasPrefix(body.Token, "sk-ant-oat01-") {
		tokenType = "OAuth Token (oat01)"
	}

	result := map[string]any{
		"valid":       valid,
		"status_code": resp.StatusCode,
		"token_type":  tokenType,
	}
	if !valid {
		result["error"] = "Token 被 Anthropic 拒绝 (HTTP " + strconv.Itoa(resp.StatusCode) + "): " + string(rawBody)
	}
	writeJSON(w, 200, result)
}

// ============================================================================
// API Keys
// ============================================================================

func (ah *AdminHandler) listKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := ah.store.ListAPIKeys()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if keys == nil {
		keys = []APIKey{}
	}
	writeJSON(w, 200, keys)
}

func (ah *AdminHandler) createKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string `json:"name"`
		DailyLimit int    `json:"daily_limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "invalid json")
		return
	}
	if req.DailyLimit <= 0 {
		req.DailyLimit = 1000
	}

	key := "sk-gw-" + generateRandomKey(24)
	id, err := ah.store.CreateAPIKey(key, req.Name, req.DailyLimit)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	logInfo("created api key %d [%s]", id, req.Name)
	writeJSON(w, 201, map[string]any{"id": id, "key": key})
}

func (ah *AdminHandler) updateKey(w http.ResponseWriter, r *http.Request) {
	id := pathInt64(r, "id")
	var req struct {
		Enabled    *int    `json:"enabled"`
		DailyLimit *int    `json:"daily_limit"`
		Name       *string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "invalid json")
		return
	}

	// Get current values
	keys, _ := ah.store.ListAPIKeys()
	var current *APIKey
	for _, k := range keys {
		if k.ID == id {
			current = &k
			break
		}
	}
	if current == nil {
		writeError(w, 404, "key not found")
		return
	}

	enabled := current.Enabled
	dailyLimit := current.DailyLimit
	name := current.Name
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	if req.DailyLimit != nil {
		dailyLimit = *req.DailyLimit
	}
	if req.Name != nil {
		name = *req.Name
	}

	if err := ah.store.UpdateAPIKey(id, enabled, dailyLimit, name); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "updated"})
}

func (ah *AdminHandler) deleteKey(w http.ResponseWriter, r *http.Request) {
	id := pathInt64(r, "id")
	if err := ah.store.DeleteAPIKey(id); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// ============================================================================
// Proxies
// ============================================================================

func (ah *AdminHandler) listProxies(w http.ResponseWriter, r *http.Request) {
	proxies, err := ah.store.ListProxies()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if proxies == nil {
		proxies = []Proxy{}
	}
	writeJSON(w, 200, proxies)
}

func (ah *AdminHandler) createProxy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL  string `json:"url"`
		Type string `json:"type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" {
		writeError(w, 400, "url is required")
		return
	}
	if req.Type == "" {
		req.Type = "socks5"
	}
	id, err := ah.store.CreateProxy(req.URL, req.Type)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]int64{"id": id})
}

func (ah *AdminHandler) createProxiesBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URLs string `json:"urls"`
		Type string `json:"type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "invalid json")
		return
	}
	if req.Type == "" {
		req.Type = "socks5"
	}
	urls := splitAndTrim(req.URLs, "\n")
	if len(urls) == 0 {
		writeError(w, 400, "no urls provided")
		return
	}
	count, err := ah.store.CreateProxiesBatch(urls, req.Type)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]int{"created": count})
}

func (ah *AdminHandler) deleteProxy(w http.ResponseWriter, r *http.Request) {
	id := pathInt64(r, "id")
	if err := ah.store.DeleteProxy(id); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	ah.pool.Reload()
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// ============================================================================
// Statistics
// ============================================================================

func (ah *AdminHandler) dailyStats(w http.ResponseWriter, r *http.Request) {
	days := queryInt(r, "days", 30)
	stats, err := ah.store.GetDailyStats(days)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if stats == nil {
		stats = []DailyStat{}
	}
	writeJSON(w, 200, stats)
}

func (ah *AdminHandler) hourlyStats(w http.ResponseWriter, r *http.Request) {
	hours := queryInt(r, "hours", 48)
	stats, err := ah.store.GetHourlyStats(hours)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if stats == nil {
		stats = []HourlyStat{}
	}
	writeJSON(w, 200, stats)
}

func (ah *AdminHandler) keyStats(w http.ResponseWriter, r *http.Request) {
	days := queryInt(r, "days", 30)
	stats, err := ah.store.GetKeyStats(days)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if stats == nil {
		stats = []KeyStat{}
	}
	writeJSON(w, 200, stats)
}

func (ah *AdminHandler) modelStats(w http.ResponseWriter, r *http.Request) {
	days := queryInt(r, "days", 30)
	stats, err := ah.store.GetModelStats(days)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if stats == nil {
		stats = []ModelStat{}
	}
	writeJSON(w, 200, stats)
}

func (ah *AdminHandler) poolStatus(w http.ResponseWriter, r *http.Request) {
	states := ah.pool.GetStates()
	if states == nil {
		states = []map[string]any{}
	}
	writeJSON(w, 200, states)
}

func (ah *AdminHandler) recentLogs(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 100)
	if limit > 500 {
		limit = 500
	}
	logs, err := ah.store.GetRecentLogs(limit)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if logs == nil {
		logs = []map[string]any{}
	}
	writeJSON(w, 200, logs)
}

// ============================================================================
// Ops
// ============================================================================

func (ah *AdminHandler) reloadPool(w http.ResponseWriter, r *http.Request) {
	if err := ah.pool.Reload(); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"status":   "reloaded",
		"accounts": ah.pool.AccountCount(),
	})
}

func (ah *AdminHandler) purgeLogs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Days int `json:"days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Days < 1 {
		req.Days = 90
	}
	deleted, err := ah.store.PurgeOldLogs(req.Days)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"deleted":        deleted,
		"older_than_days": req.Days,
	})
}

// ============================================================================
// Helpers
// ============================================================================

func pathInt64(r *http.Request, name string) int64 {
	n, _ := strconv.ParseInt(r.PathValue(name), 10, 64)
	return n
}

func queryInt(r *http.Request, name string, fallback int) int {
	if v := r.URL.Query().Get(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

// HealthHandler serves /health for monitoring.
func HealthHandler(store *Store, pool *Pool, startedAt time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"status":   "ok",
			"uptime_s": int(time.Since(startedAt).Seconds()),
			"accounts": pool.AccountCount(),
		})
	}
}
