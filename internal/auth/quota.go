package auth

import (
	"context"
	"log/slog"
	"time"

	"agy-gateway/internal/config"
)

// QuotaPoller periodically refreshes account quota data and transitions accounts into cooldown states.
type QuotaPoller struct {
	mgr    *Manager
	cfg    *config.Config
	logger *slog.Logger
	stopCh chan struct{}
}

// NewQuotaPoller initializes a QuotaPoller.
func NewQuotaPoller(mgr *Manager, cfg *config.Config, logger *slog.Logger) *QuotaPoller {
	if logger == nil {
		logger = slog.Default()
	}
	return &QuotaPoller{
		mgr:    mgr,
		cfg:    cfg,
		logger: logger,
		stopCh: make(chan struct{}),
	}
}

// Start launches the background quota polling loop.
func (qp *QuotaPoller) Start(ctx context.Context) {
	interval := qp.cfg.Accounts.QuotaPollInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-qp.stopCh:
				return
			case <-ticker.C:
				qp.Poll(ctx)
			}
		}
	}()
}

// Stop cleanly terminates the polling loop.
func (qp *QuotaPoller) Stop() {
	close(qp.stopCh)
}

// Poll runs a single quota inspection turn and updates account quotas & cooldowns based on live agy /usage data.
func (qp *QuotaPoller) Poll(ctx context.Context) {
	res, err := qp.mgr.GetQuota(ctx, "")
	if err != nil {
		qp.logger.Debug("quota poll check skipped/failed", "err", err)
		return
	}

	qp.mgr.accountsMu.Lock()
	defer qp.mgr.accountsMu.Unlock()

	for i := range qp.mgr.accounts {
		acc := &qp.mgr.accounts[i]
		for _, qInfo := range res.Accounts {
			if qInfo.ID == acc.ID {
				acc.Quotas = qInfo.Quotas

				// Evaluate bucket exhaustion
				has5hExhausted := false
				var reset5h time.Time
				hasWeeklyExhausted := false
				var resetWeekly time.Time

				for _, grp := range qInfo.Groups {
					for _, b := range grp.Buckets {
						if b.IsExhausted || b.RemainingPct <= qp.cfg.Accounts.MinRemainingQuotaPct {
							if b.Window == "5h" {
								has5hExhausted = true
								reset5h = b.ResetTime
							} else if b.Window == "weekly" {
								hasWeeklyExhausted = true
								resetWeekly = b.ResetTime
							}
						}
					}
				}

				if hasWeeklyExhausted {
					acc.Status = StatusCooldownWeekly
					acc.CooldownUntil = resetWeekly
				} else if has5hExhausted {
					acc.Status = StatusCooldown5h
					acc.CooldownUntil = reset5h
				} else if acc.Status == StatusCooldown5h || acc.Status == StatusCooldownWeekly {
					acc.Status = StatusHealthy
					acc.CooldownUntil = time.Time{}
				}
				break
			}
		}
	}

	qp.logger.Debug("quota polled successfully", "accounts", len(res.Accounts))
}
