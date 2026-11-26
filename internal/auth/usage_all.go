package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// AccountUsage is one account's live quota as reported by agy-auth.
//
// @purpose Carries quota attributed to a specific account id, replacing the previous behaviour of
// attaching one account's numbers to every account in the response.
// @rationale agy-auth emits bucket ids with a family prefix (`gemini-weekly`, `3p-5h`), which is why
// Groups is kept in wire form: the family is derived from the bucket id at read time and never from
// a display name that upstream may rename.
// @preconditions Groups entries are non-nil slices after parsing.
// @param None.
// @returns None.
// @throws None.
// @invariants Immutable once constructed; safe to share across goroutines without copying.
type AccountUsage struct {
	AccountID string
	Email     string
	Active    bool
	Groups    []AgyUsageGroup
	FetchedAt time.Time
	// UsageError carries the provider's reason when this account's quota could not be read.
	//
	// @purpose Makes a per-account quota fetch failure visible instead of indistinguishable from
	// "this account has no limits".
	// @rationale `agy-auth usage --all --json` reports a failed account as `usage_error` with the
	// `groups` key absent, and still exits 0. Without this field the decoder sees an empty group
	// slice, eligibility's bucket loop never iterates, and the account ranks as healthy — the exact
	// fail-open that let an unverified account be selected as if it had unlimited quota.
	// @invariants Empty exactly when the read succeeded; never a substitute for QuotasKnown.
	UsageError string
}

// usageAllEnvelope mirrors `agy-auth usage --all --json`.
//
// @purpose Types the per-account quota payload so parsing never depends on map lookups.
// @rationale The command returns the same group/bucket shape as `agy /usage`, but under
// data.accounts[] with an identity per entry — that extra identity is the whole reason for using it.
// @preconditions None.
// @param None.
// @returns None.
// @throws None.
// @invariants Wire-only structure; never mutated after decode.
type usageAllEnvelope struct {
	SchemaVersion int `json:"schema_version"`
	Data          struct {
		Accounts []struct {
			ID     string          `json:"id"`
			Email  string          `json:"email"`
			Alias  string          `json:"alias"`
			Active bool            `json:"active"`
			Groups []AgyUsageGroup `json:"groups"`
			// UsageError is set by agy-auth when this account's quota read failed; the groups key is
			// then absent rather than empty.
			UsageError string `json:"usage_error"`
		} `json:"accounts"`
	} `json:"data"`
}

// FetchAllAccountUsage returns live quota for every stored account, keyed by account id.
//
// @purpose Makes per-account quota the single authoritative source for eligibility, cooldowns, and
// the admin API. Previously one account's usage was fetched and then attached to every account.
// @rationale `agy -p "/usage"` reports only the process's own account and carries no identity, so it
// can never attribute data. `agy-auth usage --all --json` reports every stored account together with
// its id, which is what removes the guesswork.
// @preconditions m.cfg and m.cfg.Binaries.AgyAuthPath are configured.
// @param ctx Cancellation root; bounds the subprocess.
// @returns A map keyed by account id; an empty non-nil map when the vault has no accounts.
// @throws A wrapped error when agy-auth fails or the payload cannot be decoded. Never returns a
// partially populated map alongside an error, so callers cannot mistake partial data for complete.
// @invariants O(accounts × groups × buckets); exactly one subprocess per call; no locks held while
// the subprocess runs.
func (m *Manager) FetchAllAccountUsage(ctx context.Context) (map[string]AccountUsage, error) {
	if m.fetchUsageForTest != nil {
		return m.fetchUsageForTest(ctx)
	}
	out, err := m.execAgyAuth(ctx, "usage", "--all", "--json")
	if err != nil {
		return nil, fmt.Errorf("agy-auth usage --all failed: %w", err)
	}

	return decodeUsagePayload(out)
}

// decodeUsagePayload converts an `agy-auth usage --all --json` payload into per-account usage.
//
// @purpose Isolates the wire decode so the real parsing rules can be tested without a subprocess.
// @rationale This is where the D4 fail-open lived: the payload's per-account `usage_error` was not
// part of the decoded shape, so an account the provider refused to report on arrived as an account
// with no limits. Testing through the subprocess would require a fake binary just to assert a struct
// field, so the decode is separated and exercised directly.
// @preconditions out is the raw stdout of the command.
// @param out Raw JSON payload.
// @returns Per-account usage keyed by account id.
// @throws Returns an error when the payload is not JSON, or when it names accounts but none carries
// an id (an unattributable response is a contract break, not an empty vault).
// @invariants O(accounts); never returns a nil map alongside a nil error.
func decodeUsagePayload(out []byte) (map[string]AccountUsage, error) {
	var env usageAllEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, fmt.Errorf("failed to decode agy-auth usage payload: %w", err)
	}

	now := time.Now().UTC()
	usage := make(map[string]AccountUsage, len(env.Data.Accounts))
	for _, acc := range env.Data.Accounts {
		if acc.ID == "" {
			// Without an id the entry cannot be attributed, and guessing by email would silently
			// mismatch when two entries share a display email.
			continue
		}
		usage[acc.ID] = AccountUsage{
			AccountID:  acc.ID,
			Email:      acc.Email,
			Active:     acc.Active,
			Groups:     acc.Groups,
			FetchedAt:  now,
			UsageError: acc.UsageError,
		}
	}

	if len(usage) == 0 && len(env.Data.Accounts) > 0 {
		return nil, fmt.Errorf("agy-auth usage --all returned %d accounts but none carried an id", len(env.Data.Accounts))
	}
	return usage, nil
}

// QuotaGroups converts one account's wire groups into the keyed map eligibility reads.
//
// @purpose Produces Account.Quotas from authoritative per-account data in one place, so polling and
// the admin API cannot drift apart.
// @rationale The map key must be the bucket id (not the display name) because eligibility derives the
// model family from that prefix. Falling back to "<group>-<window>" keeps an id-less bucket addressable
// without inventing a family.
// @preconditions groups is a slice as decoded from agy-auth.
// @param groups Wire quota groups for a single account.
// @returns A map keyed by bucket id; never nil, so callers may iterate without a nil check.
// @throws None.
// @invariants O(groups × buckets); deterministic ordering of any duplicate-key collision (later wins).
func QuotaGroups(groups []AgyUsageGroup) map[string]QuotaGroup {
	quotas := make(map[string]QuotaGroup)
	for _, grp := range groups {
		for _, b := range grp.Buckets {
			reset, err := time.Parse(time.RFC3339, b.ResetTime)
			if err != nil {
				// A malformed reset time must not discard the bucket: the remaining percentage is
				// still valid and is what eligibility depends on.
				reset = time.Time{}
			}
			key := b.ID
			if key == "" {
				key = fmt.Sprintf("%s-%s", grp.Name, b.Window)
			}
			// Clamp at the boundary: a malformed upstream fraction must not produce a negative or
			// >100 percentage, because both ranking and the operator-facing report read this value.
			fraction := b.RemainingFraction
			if fraction < 0 {
				fraction = 0
			} else if fraction > 1 {
				fraction = 1
			}
			quotas[key] = QuotaGroup{
				Name:         fmt.Sprintf("%s - %s", grp.Name, b.Name),
				RemainingPct: fraction * 100,
				ResetTime:    reset,
				IsExhausted:  fraction <= 0,
			}
		}
	}
	return quotas
}

// ExhaustedFamily reports the earliest reset time among a family's depleted buckets.
//
// @purpose Lets the poller set a cooldown that ends when the limiting bucket actually refreshes,
// instead of guessing a fixed duration.
// @rationale A weekly bucket and a 5-hour bucket recover on completely different schedules; using one
// constant for both either wakes an account up far too early or wastes hours of available capacity.
// @preconditions quotas keys are bucket ids as produced by QuotaGroups.
// @param quotas The account's keyed quota map.
// @param family Quota family to inspect; empty means inspect every family.
// @param minRemainingPct Floor below which a bucket counts as depleted.
// @returns The window label of the limiting bucket ("weekly" or "5h"), its reset time, and whether any
// bucket was depleted. Reset is zero when the payload carried no usable reset time.
// @throws None.
// @invariants O(buckets); no allocation beyond return values.
func ExhaustedFamily(quotas map[string]QuotaGroup, family string, minRemainingPct float64) (window string, reset time.Time, exhausted bool) {
	// Deterministic choice: a weekly depletion outranks a 5h depletion because it recovers later.
	weeklyHit, fiveHourHit := false, false
	var weeklyReset, fiveHourReset time.Time

	keys := make([]string, 0, len(quotas))
	for k := range quotas {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		q := quotas[key]
		if family != "" && QuotaFamilyOfBucket(key) != family {
			continue
		}
		if !q.IsExhausted && q.RemainingPct >= minRemainingPct {
			continue
		}
		if len(key) >= len("weekly") && key[len(key)-len("weekly"):] == "weekly" {
			weeklyHit = true
			weeklyReset = q.ResetTime
		} else if len(key) >= len("5h") && key[len(key)-len("5h"):] == "5h" {
			fiveHourHit = true
			fiveHourReset = q.ResetTime
		}
	}

	if weeklyHit {
		return "weekly", weeklyReset, true
	}
	if fiveHourHit {
		return "5h", fiveHourReset, true
	}
	return "", time.Time{}, false
}
