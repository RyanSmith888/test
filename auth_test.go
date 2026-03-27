package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAuthMiddleware_ValidKey(t *testing.T) {
	store, _ := setupTestStore(t)
	store.CreateAPIKey("sk-gw-valid", "tester", 1000)

	handler := AuthMiddleware(store)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keyID := ctxAPIKeyID(r.Context())
		if keyID == 0 {
			t.Error("expected key ID in context")
		}
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("x-api-key", "sk-gw-valid")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestAuthMiddleware_MissingKey(t *testing.T) {
	store, _ := setupTestStore(t)

	handler := AuthMiddleware(store)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not reach handler")
	}))

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestAuthMiddleware_InvalidKey(t *testing.T) {
	store, _ := setupTestStore(t)

	handler := AuthMiddleware(store)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not reach handler")
	}))

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("x-api-key", "sk-gw-invalid")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestAuthMiddleware_DisabledKey(t *testing.T) {
	store, _ := setupTestStore(t)
	id, _ := store.CreateAPIKey("sk-gw-disabled", "disabled", 1000)
	store.UpdateAPIKey(id, 0, 1000, "disabled")

	handler := AuthMiddleware(store)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not reach handler")
	}))

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("x-api-key", "sk-gw-disabled")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Errorf("expected 403, got %d", rec.Code)
	}
}

func TestAuthMiddleware_BearerToken(t *testing.T) {
	store, _ := setupTestStore(t)
	store.CreateAPIKey("sk-gw-bearer", "bearer-test", 1000)

	handler := AuthMiddleware(store)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer sk-gw-bearer")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestAdminAuthMiddleware_Valid(t *testing.T) {
	handler := AdminAuthMiddleware("secret-admin-key")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("GET", "/admin/accounts", nil)
	req.Header.Set("Authorization", "Bearer secret-admin-key")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestAdminAuthMiddleware_QueryParam(t *testing.T) {
	handler := AdminAuthMiddleware("qp-key")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("GET", "/admin/accounts?admin_key=qp-key", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestAdminAuthMiddleware_Invalid(t *testing.T) {
	handler := AdminAuthMiddleware("real-key")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not reach handler")
	}))

	req := httptest.NewRequest("GET", "/admin/accounts", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestCORSMiddleware(t *testing.T) {
	handler := CORSMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	// Preflight
	req := httptest.NewRequest("OPTIONS", "/v1/messages", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 204 {
		t.Errorf("preflight: expected 204, got %d", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("CORS origin header missing")
	}

	// Normal request
	req2 := httptest.NewRequest("POST", "/v1/messages", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if rec2.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("CORS header missing on normal request")
	}
}

func TestHealthHandler(t *testing.T) {
	store, cfg := setupTestStore(t)
	pool := NewPool(store, cfg)
	pool.Start()
	defer pool.Stop()

	handler := HealthHandler(store, pool, time.Now())

	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["status"] != "ok" {
		t.Error("expected status ok")
	}
}
