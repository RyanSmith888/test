package service

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// AccountHealthScore 账号健康分数据
type AccountHealthScore struct {
	AccountID          int64     `json:"account_id"`
	SuccessCount       int64     `json:"success_count"`
	FailureCount       int64     `json:"failure_count"`
	Error403Count      int64     `json:"error_403_count"`
	Error401Count      int64     `json:"error_401_count"`
	LastCheckTime      time.Time `json:"last_check_time"`
	CurrentHealthScore float64   `json:"current_health_score"` // 0-1 范围
	AllowedConcurrency int       `json:"allowed_concurrency"`  // 动态调整的并发数
}

// AccountSessionBinding 账号会话绑定
type AccountSessionBinding struct {
	AccountID      int64     `json:"account_id"`
	FixedSessionID string    `json:"fixed_session_id"`
	LastUsedTime   time.Time `json:"last_used_time"`
	RequestCount   int64     `json:"request_count"`
}

// AccountRateLimit 账号级别的速率限制
type AccountRateLimit struct {
	AccountID         int64     `json:"account_id"`
	MaxRequestsPerSec float64   `json:"max_requests_per_sec"`
	MaxTokensPerMin   int64     `json:"max_tokens_per_min"`
	LastResetTime     time.Time `json:"last_reset_time"`
	RequestCount      int       `json:"request_count"`
	TokenCount        int64     `json:"token_count"`
}

// AccountHealthMonitor 账号健康监控服务
// 跟踪每个账号的错误率和健康分，动态调整并发限制
type AccountHealthMonitor struct {
	mu           sync.RWMutex
	healthScores map[int64]*AccountHealthScore
	errorCounts  map[int64]*errorWindowCounter
	rateLimits   map[int64]*AccountRateLimit
	bindings     map[int64]*AccountSessionBinding
}

// errorWindowCounter 滑动窗口错误计数器
type errorWindowCounter struct {
	mu        sync.Mutex
	counts    []errorEntry
	windowDur time.Duration
}

type errorEntry struct {
	timestamp  time.Time
	statusCode int
}

// NewAccountHealthMonitor 创建新的账号健康监控服务
func NewAccountHealthMonitor() *AccountHealthMonitor {
	return &AccountHealthMonitor{
		healthScores: make(map[int64]*AccountHealthScore),
		errorCounts:  make(map[int64]*errorWindowCounter),
		rateLimits:   make(map[int64]*AccountRateLimit),
		bindings:     make(map[int64]*AccountSessionBinding),
	}
}

// RecordSuccess 记录成功请求
func (m *AccountHealthMonitor) RecordSuccess(accountID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	score := m.getOrCreateHealthScore(accountID)
	score.SuccessCount++
}

// RecordError 记录错误请求
func (m *AccountHealthMonitor) RecordError(accountID int64, statusCode int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	score := m.getOrCreateHealthScore(accountID)
	score.FailureCount++

	if statusCode == 403 {
		score.Error403Count++
	}
	if statusCode == 401 {
		score.Error401Count++
	}

	// 记录到滑动窗口
	counter := m.getOrCreateErrorCounter(accountID)
	counter.mu.Lock()
	counter.counts = append(counter.counts, errorEntry{
		timestamp:  time.Now(),
		statusCode: statusCode,
	})
	counter.mu.Unlock()
}

// RecordErrorPattern 监控账号的错误模式
// 如果 10 分钟内出现 5 次 403 错误，返回 true 表示高风险
func (m *AccountHealthMonitor) RecordErrorPattern(accountID int64, statusCode int) bool {
	if statusCode != 401 && statusCode != 403 {
		return false
	}

	m.RecordError(accountID, statusCode)

	// 检查 10 分钟内的 403 错误数量
	counter := m.getOrCreateErrorCounter(accountID)
	counter.mu.Lock()
	defer counter.mu.Unlock()

	now := time.Now()
	windowStart := now.Add(-10 * time.Minute)

	count := 0
	for _, entry := range counter.counts {
		if entry.timestamp.After(windowStart) && entry.statusCode == statusCode {
			count++
		}
	}

	// 清理过期条目
	m.cleanupExpiredEntries(counter, windowStart)

	if count >= 5 {
		slog.Warn("account_high_error_rate",
			"account_id", accountID,
			"status_code", statusCode,
			"count_in_10min", count,
		)
		return true
	}

	return false
}

// GetHealthScore 获取账号的健康分
func (m *AccountHealthMonitor) GetHealthScore(_ context.Context, accountID int64) *AccountHealthScore {
	m.mu.RLock()
	defer m.mu.RUnlock()

	score, exists := m.healthScores[accountID]
	if !exists {
		return &AccountHealthScore{
			AccountID:          accountID,
			CurrentHealthScore: 0.5,
			AllowedConcurrency: 1,
		}
	}

	// 每 5 分钟重新计算一次
	if time.Since(score.LastCheckTime) > 5*time.Minute {
		m.recalculateHealthScore(score)
	}

	return score
}

// GetDynamicConcurrency 获取账号的动态并发限制
func (m *AccountHealthMonitor) GetDynamicConcurrency(_ context.Context, accountID int64) int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	score, exists := m.healthScores[accountID]
	if !exists {
		return 1
	}

	return score.AllowedConcurrency
}

// GetAllHealthScores 获取所有账号的健康分（用于监控面板）
func (m *AccountHealthMonitor) GetAllHealthScores() map[int64]*AccountHealthScore {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[int64]*AccountHealthScore, len(m.healthScores))
	for id, score := range m.healthScores {
		copied := *score
		result[id] = &copied
	}
	return result
}

// CheckAccountRateLimit 检查账号级速率限制
func (m *AccountHealthMonitor) CheckAccountRateLimit(accountID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	limit, exists := m.rateLimits[accountID]
	if !exists {
		limit = &AccountRateLimit{
			AccountID:     accountID,
			LastResetTime: time.Now(),
		}
		m.rateLimits[accountID] = limit
	}

	now := time.Now()

	// 每秒重置计数器
	if now.Sub(limit.LastResetTime) >= time.Second {
		limit.RequestCount = 0
		limit.LastResetTime = now
	}

	limit.RequestCount++

	// 根据健康分确定允许的速率
	maxReq := m.calculateMaxRequestsPerSec(accountID)
	if float64(limit.RequestCount) > maxReq {
		return fmt.Errorf("account rate limit exceeded: %d > %.2f req/s",
			limit.RequestCount, maxReq)
	}

	return nil
}

// GetOrCreateSessionBinding 获取或创建账号的会话绑定
func (m *AccountHealthMonitor) GetOrCreateSessionBinding(accountID int64) *AccountSessionBinding {
	m.mu.Lock()
	defer m.mu.Unlock()

	binding, exists := m.bindings[accountID]
	if exists {
		binding.LastUsedTime = time.Now()
		binding.RequestCount++
		return binding
	}

	binding = &AccountSessionBinding{
		AccountID:      accountID,
		FixedSessionID: generateSessionIDForAccount(accountID),
		LastUsedTime:   time.Now(),
		RequestCount:   1,
	}
	m.bindings[accountID] = binding
	return binding
}

// --- internal helpers ---

func (m *AccountHealthMonitor) getOrCreateHealthScore(accountID int64) *AccountHealthScore {
	score, exists := m.healthScores[accountID]
	if !exists {
		score = &AccountHealthScore{
			AccountID:          accountID,
			CurrentHealthScore: 0.5,
			AllowedConcurrency: 1,
			LastCheckTime:      time.Now(),
		}
		m.healthScores[accountID] = score
	}
	return score
}

func (m *AccountHealthMonitor) getOrCreateErrorCounter(accountID int64) *errorWindowCounter {
	counter, exists := m.errorCounts[accountID]
	if !exists {
		counter = &errorWindowCounter{
			windowDur: 10 * time.Minute,
		}
		m.errorCounts[accountID] = counter
	}
	return counter
}

func (m *AccountHealthMonitor) cleanupExpiredEntries(counter *errorWindowCounter, windowStart time.Time) {
	filtered := counter.counts[:0]
	for _, entry := range counter.counts {
		if entry.timestamp.After(windowStart) {
			filtered = append(filtered, entry)
		}
	}
	counter.counts = filtered
}

func (m *AccountHealthMonitor) recalculateHealthScore(score *AccountHealthScore) {
	total := score.SuccessCount + score.FailureCount
	if total == 0 {
		return
	}

	successRate := float64(score.SuccessCount) / float64(total)

	// 检查是否有 401/403 错误
	if score.Error403Count > 0 || score.Error401Count > 0 {
		score.CurrentHealthScore = 0.3
		score.AllowedConcurrency = 1
		score.LastCheckTime = time.Now()
		return
	}

	// 根据成功率动态调整并发数
	if successRate > 0.95 {
		score.CurrentHealthScore = 0.9
		score.AllowedConcurrency = 3
	} else if successRate > 0.80 {
		score.CurrentHealthScore = 0.7
		score.AllowedConcurrency = 2
	} else {
		score.CurrentHealthScore = 0.5
		score.AllowedConcurrency = 1
	}

	score.LastCheckTime = time.Now()
}

// calculateMaxRequestsPerSec 根据账号健康状态确定最大请求速率
func (m *AccountHealthMonitor) calculateMaxRequestsPerSec(accountID int64) float64 {
	score, exists := m.healthScores[accountID]
	if !exists {
		return 2.0 // 默认 2 req/s
	}

	if score.CurrentHealthScore >= 0.9 {
		return 3.0
	} else if score.CurrentHealthScore >= 0.7 {
		return 2.0
	}
	return 1.0
}

// generateSessionIDForAccount 基于 accountID 生成确定性的会话 ID
func generateSessionIDForAccount(accountID int64) string {
	return generateUUIDFromSeed(fmt.Sprintf("session_binding_%d", accountID))
}
