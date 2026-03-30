package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/redis/go-redis/v9"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// TranslationService 提供请求体中文→英文翻译功能，
// 用于隐藏用户语言特征，防止上游检测到中文用户。
type TranslationService struct {
	provider           TranslationProvider
	cache              TranslationCache
	translateResponses bool
}

// TranslationProvider 翻译提供者接口
type TranslationProvider interface {
	Translate(ctx context.Context, text, sourceLang, targetLang string) (string, error)
	Name() string
}

// TranslationCache 翻译缓存接口
type TranslationCache interface {
	GetTranslation(ctx context.Context, key string) (string, error)
	SetTranslation(ctx context.Context, key, value string) error
}

// NewTranslationService 创建翻译服务（provider 为 nil 时返回 nil）
func NewTranslationService(provider TranslationProvider, cache TranslationCache, translateResponses bool) *TranslationService {
	if provider == nil {
		return nil
	}
	return &TranslationService{
		provider:           provider,
		cache:              cache,
		translateResponses: translateResponses,
	}
}

// ProvideTranslationService Wire 提供者函数
func ProvideTranslationService(cfg *config.Config, redisClient *redis.Client) *TranslationService {
	if cfg == nil || !cfg.Translation.Enabled {
		return nil
	}
	if cfg.Translation.AliyunAccessKeyID == "" || cfg.Translation.AliyunAccessKeySecret == "" {
		slog.Warn("translation_service_disabled", "reason", "aliyun credentials not configured")
		return nil
	}

	provider := NewAliyunTranslator(cfg.Translation.AliyunAccessKeyID, cfg.Translation.AliyunAccessKeySecret)

	var cache TranslationCache
	if redisClient != nil {
		ttl := cfg.Translation.CacheTTLSeconds
		if ttl <= 0 {
			ttl = 86400 // 默认 24 小时
		}
		cache = NewRedisTranslationCache(redisClient, ttl)
	}

	return NewTranslationService(provider, cache, cfg.Translation.TranslateResponses)
}

// DetectLanguage 检测文本语言（根据中文字符比例判断）
// 返回 "zh" 或 "en"
func DetectLanguage(text string) string {
	if len(text) == 0 {
		return "en"
	}

	chineseCount := 0
	totalRunes := 0
	for _, r := range text {
		totalRunes++
		// CJK Unified Ideographs 基本区
		if r >= 0x4e00 && r <= 0x9fff {
			chineseCount++
		}
	}

	if totalRunes == 0 {
		return "en"
	}

	// 中文字符占比 > 5% 即判定为中文（兼顾中英混排场景）
	ratio := float64(chineseCount) / float64(totalRunes)
	if ratio > 0.05 {
		return "zh"
	}
	return "en"
}

// translateText 翻译单段文本，带缓存
func (s *TranslationService) translateText(ctx context.Context, text, targetLang string) (string, error) {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) == 0 {
		return text, nil
	}

	// 检测源语言 — 同语言跳过
	sourceLang := DetectLanguage(text)
	if sourceLang == targetLang {
		return text, nil
	}

	// 尝试从缓存获取
	cacheKey := translationCacheKey(text, targetLang)
	if s.cache != nil {
		if cached, err := s.cache.GetTranslation(ctx, cacheKey); err == nil && cached != "" {
			return cached, nil
		}
	}

	// 调用翻译 API
	result, err := s.provider.Translate(ctx, text, sourceLang, targetLang)
	if err != nil {
		slog.Warn("translation_failed",
			"provider", s.provider.Name(),
			"error", err,
			"text_len", len(text),
			"direction", sourceLang+"→"+targetLang,
		)
		return text, err
	}

	// 写入缓存
	if s.cache != nil && result != "" {
		_ = s.cache.SetTranslation(ctx, cacheKey, result)
	}

	return result, nil
}

// TranslateRequestBody 翻译请求体中的中文内容为英文。
// 返回 (翻译后的body, 是否执行了翻译)。
// 翻译范围：messages[*].content（字符串或 text 块）、system（字符串或 text 块）。
// 失败时返回原始 body，不中断请求流程。
func (s *TranslationService) TranslateRequestBody(ctx context.Context, body []byte) ([]byte, bool) {
	if s == nil || s.provider == nil || len(body) == 0 {
		return body, false
	}

	translated := false
	result := body

	// 1. 翻译 system prompt
	result, sysTranslated := s.translateSystemField(ctx, result)
	if sysTranslated {
		translated = true
	}

	// 2. 翻译 messages
	result, msgTranslated := s.translateMessagesField(ctx, result)
	if msgTranslated {
		translated = true
	}

	return result, translated
}

// ShouldTranslateResponses 是否需要翻译响应
func (s *TranslationService) ShouldTranslateResponses() bool {
	return s != nil && s.translateResponses
}

// TranslateResponseBody 翻译非流式响应体中的英文文本为中文
func (s *TranslationService) TranslateResponseBody(ctx context.Context, body []byte) []byte {
	if s == nil || s.provider == nil || len(body) == 0 {
		return body
	}

	content := gjson.GetBytes(body, "content")
	if !content.IsArray() {
		return body
	}

	result := body
	idx := 0
	content.ForEach(func(_, block gjson.Result) bool {
		if block.Get("type").String() == "text" {
			text := block.Get("text").String()
			if text != "" && DetectLanguage(text) == "en" {
				if t, err := s.translateText(ctx, text, "zh"); err == nil && t != text {
					path := fmt.Sprintf("content.%d.text", idx)
					result, _ = sjson.SetBytes(result, path, t)
				}
			}
		}
		idx++
		return true
	})

	return result
}

// translateSystemField 翻译 system 字段
func (s *TranslationService) translateSystemField(ctx context.Context, body []byte) ([]byte, bool) {
	system := gjson.GetBytes(body, "system")
	if !system.Exists() {
		return body, false
	}

	translated := false
	result := body

	if system.Type == gjson.String {
		text := system.String()
		if text != "" && DetectLanguage(text) == "zh" {
			if t, err := s.translateText(ctx, text, "en"); err == nil && t != text {
				result, _ = sjson.SetBytes(result, "system", t)
				translated = true
			}
		}
	} else if system.IsArray() {
		idx := 0
		system.ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() == "text" {
				text := block.Get("text").String()
				if text != "" && DetectLanguage(text) == "zh" {
					if t, err := s.translateText(ctx, text, "en"); err == nil && t != text {
						path := fmt.Sprintf("system.%d.text", idx)
						result, _ = sjson.SetBytes(result, path, t)
						translated = true
					}
				}
			}
			idx++
			return true
		})
	}

	return result, translated
}

// translateMessagesField 翻译 messages 数组中的文本内容
func (s *TranslationService) translateMessagesField(ctx context.Context, body []byte) ([]byte, bool) {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body, false
	}

	translated := false
	result := body

	msgIdx := 0
	messages.ForEach(func(_, msg gjson.Result) bool {
		content := msg.Get("content")

		if content.Type == gjson.String {
			// 简单字符串格式的 content
			text := content.String()
			if text != "" && DetectLanguage(text) == "zh" {
				if t, err := s.translateText(ctx, text, "en"); err == nil && t != text {
					path := fmt.Sprintf("messages.%d.content", msgIdx)
					result, _ = sjson.SetBytes(result, path, t)
					translated = true
				}
			}
		} else if content.IsArray() {
			// 数组格式的 content（包含 text/image/tool_use 等块）
			blockIdx := 0
			content.ForEach(func(_, block gjson.Result) bool {
				if block.Get("type").String() == "text" {
					text := block.Get("text").String()
					if text != "" && DetectLanguage(text) == "zh" {
						if t, err := s.translateText(ctx, text, "en"); err == nil && t != text {
							path := fmt.Sprintf("messages.%d.content.%d.text", msgIdx, blockIdx)
							result, _ = sjson.SetBytes(result, path, t)
							translated = true
						}
					}
				}
				blockIdx++
				return true
			})
		}

		msgIdx++
		return true
	})

	return result, translated
}

// translationCacheKey 生成翻译缓存 key
func translationCacheKey(text, targetLang string) string {
	h := sha256.Sum256([]byte(text))
	return fmt.Sprintf("sub2api:trans:%x:%s", h[:8], targetLang)
}
