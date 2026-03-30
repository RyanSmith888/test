package service

import (
	"crypto/sha256"
	"fmt"
)

// VirtualIdentity 为每个账号生成持久化的虚拟身份
// 使用确定性算法：相同 accountID 始终生成相同的身份，保证幂等性
type VirtualIdentity struct {
	ClientID     string `json:"client_id"`      // 唯一的虚拟 Client ID
	DeviceID     string `json:"device_id"`      // 虚拟设备 ID
	SessionSeed  string `json:"session_seed"`   // 会话种子（保持不变）
	UserAgent    string `json:"user_agent"`     // 伪造的 User-Agent
	OSType       string `json:"os_type"`        // 虚拟 OS 类型
	Architecture string `json:"architecture"`   // 虚拟架构
	RuntimeVer   string `json:"runtime_ver"`    // 虚拟运行时版本
}

// 预定义的 User-Agent 池（模拟不同的客户端环境）
var userAgentPool = []string{
	"claude-cli/2.1.78 (darwin; arm64)",
	"claude-cli/2.1.78 (darwin; x86_64)",
	"claude-cli/2.1.78 (linux; x86_64)",
	"claude-cli/2.1.78 (linux; arm64)",
	"claude-cli/2.1.80 (windows; amd64)",
	"claude-cli/2.1.81 (darwin; arm64)",
	"claude-cli/2.1.82 (linux; x86_64)",
	"claude-cli/2.1.83 (windows; amd64)",
	"claude-cli/2.1.84 (darwin; arm64)",
	"claude-cli/2.1.85 (linux; arm64)",
}

// 预定义的 OS 池
var osPool = []string{"darwin", "linux", "windows"}

// 预定义的架构池
var archPool = []string{"arm64", "x86_64", "amd64"}

// 预定义的运行时版本池
var runtimeVersionPool = []string{"v20.10.0", "v20.11.0", "v21.0.0", "v22.0.0"}

// 预定义的 TLS 指纹池
var tlsFingerprintPool = []string{
	"profile_claude_cli_v1",
	"profile_claude_cli_v2",
	"profile_node_20",
	"profile_node_21",
	"profile_node_22",
}

// GenerateVirtualIdentity 基于 accountID 生成确定性的虚拟身份
// 相同的 accountID 始终返回相同的身份，无需额外缓存
func GenerateVirtualIdentity(accountID int64) *VirtualIdentity {
	return &VirtualIdentity{
		ClientID:     generateDeterministicVirtualClientID(accountID),
		DeviceID:     generateDeterministicVirtualDeviceID(accountID),
		SessionSeed:  fmt.Sprintf("session_%d", accountID),
		UserAgent:    selectFromPool(accountID, userAgentPool),
		OSType:       selectFromPool(accountID, osPool),
		Architecture: selectFromPool(accountID, archPool),
		RuntimeVer:   selectFromPool(accountID, runtimeVersionPool),
	}
}

// SelectTLSFingerprint 为账号分配固定的 TLS 指纹标识
func SelectTLSFingerprint(accountID int64) string {
	return selectFromPool(accountID, tlsFingerprintPool)
}

// GetDefaultVirtualIdentity 返回默认虚拟身份（降级使用）
func GetDefaultVirtualIdentity() *VirtualIdentity {
	return &VirtualIdentity{
		ClientID:     "default_client_id",
		DeviceID:     "00000000-0000-0000-0000-000000000000",
		SessionSeed:  "session_default",
		UserAgent:    "claude-cli/2.1.78 (darwin; arm64)",
		OSType:       "darwin",
		Architecture: "arm64",
		RuntimeVer:   "v20.10.0",
	}
}

// selectFromPool 根据 accountID 从池中选择一个确定性的值
func selectFromPool(accountID int64, pool []string) string {
	if len(pool) == 0 {
		return ""
	}
	idx := accountID % int64(len(pool))
	if idx < 0 {
		idx = -idx
	}
	return pool[idx]
}

// generateDeterministicVirtualClientID 基于 accountID 生成确定性的 Client ID
// 使用 SHA256(accountID) 生成持久化的 Client ID
func generateDeterministicVirtualClientID(accountID int64) string {
	h := sha256.New()
	h.Write([]byte(fmt.Sprintf("virtual_client_%d", accountID)))
	return fmt.Sprintf("%x", h.Sum(nil))[:64]
}

// generateDeterministicVirtualDeviceID 基于 accountID 生成确定性的 Device ID（UUID 格式）
func generateDeterministicVirtualDeviceID(accountID int64) string {
	h := sha256.New()
	h.Write([]byte(fmt.Sprintf("virtual_device_%d", accountID)))
	hash := fmt.Sprintf("%x", h.Sum(nil))
	return fmt.Sprintf(
		"%s-%s-%s-%s-%s",
		hash[0:8],
		hash[8:12],
		hash[12:16],
		hash[16:20],
		hash[20:32],
	)
}

// GetVirtualIdentityForAccount 获取账号的虚拟身份
// 先尝试从缓存获取，缓存未命中则生成并缓存
func (s *IdentityService) GetVirtualIdentityForAccount(accountID int64) *VirtualIdentity {
	// VirtualIdentity 是确定性的（基于 accountID），无需持久化缓存
	// 相同 accountID 始终返回相同结果
	return GenerateVirtualIdentity(accountID)
}

// ApplyVirtualIdentity 将虚拟身份应用到 Fingerprint 的默认值中
// 当创建新 Fingerprint 时，使用虚拟身份而非全局固定默认值
func GetDiverseDefaultFingerprint(accountID int64) Fingerprint {
	vi := GenerateVirtualIdentity(accountID)
	return Fingerprint{
		UserAgent:               vi.UserAgent,
		StainlessLang:           "js",
		StainlessPackageVersion: "0.70.0",
		StainlessOS:             vi.OSType,
		StainlessArch:           vi.Architecture,
		StainlessRuntime:        "node",
		StainlessRuntimeVersion: vi.RuntimeVer,
	}
}
