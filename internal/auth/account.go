// Package auth integrates with @fikrilal/agy-auth CLI to manage Google Antigravity
// credentials, OAuth onboarding, quota cooldowns, and isolated worker HOME environments.
package auth

import (
	"strings"
	"time"
)

// AccountStatus represents the health and quota state of a stored account.
type AccountStatus string

const (
	StatusHealthy        AccountStatus = "HEALTHY"
	StatusCooldown5h     AccountStatus = "COOLDOWN_5H"
	StatusCooldownWeekly AccountStatus = "COOLDOWN_WEEKLY"
	StatusAuthExpired    AccountStatus = "AUTH_EXPIRED"
	StatusDisabled       AccountStatus = "DISABLED"
)

// QuotaGroup represents quota metrics for a specific model group.
type QuotaGroup struct {
	Name         string    `json:"name" example:"Gemini Models"`
	RemainingPct float64   `json:"remaining_pct" example:"70.38"`
	ResetTime    time.Time `json:"reset_time" example:"2026-11-24T17:47:05Z"`
	IsExhausted  bool      `json:"is_exhausted" example:"false"`
}

// Account represents an individual Google Antigravity credential managed by the gateway.
type Account struct {
	ID             string        `json:"id" example:"acc_9f3b18e2"`
	Email          string        `json:"email" example:"dev@shukoilab.com"`
	Alias          string        `json:"alias,omitempty" example:"primary-account"`
	AuthMethod     string        `json:"auth_method" example:"oauth"`
	Tier           string        `json:"tier" example:"payg"` // "free", "payg", "standard"
	Status         AccountStatus `json:"status" example:"HEALTHY"`
	Enabled        bool          `json:"enabled" example:"true"`
	AssignedWorker int           `json:"assigned_worker" example:"0"` // -1 if unassigned
	// VaultExpired records that the AUTH_EXPIRED state came from the vault's own report rather than
	// from a runtime failure.
	//
	// @purpose Lets Sync tell "the vault still says this credential is dead" apart from "the vault has
	// since been refreshed by `agy-auth login`", so a re-login actually restores the account.
	// @rationale A runtime auth fault is retried on a timer, but a vault-reported expiry is permanent
	// until the credential changes. Without this flag both look identical in the previous snapshot and
	// a carried-forward AUTH_EXPIRED would outlive the re-login that fixed it.
	// @invariants True only while Status is AUTH_EXPIRED and the vault itself reported the expiry.
	VaultExpired  bool      `json:"vault_expired,omitempty"`
	CooldownUntil time.Time `json:"cooldown_until,omitempty" example:"2026-11-24T17:47:05Z"`
	// FamilyCooldowns holds a cooldown window per quota family ("gemini", "3p").
	//
	// @purpose Keeps an exhausted third-party bucket from deleting healthy Gemini capacity.
	// @rationale Google meters Gemini and Claude/GPT traffic independently, so exhaustion is a
	// property of a bucket family, not of the account. Status stays as a human-readable summary of
	// the worst window; eligibility is decided from this map alone.
	// @invariants Keys are quota families; absent means no active cooldown for that family.
	FamilyCooldowns map[string]time.Time  `json:"family_cooldowns,omitempty"`
	Quotas          map[string]QuotaGroup `json:"quotas,omitempty"`
	LastUsed        time.Time             `json:"last_used" example:"2026-11-24T10:00:00Z"`
}

// QuotaFamilyGemini and QuotaFamilyThirdParty are the quota-family keys derived from bucket ids.
//
// @purpose Gives eligibility a model-scoped vocabulary instead of an all-or-nothing account verdict.
// @rationale Google meters "Gemini Models" and "Claude and GPT models" as independent buckets. An
// account at 8% on the third-party bucket is still fully usable for Gemini traffic, so treating it
// as globally exhausted needlessly removes capacity.
// @preconditions None.
// @param None.
// @returns The two family identifiers used as map keys in Account.Quotas.
// @throws None.
// @invariants Immutable constants; safe for concurrent use.
const (
	QuotaFamilyGemini     = "gemini"
	QuotaFamilyThirdParty = "3p"
)

// IsEligible returns true if the account is enabled, healthy, and has available quota across every
// bucket.
//
// @purpose The conservative "any family" check for callers that have no model context.
// @rationale Kept unchanged so existing callers keep their strict semantics; model-aware callers
// must use IsEligibleForModel, which is what the pool actually needs.
// @preconditions None.
// @param requiresPaidTier Reject free accounts when the model demands a paid tier.
// @param minRemainingPct Minimum remaining percentage required per bucket.
// @returns True only when enabled, healthy, tier-eligible, and no bucket is at or below the floor.
// @throws None.
// @invariants O(number of buckets); no allocation.
func (a *Account) IsEligible(requiresPaidTier bool, minRemainingPct float64) bool {
	return a.IsEligibleForModel("", requiresPaidTier, minRemainingPct)
}

// QuotaFamilyForModel maps an upstream model id onto its quota family.
//
// @purpose Prevents an unrelated depleted bucket from masking a usable account.
// @rationale Claude and GPT models are billed from the "Claude and GPT models" group while Gemini
// models draw from "Gemini Models"; eligibility must therefore be evaluated per family.
// @preconditions model may be any case and may carry a variant suffix (e.g. "-high").
// @param model Upstream model identifier as requested by the client.
// @returns "gemini" or "3p" when the family is identifiable, "" when it is not.
// @throws None.
// @invariants O(len(model)); no allocation beyond one lowercase conversion.
func QuotaFamilyForModel(model string) string {
	id := strings.ToLower(strings.TrimSpace(model))
	switch {
	case id == "":
		return ""
	case strings.HasPrefix(id, "claude"), strings.HasPrefix(id, "gpt"):
		return QuotaFamilyThirdParty
	case strings.HasPrefix(id, "gemini"):
		return QuotaFamilyGemini
	default:
		// An unknown vendor cannot be attributed to a bucket. Callers treat "" as "check every
		// family" so the account is never wrongly declared eligible.
		return ""
	}
}

// QuotaFamilyOfBucket derives the family from a quota bucket id.
//
// @purpose Attributes live quota data to a family without parsing human-readable display names.
// @rationale `agy-auth usage --all` emits ids `gemini-weekly`, `gemini-5h`, `3p-weekly`, `3p-5h`.
// Parsing "Claude and GPT models" would break the moment upstream renames the group.
// @preconditions bucketID is the bucket id or its map key (both share the same prefix).
// @param bucketID Bucket identifier such as "3p-weekly".
// @returns "gemini", "3p", or "" when the id carries no recognised prefix.
// @throws None.
// @invariants O(len(bucketID)).
func QuotaFamilyOfBucket(bucketID string) string {
	id := strings.ToLower(bucketID)
	switch {
	case strings.HasPrefix(id, QuotaFamilyGemini):
		return QuotaFamilyGemini
	case strings.HasPrefix(id, QuotaFamilyThirdParty):
		return QuotaFamilyThirdParty
	default:
		return ""
	}
}

// IsEligibleForModel reports whether this account can serve one model's quota family.
//
// @purpose Makes account selection model-aware so a healthy Gemini account is not skipped merely
// because its third-party bucket is nearly spent (and vice versa).
// @rationale Iteration is by map key rather than by value because the key is the bucket id, which
// is the only reliable family discriminator available without another API call.
// @preconditions model may be empty, in which case every family is checked.
// @param model Upstream model id; empty or unrecognised means check all families.
// @param requiresPaidTier Reject free accounts when the model demands a paid tier.
// @param minRemainingPct Minimum remaining percentage required within the family.
// @returns True when enabled, healthy, tier-eligible, and every relevant bucket is above the floor.
// @throws None.
// @invariants O(number of buckets); no allocation.
func (a *Account) IsEligibleForModel(model string, requiresPaidTier bool, minRemainingPct float64) bool {
	if a == nil || !a.Enabled {
		return false
	}
	// A credential-level fault removes every family at once; bucket cooldowns do not.
	if a.Status == StatusAuthExpired || a.Status == StatusDisabled {
		return false
	}
	// Legacy/unknown cooldown source: a quota-status set without any per-family window (a fixture, or
	// state written before FamilyCooldowns existed) cannot be attributed to one family, so it is
	// treated as all-family to fail safe. The poller and MarkAccountStatus always populate the map,
	// so a genuine partial cooldown never takes this branch.
	if len(a.FamilyCooldowns) == 0 && (a.Status == StatusCooldown5h || a.Status == StatusCooldownWeekly) {
		return false
	}
	if requiresPaidTier && a.Tier == "free" {
		return false
	}

	family := QuotaFamilyForModel(model)
	now := time.Now()
	if family == "" {
		// Unknown model: every family the account knows about must still be serviceable.
		for _, until := range a.FamilyCooldowns {
			if now.Before(until) {
				return false
			}
		}
	} else if until, ok := a.FamilyCooldowns[family]; ok && now.Before(until) {
		// Only the family this model draws from is disqualified; other families stay usable.
		return false
	}

	for bucketID, q := range a.Quotas {
		if family != "" && QuotaFamilyOfBucket(bucketID) != family {
			continue
		}
		if q.IsExhausted || q.RemainingPct < minRemainingPct {
			return false
		}
	}
	return true
}
