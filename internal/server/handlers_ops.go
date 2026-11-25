package server

import (
	"encoding/json"
	"net/http"

	"agy-gateway/internal/pool"
	"agy-gateway/internal/telemetry"
)

// OpsHandlers provides operational probes and telemetry endpoints.
//
// @purpose Exposes liveness, readiness and Prometheus metrics for orchestrators.
// @rationale Readiness must reflect real pool availability; a static "ready" routes traffic to a
// gateway that cannot serve it.
// @preconditions p may be nil (metrics-only deployments).
// @invariants All handlers are safe for concurrent invocation and never panic on a nil pool.
type OpsHandlers struct {
	pool *pool.Pool
}

// NewOpsHandlers creates an OpsHandlers instance bound to the worker pool.
//
// @purpose Supplies readiness with live worker availability.
// @rationale Keeps /readyz honest.
// @preconditions None; pool may be nil.
// @param p Worker pool used for readiness reporting.
// @returns Initialized OpsHandlers.
// @throws None.
// @invariants O(1).
func NewOpsHandlers(p *pool.Pool) *OpsHandlers {
	return &OpsHandlers{pool: p}
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
// @Description  Returns 503 when no worker is available to serve a request, 200 otherwise.
// @Tags         Operations & Telemetry
// @Accept       json
// @Produce      json
// @Success      200  {object}  OpsReadinessResponse  "Gateway can serve traffic"
// @Failure      503  {object}  OpsReadinessResponse  "No worker is currently available"
// @Router       /readyz [get]
func (h *OpsHandlers) HandleReadyz(w http.ResponseWriter, r *http.Request) {
	total, available := h.pool.Availability()
	// active_workers is the busy count, not the idle count: an operator reading this gauge wants
	// to know how many workers are currently leased.
	telemetry.DefaultMetrics.ActiveWorkersCount.Store(int64(total - available))

	resp := OpsReadinessResponse{
		Status:           "ready",
		WorkersTotal:     total,
		WorkersAvailable: available,
	}
	code := http.StatusOK
	if available == 0 {
		resp.Status = "not_ready"
		code = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(resp)
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
	total, available := h.pool.Availability()
	telemetry.DefaultMetrics.ActiveWorkersCount.Store(int64(total - available))
	telemetry.DefaultMetrics.Handler().ServeHTTP(w, r)
}
