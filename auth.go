package main

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// ============================================================================
// API Key Auth Middleware
// ============================================================================

func AuthMiddleware(store *Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Extract key from x-api-key or Authorization: Bearer
			key := r.Header.Get("x-api-key")
			if key == "" {
				if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
					key = strings.TrimPrefix(auth, "Bearer ")
				}
			}
			if key == "" {
				writeError(w, 401, "missing api key: set x-api-key header or Authorization: Bearer <key>")
				return
			}

			apiKey, err := store.GetAPIKeyByKey(key)
			if err != nil {
				writeError(w, 401, "invalid api key")
				return
			}
			if apiKey.Enabled == 0 {
				writeError(w, 403, "api key is disabled")
				return
			}

			// Check daily limit
			usage, err := store.GetKeyDailyUsage(apiKey.ID)
			if err != nil {
				logError("daily usage check for key %d: %v", apiKey.ID, err)
				writeError(w, 500, "internal error")
				return
			}
			if usage >= int64(apiKey.DailyLimit) {
				writeError(w, 429, "daily request limit exceeded")
				return
			}

			// Inject into context
			ctx := r.Context()
			ctx = context.WithValue(ctx, ctxKeyAPIKeyID, apiKey.ID)
			ctx = context.WithValue(ctx, ctxKeyAPIKeyName, apiKey.Name)
			ctx = context.WithValue(ctx, ctxKeyStartTime, time.Now())

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ============================================================================
// Account Picker Middleware
// ============================================================================

func AccountPickerMiddleware(pool *Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sessionID := r.Header.Get("session-id")

			as, err := pool.Pick(sessionID)
			if err != nil {
				logError("no available account for request: %v", err)
				writeError(w, 503, "no available upstream accounts, please try again later")
				return
			}

			pool.Acquire(as)

			ctx := context.WithValue(r.Context(), ctxKeyAccountState, as)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ============================================================================
// Admin Auth Middleware
// ============================================================================

func AdminAuthMiddleware(adminKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var provided string

			// Try Authorization header first
			if auth := r.Header.Get("Authorization"); auth != "" {
				provided = strings.TrimPrefix(auth, "Bearer ")
			}
			// Fallback to query param
			if provided == "" {
				provided = r.URL.Query().Get("admin_key")
			}

			if provided == "" || provided != adminKey {
				writeError(w, 401, "invalid or missing admin key")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// ============================================================================
// CORS Middleware
// ============================================================================

func CORSMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key, anthropic-version, anthropic-beta, session-id")
		w.Header().Set("Access-Control-Max-Age", "86400")

		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// ============================================================================
// Request Logging Middleware
// ============================================================================

func RequestLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		logDebug("%s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
		next.ServeHTTP(w, r)
		logDebug("%s %s completed in %v", r.Method, r.URL.Path, time.Since(start))
	})
}

// ============================================================================
// Context Helpers
// ============================================================================

func ctxAPIKeyID(ctx context.Context) int64 {
	v, _ := ctx.Value(ctxKeyAPIKeyID).(int64)
	return v
}

func ctxAPIKeyName(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyAPIKeyName).(string)
	return v
}

func ctxAccountState(ctx context.Context) *AccountState {
	v, _ := ctx.Value(ctxKeyAccountState).(*AccountState)
	return v
}

func ctxStartTime(ctx context.Context) time.Time {
	v, _ := ctx.Value(ctxKeyStartTime).(time.Time)
	return v
}
