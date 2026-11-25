package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestReadyzReportsUnavailablePool proves FR-20: readiness must not lie.
//
// @purpose Fails readiness when no worker can be leased.
// @rationale A static "ready" routes traffic to a gateway that cannot serve it.
// @preconditions None.
// @param t Go test handle.
// @returns None.
// @throws None.
// @invariants Deterministic; nil pool => not ready.
func TestReadyzReportsUnavailablePool(t *testing.T) {
	rec := httptest.NewRecorder()
	NewOpsHandlers(nil).HandleReadyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 with a nil pool, got %d", rec.Code)
	}
	var body OpsReadinessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}
	if body.Status != "not_ready" || body.WorkersAvailable != 0 {
		t.Fatalf("expected not_ready/0, got %+v", body)
	}
}

// TestHealthzStaysAlive proves liveness is independent of pool state.
//
// @purpose Keeps /healthz a process-level probe.
// @rationale Conflating liveness with readiness causes restart storms.
// @preconditions None.
// @param t Go test handle.
// @returns None.
// @throws None.
// @invariants Deterministic.
func TestHealthzStaysAlive(t *testing.T) {
	rec := httptest.NewRecorder()
	NewOpsHandlers(nil).HandleHealthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !json.Valid(rec.Body.Bytes()) {
		t.Fatal("healthz body must be valid JSON")
	}
}
