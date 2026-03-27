package main

import (
	"embed"
	"encoding/json"
	"fmt"
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
	admin.HandleFunc("GET /accounts/{id}/usage", ah.getAccountUsage)
	admin.HandleFunc("POST /accounts/{id}/test", ah.testAccountUpstream)

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
		PlanType     *string `json:"plan_type"`
		PlanDisplay  *string `json:"plan_display"`
		Email        *string `json:"email"`
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
	if req.RefreshToken != nil {
		ah.store.UpdateAccountRefreshToken(id, *req.RefreshToken)
	}
	// Update plan info if provided
	if req.PlanType != nil || req.PlanDisplay != nil || req.Email != nil {
		pt, pd, em := acct.PlanType, acct.PlanDisplay, acct.Email
		if req.PlanType != nil { pt = *req.PlanType }
		if req.PlanDisplay != nil { pd = *req.PlanDisplay }
		if req.Email != nil { em = *req.Email }
		ah.store.UpdateAccountPlan(id, pt, pd, em)
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

	if !isOAuthToken(body.Token) {
		// API Key — 简单验证
		writeJSON(w, 200, map[string]any{
			"valid":        true,
			"account_type": "apikey",
			"plan_display": "API Key",
			"token_type":   "API Key (api03)",
		})
		return
	}

	info, err := fetchClaudeAccountInfo(body.Token)
	if err != nil {
		writeJSON(w, 200, map[string]any{"valid": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, info)
}

func (ah *AdminHandler) getAccountUsage(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	account, err := ah.store.GetAccount(id)
	if err != nil || account == nil {
		writeJSON(w, 404, map[string]any{"error": "not found"})
		return
	}
	if !isOAuthToken(account.Token) {
		writeJSON(w, 200, map[string]any{"supported": false})
		return
	}
	info, err := fetchClaudeAccountInfo(account.Token)
	if err != nil {
		writeJSON(w, 200, map[string]any{"supported": true, "error": err.Error()})
		return
	}
	info["supported"] = true
	writeJSON(w, 200, info)
}

func (ah *AdminHandler) testAccountUpstream(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	account, err := ah.store.GetAccount(id)
	if err != nil || account == nil {
		writeJSON(w, 404, map[string]any{"error": "not found"})
		return
	}

	client := &http.Client{Timeout: 30 * time.Second}
	testBody := `{"model":"claude-haiku-4-5-20251001","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`

	// 根据类型选端点
	baseURL := ah.cfg.UpstreamURL
	if isOAuthToken(account.Token) {
		baseURL = oauthUpstreamURL
	}
	targetURL := baseURL + "/v1/messages"

	req, _ := http.NewRequest("POST", targetURL, strings.NewReader(testBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Authorization", "Bearer "+account.Token)
	if isOAuthToken(account.Token) {
		req.Header.Set("anthropic-beta", "claude-code-20250219,oauth-2025-04-20")
	}

	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, 200, map[string]any{
			"target_url": targetURL,
			"error":      err.Error(),
			"token_type": account.AccountType,
		})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var parsed any
	json.Unmarshal(body, &parsed)

	writeJSON(w, 200, map[string]any{
		"target_url":  targetURL,
		"status_code": resp.StatusCode,
		"token_type":  account.AccountType,
		"headers":     resp.Header,
		"body":        parsed,
		"body_raw":    string(body),
	})
}

func fetchClaudeAccountInfo(token string) (map[string]any, error) {
	client := &http.Client{Timeout: 15 * time.Second}

	// 1. 查用量
	usageReq, _ := http.NewRequest("GET", "https://api.anthropic.com/api/oauth/usage", nil)
	usageReq.Header.Set("Authorization", "Bearer "+token)
	usageReq.Header.Set("anthropic-beta", "oauth-2025-04-20")
	usageReq.Header.Set("User-Agent", "claude-code/2.0 (external, cli)")

	usageResp, err := client.Do(usageReq)
	if err != nil {
		return nil, fmt.Errorf("用量查询失败: %v", err)
	}
	defer usageResp.Body.Close()
	usageBody, _ := io.ReadAll(usageResp.Body)

	if usageResp.StatusCode == 401 || usageResp.StatusCode == 403 {
		return nil, fmt.Errorf("token 无效或已过期 (HTTP %d): %s", usageResp.StatusCode, string(usageBody))
	}

	var usage map[string]any
	json.Unmarshal(usageBody, &usage)

	// 2. 查账号信息
	profileReq, _ := http.NewRequest("GET", "https://api.claude.ai/api/auth/profile", nil)
	profileReq.Header.Set("Authorization", "Bearer "+token)
	profileReq.Header.Set("anthropic-beta", "oauth-2025-04-20,claude-code-20250219")

	profileResp, err := client.Do(profileReq)
	var profile map[string]any
	if err == nil && profileResp.StatusCode == 200 {
		defer profileResp.Body.Close()
		profileBody, _ := io.ReadAll(profileResp.Body)
		json.Unmarshal(profileBody, &profile)
	}

	// 3. 解析订阅类型
	planType := "unknown"
	hasClaudeCode := true // OAuth token 默认认为有 Claude Code 资格
	if profile != nil {
		if account, ok := profile["account"].(map[string]any); ok {
			if plan, ok := account["plan_type"].(string); ok {
				planType = plan
			}
		}
		if planType == "claude_pro" || planType == "claude_max_5x" ||
			planType == "claude_max_20x" || planType == "claude_team" ||
			planType == "claude_enterprise" || strings.Contains(planType, "max") ||
			strings.Contains(planType, "pro") || strings.Contains(planType, "team") {
			hasClaudeCode = true
		}
	}
	if usageResp.StatusCode == 200 {
		hasClaudeCode = true
	}

	planDisplay := map[string]string{
		"claude_pro":        "Pro (5x)",
		"claude_max_5x":     "Max (5x)",
		"claude_max_20x":    "Max (20x)",
		"claude_team":       "Team",
		"claude_enterprise": "Enterprise",
	}
	displayName := planDisplay[planType]
	if displayName == "" {
		displayName = planType
	}

	// OAuth token: 只要不是 401/403 就认为有效
	isValid := usageResp.StatusCode != 401 && usageResp.StatusCode != 403

	result := map[string]any{
		"valid":           isValid,
		"has_claude_code": hasClaudeCode,
		"usage_status":    usageResp.StatusCode,
		"account_type":    "oauth",
		"plan_type":       planType,
		"plan_display":    displayName,
	}

	if fiveHour, ok := usage["five_hour"].(map[string]any); ok {
		result["five_hour_utilization"] = fiveHour["utilization"]
		result["five_hour_resets_at"] = fiveHour["resets_at"]
	}
	if sevenDay, ok := usage["seven_day"].(map[string]any); ok {
		result["seven_day_utilization"] = sevenDay["utilization"]
		result["seven_day_resets_at"] = sevenDay["resets_at"]
	}

	if profile != nil {
		if email, ok := profile["email"].(string); ok {
			result["email"] = email
		}
	}

	if !hasClaudeCode {
		result["error"] = "该账号没有 Claude Code 资格（需要 Pro 或 Max 订阅）"
	}

	return result, nil
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
