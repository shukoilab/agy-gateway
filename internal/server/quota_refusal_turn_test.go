package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agy-gateway/internal/config"
	"agy-gateway/internal/pool"
	"agy-gateway/internal/retry"
)

// TestTurnAttempt_QuotaRefusalFailsAndEmitsNoText pins the live-observed defect end to end.
//
// @purpose Proves a turn that receives an exhausted-account message fails instead of returning the
// upsell as the assistant's answer.
// @rationale This is the exact production shape: agy sends the refusal as a plain
// `agent_message_chunk` and closes the turn with `stopReason: "end_turn"`. Returning success there is
// what handed the subscription message to the end user and prevented any rotation.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no subprocess beyond the fake ACP peer.
func TestTurnAttempt_QuotaRefusalFailsAndEmitsNoText(t *testing.T) {
	cfg := config.DefaultConfig()
	worker, _ := newWatchdogTestWorker(t, cfg, func(emit func(map[string]any)) {
		emit(map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": quotaRefusalSample},
		})
	})

	var emitted strings.Builder
	in := turnAttemptInputFor("sess_refusal", nil)
	in.OnText = func(chunk string) { emitted.WriteString(chunk) }

	result, err := executeTurnAttempt(context.Background(), worker, cfg, nil, in)

	if err == nil {
		t.Fatal("a quota refusal must fail the turn so failover can rotate")
	}
	if !errors.Is(err, retry.ErrQuotaRefusal) {
		t.Fatalf("error must carry the quota-refusal sentinel, got %v", err)
	}
	if !errors.Is(err, retry.ErrQuotaRefusal) || !pool.IsZeroTokens(err) {
		t.Fatalf("refusal must be marked as zero-token so healthy-quota guard cannot suppress it, got %v", err)
	}
	if emitted.String() != "" {
		t.Fatalf("the refusal must never reach the client, leaked %q", emitted.String())
	}
	if strings.TrimSpace(result.Text) != "" {
		t.Fatalf("refusal text must not be recorded as the answer, got %q", result.Text)
	}
	if got := retry.ClassifyFailure(err); got != retry.FailureClassQuotaExhausted {
		t.Fatalf("classification = %s, want QUOTA_EXHAUSTED", got)
	}
}

// TestTurnAttempt_OrdinaryAnswerStillSucceeds is the control.
//
// @purpose Proves the refusal detector does not break normal turns.
// @rationale A detector that failed ordinary text would replace a working gateway with a 500.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestTurnAttempt_OrdinaryAnswerStillSucceeds(t *testing.T) {
	cfg := config.DefaultConfig()
	worker, _ := newWatchdogTestWorker(t, cfg, func(emit func(map[string]any)) {
		emit(map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": "PONG"},
		})
	})

	var emitted strings.Builder
	in := turnAttemptInputFor("sess_normal", nil)
	in.OnText = func(chunk string) { emitted.WriteString(chunk) }

	result, err := executeTurnAttempt(context.Background(), worker, cfg, nil, in)
	if err != nil {
		t.Fatalf("ordinary turn must succeed, got %v", err)
	}
	if emitted.String() != "PONG" {
		t.Fatalf("client text = %q, want PONG", emitted.String())
	}
	if result.Text != "PONG" {
		t.Fatalf("recorded text = %q, want PONG", result.Text)
	}
}
