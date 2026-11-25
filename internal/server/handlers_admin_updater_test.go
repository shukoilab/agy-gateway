package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agy-gateway/internal/config"
	"agy-gateway/internal/updater"
)

func TestAdminUpdaterEndpoints(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Security.AdminAPIKeySecret = "test-admin-secret"
	cfg.AutoUpdate.Enabled = true
	cfg.AutoUpdate.Interval = 6 * time.Hour
	cfg.AutoUpdate.BinDir = t.TempDir()

	updaterSvc := updater.NewService(cfg, nil, nil)
	adminHandlers := NewAdminHandlers(cfg, nil, nil, nil, updaterSvc)
	opsHandlers := NewOpsHandlers(nil)
	srv := NewServer(cfg, nil, nil, adminHandlers, opsHandlers, nil, nil)

	// 1. GET /admin/v1/updater/status without auth -> 401
	req1 := httptest.NewRequest(http.MethodGet, "/admin/v1/updater/status", nil)
	rec1 := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthorized status request, got %d", rec1.Code)
	}

	// 2. GET /admin/v1/updater/status with auth -> 200 OK
	req2 := httptest.NewRequest(http.MethodGet, "/admin/v1/updater/status", nil)
	req2.Header.Set("Authorization", "Bearer test-admin-secret")
	rec2 := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 for authorized status request, got %d", rec2.Code)
	}

	var statusResp updater.AutoUpdateStatusResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &statusResp); err != nil {
		t.Fatalf("failed to decode updater status JSON: %v", err)
	}
	if !statusResp.Enabled {
		t.Errorf("expected statusResp.Enabled to be true")
	}

	// 3. POST /admin/v1/updater/check without auth -> 401
	req3 := httptest.NewRequest(http.MethodPost, "/admin/v1/updater/check", nil)
	rec3 := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthorized check request, got %d", rec3.Code)
	}

	// 4. Test uninitialized updater -> 503
	handlersNoSvc := NewAdminHandlers(cfg, nil, nil, nil, nil)
	srvNoSvc := NewServer(cfg, nil, nil, handlersNoSvc, opsHandlers, nil, nil)

	req4 := httptest.NewRequest(http.MethodGet, "/admin/v1/updater/status", nil)
	req4.Header.Set("Authorization", "Bearer test-admin-secret")
	rec4 := httptest.NewRecorder()
	srvNoSvc.httpServer.Handler.ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 for uninitialized updater status, got %d", rec4.Code)
	}

	req5 := httptest.NewRequest(http.MethodPost, "/admin/v1/updater/check", nil)
	req5.Header.Set("Authorization", "Bearer test-admin-secret")
	rec5 := httptest.NewRecorder()
	srvNoSvc.httpServer.Handler.ServeHTTP(rec5, req5)
	if rec5.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 for uninitialized updater check, got %d", rec5.Code)
	}
}
