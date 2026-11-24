// Package retry provides zero-allocation failure classification, exponential backoff
// calculation with full jitter, and resiliency primitives for 3rd-party interactions.
package retry

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"

	"agy-gateway/internal/acp"
)

// FailureClass enumerates the deterministic category of a runtime error.
type FailureClass int

const (
	// FailureClassNone indicates no failure (nil error).
	FailureClassNone FailureClass = iota

	// FailureClassAuthExpired indicates an OAuth/session token expiry or missing credentials.
	FailureClassAuthExpired

	// FailureClassQuotaExhausted indicates upstream rate limiting or depleted quota buckets (429/RESOURCE_EXHAUSTED).
	FailureClassQuotaExhausted

	// FailureClassTransientOverload indicates temporary Google infrastructure pressure (503/502/Model overloaded).
	FailureClassTransientOverload

	// FailureClassTransportClosed indicates subprocess termination, broken pipe, or abrupt socket drop.
	FailureClassTransportClosed

	// FailureClassFirstTokenTimeout indicates the Time-to-First-Token watchdog fired before inference stream began.
	FailureClassFirstTokenTimeout

	// FailureClassTransientCLI indicates transient SQLite vault lock contention or fork pressure.
	FailureClassTransientCLI

	// FailureClassNonRetryable indicates client aborts, validation failures, or unrecoverable application errors.
	FailureClassNonRetryable
)

// String returns the canonical human-readable identifier for the failure classification.
func (f FailureClass) String() string {
	switch f {
	case FailureClassNone:
		return "NONE"
	case FailureClassAuthExpired:
		return "AUTH_EXPIRED"
	case FailureClassQuotaExhausted:
		return "QUOTA_EXHAUSTED"
	case FailureClassTransientOverload:
		return "TRANSIENT_OVERLOAD"
	case FailureClassTransportClosed:
		return "TRANSPORT_CLOSED"
	case FailureClassFirstTokenTimeout:
		return "FIRST_TOKEN_TIMEOUT"
	case FailureClassTransientCLI:
		return "TRANSIENT_CLI"
	default:
		return "NON_RETRYABLE"
	}
}

// IsRetryable returns true if the failure class represents a transient or recoverable state.
func (f FailureClass) IsRetryable() bool {
	switch f {
	case FailureClassAuthExpired,
		FailureClassQuotaExhausted,
		FailureClassTransientOverload,
		FailureClassTransportClosed,
		FailureClassFirstTokenTimeout,
		FailureClassTransientCLI:
		return true
	default:
		return false
	}
}

// ClassifyFailure inspects the provided error and maps it into a deterministic FailureClass.
//
// @purpose Deterministically categorizes errors from ACP subprocesses, HTTP proxies, and CLI executions.
// @rationale Decouples recovery logic (rotation, backoff, recycling) from arbitrary error string shapes.
// @preconditions err may be nil or any typed Go error.
// - @param: err The error to evaluate and classify.
// @returns The matching FailureClass enum value.
// @throws Never panics or returns typed errors.
// @invariants Operates in O(1) string pattern checks without heap allocations where possible.
func ClassifyFailure(err error) FailureClass {
	if err == nil {
		return FailureClassNone
	}

	// 1. Check for client cancellation (strictly non-retryable)
	if errors.Is(err, context.Canceled) {
		return FailureClassNonRetryable
	}

	// 2. Check for typed ACP ResponseError
	var respErr *acp.ResponseError
	if errors.As(err, &respErr) {
		if respErr.Code == acp.ErrCodeAuthRequired {
			return FailureClassAuthExpired
		}
	}

	// 3. Check for typed network / syscall errors
	if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return FailureClassTransportClosed
	}
	var netOpErr *net.OpError
	if errors.As(err, &netOpErr) {
		if netOpErr.Timeout() {
			return FailureClassTransientOverload
		}
		return FailureClassTransportClosed
	}

	// 4. String inspection for upstream ACP & API messages (case-insensitive)
	lowerMsg := strings.ToLower(err.Error())

	// Auth expired indicators
	if strings.Contains(lowerMsg, "authentication required") ||
		strings.Contains(lowerMsg, "unauthorized") ||
		strings.Contains(lowerMsg, "invalid_grant") ||
		strings.Contains(lowerMsg, "session expired") ||
		strings.Contains(lowerMsg, "token expired") ||
		strings.Contains(lowerMsg, "keyring authorization failed") ||
		strings.Contains(lowerMsg, "account_auth_required") {
		return FailureClassAuthExpired
	}

	// Quota exhaustion & rate limiting indicators
	if strings.Contains(lowerMsg, "resource_exhausted") ||
		strings.Contains(lowerMsg, "resource has been exhausted") ||
		strings.Contains(lowerMsg, "quota exceeded") ||
		strings.Contains(lowerMsg, "rate limit exceeded") ||
		strings.Contains(lowerMsg, "ratelimiterror") ||
		strings.Contains(lowerMsg, "too many requests") ||
		strings.Contains(lowerMsg, "429") {
		return FailureClassQuotaExhausted
	}

	// TTFT Watchdog timeout indicator
	if strings.Contains(lowerMsg, "first_token_timeout") ||
		strings.Contains(lowerMsg, "time to first token exceeded") {
		return FailureClassFirstTokenTimeout
	}

	// Transient server overload & 5xx indicators
	if strings.Contains(lowerMsg, "model is overloaded") ||
		strings.Contains(lowerMsg, "service unavailable") ||
		strings.Contains(lowerMsg, "server is temporarily unavailable") ||
		strings.Contains(lowerMsg, "internal error encountered") ||
		strings.Contains(lowerMsg, "503") ||
		strings.Contains(lowerMsg, "502 bad gateway") ||
		strings.Contains(lowerMsg, "bad gateway") {
		return FailureClassTransientOverload
	}

	// Subprocess pipe & transport drops
	if strings.Contains(lowerMsg, "transport is closed") ||
		strings.Contains(lowerMsg, "broken pipe") ||
		strings.Contains(lowerMsg, "connection reset by peer") ||
		strings.Contains(lowerMsg, "unexpected eof") {
		return FailureClassTransportClosed
	}

	// CLI SQLite busy lock contention & fork pressure
	if strings.Contains(lowerMsg, "database is locked") ||
		strings.Contains(lowerMsg, "sqlite_busy") ||
		strings.Contains(lowerMsg, "resource temporarily unavailable") {
		return FailureClassTransientCLI
	}

	return FailureClassNonRetryable
}
