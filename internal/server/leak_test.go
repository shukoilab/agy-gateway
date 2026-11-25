package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"agy-gateway/internal/acp"
	"agy-gateway/internal/pool"
	"agy-gateway/internal/server"
)

// settleGoroutines waits for transient goroutines (release workers, timers, HTTP
// teardown) to finish, returning the observed count once it stabilises.
//
// @purpose Gives leak assertions a deterministic observation point instead of a racy snapshot.
// @rationale Goroutine counts fluctuate for milliseconds after a request completes because the
// pool spawns a Release goroutine per turn; sampling immediately produces false positives.
// @preconditions None.
// @param: tolerance Consecutive stable samples required before returning.
// @param: quiet Period to sleep between samples.
// @returns Stable goroutine count, or the last observed count when it never stabilises.
// @throws None.
// @invariants O(tolerance) samples, bounded wall-clock (<= tolerance*quiet on the happy path).
func settleGoroutines(tolerance int, quiet time.Duration) int {
	last := runtime.NumGoroutine()
	stable := 0
	for i := 0; i < 200; i++ {
		time.Sleep(quiet)
		current := runtime.NumGoroutine()
		if current == last {
			stable++
			if stable >= tolerance {
				return current
			}
			continue
		}
		stable = 0
		last = current
	}
	return last
}

// TestTurnExecutor_NoGoroutineLeakAcrossCycles proves that repeated identical turns do not
// accumulate goroutines (VC-05: runtime.NumGoroutine delta over many request cycles).
//
// @purpose Detects leaked listener, loopback-drain, watchdog, and keepalive goroutines.
// @rationale A single-turn assertion cannot distinguish steady-state goroutines from leaks; the
// only reliable signal is a flat count across many independent request lifecycles.
// @preconditions The mock worker and pool must be created per test (they own long-lived goroutines).
// @param: t Testing handle.
// @returns None (fails the test when growth exceeds the allowance).
// @throws None; calls t.Fatalf on violation.
// @invariants O(cycles) requests, constant memory, no network egress.
func TestTurnExecutor_NoGoroutineLeakAcrossCycles(t *testing.T) {
	handlers, cfg, _, _, _, _ := setupAnthropicTestEnvironment(t)

	mockWorker := createMockWorkerWithPipe(t, 0, cfg, func(method string, params json.RawMessage, inWriter io.Writer) {
		update := map[string]any{
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
		raw, _ := json.Marshal(update)
		_, _ = inWriter.Write(append(raw, '\n'))
	})

	testPool := pool.NewTestPoolWithWorker(mockWorker, cfg)
	failoverCoord := pool.NewFailoverCoordinator(testPool, nil, nil)
	handlers = server.NewAnthropicHandlers(cfg, testPool, failoverCoord, handlers.ModelRegistry(), nil, nil)

	payload := `{"model":"claude-3-5-sonnet","max_tokens":16,"messages":[{"role":"user","content":"ping"}]}`

	// Warm one cycle so lazy initialisers (slog, sync pools) are not counted as a leak.
	rec := httptest.NewRecorder()
	handlers.HandleMessages(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("warmup turn failed: %d %s", rec.Code, rec.Body.String())
	}
	baseline := settleGoroutines(3, 25*time.Millisecond)

	const cycles = 40
	for i := 0; i < cycles; i++ {
		rec := httptest.NewRecorder()
		handlers.HandleMessages(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload)))
		if rec.Code != http.StatusOK {
			t.Fatalf("cycle %d failed: %d %s", i, rec.Code, rec.Body.String())
		}
	}

	after := settleGoroutines(3, 25*time.Millisecond)
	// Allowance: pool.Release spawns one short-lived goroutine per turn; a bounded residue of
	// up to 4 covers scheduling lag without masking a real per-turn leak (which would add cycles).
	if after > baseline+4 {
		t.Fatalf("goroutine leak detected: baseline=%d after=%d cycles=%d", baseline, after, cycles)
	}
}
