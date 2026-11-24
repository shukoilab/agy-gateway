package retry

import (
	"context"
	"math/rand/v2"
	"time"
)

// ComputeBackoff calculates an exponential backoff duration with full jitter.
//
// Formula: delay = rand(0, min(maxBackoff, initialBackoff * 2^attempt))
//
// @purpose Prevents thundering-herd spikes by spreading retry requests uniformly.
// @rationale Full jitter offers superior desynchronization over decorrelated jitter for distributed clients.
// @preconditions initialBackoff and maxBackoff must be positive durations; attempt >= 0.
// - @param: attempt The 0-based retry iteration count.
// - @param: initialBackoff The baseline backoff duration (e.g. 500ms).
// - @param: maxBackoff The ceiling duration limit (e.g. 10s).
// @returns A randomized duration between 0 and the capped exponential ceiling.
// @throws Never panics; gracefully handles negative or zero inputs.
// @invariants Computed delay is always >= 0 and <= maxBackoff.
func ComputeBackoff(attempt int, initialBackoff, maxBackoff time.Duration) time.Duration {
	if initialBackoff <= 0 {
		initialBackoff = 500 * time.Millisecond
	}
	if maxBackoff < initialBackoff {
		maxBackoff = initialBackoff
	}
	if attempt < 0 {
		attempt = 0
	}

	// Guard against integer overflow when shifting
	var factor int64 = 1
	if attempt > 30 {
		factor = 1 << 30
	} else {
		factor = 1 << attempt
	}

	cappedDelay := int64(initialBackoff) * factor
	if cappedDelay > int64(maxBackoff) || cappedDelay < 0 {
		cappedDelay = int64(maxBackoff)
	}

	if cappedDelay <= 0 {
		return 0
	}

	// Full jitter: uniformly random between [cappedDelay/2, cappedDelay]
	// or [0, cappedDelay]. AWS Full Jitter: rand(0, cappedDelay).
	// To prevent immediate 0ms tight loops, guarantee at least 25ms if cappedDelay >= 50ms.
	jittered := rand.Int64N(cappedDelay)
	minFloor := int64(25 * time.Millisecond)
	if cappedDelay >= minFloor && jittered < minFloor {
		jittered = minFloor
	}

	return time.Duration(jittered)
}

// SleepWithContext pauses execution for the computed backoff duration while respecting context cancellation.
//
// @purpose Implements interruptible backoff delays without goroutine leaks.
// @rationale Returns immediately if the caller's context is cancelled or timed out.
// @preconditions ctx must not be nil.
// - @param: ctx Execution context to monitor for cancellation.
// - @param: d Duration to sleep.
// @returns ctx.Err() if context fired before duration expired, or nil on completed sleep.
// @throws Propagates context.Canceled or context.DeadlineExceeded.
// @invariants Cleans up timer channel handles deterministically.
func SleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
