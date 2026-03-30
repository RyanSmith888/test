package admin

import (
	"fmt"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// AccountHealthHandler 提供账号健康监控端点
type AccountHealthHandler struct {
	gatewayService *service.GatewayService
	adminService   service.AdminService
}

// NewAccountHealthHandler 创建账号健康监控 handler
func NewAccountHealthHandler(
	gatewayService *service.GatewayService,
	adminService service.AdminService,
) *AccountHealthHandler {
	return &AccountHealthHandler{
		gatewayService: gatewayService,
		adminService:   adminService,
	}
}

// GetAccountsHealth 获取所有账号的健康状态
// GET /api/v1/admin/accounts/health
func (h *AccountHealthHandler) GetAccountsHealth(c *gin.Context) {
	if h.gatewayService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Gateway service not available")
		return
	}

	monitor := h.gatewayService.GetHealthMonitor()
	if monitor == nil {
		response.Error(c, http.StatusServiceUnavailable, "Health monitor not initialized")
		return
	}

	// 获取所有账号（不分页，获取全部）
	accounts, _, err := h.adminService.ListAccounts(c.Request.Context(), 1, 10000, "", "", "", "", 0, "")
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "Failed to list accounts: "+err.Error())
		return
	}

	healthScores := monitor.GetAllHealthScores()

	var healthReport []gin.H
	for _, account := range accounts {
		score, exists := healthScores[account.ID]
		if !exists {
			score = &service.AccountHealthScore{
				AccountID:          account.ID,
				CurrentHealthScore: 0.5,
				AllowedConcurrency: 1,
			}
		}

		successRate := float64(0)
		total := score.SuccessCount + score.FailureCount
		if total > 0 {
			successRate = float64(score.SuccessCount) / float64(total)
		}

		healthReport = append(healthReport, gin.H{
			"account_id":          account.ID,
			"account_name":        account.Name,
			"status":              account.Status,
			"platform":            account.Platform,
			"health_score":        score.CurrentHealthScore,
			"allowed_concurrency": score.AllowedConcurrency,
			"success_count":       score.SuccessCount,
			"failure_count":       score.FailureCount,
			"success_rate":        successRate,
			"error_403_count":     score.Error403Count,
			"error_401_count":     score.Error401Count,
			"last_check_time":     score.LastCheckTime,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"total_accounts": len(accounts),
		"health_report":  healthReport,
	})
}

// GetAccountHealth 获取单个账号的健康状态
// GET /api/v1/admin/accounts/:id/health
func (h *AccountHealthHandler) GetAccountHealth(c *gin.Context) {
	if h.gatewayService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Gateway service not available")
		return
	}

	monitor := h.gatewayService.GetHealthMonitor()
	if monitor == nil {
		response.Error(c, http.StatusServiceUnavailable, "Health monitor not initialized")
		return
	}

	accountID := int64(0)
	if id, err := parseID(c.Param("id")); err == nil {
		accountID = id
	} else {
		response.BadRequest(c, "Invalid account ID")
		return
	}

	score := monitor.GetHealthScore(c.Request.Context(), accountID)

	// 获取虚拟身份信息
	vi := service.GenerateVirtualIdentity(accountID)

	successRate := float64(0)
	total := score.SuccessCount + score.FailureCount
	if total > 0 {
		successRate = float64(score.SuccessCount) / float64(total)
	}

	c.JSON(http.StatusOK, gin.H{
		"account_id":          accountID,
		"health_score":        score.CurrentHealthScore,
		"allowed_concurrency": score.AllowedConcurrency,
		"success_count":       score.SuccessCount,
		"failure_count":       score.FailureCount,
		"success_rate":        successRate,
		"error_403_count":     score.Error403Count,
		"error_401_count":     score.Error401Count,
		"last_check_time":     score.LastCheckTime,
		"virtual_identity": gin.H{
			"client_id":    vi.ClientID,
			"device_id":    vi.DeviceID,
			"user_agent":   vi.UserAgent,
			"os_type":      vi.OSType,
			"architecture": vi.Architecture,
			"runtime_ver":  vi.RuntimeVer,
		},
	})
}

func parseID(s string) (int64, error) {
	var id int64
	_, err := fmt.Sscanf(s, "%d", &id)
	return id, err
}
