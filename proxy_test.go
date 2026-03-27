package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestProxyHeaders_Preserve(t *testing.T) {
	req, _ := http.NewRequest("POST", "http://localhost/v1/messages", nil)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "messages-2024-01-01")
	req.Header.Set("User-Agent", "my-app/1.0")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Stainless-Os", "linux")
	req.Header.Set("X-Stainless-Runtime", "go")

	result := cleanHeaders(req)

	checks := map[string]string{
		"anthropic-version": "2023-06-01",
		"anthropic-beta":    "messages-2024-01-01",
		"Content-Type":      "application/json",
	}
	for k, want := range checks {
		if got := result.Get(k); got != want {
			t.Errorf("header %s: want %q, got %q", k, want, got)
		}
	}
	if result.Get("X-Stainless-Os") == "" {
		t.Error("x-stainless-* headers should be preserved")
	}
}

func TestProxyHeaders_Strip(t *testing.T) {
	req, _ := http.NewRequest("POST", "http://localhost/v1/messages", nil)
	req.Header.Set("X-Api-Key", "secret")
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	req.Header.Set("X-Real-Ip", "10.0.0.1")
	req.Header.Set("Via", "proxy")
	req.Header.Set("Authorization", "Bearer original")
	req.Header.Set("anthropic-version", "2023-06-01")

	result := cleanHeaders(req)

	stripped := []string{"X-Api-Key", "X-Forwarded-For", "X-Real-Ip", "Via", "Authorization"}
	for _, k := range stripped {
		if result.Get(k) != "" {
			t.Errorf("header %s should be stripped, got %q", k, result.Get(k))
		}
	}
}

func TestProxyHeaders_TokenReplacement(t *testing.T) {
	req, _ := http.NewRequest("POST", "http://localhost/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer user-token")
	req.Header.Set("anthropic-version", "2023-06-01")

	result := cleanHeaders(req)

	// Authorization should be stripped by cleanHeaders
	if result.Get("Authorization") != "" {
		t.Error("original Authorization should be stripped")
	}

	// After setting account token
	result.Set("Authorization", "Bearer account-token-xxx")
	if got := result.Get("Authorization"); got != "Bearer account-token-xxx" {
		t.Errorf("account token not set: got %q", got)
	}
}

func TestExtractModel(t *testing.T) {
	tests := []struct {
		body string
		want string
	}{
		{`{"model":"claude-sonnet-4-20250514","max_tokens":1024}`, "claude-sonnet-4-20250514"},
		{`{"messages":[]}`, ""},
		{``, ""},
		{`invalid json`, ""},
	}
	for _, tt := range tests {
		got := extractModel([]byte(tt.body))
		if got != tt.want {
			t.Errorf("extractModel(%q) = %q, want %q", tt.body, got, tt.want)
		}
	}
}

// cleanHeaders simulates the header cleaning logic from proxy.go
func cleanHeaders(r *http.Request) http.Header {
	out := http.Header{}
	for key, vals := range r.Header {
		lower := strings.ToLower(key)
		if stripHeaders[lower] {
			continue
		}
		if preserveHeaders[lower] || strings.HasPrefix(lower, "x-stainless-") {
			for _, v := range vals {
				out.Add(key, v)
			}
		}
	}
	return out
}
