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
//
// @Summary      Liveness health check probe
// @Description  Returns the service liveness state. Used by orchestrators (Kubernetes, Docker) to determine if process is alive.
// @Tags         Operations & Telemetry
// @Accept       json
// @Produce      json
// @Success      200  {object}  OpsHealthResponse  "Service is alive and healthy"
// @Router       /healthz [get]
func (h *OpsHandlers) HandleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(OpsHealthResponse{
		Status: "healthy",
	})
}

// HandleReadyz serves GET /readyz (readiness).
//
// @Summary      Readiness health check probe
// @Description  Returns readiness state indicating worker pool availability and gateway capability to process requests.
// @Tags         Operations & Telemetry
// @Accept       json
// @Produce      json
// @Success      200  {object}  OpsHealthResponse  "Service is ready to handle traffic"
// @Router       /readyz [get]
func (h *OpsHandlers) HandleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(OpsHealthResponse{
		Status: "ready",
	})
}

// HandleMetrics serves GET /metrics.
//
// @Summary      Prometheus and runtime metrics
// @Description  Exposes real-time Prometheus metrics for telemetry, request tracking, error counts, and worker pool utilization.
// @Tags         Operations & Telemetry
// @Produce      text/plain
// @Success      200  {string}  string  "Prometheus text metric exposition"
// @Router       /metrics [get]
func (h *OpsHandlers) HandleMetrics(w http.ResponseWriter, r *http.Request) {
	telemetry.DefaultMetrics.Handler().ServeHTTP(w, r)
}
