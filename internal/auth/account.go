// Package auth integrates with @fikrilal/agy-auth CLI to manage Google Antigravity
// credentials, OAuth onboarding, quota cooldowns, and isolated worker HOME environments.
package auth

import "time"

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
	Name            string    `json:"name"`
	RemainingPct    float64   `json:"remaining_pct"`
	ResetTime       time.Time `json:"reset_time"`
	IsExhausted     bool      `json:"is_exhausted"`
}

// Account represents an individual Google Antigravity credential managed by the gateway.
type Account struct {
	ID             string                `json:"id"`
	Email          string                `json:"email"`
	Alias          string                `json:"alias,omitempty"`
	AuthMethod     string                `json:"auth_method"`
	Tier           string                `json:"tier"` // "free", "payg", "standard"
	Status         AccountStatus         `json:"status"`
	Enabled        bool                  `json:"enabled"`
	AssignedWorker int                   `json:"assigned_worker"` // -1 if unassigned
	CooldownUntil  time.Time             `json:"cooldown_until,omitempty"`
	Quotas         map[string]QuotaGroup `json:"quotas,omitempty"`
	LastUsed       time.Time             `json:"last_used"`
}

// IsEligible returns true if the account is enabled, healthy, and has available quota.
func (a *Account) IsEligible(requiresPaidTier bool, minRemainingPct float64) bool {
	if !a.Enabled || a.Status != StatusHealthy {
		return false
	}
	if requiresPaidTier && a.Tier == "free" {
		return false
	}
	for _, q := range a.Quotas {
		if q.IsExhausted || q.RemainingPct < minRemainingPct {
			return false
		}
	}
	return true
}
