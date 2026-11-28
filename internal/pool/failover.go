package pool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"agy-gateway/internal/auth"
	"agy-gateway/internal/retry"
)

// ErrNonRetryableStream indicates that tokens have already been emitted to the client stream,
// making transparent failover prohibited to prevent duplicated or conflicting tokens.
var ErrNonRetryableStream = errors.New("stream already started; failover prohibited")

// zeroTokenError marks a turn that failed without producing any output.
//
// @purpose Lets the failover path tell "the agent refused before doing any work" apart from "the turn
// ran and then hit a quota wall", which need different account penalties.
// @rationale The signal is produced by the executor, which knows the output counts, and consumed by
// the coordinator, which knows the account state. A wrapper keeps that knowledge flowing through a
// generic interface without widening ExecuteWithFailover's signature.
// @invariants Unwraps to the original error, so classification and `errors.Is` are unaffected.
type zeroTokenError struct{ err error }

func (e *zeroTokenError) Error() string { return e.err.Error() }
func (e *zeroTokenError) Unwrap() error { return e.err }

// MarkZeroTokens wraps err to record that the turn produced no text, thoughts or tool calls.
//
// @purpose Records an output-count fact alongside the failure.
// @rationale See zeroTokenError.
// @preconditions err is non-nil.
// @param err The attempt failure.
// @returns err wrapped, or err unchanged when it is nil.
// @throws None.
// @invariants Idempotent; preserves errors.Is/errors.As through Unwrap.
func MarkZeroTokens(err error) error {
	if err == nil || IsZeroTokens(err) {
		return err
	}
	return &zeroTokenError{err: err}
}

// IsZeroTokens reports whether err was marked as producing no output.
//
// @purpose Failover guard predicate.
// @param err Candidate error.
// @returns true when err wraps a zero-token marker.
// @throws None.
// @invariants O(1).
func IsZeroTokens(err error) bool {
	var target *zeroTokenError
	return errors.As(err, &target)
}

// FailoverCoordinator handles transparent pre-first-byte retries, multi-account failover,
// and exponential backoff with full jitter across worker lease attempts.
type FailoverCoordinator struct {
	pool    *Pool
	authMgr *auth.Manager
	logger  *slog.Logger
}

// NewFailoverCoordinator creates an initialized FailoverCoordinator.
func NewFailoverCoordinator(pool *Pool, authMgr *auth.Manager, logger *slog.Logger) *FailoverCoordinator {
	if logger == nil {
		logger = slog.Default()
	}
	return &FailoverCoordinator{
		pool:    pool,
		authMgr: authMgr,
		logger:  logger,
	}
}

// NextAvailableIn reports how long until some account can serve requests again.
//
// @purpose Supplies an honest Retry-After value when every account is unavailable.
// @rationale A 429 without a hint forces the client to guess; the earliest cooldown expiry is known
// precisely and is the most useful thing the gateway can tell it. CooldownUntil also carries the
// bounded retry deadline of a runtime credential fault, which is equally worth waiting for, so the
// hint stays correct for both cases.
// @preconditions None; safe to call with a nil coordinator or nil auth manager.
// @param now Reference instant, injected so the value is testable.
// @returns The duration until the earliest known cooldown expiry, or zero when unknown/expired.
// @throws None.
// @invariants O(accounts); takes no subprocess and no filesystem action.
func (fc *FailoverCoordinator) NextAvailableIn(now time.Time) time.Duration {
	if fc == nil || fc.authMgr == nil {
		return 0
	}
	var earliest time.Time
	for _, acc := range fc.authMgr.GetAccounts() {
		if acc.CooldownUntil.After(now) && (earliest.IsZero() || acc.CooldownUntil.Before(earliest)) {
			earliest = acc.CooldownUntil
		}
	}
	if earliest.IsZero() {
		return 0
	}
	return earliest.Sub(now)
}

// coolRefusedAccount records that one account cannot serve a model until its quota returns.
//
// @purpose Applies the cooldown implied by a quota failure, scoped to the family that was refused.
// @rationale Two facts decide the window: the family of the requested model (so a spent Gemini bucket
// does not bench working Claude/GPT capacity) and, when the account stated it, the upstream's own
// reset time (so a six-day weekly exhaustion is not retried twice an hour). It lives in one place
// because both the retryable path and the budget-exhausted path must apply it; applying it only on the
// retry path left the account HEALTHY whenever the last attempt failed, and every later request leased
// it again.
// @preconditions w is leased and non-nil.
// @param w Worker that received the refusal.
// @param model Model that was requested; empty means the family cannot be attributed.
// @param err The classified failure, inspected for a stated reset window.
// @returns None; mutates manager state and taints the worker.
// @throws None.
// @invariants O(accounts) to cool, O(1) otherwise; never spawns a subprocess.
func (fc *FailoverCoordinator) coolRefusedAccount(w *Worker, model string, err error) {
	if w == nil {
		return
	}
	currentAccID := ""
	if bound := w.GetAccount(); bound != nil {
		currentAccID = bound.ID
	}
	if currentAccID == "" || fc.authMgr == nil {
		return
	}

	// Duration comes from configuration; a literal here silently drifted from the operator's setting
	// and could keep an account out of service far longer than its real reset.
	cooldownDuration := 30 * time.Minute
	if fc.pool != nil && fc.pool.cfg != nil && fc.pool.cfg.Accounts.DefaultCooldown5h > 0 {
		cooldownDuration = fc.pool.cfg.Accounts.DefaultCooldown5h
	}
	// When the account named its own reset window, use it: it is the moment capacity actually returns.
	if errors.Is(err, retry.ErrQuotaRefusal) {
		var resetErr interface{ ResetWindow() (time.Duration, bool) }
		if errors.As(err, &resetErr) {
			if reset, ok := resetErr.ResetWindow(); ok && reset > cooldownDuration {
				cooldownDuration = reset
			}
		}
	}

	if family := auth.QuotaFamilyForModel(model); family != "" {
		fc.authMgr.MarkFamilyCooldown(currentAccID, family, time.Now().Add(cooldownDuration))
	} else {
		fc.authMgr.MarkAccountStatus(currentAccID, auth.StatusCooldown5h, cooldownDuration)
	}
	w.TaintedForRecycle.Store(true)
}

// hasAlternativeAccount reports whether any account other than the worker's own could serve the model.
//
// @purpose Decides between rotating and answering 429 immediately.
// @rationale When the last account is cooling, continuing the retry loop re-leases an account that
// cannot serve the request: a guaranteed failure that costs a subprocess per attempt and delays the
// client's honest answer.
// @preconditions None; safe with a nil manager.
// @param w Worker whose current account must be excluded.
// @param model Requested model.
// @param requiresPaidTier Catalog-derived tier requirement.
// @returns true when a different eligible account exists.
// @throws None.
// @invariants O(accounts); never spawns a subprocess.
func (fc *FailoverCoordinator) hasAlternativeAccount(w *Worker, model string, requiresPaidTier bool) bool {
	if fc.authMgr == nil {
		return false
	}
	currentID := ""
	if w != nil {
		if bound := w.GetAccount(); bound != nil {
			currentID = bound.ID
		}
	}
	next, err := fc.authMgr.FindNextEligibleAccount(currentID, model, requiresPaidTier, fc.pool.accountsInUse(w))
	return err == nil && next != nil
}

// ExecuteWithFailover leases a worker, attempts the operation, and if a recoverable
// failure (e.g. auth expired, quota exhausted, model overloaded, pipe closed, TTFT timeout)
// occurs pre-first-token, intelligently switches accounts, applies jittered backoff, and retries.
//
// @purpose Coordinates resilient inference turn executions across transient failures and multi-account rotations.
// @rationale Ensures zero-semantic-loss for API callers by hiding transient Google API glitches and quota limits.
// @preconditions ctx must be active; maxRetries >= 0; op must not be nil.
// - @param: ctx Context governing overall operation lifetime.
// - @param: requiresPaidTier True if the model requires a paid Google tier.
// - @param: maxRetries Maximum number of retry attempts before giving up.
// - @param: op The closure executing the turn with the leased worker.
// @returns Nil on successful completion, or the terminating error.
// @throws Returns typed errors if all retries are exhausted or non-retryable failure occurs.
// @invariants Releases leased workers on every code path; guarantees zero worker leakage.
func (fc *FailoverCoordinator) ExecuteWithFailover(
	ctx context.Context,
	model string,
	requiresPaidTier bool,
	maxRetries int,
	op func(ctx context.Context, w *Worker) error,
) error {
	if maxRetries < 0 {
		maxRetries = 0
	}

	initialBackoff := 500 * time.Millisecond
	maxBackoff := 10 * time.Second
	if fc.pool != nil && fc.pool.cfg != nil {
		if fc.pool.cfg.Execution.RetryInitialBackoff > 0 {
			initialBackoff = fc.pool.cfg.Execution.RetryInitialBackoff
		}
		if fc.pool.cfg.Execution.RetryMaxBackoff > 0 {
			maxBackoff = fc.pool.cfg.Execution.RetryMaxBackoff
		}
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		// Check context before leasing
		if ctx.Err() != nil {
			return ctx.Err()
		}

		w, err := fc.pool.Acquire(ctx, model, requiresPaidTier)
		if err != nil {
			return fmt.Errorf("failed to acquire worker from pool: %w", err)
		}

		err = op(ctx, w)
		if err == nil {
			fc.pool.Release(w)
			return nil
		}

		lastErr = err

		// If mid-stream tokens were already sent, retry is strictly prohibited
		if errors.Is(err, ErrNonRetryableStream) {
			fc.pool.Release(w)
			fc.logger.Warn("non-retryable stream error encountered; aborting failover", "err", err)
			return err
		}

		failureClass := retry.ClassifyFailure(err)
		fc.logger.Warn("worker turn operation failed",
			"attempt", attempt+1,
			"maxRetries", maxRetries,
			"workerID", w.ID,
			"failureClass", failureClass.String(),
			"err", err,
		)

		// A quota refusal the account stated itself must be recorded even when the retry budget is
		// already spent. The gate below used to return first, which left an exhausted account marked
		// HEALTHY: every later request leased it again, paid for another failing turn, and the client
		// got a generic 500 instead of the honest 429. A stated refusal therefore always gets one pass
		// through the switch so the cooldown is applied; the loop then ends naturally because the next
		// Acquire finds no eligible account.
		statedQuotaRefusal := failureClass == retry.FailureClassQuotaExhausted &&
			errors.Is(err, retry.ErrQuotaRefusal)

		if !failureClass.IsRetryable() || (attempt >= maxRetries && !statedQuotaRefusal) {
			fc.pool.Release(w)
			return err
		}

		var currentAccID string
		if bound := w.GetAccount(); bound != nil {
			currentAccID = bound.ID
		}

		switch failureClass {
		case retry.FailureClassAuthExpired:
			if currentAccID != "" && fc.authMgr != nil {
				// Bounded retry, not a tombstone: a momentary upstream rejection must not remove the
				// account until an operator logs in again.
				fc.authMgr.MarkAccountStatus(currentAccID, auth.StatusAuthExpired,
					fc.pool.cfg.Accounts.AuthExpiredRetryInterval)
			}
			w.TaintedForRecycle.Store(true)

			// Check for alternate eligible account in vault
			if fc.authMgr != nil {
				nextAcc, findErr := fc.authMgr.FindNextEligibleAccount(currentAccID, model, requiresPaidTier, fc.pool.accountsInUse(w))
				if findErr == nil && nextAcc != nil {
					fc.logger.Info("auth expired; auto-switching worker to alternate account",
						"workerID", w.ID,
						"fromAccount", currentAccID,
						"toAccount", nextAcc.ID,
					)
					switchErr := fc.authMgr.SwitchWorkerAccount(ctx, w.ID, nextAcc.ID)
					if switchErr != nil {
						// Publication asserts "this worker holds this credential". Writing it after a
						// failed install would make the admin view and the sticky cache claim an account
						// that was never materialised, and the next lease's fast path would then accept
						// that binding without re-materialising — so a failed switch must not publish.
						fc.logger.Warn("agy-auth switch failed; not publishing the failed binding",
							"workerID", w.ID, "accountID", nextAcc.ID, "err", switchErr)
						fc.pool.Release(w)
						_ = retry.SleepWithContext(ctx, 30*time.Millisecond)
						continue
					}
					// Failover rebinds the worker exactly like a lease-time rotation does. Publication
					// reconciles both `assigned_worker` and the sticky cache from the live worker set,
					// so the pre-failover account is released as a side effect of SetAccount.
					w.SetAccount(nextAcc)
					fc.pool.publishBinding(w.ID)
					fc.pool.Release(w)
					_ = retry.SleepWithContext(ctx, 30*time.Millisecond)
					continue
				}
			}

			fc.pool.Release(w)
			delay := retry.ComputeBackoff(attempt, initialBackoff, maxBackoff)
			if sleepErr := retry.SleepWithContext(ctx, delay); sleepErr != nil {
				return sleepErr
			}

		case retry.FailureClassQuotaExhausted:
			// A quota refusal the account stated itself is authoritative: the upstream already said this
			// credential cannot serve the request, whatever the last measured snapshot claims. Trusting a
			// stale percentage over the refusal is exactly how the client ended up seeing the upsell.
			statedRefusal := errors.Is(err, retry.ErrQuotaRefusal)

			// A 429 that arrives while the account still has measured capacity is a refusal, not
			// exhaustion. Cooling the credential and recycling the worker only shrinks the pool.
			if !statedRefusal && currentAccID != "" && fc.authMgr != nil && IsZeroTokens(err) {
				if pct, known := fc.authMgr.KnownQuotaPct(currentAccID); known &&
					fc.pool != nil && fc.pool.cfg != nil &&
					pct >= fc.pool.cfg.Accounts.MinRemainingQuotaPct {
					fc.logger.Warn("upstream 429 with healthy quota; not cooling the account",
						"workerID", w.ID, "accountID", currentAccID, "quotaPct", pct)
					fc.pool.Release(w)
					return fmt.Errorf("upstream refused the turn with healthy measured quota (%.1f%% remaining); account not cooled", pct)
				}
			}

			fc.coolRefusedAccount(w, model, err)

			// Ultra-smart auto-switch to alternate account if available
			if fc.authMgr != nil {
				nextAcc, findErr := fc.authMgr.FindNextEligibleAccount(currentAccID, model, requiresPaidTier, fc.pool.accountsInUse(w))
				if findErr == nil && nextAcc != nil {
					fc.logger.Info("quota exhausted; auto-switching worker to alternate account",
						"workerID", w.ID,
						"fromAccount", currentAccID,
						"toAccount", nextAcc.ID,
					)
					switchErr := fc.authMgr.SwitchWorkerAccount(ctx, w.ID, nextAcc.ID)
					if switchErr != nil {
						// Publication asserts "this worker holds this credential". Writing it after a
						// failed install would make the admin view and the sticky cache claim an account
						// that was never materialised, and the next lease's fast path would then accept
						// that binding without re-materialising — so a failed switch must not publish.
						fc.logger.Warn("agy-auth switch failed; not publishing the failed binding",
							"workerID", w.ID, "accountID", nextAcc.ID, "err", switchErr)
						fc.pool.Release(w)
						_ = retry.SleepWithContext(ctx, 30*time.Millisecond)
						continue
					}
					// Failover rebinds the worker exactly like a lease-time rotation does. Publication
					// reconciles both `assigned_worker` and the sticky cache from the live worker set,
					// so the pre-failover account is released as a side effect of SetAccount.
					w.SetAccount(nextAcc)
					fc.pool.publishBinding(w.ID)
					fc.pool.Release(w)
					_ = retry.SleepWithContext(ctx, 30*time.Millisecond)
					continue
				}
			}

			fc.pool.Release(w)

			// Every account that could serve the model is now cooling, so another attempt would lease
			// one of them and fail again. Answer once with the honest 429: the handler derives Retry-After
			// from the earliest reset that cooling just recorded.
			if attempt >= maxRetries || !fc.hasAlternativeAccount(w, model, requiresPaidTier) {
				fc.logger.Warn("all accounts are out of quota; answering without another attempt",
					"workerID", w.ID, "attempt", attempt+1, "maxRetries", maxRetries,
					"statedRefusal", statedRefusal)
				return fmt.Errorf("%w: %v", ErrNoEligibleAccounts, err)
			}

			delay := retry.ComputeBackoff(attempt, initialBackoff, maxBackoff)
			if sleepErr := retry.SleepWithContext(ctx, delay); sleepErr != nil {
				return sleepErr
			}

		case retry.FailureClassTransportClosed, retry.FailureClassFirstTokenTimeout:
			w.TaintedForRecycle.Store(true)
			fc.pool.Release(w)
			delay := retry.ComputeBackoff(attempt, initialBackoff, maxBackoff)
			if sleepErr := retry.SleepWithContext(ctx, delay); sleepErr != nil {
				return sleepErr
			}

		case retry.FailureClassTransientOverload:
			fc.pool.Release(w)
			delay := retry.ComputeBackoff(attempt, initialBackoff, maxBackoff)
			if sleepErr := retry.SleepWithContext(ctx, delay); sleepErr != nil {
				return sleepErr
			}

		default:
			fc.pool.Release(w)
			return err
		}
	}

	return lastErr
}
