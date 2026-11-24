package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agy-gateway/internal/config"
	"agy-gateway/internal/server"
)

func TestMiddleware_CORS_PermissiveDefault(t *testing.T) {
	cfg := &config.Config{
		Security: config.SecurityConfig{
			AllowedCORSOrigins: []string{}, // Permissive
		},
	}
	mw := server.NewMiddleware(cfg)

	handler := mw.CORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))

	// Test OPTIONS preflight
	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected status 204 for OPTIONS preflight, got %d", rec.Code)
	}

	if allowOrigin := rec.Header().Get("Access-Control-Allow-Origin"); allowOrigin != "*" && allowOrigin != "http://localhost:3000" {
		t.Errorf("unexpected Access-Control-Allow-Origin: %s", allowOrigin)
	}
	if allowMethods := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(allowMethods, "POST") {
		t.Errorf("expected Access-Control-Allow-Methods to contain POST, got %s", allowMethods)
	}
}

func TestMiddleware_CORS_RestrictedOrigins(t *testing.T) {
	cfg := &config.Config{
		Security: config.SecurityConfig{
			AllowedCORSOrigins: []string{"https://app.shukoilab.com", "http://localhost:3000"},
		},
	}
	mw := server.NewMiddleware(cfg)

	handler := mw.CORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// 1. Allowed origin
	req1 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req1.Header.Set("Origin", "https://app.shukoilab.com")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)

	if origin := rec1.Header().Get("Access-Control-Allow-Origin"); origin != "https://app.shukoilab.com" {
		t.Errorf("expected allowed origin https://app.shukoilab.com, got %s", origin)
	}

	// 2. Disallowed origin
	req2 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req2.Header.Set("Origin", "https://malicious-site.com")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if origin := rec2.Header().Get("Access-Control-Allow-Origin"); origin != "" {
		t.Errorf("expected empty Access-Control-Allow-Origin for untrusted origin, got %s", origin)
	}
}

func TestMiddleware_Recovery(t *testing.T) {
	cfg := &config.Config{}
	mw := server.NewMiddleware(cfg)

	panickingHandler := mw.Recovery(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("unexpected runtime pointer fault simulation")
	}))

	req := httptest.NewRequest(http.MethodGet, "/test-panic", nil)
	rec := httptest.NewRecorder()

	panickingHandler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500 for intercepted panic, got %d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "Internal gateway panic") {
		t.Errorf("expected JSON body to contain 'Internal gateway panic', got %s", body)
	}
}

func TestMiddleware_MaxBytes_100MiBDefault(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			MaxRequestBodyBytes: 104857600, // 100 MiB
		},
	}
	mw := server.NewMiddleware(cfg)

	handler := mw.MaxBytes(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1024)
		total := 0
		for {
			n, err := r.Body.Read(buf)
			total += n
			if err != nil {
				break
			}
		}
		w.WriteHeader(http.StatusOK)
	}))

	// Test a large 15 MiB request body (exceeds old 10 MiB limit, but well within 100 MiB)
	payload := strings.NewReader(strings.Repeat("X", 15*1024*1024))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", payload)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 15 MiB payload to be accepted under 100 MiB ceiling, got status %d", rec.Code)
	}
}

func TestMiddleware_ClientAuth_DualHeader(t *testing.T) {
	cfg := &config.Config{
		Security: config.SecurityConfig{
			APIKeys: []config.APIKeyConfig{
				{
					ID:        "test-client-key",
					KeySecret: "sk-anthropic-secret-12345",
				},
			},
		},
	}
	mw := server.NewMiddleware(cfg)

	handler := mw.ClientAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("authorized"))
	}))

	// 1. Anthropic x-api-key valid
	req1 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	req1.Header.Set("x-api-key", "sk-anthropic-secret-12345")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Errorf("expected 200 for valid x-api-key, got %d", rec1.Code)
	}

	// 2. Anthropic x-api-key missing -> Anthropic JSON error format
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing x-api-key, got %d", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "authentication_error") {
		t.Errorf("expected Anthropic authentication_error format, got %s", rec2.Body.String())
	}

	// 3. OpenAI Bearer valid
	req3 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	req3.Header.Set("Authorization", "Bearer sk-anthropic-secret-12345")
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Errorf("expected 200 for valid Bearer token, got %d", rec3.Code)
	}

	// 4. OpenAI raw token in Authorization header (Swagger UI default when user inputs key directly)
	req4 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	req4.Header.Set("Authorization", "sk-anthropic-secret-12345")
	rec4 := httptest.NewRecorder()
	handler.ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusOK {
		t.Errorf("expected 200 for raw token in Authorization header, got %d", rec4.Code)
	}

	// 5. OpenAI lowercase bearer prefix in Authorization header
	req5 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	req5.Header.Set("Authorization", "bearer sk-anthropic-secret-12345")
	rec5 := httptest.NewRecorder()
	handler.ServeHTTP(rec5, req5)
	if rec5.Code != http.StatusOK {
		t.Errorf("expected 200 for lowercase bearer token, got %d", rec5.Code)
	}

	// 6. OpenAI Bearer invalid -> OpenAI JSON error format
	req6 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	req6.Header.Set("Authorization", "Bearer wrong-key")
	rec6 := httptest.NewRecorder()
	handler.ServeHTTP(rec6, req6)
	if rec6.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for wrong Bearer key, got %d", rec6.Code)
	}
	if !strings.Contains(rec6.Body.String(), "invalid_request_error") {
		t.Errorf("expected OpenAI invalid_request_error format, got %s", rec6.Body.String())
	}
}

func TestMiddleware_AdminAuth(t *testing.T) {
	cfg := &config.Config{
		Security: config.SecurityConfig{
			AdminAPIKeySecret: "admin-super-secret-123",
		},
	}
	mw := server.NewMiddleware(cfg)

	handler := mw.AdminAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("admin-ok"))
	}))

	// 1. Bearer valid
	req1 := httptest.NewRequest(http.MethodGet, "/admin/v1/accounts", nil)
	req1.Header.Set("Authorization", "Bearer admin-super-secret-123")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Errorf("expected 200 for valid admin Bearer token, got %d", rec1.Code)
	}

	// 2. Raw token (Swagger UI)
	req2 := httptest.NewRequest(http.MethodGet, "/admin/v1/accounts", nil)
	req2.Header.Set("Authorization", "admin-super-secret-123")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Errorf("expected 200 for valid admin raw token, got %d", rec2.Code)
	}

	// 3. Lowercase bearer
	req3 := httptest.NewRequest(http.MethodGet, "/admin/v1/accounts", nil)
	req3.Header.Set("Authorization", "bearer admin-super-secret-123")
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Errorf("expected 200 for valid admin lowercase bearer token, got %d", rec3.Code)
	}

	// 4. x-api-key valid
	req4 := httptest.NewRequest(http.MethodGet, "/admin/v1/accounts", nil)
	req4.Header.Set("x-api-key", "admin-super-secret-123")
	rec4 := httptest.NewRecorder()
	handler.ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusOK {
		t.Errorf("expected 200 for valid admin x-api-key, got %d", rec4.Code)
	}

	// 5. Wrong key
	req5 := httptest.NewRequest(http.MethodGet, "/admin/v1/accounts", nil)
	req5.Header.Set("Authorization", "Bearer wrong-secret")
	rec5 := httptest.NewRecorder()
	handler.ServeHTTP(rec5, req5)
	if rec5.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for wrong admin key, got %d", rec5.Code)
	}
}
