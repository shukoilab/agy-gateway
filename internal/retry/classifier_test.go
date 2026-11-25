package retry

import (
	"errors"
	"fmt"
	"testing"
)

// TestClassifyFailure_EmptyCompletion pins defect A9's classification.
//
// @purpose Makes a turn that produced nothing retryable instead of a reported success.
// @rationale agy answers an internally exhausted quota with a normal `end_turn` and no content. The
// gateway previously returned HTTP 200 with an empty message, so a caller could not tell that the work
// never happened. Classifying it is what allows failover to try another account.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no I/O.
func TestClassifyFailure_EmptyCompletion(t *testing.T) {
	// Bare sentinel.
	if got := ClassifyFailure(ErrEmptyCompletion); got != FailureClassEmptyCompletion {
		t.Errorf("bare sentinel classified as %v, want %v", got, FailureClassEmptyCompletion)
	}

	// Wrapped, as the failover layer will carry it.
	wrapped := fmt.Errorf("turn attempt failed: %w", ErrEmptyCompletion)
	if got := ClassifyFailure(wrapped); got != FailureClassEmptyCompletion {
		t.Errorf("wrapped sentinel classified as %v, want %v", got, FailureClassEmptyCompletion)
	}

	if got := FailureClassEmptyCompletion.String(); got != "EMPTY_COMPLETION" {
		t.Errorf("String() = %q, want EMPTY_COMPLETION", got)
	}

	// Retryability is the point: a different account may well answer.
	if !FailureClassEmptyCompletion.IsRetryable() {
		t.Error("EMPTY_COMPLETION must be retryable so failover can try another account")
	}
}

// TestClassifyFailure_EmptyCompletionPrecedesStringHeuristics pins the ordering.
//
// @purpose Prevents the sentinel from being swallowed by an earlier substring rule.
// @rationale Classification order matters: if the string heuristics ran first, a message that merely
// mentions "context" or "canceled" would mask the real cause. The sentinel identity check must win.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no I/O.
func TestClassifyFailure_EmptyCompletionPrecedesStringHeuristics(t *testing.T) {
	// A message that would otherwise match the non-retryable "context canceled" rule.
	err := fmt.Errorf("context canceled while %w", ErrEmptyCompletion)
	if got := ClassifyFailure(err); got != FailureClassEmptyCompletion {
		t.Errorf("classified as %v, want EMPTY_COMPLETION (sentinel identity must win)", got)
	}
}

// TestClassifyFailure_StillDetectsQuota pins that the new class did not disturb quota detection.
//
// @purpose Guards against an ordering regression that would stop rotation from ever firing.
// @rationale Defect A3 is that quota failures never reached this classifier at all; the classifier
// itself must keep recognising the strings it already handled.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no I/O.
func TestClassifyFailure_StillDetectsQuota(t *testing.T) {
	for _, err := range []error{
		errors.New("ResourceExhausted, HTTP 429: Resource has been exhausted"),
		errors.New("RESOURCE_EXHAUSTED (code 429)"),
		errors.New("rate limit exceeded for 5h window"),
	} {
		if got := ClassifyFailure(err); got != FailureClassQuotaExhausted {
			t.Errorf("ClassifyFailure(%q) = %v, want QUOTA_EXHAUSTED", err, got)
		}
	}
}
