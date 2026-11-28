package telemetry

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandler_ExposesEveryCounterWithoutNaN pins the exposition contract.
//
// @purpose Proves the endpoint emits every metric and never prints NaN for an unmeasured mean.
// @rationale A metrics endpoint that emits NaN breaks Prometheus ingestion, and a silently missing
// series is indistinguishable from "no traffic" — both make an operator blind right when they need
// the numbers.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; uses a private Metrics so global state cannot leak between tests.
func TestHandler_ExposesEveryCounterWithoutNaN(t *testing.T) {
	m := &Metrics{}

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()

	// Ratios must be 0, not NaN, before anything is measured.
	if strings.Contains(body, "NaN") {
		t.Fatalf("metrics exposition contains NaN:\n%s", body)
	}

	required := []string{
		"gateway_requests_total",
		"acp_sessions_created_total",
		"acp_sessions_closed_total",
		"acp_sessions_live",
		"acp_session_resumes_total",
		"acp_prompts_started_total",
		"acp_prompts_completed_total",
		"acp_prompts_failed_total",
		"acp_ttft_millis_mean",
		"acp_turn_millis_mean",
		"mcp_bridge_connects_total",
		"mcp_bridge_calls_total",
		"mcp_bridge_rejections_total",
		"worker_recycle_total",
		"worker_recycle_tainted_total",
		"worker_recycle_requests_total",
		"worker_recycle_uptime_total",
	}
	for _, name := range required {
		if !strings.Contains(body, name+" ") {
			t.Errorf("metrics output is missing %s", name)
		}
	}
}

// TestHandler_LiveSessionsIsTheDifference pins the gauge derivation.
//
// @purpose Proves the live-session gauge is created minus closed, which is the leak detector.
// @rationale Reporting created and closed separately leaves the operator to do arithmetic; the
// gauge exists so a leak is one number.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestHandler_LiveSessionsIsTheDifference(t *testing.T) {
	m := &Metrics{}
	m.SessionsCreated.Add(7)
	m.SessionsClosed.Add(5)

	// Both means must reflect their sums once measured.
	m.TTFTMillisSum.Add(300)
	m.TTFTCount.Add(2)
	m.TurnMillisSum.Add(900)
	m.TurnCount.Add(4)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()

	for want := range map[string]string{
		"acp_sessions_live 2":         "",
		"acp_ttft_millis_mean 150.00": "",
		"acp_turn_millis_mean 225.00": "",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %q in metrics output:\n%s", want, body)
		}
	}
}
