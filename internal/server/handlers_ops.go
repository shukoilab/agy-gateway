package server

import (
	"encoding/json"
	"net/http"

	"agy-gateway/internal/telemetry"
)

// OpsHandlers provides operational probes and telemetry endpoints.
type OpsHandlers struct{}

// NewOpsHandlers creates an OpsHandlers instance.
func NewOpsHandlers() *OpsHandlers {
	return &OpsHandlers{}
}

// HandleHealthz serves GET /healthz (liveness).
func (h *OpsHandlers) HandleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "healthy",
	})
}

// HandleReadyz serves GET /readyz (readiness).
func (h *OpsHandlers) HandleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "ready",
	})
}

// HandleMetrics serves GET /metrics.
func (h *OpsHandlers) HandleMetrics(w http.ResponseWriter, r *http.Request) {
	telemetry.DefaultMetrics.Handler().ServeHTTP(w, r)
}
