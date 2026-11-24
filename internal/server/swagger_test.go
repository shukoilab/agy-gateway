package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agy-gateway/internal/config"
	"agy-gateway/internal/server"
)

func TestSwaggerUIEndpoints(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Port: 8080,
			Host: "127.0.0.1",
		},
	}

	opsHandlers := server.NewOpsHandlers()
	// Create minimal server instance using NewServer
	srv := server.NewServer(cfg, nil, nil, nil, opsHandlers, nil)

	// 1. Test GET /swagger/index.html
	t.Run("GET /swagger/index.html", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/swagger/index.html", nil)
		rec := httptest.NewRecorder()

		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", rec.Code)
		}

		body := rec.Body.String()
		if !strings.Contains(body, "Swagger UI") {
			t.Errorf("expected HTML to contain 'Swagger UI'")
		}
		if !strings.Contains(body, "doc.json") {
			t.Errorf("expected HTML to contain doc.json url configuration")
		}
		if !strings.Contains(body, "persistAuthorization") || !strings.Contains(body, "true") {
			t.Errorf("expected HTML to configure persistAuthorization: true")
		}
	})

	// 2. Test GET /swagger redirect
	t.Run("GET /swagger redirect", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/swagger", nil)
		rec := httptest.NewRecorder()

		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusMovedPermanently {
			t.Fatalf("expected status 301, got %d", rec.Code)
		}
		loc := rec.Header().Get("Location")
		if loc != "/swagger/index.html" {
			t.Errorf("expected redirect to /swagger/index.html, got %s", loc)
		}
	})

	// 3. Test GET /docs redirect
	t.Run("GET /docs redirect", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/docs", nil)
		rec := httptest.NewRecorder()

		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusMovedPermanently {
			t.Fatalf("expected status 301, got %d", rec.Code)
		}
		loc := rec.Header().Get("Location")
		if loc != "/swagger/index.html" {
			t.Errorf("expected redirect to /swagger/index.html, got %s", loc)
		}
	})

	// 4. Test GET /swagger/doc.json
	t.Run("GET /swagger/doc.json Schema Verification", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/swagger/doc.json", nil)
		rec := httptest.NewRecorder()

		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", rec.Code)
		}

		var doc struct {
			Swagger             string                    `json:"swagger"`
			Info                map[string]any            `json:"info"`
			SecurityDefinitions map[string]map[string]any `json:"securityDefinitions"`
			Paths               map[string]map[string]any `json:"paths"`
			Definitions         map[string]any            `json:"definitions"`
		}

		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("failed to parse swagger doc.json: %v", err)
		}

		if doc.Swagger != "2.0" {
			t.Errorf("expected swagger version '2.0', got %s", doc.Swagger)
		}

		// Verify Title and Version
		if title, ok := doc.Info["title"].(string); !ok || title != "agy-gateway API" {
			t.Errorf("expected info.title to be 'agy-gateway API', got %v", doc.Info["title"])
		}

		// Verify Interactive Auth Security Definitions
		if _, ok := doc.SecurityDefinitions["ClientBearerAuth"]; !ok {
			t.Errorf("missing ClientBearerAuth in securityDefinitions")
		} else {
			def := doc.SecurityDefinitions["ClientBearerAuth"]
			if def["type"] != "apiKey" || def["in"] != "header" || def["name"] != "Authorization" {
				t.Errorf("invalid ClientBearerAuth schema: %v", def)
			}
		}

		if _, ok := doc.SecurityDefinitions["AdminBearerAuth"]; !ok {
			t.Errorf("missing AdminBearerAuth in securityDefinitions")
		} else {
			def := doc.SecurityDefinitions["AdminBearerAuth"]
			if def["type"] != "apiKey" || def["in"] != "header" || def["name"] != "Authorization" {
				t.Errorf("invalid AdminBearerAuth schema: %v", def)
			}
		}

		// Verify Critical Endpoints
		expectedPaths := []string{
			"/v1/chat/completions",
			"/v1/models",
			"/v1/models/{model}",
			"/admin/v1/accounts",
			"/admin/v1/accounts/quota",
			"/admin/v1/accounts/quota/{id}",
			"/admin/v1/accounts/usage",
			"/admin/v1/accounts/switch",
			"/admin/v1/accounts/rotate",
			"/admin/v1/accounts/login/init",
			"/admin/v1/accounts/login/callback",
			"/admin/v1/accounts/login/{login_id}",
			"/admin/v1/vault/status",
			"/admin/v1/vault/clean",
			"/healthz",
			"/readyz",
			"/metrics",
		}

		for _, path := range expectedPaths {
			if _, exists := doc.Paths[path]; !exists {
				t.Errorf("expected path %q missing from generated Swagger documentation", path)
			}
		}
	})
}
