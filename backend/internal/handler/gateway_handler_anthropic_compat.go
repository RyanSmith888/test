package handler

import (
	"errors"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

type compatFileRecord struct {
	ID         string
	Filename   string
	ContentType string
	Bytes      []byte
	SizeBytes  int
	CreatedAt  time.Time
}

type compatSessionEvent struct {
	ID      string         `json:"id"`
	Payload map[string]any `json:"payload"`
}

type compatSessionRecord struct {
	ID            string
	Title         string
	EnvironmentID string
	Status        string
	Source        string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	Events        []compatSessionEvent
}

type compatUserSettingsRecord struct {
	Version      int64
	LastModified time.Time
	Entries      map[string]any
}

type compatManagedSettingsRecord struct {
	UUID     string
	Checksum string
	Settings map[string]any
}

var anthropicCompatState = struct {
	mu              sync.RWMutex
	loadedFromStore bool
	files           map[string]compatFileRecord
	sessions        map[string]compatSessionRecord
	userSettings    map[string]compatUserSettingsRecord
	managedSettings compatManagedSettingsRecord
}{
	files:        make(map[string]compatFileRecord),
	sessions:     make(map[string]compatSessionRecord),
	userSettings: make(map[string]compatUserSettingsRecord),
	managedSettings: compatManagedSettingsRecord{
		UUID:     "sub2api-managed-settings",
		Checksum: compatChecksumJSON(map[string]any{}),
		Settings: map[string]any{},
	},
}

var anthropicCompatUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func (h *GatewayHandler) UploadFile(c *gin.Context) {
	h.ensureCompatStateLoaded(c.Request.Context())

	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}

	record, err := compatReadUploadedFile(c)
	if err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	record.ID = compatNewID("file")
	record.CreatedAt = time.Now().UTC()

	anthropicCompatState.mu.Lock()
	anthropicCompatState.files[record.ID] = record
	anthropicCompatState.mu.Unlock()
	h.persistCompatState(c.Request.Context())

	setOpsRequestContext(c, "", false, nil)
	setOpsEndpointContext(c, "", int16(service.RequestTypeFromLegacy(false, false)))

	c.JSON(http.StatusOK, compatFileResponse(record, apiKey))
}

func (h *GatewayHandler) ListFiles(c *gin.Context) {
	h.ensureCompatStateLoaded(c.Request.Context())

	anthropicCompatState.mu.RLock()
	files := make([]gin.H, 0, len(anthropicCompatState.files))
	for _, record := range anthropicCompatState.files {
		files = append(files, compatFileResponse(record, nil))
	}
	anthropicCompatState.mu.RUnlock()

	c.JSON(http.StatusOK, gin.H{
		"data": files,
		"has_more": false,
	})
}

func (h *GatewayHandler) GetFile(c *gin.Context) {
	h.ensureCompatStateLoaded(c.Request.Context())

	fileID := strings.TrimSpace(c.Param("file_id"))
	if fileID == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "file_id is required")
		return
	}

	anthropicCompatState.mu.RLock()
	record, ok := anthropicCompatState.files[fileID]
	anthropicCompatState.mu.RUnlock()
	if !ok {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "File not found")
		return
	}

	c.JSON(http.StatusOK, compatFileResponse(record, nil))
}

func (h *GatewayHandler) FileContent(c *gin.Context) {
	h.ensureCompatStateLoaded(c.Request.Context())

	fileID := strings.TrimSpace(c.Param("file_id"))
	if fileID == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "file_id is required")
		return
	}

	anthropicCompatState.mu.RLock()
	record, ok := anthropicCompatState.files[fileID]
	anthropicCompatState.mu.RUnlock()
	if !ok {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "File not found")
		return
	}

	if record.ContentType != "" {
		c.Header("Content-Type", record.ContentType)
	} else {
		c.Header("Content-Type", "application/octet-stream")
	}
	c.Header("Content-Disposition", fmt.Sprintf(`inline; filename="%s"`, strings.ReplaceAll(record.Filename, `"`, "")))
	c.Data(http.StatusOK, compatStringOrDefault(record.ContentType, "application/octet-stream"), record.Bytes)
}

func (h *GatewayHandler) CreateSession(c *gin.Context) {
	h.ensureCompatStateLoaded(c.Request.Context())

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		return
	}

	var req struct {
		ID            string           `json:"id"`
		Title         string           `json:"title"`
		EnvironmentID string           `json:"environment_id"`
		Source        string           `json:"source"`
		Events        []map[string]any `json:"events"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
			return
		}
	}

	now := time.Now().UTC()
	session := compatSessionRecord{
		ID:            compatChoose(req.ID, compatNewID("session")),
		Title:         compatChoose(req.Title, "New session"),
		EnvironmentID: compatChoose(req.EnvironmentID, compatNewID("env")),
		Status:        "active",
		Source:        compatChoose(req.Source, "sub2api"),
		CreatedAt:     now,
		UpdatedAt:     now,
		Events:        compatNormalizeSessionEvents(req.Events),
	}

	anthropicCompatState.mu.Lock()
	anthropicCompatState.sessions[session.ID] = session
	anthropicCompatState.mu.Unlock()
	h.persistCompatState(c.Request.Context())

	c.JSON(http.StatusOK, compatSessionResponse(session))
}

func (h *GatewayHandler) GetSession(c *gin.Context) {
	h.ensureCompatStateLoaded(c.Request.Context())

	sessionID := strings.TrimSpace(c.Param("id"))
	session, ok := compatGetSession(sessionID)
	if !ok {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "Session not found")
		return
	}
	c.JSON(http.StatusOK, compatSessionResponse(session))
}

func (h *GatewayHandler) ArchiveSession(c *gin.Context) {
	h.ensureCompatStateLoaded(c.Request.Context())

	sessionID := strings.TrimSpace(c.Param("id"))

	anthropicCompatState.mu.Lock()
	session, ok := anthropicCompatState.sessions[sessionID]
	if ok {
		session.Status = "archived"
		session.UpdatedAt = time.Now().UTC()
		anthropicCompatState.sessions[sessionID] = session
	}
	anthropicCompatState.mu.Unlock()
	if ok {
		h.persistCompatState(c.Request.Context())
	}

	if !ok {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "Session not found")
		return
	}

	c.JSON(http.StatusOK, compatSessionResponse(session))
}

func (h *GatewayHandler) SessionEvents(c *gin.Context) {
	h.ensureCompatStateLoaded(c.Request.Context())

	sessionID := strings.TrimSpace(c.Param("id"))
	session, ok := compatGetSession(sessionID)
	if !ok {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "Session not found")
		return
	}

	data := make([]map[string]any, 0, len(session.Events))
	for _, event := range session.Events {
		data = append(data, compatSessionEventResponse(event))
	}

	resp := gin.H{
		"data":     data,
		"has_more": false,
	}
	if len(session.Events) > 0 {
		resp["first_id"] = session.Events[0].ID
		resp["last_id"] = session.Events[len(session.Events)-1].ID
	}

	c.JSON(http.StatusOK, resp)
}

func (h *GatewayHandler) SessionWebSocket(c *gin.Context) {
	h.ensureCompatStateLoaded(c.Request.Context())

	sessionID := strings.TrimSpace(c.Param("id"))
	session, ok := compatGetSession(sessionID)
	if !ok {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "Session not found")
		return
	}

	conn, err := anthropicCompatUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	})

	for _, event := range session.Events {
		if err := conn.WriteJSON(compatSessionEventResponse(event)); err != nil {
			return
		}
	}

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-readDone:
			return
		case <-ticker.C:
			if err := conn.WriteJSON(gin.H{
				"type":      "ping",
				"session_id": sessionID,
				"status":    session.Status,
			}); err != nil {
				return
			}
		}
	}
}

func (h *GatewayHandler) ClaudeCLIProfile(c *gin.Context) {
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}

	user := apiKey.User
	group := apiKey.Group
	now := time.Now().UTC().Format(time.RFC3339)

	c.JSON(http.StatusOK, gin.H{
		"account": gin.H{
			"uuid":          strconv.FormatInt(apiKey.UserID, 10),
			"email_address": compatUserEmail(user),
			"display_name":  compatUserDisplayName(user),
			"created_at":    compatTimeOr(now, userCreatedAt(user)),
		},
		"organization": gin.H{
			"uuid":                   compatGroupID(group),
			"name":                   compatGroupName(group),
			"billing_type":           compatGroupBillingType(group),
			"rate_limit_tier":        "custom",
			"has_extra_usage_enabled": false,
		},
	})
}

func (h *GatewayHandler) OAuthProfile(c *gin.Context) {
	h.ClaudeCLIProfile(c)
}

func (h *GatewayHandler) OAuthUsage(c *gin.Context) {
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}

	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	resp := gin.H{
		"five_hour": compatRateLimitWindow(apiKey.RateLimit5h, apiKey.EffectiveUsage5h(), apiKey.Window5hStart, service.RateLimitWindow5h),
		"seven_day": compatRateLimitWindow(apiKey.RateLimit7d, apiKey.EffectiveUsage7d(), apiKey.Window7dStart, service.RateLimitWindow7d),
		"extra_usage": gin.H{
			"is_enabled":    false,
			"monthly_limit": nil,
			"used_credits":  nil,
			"utilization":   nil,
		},
	}
	if subscription != nil && subscription.Group != nil && subscription.Group.MonthlyLimitUSD != nil {
		limit := *subscription.Group.MonthlyLimitUSD
		utilization := compatUtilization(subscription.MonthlyUsageUSD, limit)
		resp["extra_usage"] = gin.H{
			"is_enabled":    false,
			"monthly_limit": limit,
			"used_credits":  subscription.MonthlyUsageUSD,
			"utilization":   utilization,
		}
	}

	c.JSON(http.StatusOK, resp)
}

func (h *GatewayHandler) OAuthRoles(c *gin.Context) {
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"organization_role": "member",
		"workspace_role":    "developer",
		"organization_name": compatGroupName(apiKey.Group),
	})
}

func (h *GatewayHandler) OAuthCreateAPIKey(c *gin.Context) {
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"raw_key":    apiKey.Key,
			"name":       compatStringOrDefault(apiKey.Name, "sub2api compatibility key"),
			"created_at": apiKey.CreatedAt.UTC().Format(time.RFC3339),
		},
	})
}

func (h *GatewayHandler) ClaudeBootstrap(c *gin.Context) {
	modelOptions := make([]string, 0, len(claude.DefaultModels))
	for _, model := range claude.DefaultModels {
		modelOptions = append(modelOptions, model.ID)
	}

	c.JSON(http.StatusOK, gin.H{
		"client_data": gin.H{
			"api_base_url":              c.Request.Host,
			"auth_mode":                 "api_key",
			"capabilities":              []string{"messages", "files", "sessions", "settings"},
			"managed_settings_supported": true,
			"policy_limits_supported":    true,
		},
		"additional_model_options": modelOptions,
	})
}

func (h *GatewayHandler) GetUserSettings(c *gin.Context) {
	h.ensureCompatStateLoaded(c.Request.Context())

	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}

	userKey := strconv.FormatInt(subject.UserID, 10)

	anthropicCompatState.mu.Lock()
	record, ok := anthropicCompatState.userSettings[userKey]
	if !ok {
		record = compatUserSettingsRecord{
			Version:      1,
			LastModified: time.Now().UTC(),
			Entries:      map[string]any{},
		}
		anthropicCompatState.userSettings[userKey] = record
	}
	anthropicCompatState.mu.Unlock()

	c.JSON(http.StatusOK, compatUserSettingsResponse(userKey, record))
}

func (h *GatewayHandler) PutUserSettings(c *gin.Context) {
	h.ensureCompatStateLoaded(c.Request.Context())

	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}

	var req struct {
		Entries map[string]any `json:"entries"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return
	}
	if req.Entries == nil {
		req.Entries = map[string]any{}
	}

	userKey := strconv.FormatInt(subject.UserID, 10)

	anthropicCompatState.mu.Lock()
	record, exists := anthropicCompatState.userSettings[userKey]
	if !exists {
		record.Version = 0
	}
	record.Version++
	record.LastModified = time.Now().UTC()
	record.Entries = req.Entries
	anthropicCompatState.userSettings[userKey] = record
	anthropicCompatState.mu.Unlock()
	h.persistCompatState(c.Request.Context())

	c.JSON(http.StatusOK, compatUserSettingsResponse(userKey, record))
}

func (h *GatewayHandler) ManagedSettings(c *gin.Context) {
	h.ensureCompatStateLoaded(c.Request.Context())

	anthropicCompatState.mu.RLock()
	record := anthropicCompatState.managedSettings
	anthropicCompatState.mu.RUnlock()

	c.JSON(http.StatusOK, gin.H{
		"uuid":     record.UUID,
		"checksum": record.Checksum,
		"settings": record.Settings,
	})
}

func (h *GatewayHandler) PolicyLimits(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"restrictions": gin.H{
			"settings_sync": gin.H{"allowed": true},
			"session_history": gin.H{"allowed": true},
			"file_uploads": gin.H{"allowed": true},
			"oauth_profile": gin.H{"allowed": true},
		},
	})
}

type compatPersistentState struct {
	Files           map[string]compatFileRecord         `json:"files"`
	Sessions        map[string]compatSessionRecord      `json:"sessions"`
	UserSettings    map[string]compatUserSettingsRecord `json:"user_settings"`
	ManagedSettings compatManagedSettingsRecord         `json:"managed_settings"`
}

func (h *GatewayHandler) ensureCompatStateLoaded(ctx context.Context) {
	anthropicCompatState.mu.RLock()
	alreadyLoaded := anthropicCompatState.loadedFromStore
	anthropicCompatState.mu.RUnlock()
	if alreadyLoaded || h.settingService == nil {
		return
	}

	state := compatDefaultPersistentState()
	if err := h.settingService.GetJSONSetting(ctx, service.SettingKeyClaudeCompatState, &state); err != nil {
		if !errors.Is(err, service.ErrSettingNotFound) {
			anthropicCompatState.mu.Lock()
			anthropicCompatState.loadedFromStore = true
			anthropicCompatState.mu.Unlock()
			return
		}
	}
	compatNormalizePersistentState(&state)

	anthropicCompatState.mu.Lock()
	anthropicCompatState.files = state.Files
	anthropicCompatState.sessions = state.Sessions
	anthropicCompatState.userSettings = state.UserSettings
	anthropicCompatState.managedSettings = state.ManagedSettings
	anthropicCompatState.loadedFromStore = true
	anthropicCompatState.mu.Unlock()
}

func (h *GatewayHandler) persistCompatState(ctx context.Context) {
	if h.settingService == nil {
		return
	}
	anthropicCompatState.mu.RLock()
	state := compatPersistentState{
		Files:           compatCloneFiles(anthropicCompatState.files),
		Sessions:        compatCloneSessions(anthropicCompatState.sessions),
		UserSettings:    compatCloneUserSettings(anthropicCompatState.userSettings),
		ManagedSettings: compatCloneManagedSettings(anthropicCompatState.managedSettings),
	}
	anthropicCompatState.mu.RUnlock()
	_ = h.settingService.SetJSONSetting(ctx, service.SettingKeyClaudeCompatState, state)
}

func compatReadUploadedFile(c *gin.Context) (compatFileRecord, error) {
	contentType := c.ContentType()
	if strings.HasPrefix(contentType, "multipart/") {
		file, header, err := c.Request.FormFile("file")
		if err != nil {
			return compatFileRecord{}, fmt.Errorf("file field is required")
		}
		defer file.Close()
		return compatReadMultipartFile(file, header)
	}

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return compatFileRecord{}, fmt.Errorf("failed to read request body")
	}
	if len(body) == 0 {
		return compatFileRecord{}, fmt.Errorf("file body is empty")
	}

	filename := strings.TrimSpace(c.GetHeader("x-filename"))
	if filename == "" {
		filename = "upload.bin"
	}

	return compatFileRecord{
		Filename:    filename,
		ContentType: c.GetHeader("Content-Type"),
		Bytes:       body,
		SizeBytes:   len(body),
	}, nil
}

func compatReadMultipartFile(file multipart.File, header *multipart.FileHeader) (compatFileRecord, error) {
	body, err := io.ReadAll(file)
	if err != nil {
		return compatFileRecord{}, fmt.Errorf("failed to read uploaded file")
	}
	if len(body) == 0 {
		return compatFileRecord{}, fmt.Errorf("uploaded file is empty")
	}

	return compatFileRecord{
		Filename:    header.Filename,
		ContentType: header.Header.Get("Content-Type"),
		Bytes:       body,
		SizeBytes:   len(body),
	}, nil
}

func compatGetSession(sessionID string) (compatSessionRecord, bool) {
	anthropicCompatState.mu.RLock()
	defer anthropicCompatState.mu.RUnlock()
	session, ok := anthropicCompatState.sessions[sessionID]
	return session, ok
}

func compatDefaultPersistentState() compatPersistentState {
	return compatPersistentState{
		Files:        map[string]compatFileRecord{},
		Sessions:     map[string]compatSessionRecord{},
		UserSettings: map[string]compatUserSettingsRecord{},
		ManagedSettings: compatManagedSettingsRecord{
			UUID:     "sub2api-managed-settings",
			Checksum: compatChecksumJSON(map[string]any{}),
			Settings: map[string]any{},
		},
	}
}

func compatNormalizePersistentState(state *compatPersistentState) {
	if state.Files == nil {
		state.Files = map[string]compatFileRecord{}
	}
	if state.Sessions == nil {
		state.Sessions = map[string]compatSessionRecord{}
	}
	if state.UserSettings == nil {
		state.UserSettings = map[string]compatUserSettingsRecord{}
	}
	if state.ManagedSettings.UUID == "" {
		state.ManagedSettings.UUID = "sub2api-managed-settings"
	}
	if state.ManagedSettings.Settings == nil {
		state.ManagedSettings.Settings = map[string]any{}
	}
	state.ManagedSettings.Checksum = compatChecksumJSON(state.ManagedSettings.Settings)
}

func compatNormalizeSessionEvents(raw []map[string]any) []compatSessionEvent {
	if len(raw) == 0 {
		return nil
	}
	events := make([]compatSessionEvent, 0, len(raw))
	for _, item := range raw {
		if item == nil {
			item = map[string]any{}
		}
		id, _ := item["id"].(string)
		if strings.TrimSpace(id) == "" {
			id = compatNewID("event")
		}
		events = append(events, compatSessionEvent{
			ID:      id,
			Payload: item,
		})
	}
	return events
}

func compatSessionResponse(session compatSessionRecord) gin.H {
	return gin.H{
		"id":             session.ID,
		"title":          session.Title,
		"environment_id": session.EnvironmentID,
		"status":         session.Status,
		"source":         session.Source,
		"created_at":     session.CreatedAt.Format(time.RFC3339),
		"updated_at":     session.UpdatedAt.Format(time.RFC3339),
	}
}

func compatSessionEventResponse(event compatSessionEvent) map[string]any {
	payload := make(map[string]any, len(event.Payload)+1)
	for k, v := range event.Payload {
		payload[k] = v
	}
	if _, ok := payload["id"]; !ok {
		payload["id"] = event.ID
	}
	return payload
}

func compatActorSummary(apiKey *service.APIKey) gin.H {
	return gin.H{
		"api_key_id": apiKey.ID,
		"user_id":    apiKey.UserID,
		"group_id":   apiKey.GroupID,
	}
}

func compatCloneFiles(in map[string]compatFileRecord) map[string]compatFileRecord {
	out := make(map[string]compatFileRecord, len(in))
	for key, value := range in {
		cloned := value
		if value.Bytes != nil {
			cloned.Bytes = append([]byte(nil), value.Bytes...)
		}
		out[key] = cloned
	}
	return out
}

func compatCloneSessions(in map[string]compatSessionRecord) map[string]compatSessionRecord {
	out := make(map[string]compatSessionRecord, len(in))
	for key, value := range in {
		cloned := value
		if value.Events != nil {
			cloned.Events = make([]compatSessionEvent, 0, len(value.Events))
			for _, event := range value.Events {
				payload := map[string]any{}
				for k, v := range event.Payload {
					payload[k] = v
				}
				cloned.Events = append(cloned.Events, compatSessionEvent{
					ID:      event.ID,
					Payload: payload,
				})
			}
		}
		out[key] = cloned
	}
	return out
}

func compatCloneUserSettings(in map[string]compatUserSettingsRecord) map[string]compatUserSettingsRecord {
	out := make(map[string]compatUserSettingsRecord, len(in))
	for key, value := range in {
		cloned := value
		if value.Entries != nil {
			entries := make(map[string]any, len(value.Entries))
			for k, v := range value.Entries {
				entries[k] = v
			}
			cloned.Entries = entries
		}
		out[key] = cloned
	}
	return out
}

func compatCloneManagedSettings(in compatManagedSettingsRecord) compatManagedSettingsRecord {
	out := in
	if in.Settings != nil {
		settings := make(map[string]any, len(in.Settings))
		for k, v := range in.Settings {
			settings[k] = v
		}
		out.Settings = settings
	}
	return out
}

func compatFileResponse(record compatFileRecord, apiKey *service.APIKey) gin.H {
	resp := gin.H{
		"id":           record.ID,
		"type":         "file",
		"filename":     record.Filename,
		"mime_type":    compatStringOrDefault(record.ContentType, "application/octet-stream"),
		"size_bytes":   record.SizeBytes,
		"created_at":   record.CreatedAt.Format(time.RFC3339),
		"download_url": fmt.Sprintf("/v1/files/%s/content", record.ID),
	}
	if apiKey != nil {
		resp["uploaded_by"] = compatActorSummary(apiKey)
	}
	return resp
}

func compatRateLimitWindow(limit, used float64, windowStart *time.Time, duration time.Duration) gin.H {
	if limit <= 0 {
		return gin.H{
			"utilization": nil,
			"resets_at":   nil,
		}
	}

	var resetsAt any
	if windowStart != nil && !service.IsWindowExpired(windowStart, duration) {
		resetsAt = windowStart.Add(duration).UTC().Format(time.RFC3339)
	} else {
		resetsAt = nil
	}

	return gin.H{
		"utilization": compatUtilization(used, limit),
		"resets_at":   resetsAt,
	}
}

func compatUtilization(used, limit float64) any {
	if limit <= 0 {
		return nil
	}
	value := used / limit * 100
	if value < 0 {
		value = 0
	}
	if value > 100 {
		value = 100
	}
	return value
}

func compatUserSettingsResponse(userID string, record compatUserSettingsRecord) gin.H {
	return gin.H{
		"userId":       userID,
		"version":      record.Version,
		"lastModified": record.LastModified.Format(time.RFC3339),
		"checksum":     compatChecksumJSON(record.Entries),
		"content": gin.H{
			"entries": record.Entries,
		},
	}
}

func compatChecksumJSON(v any) string {
	body, _ := json.Marshal(v)
	sum := md5.Sum(body)
	return hex.EncodeToString(sum[:])
}

func compatNewID(prefix string) string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UTC().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(buf)
}

func compatChoose(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func compatStringOrDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func compatGroupName(group *service.Group) string {
	if group == nil || strings.TrimSpace(group.Name) == "" {
		return "sub2api"
	}
	return group.Name
}

func compatGroupID(group *service.Group) string {
	if group == nil {
		return "0"
	}
	return strconv.FormatInt(group.ID, 10)
}

func compatGroupBillingType(group *service.Group) string {
	if group != nil && group.IsSubscriptionType() {
		return "subscription"
	}
	return "payg"
}

func compatUserEmail(user *service.User) string {
	if user == nil || strings.TrimSpace(user.Email) == "" {
		return ""
	}
	return user.Email
}

func compatUserDisplayName(user *service.User) string {
	if user == nil {
		return "sub2api"
	}
	if strings.TrimSpace(user.Username) != "" {
		return user.Username
	}
	if strings.TrimSpace(user.Email) != "" {
		return user.Email
	}
	return "sub2api"
}

func userCreatedAt(user *service.User) string {
	if user == nil || user.CreatedAt.IsZero() {
		return ""
	}
	return user.CreatedAt.UTC().Format(time.RFC3339)
}

func compatTimeOr(primary, fallback string) string {
	if strings.TrimSpace(fallback) != "" {
		return fallback
	}
	return primary
}
