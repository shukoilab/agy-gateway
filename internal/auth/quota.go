package auth

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"agy-gateway/internal/config"
)

// QuotaPoller periodically refreshes per-account quota and derives account cooldown state.
//
// @purpose Keeps account status truthful so selection can avoid accounts that are already spent,
// before a client request has to discover it.
// @rationale The previous implementation polled one account's usage and applied the same exhausted
// verdict to every account, then only wrote a status field that nothing read. Status is now derived
// from each account's own buckets, and the pool consults it on every lease.
// @invariants Start is idempotent-guarded by callers; Stop is safe to call more than once.
type QuotaPoller struct {
	mgr    *Manager
	cfg    *config.Config
	logger *slog.Logger
	stopCh chan struct{}
	stopOn sync.Once
}

// NewQuotaPoller initializes a QuotaPoller.
//
// @purpose Constructs the poller without starting it.
// @rationale Separating construction from Start lets the caller control lifecycle and shutdown order.
// @preconditions mgr and cfg are non-nil.
// @param mgr Manager providing account state and live quota.
// @param cfg Gateway configuration supplying the interval and thresholds.
// @param logger Structured logger; nil falls back to the default.
// @returns A ready-to-start poller.
// @throws None.
// @invariants O(1); owns no goroutine until Start is called.
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
//
// @purpose Refreshes quota on a fixed interval until the context is cancelled or Stop is called.
// @rationale The first poll runs synchronously before the goroutine starts, so account eligibility is
// already populated by the time the caller initialises the worker pool. Polling only from a ticker
// left the pool running for a full interval with no quota knowledge, during which an already-spent
// account would still be leased.
// @preconditions cfg.Accounts.QuotaPollInterval is either zero (defaults to 30s) or positive.
// @param ctx Cancellation root tied to the process lifetime.
// @returns None; after the initial poll it runs until cancellation.
// @throws None.
// @invariants Exactly one goroutine; the ticker is always stopped; the initial poll is bounded by the
// agy-auth subprocess timeout and never by the poll interval.
func (qp *QuotaPoller) Start(ctx context.Context) {
	interval := qp.cfg.Accounts.QuotaPollInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}

	// Populate eligibility before the pool is built. Callers start the poller first for this reason.
	qp.Poll(ctx)

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
//
// @purpose Signals the loop to exit without racing a concurrent Stop.
// @rationale A bare close would panic on a double Stop, which is exactly what a shutdown path that
// also cancels the context can produce.
// @preconditions None.
// @param None.
// @returns None.
// @throws None; safe to call repeatedly and concurrently.
// @invariants Idempotent; guarantees the channel is closed exactly once.
func (qp *QuotaPoller) Stop() {
	qp.stopOn.Do(func() { close(qp.stopCh) })
}

// Poll runs one quota inspection pass and updates each account's status and cooldown.
//
// @purpose Converts live quota into the account states that worker selection reads.
// @rationale Every account is evaluated against its OWN buckets. Using one account's usage for all of
// them, as before, both hid exhausted accounts and removed healthy ones from service.
// @preconditions m.cfg.Accounts thresholds are validated at startup.
// @param ctx Cancellation root for the agy-auth subprocess.
// @returns None; state changes are applied through Manager.
// @throws None; failures are logged because a poll must never take the gateway down.
// @invariants O(accounts × buckets) plus one subprocess; leaves statuses untouched when the fetch
// fails so no rotation decision is ever based on stale data.
func (qp *QuotaPoller) Poll(ctx context.Context) {
	// Sync every interval, not just while the vault looks empty. An operator can add or remove an
	// account at any time through the CLI, which is a separate process; without a resync the daemon
	// kept serving a deleted account as HEALTHY forever and could lease it again. One `agy-auth list`
	// per poll interval is the price of a truthful account set.
	if err := qp.mgr.Sync(ctx); err != nil {
		// Keep the previous set: an unreadable vault is not evidence that accounts disappeared.
		qp.logger.Warn("vault resync failed; retaining previous account set", "err", err)
	}

	usage, err := qp.mgr.FetchAllAccountUsage(ctx)
	if err != nil {
		// Deliberately no state change: acting on the previous snapshot would rotate accounts on the
		// strength of numbers that may be minutes old.
		qp.logger.Warn("quota poll failed; retaining previous account statuses", "err", err)
		return
	}

	minPct := qp.cfg.Accounts.MinRemainingQuotaPct
	autoRotate := qp.cfg.Accounts.AutoRotateEnabled

	// The `agy /usage` fallback spawns a subprocess, which must never happen while accountsMu is held:
	// the pool reads accounts lock-free on the lease path and would stall behind a ~2 s subprocess on
	// every poll. Resolve all fallbacks first, then apply every result under one short lock.
	fallbacks := qp.mgr.ResolveQuotaFallbacks(ctx, usage)

	qp.mgr.accountsMu.Lock()
	defer qp.mgr.accountsMu.Unlock()

	now := time.Now()
	for i := range qp.mgr.accounts {
		acc := &qp.mgr.accounts[i]
		live, ok := usage[acc.ID]
		if !ok {
			// No live data for this account: leave its status alone rather than guessing.
			continue
		}

		// A failed per-account read is not evidence about the quota. `agy-auth usage --all --json`
		// reports it as `usage_error` with the `groups` key absent, and still exits 0, so this is the
		// only place the failure can be noticed. Keep the previously known map intact (a diagnostic
		// failure must not erase good data) and mark the state unknown so ranking demotes the account
		// instead of silently treating it as unlimited.
		if live.UsageError != "" {
			if groups, recovered := fallbacks[acc.ID]; recovered {
				acc.Quotas = QuotaGroups(groups)
				acc.QuotasKnown = true
				acc.UsageError = ""
				qp.logger.Info("quota recovered via agy /usage fallback",
					"accountID", acc.ID, "groups", len(groups))
			} else {
				acc.QuotasKnown = false
				acc.UsageError = truncateForError([]byte(live.UsageError))
				qp.logger.Warn("quota read failed for account; retaining previous quota data",
					"accountID", acc.ID, "usageError", acc.UsageError)
				// Do not touch acc.Quotas, and do not derive family cooldowns from data we do not have.
				continue
			}
		} else {
			acc.Quotas = QuotaGroups(live.Groups)
			acc.QuotasKnown = true
			acc.UsageError = ""
		}

		// A credential-level fault is owned by the auth layer, not by quota data: refreshing the
		// buckets must not replace AUTH_EXPIRED/DISABLED with a quota summary and thereby resurrect an
		// account the operator or the retry window has taken out of service.
		if acc.Status == StatusAuthExpired || acc.Status == StatusDisabled {
			continue
		}

		if acc.FamilyCooldowns == nil {
			acc.FamilyCooldowns = make(map[string]time.Time, 2)
		}

		// Recover every family whose window has already reset, so capacity returns without waiting for
		// an external trigger.
		for family, until := range acc.FamilyCooldowns {
			if now.After(until) {
				delete(acc.FamilyCooldowns, family)
				qp.logger.Info("account cooldown expired; recovering family to healthy",
					"accountID", acc.ID, "family", family)
			}
		}

		if autoRotate {
			// Judge each family independently. A spent Claude/GPT bucket must not stop this account
			// from serving Gemini traffic, which is the whole point of family-scoped eligibility.
			for _, family := range []string{QuotaFamilyGemini, QuotaFamilyThirdParty} {
				window, reset, exhausted := ExhaustedFamily(acc.Quotas, family, minPct)
				if !exhausted {
					delete(acc.FamilyCooldowns, family)
					continue
				}

				cooldown := qp.cfg.Accounts.DefaultCooldown5h
				if window == "weekly" {
					cooldown = qp.cfg.Accounts.DefaultCooldownWeekly
				}
				// Prefer the bucket's own reset time: it is when capacity actually returns.
				until := now.Add(cooldown)
				if !reset.IsZero() && reset.After(now) {
					until = reset
				}

				if existing, ok := acc.FamilyCooldowns[family]; ok && existing.Equal(until) {
					continue
				}
				acc.FamilyCooldowns[family] = until
				qp.logger.Info("account family marked cooling from live quota",
					"accountID", acc.ID, "family", family, "window", window, "cooldownUntil", until.UTC())
			}
		}

		// Keep Status as a human-readable summary for operators and the admin API. It reflects the
		// furthest-out family cooldown; eligibility never reads it for quota decisions.
		summarizeCooldown(acc)
	}
}

// summarizeCooldown folds per-family cooldowns into the operator-facing Status and CooldownUntil.
//
// @purpose Keeps the admin API and logs meaningful without letting a single spent family hide the
// account's remaining capacity.
// @rationale Status is a summary, not an input to eligibility: `IsEligibleForModel` reads
// FamilyCooldowns. Deriving one from the other would let a summarisation choice silently change
// routing, so the two are deliberately independent.
// @preconditions acc is non-nil and its accountsMu is held.
// @param acc Account whose summary fields are rewritten.
// @returns None; mutates acc.Status and acc.CooldownUntil.
// @throws None.
// @invariants O(number of families); no allocation beyond the fixed loop.
func summarizeCooldown(acc *Account) {
	now := time.Now()
	var worst time.Time
	allFamilies := true

	for _, family := range []string{QuotaFamilyGemini, QuotaFamilyThirdParty} {
		until, ok := acc.FamilyCooldowns[family]
		if !ok || !now.Before(until) {
			allFamilies = false
			continue
		}
		if until.After(worst) {
			worst = until
		}
	}

	switch {
	case worst.IsZero():
		if acc.Status == StatusCooldown5h || acc.Status == StatusCooldownWeekly {
			acc.Status = StatusHealthy
			acc.CooldownUntil = time.Time{}
		}
	case allFamilies:
		// Every family is spent: the account really is unavailable until the later reset.
		acc.CooldownUntil = worst
		if worst.Sub(now) > 24*time.Hour {
			acc.Status = StatusCooldownWeekly
		} else {
			acc.Status = StatusCooldown5h
		}
	default:
		// Partial exhaustion: the account still serves traffic, so it is not globally cooling.
		if acc.Status == StatusCooldown5h || acc.Status == StatusCooldownWeekly {
			acc.Status = StatusHealthy
		}
		acc.CooldownUntil = worst
	}
}
