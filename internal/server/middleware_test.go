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
