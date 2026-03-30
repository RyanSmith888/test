package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	aliyunTranslateEndpoint = "https://mt.aliyuncs.com/"
	aliyunAPIVersion        = "2018-10-12"
	aliyunTranslateAction   = "TranslateGeneral"
	aliyunMaxTextLength     = 5000
)

// AliyunTranslator 使用阿里云机器翻译 POP API 实现翻译功能
type AliyunTranslator struct {
	accessKeyID     string
	accessKeySecret string
	httpClient      *http.Client
}

// NewAliyunTranslator 创建阿里云翻译器
func NewAliyunTranslator(accessKeyID, accessKeySecret string) *AliyunTranslator {
	return &AliyunTranslator{
		accessKeyID:     accessKeyID,
		accessKeySecret: accessKeySecret,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

func (t *AliyunTranslator) Name() string {
	return "aliyun"
}

// Translate 翻译文本，长文本自动分块
func (t *AliyunTranslator) Translate(ctx context.Context, text, sourceLang, targetLang string) (string, error) {
	if len(strings.TrimSpace(text)) == 0 {
		return text, nil
	}

	// 长文本分块翻译
	if len([]rune(text)) > aliyunMaxTextLength {
		return t.translateChunked(ctx, text, sourceLang, targetLang)
	}

	return t.translateSingle(ctx, text, sourceLang, targetLang)
}

// translateSingle 翻译单段文本（<= 5000 字符）
func (t *AliyunTranslator) translateSingle(ctx context.Context, text, sourceLang, targetLang string) (string, error) {
	aliyunSource := mapAliyunLangCode(sourceLang)
	aliyunTarget := mapAliyunLangCode(targetLang)

	// 构建 POP API 参数
	nonce, err := generateSignatureNonce()
	if err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}

	params := map[string]string{
		"Format":           "JSON",
		"Version":          aliyunAPIVersion,
		"AccessKeyId":      t.accessKeyID,
		"SignatureMethod":  "HMAC-SHA1",
		"Timestamp":        time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"SignatureVersion": "1.0",
		"SignatureNonce":   nonce,
		"Action":           aliyunTranslateAction,
		"FormatType":       "text",
		"SourceLanguage":   aliyunSource,
		"TargetLanguage":   aliyunTarget,
		"SourceText":       text,
		"Scene":            "general",
	}

	// 生成签名
	signature := t.sign(params)
	params["Signature"] = signature

	// 构建 form body
	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", aliyunTranslateEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build aliyun request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("aliyun translate request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB 限制
	if err != nil {
		return "", fmt.Errorf("read aliyun response: %w", err)
	}

	// 解析响应
	var result aliyunTranslateResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse aliyun response: %w (body: %s)", err, truncateForLog(string(body), 200))
	}

	if result.Code != "200" {
		return "", fmt.Errorf("aliyun translate error: code=%s message=%s", result.Code, result.Message)
	}

	if result.Data.Translated == "" {
		return "", fmt.Errorf("aliyun returned empty translation")
	}

	return result.Data.Translated, nil
}

// translateChunked 分块翻译长文本
func (t *AliyunTranslator) translateChunked(ctx context.Context, text, sourceLang, targetLang string) (string, error) {
	chunks := splitTextChunks(text, aliyunMaxTextLength)
	var results []string

	for _, chunk := range chunks {
		translated, err := t.translateSingle(ctx, chunk, sourceLang, targetLang)
		if err != nil {
			// 单块翻译失败时保留原文，不中断整体流程
			results = append(results, chunk)
			continue
		}
		results = append(results, translated)
	}

	return strings.Join(results, ""), nil
}

// sign 生成阿里云 POP API v1 签名
func (t *AliyunTranslator) sign(params map[string]string) string {
	// 按 key 字典序排列
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// 构建规范化查询字符串
	var buf strings.Builder
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte('&')
		}
		buf.WriteString(percentEncode(k))
		buf.WriteByte('=')
		buf.WriteString(percentEncode(params[k]))
	}
	canonicalQuery := buf.String()

	// 待签名字符串：POST&%2F&<url-encoded canonical query>
	stringToSign := "POST&" + percentEncode("/") + "&" + percentEncode(canonicalQuery)

	// HMAC-SHA1，密钥为 AccessKeySecret + "&"
	mac := hmac.New(sha1.New, []byte(t.accessKeySecret+"&"))
	mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// percentEncode 阿里云规范的 URL 编码
func percentEncode(s string) string {
	encoded := url.QueryEscape(s)
	encoded = strings.ReplaceAll(encoded, "+", "%20")
	encoded = strings.ReplaceAll(encoded, "*", "%2A")
	encoded = strings.ReplaceAll(encoded, "%7E", "~")
	return encoded
}

// generateSignatureNonce 生成随机 Nonce
func generateSignatureNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}

// mapAliyunLangCode 映射语言代码到阿里云格式
func mapAliyunLangCode(lang string) string {
	switch lang {
	case "zh":
		return "zh"
	case "en":
		return "en"
	default:
		return lang
	}
}

// splitTextChunks 按自然边界分割长文本
func splitTextChunks(text string, maxLen int) []string {
	runes := []rune(text)
	if len(runes) <= maxLen {
		return []string{text}
	}

	var chunks []string
	for len(runes) > 0 {
		end := maxLen
		if end > len(runes) {
			end = len(runes)
		}

		// 尝试在自然断点处分割（换行、句号等）
		if end < len(runes) {
			searchStart := end - 500
			if searchStart < 0 {
				searchStart = 0
			}
			for i := end - 1; i >= searchStart; i-- {
				r := runes[i]
				if r == '\n' || r == '。' || r == '！' || r == '？' || r == '.' || r == '!' || r == '?' {
					end = i + 1
					break
				}
			}
		}

		chunks = append(chunks, string(runes[:end]))
		runes = runes[end:]
	}

	return chunks
}

// truncateForLog 截断字符串用于日志输出
func truncateForLog(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// aliyunTranslateResponse 阿里云翻译 API 响应结构
type aliyunTranslateResponse struct {
	RequestID string `json:"RequestId"`
	Code      string `json:"Code"`
	Message   string `json:"Message"`
	Data      struct {
		Translated string `json:"Translated"`
		WordCount  string `json:"WordCount"`
	} `json:"Data"`
}
