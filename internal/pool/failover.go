package pool

import (
	"context"
	"errors"
	"log/slog"

	"agy-gateway/internal/acp"
	"agy-gateway/internal/auth"
)

// FailoverCoordinator handles transparent pre-first-byte retries when an account expires.
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

// ExecuteWithFailover leases a worker, attempts the operation, and if an auth error
// (-32000) or quota exhaustion occurs pre-first-byte, marks the account and retries.
func (fc *FailoverCoordinator) ExecuteWithFailover(
	ctx context.Context,
	requiresPaidTier bool,
	maxRetries int,
	op func(ctx context.Context, w *Worker) error,
) error {
	if maxRetries < 0 {
		maxRetries = 0
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		w, err := fc.pool.Acquire(ctx, requiresPaidTier)
		if err != nil {
			return err
		}

		err = op(ctx, w)
		if err == nil {
			fc.pool.Release(w)
			return nil
		}

		fc.pool.Release(w)
		lastErr = err

		// Check for ACP auth expired error (-32000)
		var respErr *acp.ResponseError
		if errors.As(err, &respErr) && respErr.Code == acp.ErrCodeAuthRequired {
			fc.logger.Warn("detected auth required error (-32000); triggering failover",
				"workerID", w.ID,
				"attempt", attempt+1,
			)
			if w.Account != nil {
				w.Account.Status = auth.StatusAuthExpired
			}
			continue
		}

		// Non-recoverable or business logic error, return immediately
		return err
	}

	return lastErr
}
