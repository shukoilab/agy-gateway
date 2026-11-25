package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agy-gateway/internal/config"
	"agy-gateway/internal/pool"
)

// TestOpenAIWriteTurnErrorPoolSaturation pins the 503 capacity mapping on the OpenAI surface.
//
// @purpose Guards FR-18: capacity exhaustion is a retryable condition, not a server fault.
// @rationale The pool returned an ad-hoc error string before the sentinel existed, so saturation
// fell through to the generic 500 and clients were told to treat a busy queue as a gateway bug.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no network, no goroutines.
func TestOpenAIWriteTurnErrorPoolSaturation(t *testing.T) {
	h := &OpenAIHandlers{logger: slog.Default()}
	w := httptest.NewRecorder()

	h.writeTurnError(w, pool.ErrQueueSaturated, time.Second)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("saturation must map to 503, got %d", w.Code)
	}
	var body struct {
		Error struct {
			Code *string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Error.Code == nil || *body.Error.Code != "pool_saturated" {
		t.Fatalf("expected code pool_saturated, got %v", body.Error.Code)
	}
}

// TestWriteTurnErrorCallerCancelledIsNotSaturation pins that a cancelled client context is not
// reported as capacity exhaustion on either surface.
//
// @purpose Stops a cancelled request from advertising itself as a retryable 503.
// @rationale Acquire bounds itself with a context derived from the caller's, so a hung-up client
// used to hit the same `<-Done()` branch as a genuine queue timeout and was labelled saturated;
// retrying a connection the caller already abandoned is pure waste.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; pure error mapping.
func TestWriteTurnErrorCallerCancelledIsNotSaturation(t *testing.T) {
	openaiH := &OpenAIHandlers{logger: slog.Default()}
	w := httptest.NewRecorder()
	openaiH.writeTurnError(w, context.Canceled, time.Second)
	if w.Code == http.StatusServiceUnavailable {
		t.Fatalf("caller cancellation must not be reported as pool saturation, got %d", w.Code)
	}

	anthropicH := &AnthropicHandlers{logger: slog.Default()}
	w2 := httptest.NewRecorder()
	anthropicH.writeAnthropicTurnError(w2, fmt.Errorf("acquire wrapped: %w", context.Canceled))
	if w2.Code == http.StatusServiceUnavailable {
		t.Fatalf("wrapped cancellation must not be saturation, got %d", w2.Code)
	}
}

// TestAnthropicWriteTurnErrorPoolSaturation pins the 503 capacity mapping on the Anthropic surface.
//
// @purpose Same guarantee as the OpenAI surface; both were previously generic 500s.
// @rationale The branch is separate code, so testing only one handler proves nothing about the other.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no network.
func TestAnthropicWriteTurnErrorPoolSaturation(t *testing.T) {
	h := &AnthropicHandlers{logger: slog.Default()}
	for _, err := range []error{
		pool.ErrQueueSaturated,
		fmt.Errorf("failed to acquire worker from pool: %w", pool.ErrQueueSaturated),
	} {
		w := httptest.NewRecorder()
		h.writeAnthropicTurnError(w, err)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%v must map to 503, got %d", err, w.Code)
		}
		var body struct {
			Type  string `json:"type"`
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if decErr := json.Unmarshal(w.Body.Bytes(), &body); decErr != nil {
			t.Fatalf("decode error body: %v", decErr)
		}
		if body.Type != "error" || body.Error.Type != "api_error" {
			t.Fatalf("expected an api_error envelope, got %+v", body)
		}
	}
}

// TestAcquireReturnsCallerCancellation verifies Acquire's own branch, not just the HTTP mapper.
//
// @purpose Proves the sentinel/cancellation split at its source rather than only downstream.
// @rationale Error mapping can be correct while Acquire still mislabels the cause. An empty pool
// with a short acquire timeout drives the real select loop: a live caller that waited it out is
// saturation, an already-cancelled caller is cancellation.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded by the 20ms acquire timeout; no goroutines, no workers started.
func TestAcquireReturnsCallerCancellation(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Pool.AcquireTimeout = 20 * time.Millisecond
	p := pool.NewTestPoolWithNoWorkers(cfg)

	if _, err := p.Acquire(context.Background(), false); !errors.Is(err, pool.ErrQueueSaturated) {
		t.Fatalf("empty pool must report saturation after its own timeout, got %v", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Acquire(cancelled, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller must surface context.Canceled, got %v", err)
	}
}
