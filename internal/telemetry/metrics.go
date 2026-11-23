// Package telemetry provides Prometheus-compatible metric counters and observability instrumentation.
package telemetry

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

// Metrics maintains application-level counters.
type Metrics struct {
	RequestsTotal       atomic.Int64
	RequestsStreaming   atomic.Int64
	ToolCallsTotal      atomic.Int64
	FirewallBlocksTotal atomic.Int64
	ActiveWorkersCount  atomic.Int64
}

var DefaultMetrics = &Metrics{}

// Handler exports Prometheus-compatible plain-text metrics.
func (m *Metrics) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprintf(w, "# HELP gateway_requests_total Total number of HTTP requests processed\n")
		fmt.Fprintf(w, "# TYPE gateway_requests_total counter\n")
		fmt.Fprintf(w, "gateway_requests_total %d\n", m.RequestsTotal.Load())

		fmt.Fprintf(w, "# HELP gateway_requests_streaming Total number of streaming requests\n")
		fmt.Fprintf(w, "# TYPE gateway_requests_streaming counter\n")
		fmt.Fprintf(w, "gateway_requests_streaming %d\n", m.RequestsStreaming.Load())

		fmt.Fprintf(w, "# HELP mcp_tool_calls_total Total tool calls invoked\n")
		fmt.Fprintf(w, "# TYPE mcp_tool_calls_total counter\n")
		fmt.Fprintf(w, "mcp_tool_calls_total %d\n", m.ToolCallsTotal.Load())

		fmt.Fprintf(w, "# HELP mcp_firewall_blocks_total Total unrequested tools blocked by firewall\n")
		fmt.Fprintf(w, "# TYPE mcp_firewall_blocks_total counter\n")
		fmt.Fprintf(w, "mcp_firewall_blocks_total %d\n", m.FirewallBlocksTotal.Load())

		fmt.Fprintf(w, "# HELP active_workers Number of active worker instances\n")
		fmt.Fprintf(w, "# TYPE active_workers gauge\n")
		fmt.Fprintf(w, "active_workers %d\n", m.ActiveWorkersCount.Load())
	}
}
