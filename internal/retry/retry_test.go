package retry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"syscall"
	"testing"
	"time"

	"agy-gateway/internal/acp"
)

func TestClassifyFailure(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected FailureClass
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: FailureClassNone,
		},
		{
			name:     "context cancelled",
			err:      context.Canceled,
			expected: FailureClassNonRetryable,
		},
		{
			name:     "acp auth required code",
			err:      &acp.ResponseError{Code: acp.ErrCodeAuthRequired, Message: "Auth expired"},
			expected: FailureClassAuthExpired,
		},
		{
			name:     "string auth required",
			err:      errors.New("Keyring authorization failed: unauthorized"),
			expected: FailureClassAuthExpired,
		},
		{
			name:     "quota exhausted",
			err:      errors.New("upstream error: RESOURCE_EXHAUSTED: rate limit exceeded for 5h window"),
			expected: FailureClassQuotaExhausted,
		},
		{
			name:     "quota 429 status",
			err:      errors.New("HTTP 429: Too Many Requests"),
			expected: FailureClassQuotaExhausted,
		},
		{
			name:     "model overloaded",
			err:      errors.New("The model is overloaded. Please try again later."),
			expected: FailureClassTransientOverload,
		},
		{
			name:     "service unavailable 503",
			err:      errors.New("503 Service Unavailable"),
			expected: FailureClassTransientOverload,
		},
		{
			name:     "transport broken pipe",
			err:      syscall.EPIPE,
			expected: FailureClassTransportClosed,
		},
		{
			name:     "unexpected eof",
			err:      io.ErrUnexpectedEOF,
			expected: FailureClassTransportClosed,
		},
		{
			name:     "pipe closed string",
			err:      errors.New("jsonrpc: transport is closed"),
			expected: FailureClassTransportClosed,
		},
		{
			name:     "ttft watchdog string",
			err:      errors.New("first_token_timeout: Time to first token exceeded 120s"),
			expected: FailureClassFirstTokenTimeout,
		},
		{
			name:     "sqlite busy lock",
			err:      errors.New("database is locked (SQLITE_BUSY)"),
			expected: FailureClassTransientCLI,
		},
		{
			name:     "unrecognized random error",
			err:      fmt.Errorf("invalid json schema format at path /messages/0"),
			expected: FailureClassNonRetryable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyFailure(tc.err)
			if got != tc.expected {
				t.Errorf("expected %s, got %s for error %v", tc.expected, got, tc.err)
			}
		})
	}
}

func TestFailureClassProperties(t *testing.T) {
	if !FailureClassAuthExpired.IsRetryable() {
		t.Errorf("expected AuthExpired to be retryable")
	}
	if !FailureClassQuotaExhausted.IsRetryable() {
		t.Errorf("expected QuotaExhausted to be retryable")
	}
	if FailureClassNonRetryable.IsRetryable() {
		t.Errorf("expected NonRetryable NOT to be retryable")
	}
	if FailureClassNone.IsRetryable() {
		t.Errorf("expected None NOT to be retryable")
	}
}

func TestComputeBackoffBounds(t *testing.T) {
	initial := 500 * time.Millisecond
	maxBackoff := 5 * time.Second

	for attempt := 0; attempt < 10; attempt++ {
		for i := 0; i < 50; i++ {
			b := ComputeBackoff(attempt, initial, maxBackoff)
			if b < 0 {
				t.Fatalf("backoff should not be negative: %v", b)
			}
			if b > maxBackoff {
				t.Fatalf("backoff %v exceeded ceiling %v at attempt %d", b, maxBackoff, attempt)
			}
		}
	}

	// Test boundary inputs
	bZero := ComputeBackoff(-1, 0, 0)
	if bZero < 0 {
		t.Errorf("expected non-negative backoff for zero input, got %v", bZero)
	}
}

func TestSleepWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	err := SleepWithContext(ctx, 1*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}

	// Test clean sleep
	start := time.Now()
	err = SleepWithContext(context.Background(), 10*time.Millisecond)
	if err != nil {
		t.Errorf("expected nil error on normal sleep, got %v", err)
	}
	if time.Since(start) < 9*time.Millisecond {
		t.Errorf("sleep finished too quickly")
	}
}
