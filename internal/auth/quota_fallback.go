package auth

import (
	"context"
	"sort"
	"strings"
)

// quotaFallbackCodes are the `usage_error` values for which asking `agy` directly can succeed.
//
// @purpose Scopes the expensive fallback to the failures it can actually fix.
// @rationale Measured: `agy-auth usage` answers `forbidden` for an account whose quota is perfectly
// readable by `agy -p /usage`; upstream documents `unsupported` and `invalid_response` as the same
// class ("the command cannot see the quota"). The credential-fault codes (`missing_auth`, `auth`,
// `refresh_failed`) mean the token itself is unusable, so a second subprocess would fail identically
// and only add latency. `timeout`/`network`/`http_*` describe transport, which `agy` shares.
// @preconditions None.
// @param code The `usage_error` string as reported by agy-auth.
// @returns True when the `agy /usage` fallback is worth attempting.
// @throws None.
// @invariants O(len(code)); no allocation beyond the lowercase comparison.
func fallbackEligible(code string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "forbidden", "unsupported", "invalid_response":
		return true
	}
	return false
}

// ResolveQuotaFallbacks fetches quota with `agy` for every account `agy-auth usage` refused.
//
// @purpose Recovers real quota data for accounts that `agy-auth` reports as unreadable but `agy`
// reports on normally — the only path by which a Free account can ever be cooled, and the reason the
// admin quota view agrees with the poller instead of contradicting it.
// @rationale Runs OUTSIDE accountsMu because each attempt spawns a subprocess. Bounded per call by the
// number of accounts actually carrying a fallback-eligible error, which is normally zero.
// @preconditions None.
// @param ctx Cancellation root; a cancelled poll abandons remaining fallbacks.
// @param usage Per-account usage from `agy-auth usage --all`.
// @returns Groups keyed by account id for every account the fallback read successfully.
// @throws None; each failure is logged and simply omits that account.
// @invariants At most one subprocess per refused account per poll; no lock held across a subprocess;
// scratch HOMEs are removed by WithScratchHome on every path.
func (m *Manager) ResolveQuotaFallbacks(ctx context.Context, usage map[string]AccountUsage) map[string][]AgyUsageGroup {
	var refused []string
	for id, live := range usage {
		if live.UsageError != "" && fallbackEligible(live.UsageError) {
			refused = append(refused, id)
		}
	}
	if len(refused) == 0 {
		return nil
	}
	// Deterministic order keeps log output and test expectations stable.
	sort.Strings(refused)

	out := make(map[string][]AgyUsageGroup, len(refused))
	for _, id := range refused {
		if err := ctx.Err(); err != nil {
			return out
		}
		groups, err := m.fetchQuotaViaAgy(ctx, id)
		if err != nil {
			m.logger.Debug("agy /usage fallback failed for account", "accountID", id, "err", err)
			continue
		}
		if len(groups) > 0 {
			out[id] = groups
		}
	}
	return out
}

// fetchQuotaViaAgy reads one account's quota by borrowing its credential in a scratch HOME.
//
// @purpose Turns `agy -p /usage` into per-account data, which `agy-auth usage` cannot produce for
// every account.
// @rationale `agy /usage` reports the bucket groups for the account the process is signed in as, with
// no identity in the payload. Borrowing the credential per account is therefore the only way to
// attribute its numbers; doing it in a scratch HOME avoids disturbing any worker's live credential.
// @preconditions cfg is configured and the account exists in the vault.
// @param ctx Cancellation root for the export and the usage subprocess.
// @param accountID Account whose quota should be read.
// @returns The bucket groups from `command.data.groups`.
// @throws A wrapped error when the credential cannot be borrowed or the payload cannot be decoded.
// @invariants Exactly one export and one `agy` subprocess; no lock held; scratch tree always removed.
func (m *Manager) fetchQuotaViaAgy(ctx context.Context, accountID string) ([]AgyUsageGroup, error) {
	var groups []AgyUsageGroup
	err := m.WithScratchHome(ctx, accountID, func(homeDir string) error {
		env, err := m.FetchAgyUsage(ctx, homeDir)
		if err != nil {
			return err
		}
		groups = env.Command.Data.Groups
		return nil
	})
	if err != nil {
		return nil, err
	}
	return groups, nil
}
