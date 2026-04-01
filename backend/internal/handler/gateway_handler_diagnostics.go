package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

func (h *GatewayHandler) Health(c *gin.Context) {
	anthropicCompatState.mu.RLock()
	fileCount := len(anthropicCompatState.files)
	sessionCount := len(anthropicCompatState.sessions)
	userSettingsCount := len(anthropicCompatState.userSettings)
	anthropicCompatState.mu.RUnlock()

	c.JSON(http.StatusOK, gin.H{
		"status":    "ok",
		"service":   "sub2api-gateway",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"compatibility": gin.H{
			"anthropic_routes": true,
			"files":            true,
			"sessions":         true,
			"oauth_meta":       true,
			"settings":         true,
			"policy_limits":    true,
		},
		"storage": gin.H{
			"mode":               "settings_backed",
			"uploaded_files":     fileCount,
			"stored_sessions":    sessionCount,
			"stored_user_config": userSettingsCount,
		},
		"limitations": []string{
			"client-side first-party checks in official Claude Code are unaffected by server changes",
			"large compatibility payloads are persisted through the settings store and should be monitored for size",
		},
	})
}

func (h *GatewayHandler) Verify(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"service": "sub2api-gateway",
		"checks": []gin.H{
			{
				"name":   "messages-route",
				"path":   "/v1/messages",
				"status": "implemented",
			},
			{
				"name":   "count-tokens-route",
				"path":   "/v1/messages/count_tokens",
				"status": "implemented",
			},
			{
				"name":   "files-routes",
				"path":   "/v1/files",
				"status": "implemented",
			},
			{
				"name":   "sessions-routes",
				"path":   "/v1/sessions",
				"status": "implemented",
			},
			{
				"name":   "oauth-meta-routes",
				"path":   "/api/oauth/profile",
				"status": "implemented",
			},
			{
				"name":   "bootstrap-route",
				"path":   "/api/claude_cli/bootstrap",
				"status": "implemented",
			},
			{
				"name":   "policy-limits-route",
				"path":   "/api/claude_code/policy_limits",
				"status": "implemented",
			},
			{
				"name":   "first-party-gated-features",
				"status": "not-fixable-server-side",
				"details": []string{
					"policy/settings/team-memory gating inside official client",
					"eager_input_streaming enablement",
					"x-client-request-id injection tied to first-party base URL checks",
				},
			},
		},
	})
}
