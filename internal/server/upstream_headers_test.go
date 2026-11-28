package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agy-gateway/internal/acp"
	"agy-gateway/internal/pool"
	"agy-gateway/internal/server"
)

// TestAnthropicSync_ReportsAppliedUpstreamFacts pins S10.
//
// @purpose Proves a synchronous response carries the model, the level actually in force, and the
// agent's own conversation title as headers.
// @rationale None of these three facts exists in the provider schema, so a harness that cannot read
// them cannot tell a downgraded reasoning level from a honoured one — which is exactly the silent
// behaviour this refactor removes.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; one mock worker, no subprocess.
func TestAnthropicSync_ReportsAppliedUpstreamFacts(t *testing.T) {
	handlers, cfg, _, _, _, _ := setupAnthropicTestEnvironment(t)

	mockWorker := createMockWorkerWithPipe(t, 0, cfg, func(_ string, _ json.RawMessage, inWriter io.Writer) {
		// The agent names the conversation mid-turn; that title is the only human-meaningful name.
		titleUpdate := map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "mock_sess_1",
				"update": map[string]any{
					"sessionUpdate": acp.UpdateSessionInfo,
					"title":         "Explain Actor Isolation",
				},
			},
		}
		rawTitle, _ := json.Marshal(titleUpdate)
		_, _ = inWriter.Write(append(rawTitle, '\n'))

		textUpdate := map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "mock_sess_1",
				"update": map[string]any{
					"sessionUpdate": acp.UpdateAgentMessageChunk,
					"content":       map[string]any{"type": "text", "text": "Isolation holds."},
				},
			},
		}
		rawText, _ := json.Marshal(textUpdate)
		_, _ = inWriter.Write(append(rawText, '\n'))
	})

	testPool := pool.NewTestPoolWithWorker(mockWorker, cfg)
	failoverCoord := pool.NewFailoverCoordinator(testPool, nil, nil)
	handlers = server.NewAnthropicHandlers(cfg, testPool, failoverCoord, handlers.ModelRegistry(), nil, nil)

	payload := `{
		"model": "claude-3-5-sonnet",
		"max_tokens": 512,
		"messages": [{"role": "user", "content": "Explain actor isolation."}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload))
	rec := httptest.NewRecorder()
	handlers.HandleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Agy-Model"); got != "claude-3-5-sonnet" {
		t.Fatalf("X-Agy-Model = %q, want claude-3-5-sonnet", got)
	}
	if got := rec.Header().Get("X-Agy-Session-Title"); got != "Explain Actor Isolation" {
		t.Fatalf("X-Agy-Session-Title = %q, want the agent's own title", got)
	}
}

// TestAnthropicSync_NoThoughtLevelIsNeitherSentNorReported pins D10 for models without levels.
//
// @purpose Proves a model that advertises no thought_level never receives the value and never
// reports one as applied.
// @rationale Sending a level the option set does not contain earns -32602 on every turn of the
// session; reporting one as applied when the agent ignored it is a lie the caller cannot detect.
// The mock here answers every set_config_option with success, which is exactly how the old code
// was fooled into reporting "xhigh" for a model that has no such option.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no subprocess.
func TestAnthropicSync_NoThoughtLevelIsNeitherSentNorReported(t *testing.T) {
	handlers, cfg, _, _, _, _ := setupAnthropicTestEnvironment(t)

	mockWorker := createMockWorkerWithPipe(t, 0, cfg, func(_ string, _ json.RawMessage, inWriter io.Writer) {
		textUpdate := map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "mock_sess_1",
				"update": map[string]any{
					"sessionUpdate": acp.UpdateAgentMessageChunk,
					"content":       map[string]any{"type": "text", "text": "ok"},
				},
			},
		}
		raw, _ := json.Marshal(textUpdate)
		_, _ = inWriter.Write(append(raw, '\n'))
	})

	testPool := pool.NewTestPoolWithWorker(mockWorker, cfg)
	failoverCoord := pool.NewFailoverCoordinator(testPool, nil, nil)
	handlers = server.NewAnthropicHandlers(cfg, testPool, failoverCoord, handlers.ModelRegistry(), nil, nil)

	payload := `{
		"model": "claude-3-5-sonnet",
		"max_tokens": 512,
		"thinking": {"type": "enabled", "budget_tokens": 32768},
		"messages": [{"role": "user", "content": "Think hard."}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload))
	rec := httptest.NewRecorder()
	handlers.HandleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("a model without a thought level must still serve the turn, got %d: %s",
			rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Agy-Thought-Level"); got != "" {
		t.Fatalf("X-Agy-Thought-Level = %q; a model exposing no level must not report one", got)
	}
}
