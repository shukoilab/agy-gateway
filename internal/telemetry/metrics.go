// Package telemetry provides Prometheus-compatible metric counters and observability instrumentation.
package telemetry

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

// Metrics maintains application-level counters.
//
// @purpose Gives operators one place to read every operational signal the gateway produces.
// @rationale The ACP bridge lifecycle previously had no counters at all, which is why a
// systematic 401 storm (one rejected bridge call per request for 24h) went unnoticed until it was
// grepped out of the logs. Anything that can fail per-request gets a counter here.
type Metrics struct {
	RequestsTotal       atomic.Int64
	RequestsStreaming   atomic.Int64
	ToolCallsTotal      atomic.Int64
	FirewallBlocksTotal atomic.Int64
	ActiveWorkersCount  atomic.Int64

	// Session lifecycle. A session is created per turn on the stateless default path, so the
	// difference between created and closed is the live-session leak detector.
	SessionsCreated atomic.Int64
	SessionsClosed  atomic.Int64
	// SessionResumes counts successful history rehydrations; a non-zero value proves the
	// resume path is exercised rather than silently falling back to a fresh session.
	SessionResumes atomic.Int64

	// Prompt lifecycle. TTFT is the interval from prompt submission to the first streamed chunk;
	// the sum and count pair lets an operator compute the mean without a histogram bucket schema.
	PromptsStarted   atomic.Int64
	PromptsCompleted atomic.Int64
	PromptsFailed    atomic.Int64
	TTFTMillisSum    atomic.Int64
	TTFTCount        atomic.Int64
	TurnMillisSum    atomic.Int64
	TurnCount        atomic.Int64

	// MCP bridge. Rejections must stay at zero on the socket transport; a non-zero value means a
	// caller reached the bridge without a valid session socket.
	BridgeConnects   atomic.Int64
	BridgeCalls      atomic.Int64
	BridgeRejections atomic.Int64

	// Worker recycling, split by reason so a rising taint count is distinguishable from ordinary
	// rotation waste.
	WorkerRecycleTotal    atomic.Int64
	WorkerRecycleTainted  atomic.Int64
	WorkerRecycleRequests atomic.Int64
	WorkerRecycleUptime   atomic.Int64
}

var DefaultMetrics = &Metrics{}

// ratio renders a sum/count pair as a mean, or zero when nothing was measured.
//
// @purpose Avoids a division by zero in the exposition output.
// @rationale A metrics endpoint that panics or prints NaN is worse than one that prints 0.
// @preconditions None.
// @param sum Aggregate of the measured values.
// @param count Number of measurements.
// @returns The mean, or 0 when count is zero.
// @throws None.
// @invariants O(1).
func ratio(sum, count int64) float64 {
	if count == 0 {
		return 0
	}
	return float64(sum) / float64(count)
}

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

		fmt.Fprintf(w, "# HELP acp_sessions_created_total ACP sessions opened\n")
		fmt.Fprintf(w, "# TYPE acp_sessions_created_total counter\n")
		fmt.Fprintf(w, "acp_sessions_created_total %d\n", m.SessionsCreated.Load())

		fmt.Fprintf(w, "# HELP acp_sessions_closed_total ACP sessions closed\n")
		fmt.Fprintf(w, "# TYPE acp_sessions_closed_total counter\n")
		fmt.Fprintf(w, "acp_sessions_closed_total %d\n", m.SessionsClosed.Load())

		fmt.Fprintf(w, "# HELP acp_sessions_live ACP sessions opened but not yet closed\n")
		fmt.Fprintf(w, "# TYPE acp_sessions_live gauge\n")
		fmt.Fprintf(w, "acp_sessions_live %d\n", m.SessionsCreated.Load()-m.SessionsClosed.Load())

		fmt.Fprintf(w, "# HELP acp_session_resumes_total ACP sessions rehydrated from history\n")
		fmt.Fprintf(w, "# TYPE acp_session_resumes_total counter\n")
		fmt.Fprintf(w, "acp_session_resumes_total %d\n", m.SessionResumes.Load())

		fmt.Fprintf(w, "# HELP acp_prompts_started_total ACP prompts submitted\n")
		fmt.Fprintf(w, "# TYPE acp_prompts_started_total counter\n")
		fmt.Fprintf(w, "acp_prompts_started_total %d\n", m.PromptsStarted.Load())

		fmt.Fprintf(w, "# HELP acp_prompts_completed_total ACP prompts that ended normally\n")
		fmt.Fprintf(w, "# TYPE acp_prompts_completed_total counter\n")
		fmt.Fprintf(w, "acp_prompts_completed_total %d\n", m.PromptsCompleted.Load())

		fmt.Fprintf(w, "# HELP acp_prompts_failed_total ACP prompts that ended with an error\n")
		fmt.Fprintf(w, "# TYPE acp_prompts_failed_total counter\n")
		fmt.Fprintf(w, "acp_prompts_failed_total %d\n", m.PromptsFailed.Load())

		fmt.Fprintf(w, "# HELP acp_ttft_millis_mean Mean time to first token in milliseconds\n")
		fmt.Fprintf(w, "# TYPE acp_ttft_millis_mean gauge\n")
		fmt.Fprintf(w, "acp_ttft_millis_mean %.2f\n", ratio(m.TTFTMillisSum.Load(), m.TTFTCount.Load()))

		fmt.Fprintf(w, "# HELP acp_turn_millis_mean Mean turn duration in milliseconds\n")
		fmt.Fprintf(w, "# TYPE acp_turn_millis_mean gauge\n")
		fmt.Fprintf(w, "acp_turn_millis_mean %.2f\n", ratio(m.TurnMillisSum.Load(), m.TurnCount.Load()))

		fmt.Fprintf(w, "# HELP mcp_bridge_connects_total Bridge processes that connected to a session socket\n")
		fmt.Fprintf(w, "# TYPE mcp_bridge_connects_total counter\n")
		fmt.Fprintf(w, "mcp_bridge_connects_total %d\n", m.BridgeConnects.Load())

		fmt.Fprintf(w, "# HELP mcp_bridge_calls_total JSON-RPC frames served over the bridge\n")
		fmt.Fprintf(w, "# TYPE mcp_bridge_calls_total counter\n")
		fmt.Fprintf(w, "mcp_bridge_calls_total %d\n", m.BridgeCalls.Load())

		fmt.Fprintf(w, "# HELP mcp_bridge_rejections_total Bridge frames rejected (must stay 0 on the socket transport)\n")
		fmt.Fprintf(w, "# TYPE mcp_bridge_rejections_total counter\n")
		fmt.Fprintf(w, "mcp_bridge_rejections_total %d\n", m.BridgeRejections.Load())

		fmt.Fprintf(w, "# HELP worker_recycle_total Workers recycled\n")
		fmt.Fprintf(w, "# TYPE worker_recycle_total counter\n")
		fmt.Fprintf(w, "worker_recycle_total %d\n", m.WorkerRecycleTotal.Load())

		fmt.Fprintf(w, "# HELP worker_recycle_tainted_total Workers recycled because a turn tainted them\n")
		fmt.Fprintf(w, "# TYPE worker_recycle_tainted_total counter\n")
		fmt.Fprintf(w, "worker_recycle_tainted_total %d\n", m.WorkerRecycleTainted.Load())

		fmt.Fprintf(w, "# HELP worker_recycle_requests_total Workers recycled at the request-count ceiling\n")
		fmt.Fprintf(w, "# TYPE worker_recycle_requests_total counter\n")
		fmt.Fprintf(w, "worker_recycle_requests_total %d\n", m.WorkerRecycleRequests.Load())

		fmt.Fprintf(w, "# HELP worker_recycle_uptime_total Workers recycled at the uptime ceiling\n")
		fmt.Fprintf(w, "# TYPE worker_recycle_uptime_total counter\n")
		fmt.Fprintf(w, "worker_recycle_uptime_total %d\n", m.WorkerRecycleUptime.Load())
	}
}
