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
	Name         string    `json:"name" example:"Gemini Models"`
	RemainingPct float64   `json:"remaining_pct" example:"70.38"`
	ResetTime    time.Time `json:"reset_time" example:"2026-11-24T17:47:05Z"`
	IsExhausted  bool      `json:"is_exhausted" example:"false"`
}

// Account represents an individual Google Antigravity credential managed by the gateway.
type Account struct {
	ID             string                `json:"id" example:"acc_9f3b18e2"`
	Email          string                `json:"email" example:"dev@shukoilab.com"`
	Alias          string                `json:"alias,omitempty" example:"primary-account"`
	AuthMethod     string                `json:"auth_method" example:"oauth"`
	Tier           string                `json:"tier" example:"payg"` // "free", "payg", "standard"
	Status         AccountStatus         `json:"status" example:"HEALTHY"`
	Enabled        bool                  `json:"enabled" example:"true"`
	AssignedWorker int                   `json:"assigned_worker" example:"0"` // -1 if unassigned
	CooldownUntil  time.Time             `json:"cooldown_until,omitempty" example:"2026-11-24T17:47:05Z"`
	Quotas         map[string]QuotaGroup `json:"quotas,omitempty"`
	LastUsed       time.Time             `json:"last_used" example:"2026-11-24T10:00:00Z"`
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
