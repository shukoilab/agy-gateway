package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"agy-gateway/internal/auth"
	"agy-gateway/internal/config"
	"agy-gateway/internal/models"
)

func setupTestServerWithAdminModels(t *testing.T) (*Server, *models.ModelRegistry, string) {
	tempDir, err := os.MkdirTemp("", "admin-models-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	cfg := &config.Config{
		Server: config.ServerConfig{
			Host: "127.0.0.1",
			Port: 8080,
		},
		Security: config.SecurityConfig{
			AdminAPIKeySecret: "test-admin-secret-key",
		},
		Storage: config.StorageConfig{
			VaultXDGConfigDir: filepath.Join(tempDir, "vault"),
		},
	}

	authMgr := auth.NewManager(cfg, nil)
	onboard := auth.NewOAuthOnboardingManager(cfg, authMgr, nil)
	mr := models.NewModelRegistry(cfg, authMgr, nil)

	// Preload a few models
	mr.RegisterModel(&models.ModelInfo{
		ID:          "claude-sonnet-4-6",
		DisplayName: "Claude Sonnet 4.6",
		OwnedBy:     "anthropic",
	})
	mr.RegisterModel(&models.ModelInfo{
		ID:          "gemini-3.8-flash",
		DisplayName: "Gemini 3.8 Flash",
		OwnedBy:     "google",
	})

	mr.AliasStore().SetDynamicAliases(map[string]string{
		"claude-3-7-sonnet": "claude-sonnet-4-6",
	})

	adminHandlers := NewAdminHandlers(cfg, authMgr, onboard, mr, nil)
	opsHandlers := NewOpsHandlers(nil)

	srv := NewServer(cfg, nil, nil, adminHandlers, opsHandlers, nil, nil)
	return srv, mr, tempDir
}

func TestAdminAnthropicModelEndpoints_FullLifecycle(t *testing.T) {
	srv, mr, tempDir := setupTestServerWithAdminModels(t)
	defer os.RemoveAll(tempDir)

	handler := srv.Handler()

	// 1. GET /admin/v1/models/anthropic/aliases (Unauthorized)
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/models/anthropic/aliases", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized without token, got %d", rec.Code)
	}

	// 2. GET /admin/v1/models/anthropic/aliases (Authorized)
	req = httptest.NewRequest(http.MethodGet, "/admin/v1/models/anthropic/aliases", nil)
	req.Header.Set("Authorization", "Bearer test-admin-secret-key")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	var listResp AnthropicAliasesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if listResp.Total == 0 {
		t.Errorf("expected at least 1 alias, got %d", listResp.Total)
	}

	// 3. POST /admin/v1/models/anthropic/aliases (Set manual alias)
	setPayload := SetModelAliasRequest{
		Alias:       "claude-3-7-sonnet",
		Target:      "gemini-3.8-flash",
		Description: "Admin test override",
	}
	bodyBytes, _ := json.Marshal(setPayload)
	req = httptest.NewRequest(http.MethodPost, "/admin/v1/models/anthropic/aliases", bytes.NewReader(bodyBytes))
	req.Header.Set("Authorization", "Bearer test-admin-secret-key")
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from set alias, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	// Verify FindModel resolves to the manual override target
	foundModel, found := mr.FindModel("claude-3-7-sonnet")
	if !found || foundModel.ID != "gemini-3.8-flash" {
		t.Fatalf("expected resolved target to be gemini-3.8-flash, got %+v", foundModel)
	}

	// 4. DELETE /admin/v1/models/anthropic/aliases/{alias} (Delete manual alias)
	req = httptest.NewRequest(http.MethodDelete, "/admin/v1/models/anthropic/aliases/claude-3-7-sonnet", nil)
	req.Header.Set("Authorization", "Bearer test-admin-secret-key")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from delete alias, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	// 5. DELETE non-existent alias should return 404
	req = httptest.NewRequest(http.MethodDelete, "/admin/v1/models/anthropic/aliases/non-existent-alias-xyz", nil)
	req.Header.Set("Authorization", "Bearer test-admin-secret-key")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found for non-existent alias, got %d", rec.Code)
	}

	// 6. POST /admin/v1/models/anthropic/sync
	req = httptest.NewRequest(http.MethodPost, "/admin/v1/models/anthropic/sync", nil)
	req.Header.Set("Authorization", "Bearer test-admin-secret-key")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from model sync, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	var syncResp ModelSyncResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &syncResp); err != nil {
		t.Fatalf("failed to decode sync response: %v", err)
	}
	if syncResp.Status != "synchronized" {
		t.Errorf("expected status 'synchronized', got %s", syncResp.Status)
	}
}
