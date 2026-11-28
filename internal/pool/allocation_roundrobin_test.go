package pool

import (
	"testing"

	"agy-gateway/internal/auth"
	"agy-gateway/internal/config"
)

// TestAssignAccounts_RoundRobinPrefersEligible pins the F9 defect.
//
// @purpose Proves `round_robin` never hands an ineligible account to a worker while an eligible one
// exists.
// @rationale The round_robin branch was `accounts[i % len(accounts)]` with no eligibility filter, so a
// cooling account (or one whose only buckets were exhausted) was assigned at boot and every request
// on that worker failed until the next lease-time reconcile. `least_used` had three tests and
// `sticky` six; round_robin had none, which is why the branch survived.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no allocation of goroutines.
func TestAssignAccounts_RoundRobinPrefersEligible(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.AllocationStrategy = config.AllocationStrategyRoundRobin
	cfg.Accounts.MinRemainingQuotaPct = 5
	p := &Pool{cfg: cfg}

	cooling := accountWithQuotas("acct_cooling", 100, 100)
	cooling.Status = auth.StatusCooldown5h
	disabled := accountWithQuotas("acct_disabled", 100, 100)
	disabled.Enabled = false
	healthy1 := accountWithQuotas("acct_ok1", 90, 90)
	healthy2 := accountWithQuotas("acct_ok2", 90, 90)

	// The ineligible accounts sit FIRST so index-order selection would pick them on the first slot.
	accounts := []auth.Account{cooling, disabled, healthy1, healthy2}
	got := p.assignAccounts(accounts, 4)
	if len(got) != 4 {
		t.Fatalf("expected 4 assignments, got %d", len(got))
	}
	for i, acc := range got {
		if acc.ID == "acct_cooling" || acc.ID == "acct_disabled" {
			t.Fatalf("worker %d was assigned ineligible account %s", i, acc.ID)
		}
	}

	// Both eligible accounts must actually be used: with 4 workers and 2 eligible accounts the
	// round-robin order has to alternate instead of pinning one.
	seen := map[string]int{}
	for _, acc := range got {
		seen[acc.ID]++
	}
	if seen["acct_ok1"] != 2 || seen["acct_ok2"] != 2 {
		t.Fatalf("expected an even split across the two eligible accounts, got %v", seen)
	}
}

// TestAssignAccounts_RoundRobinFallsBackWhenNothingIsEligible pins the documented escape hatch.
//
// @purpose Proves a vault where every account is cooling still gets assignments rather than a panic or
// an empty pool.
// @rationale Filtering by eligibility must not turn "no eligible account" into "no worker": the pool
// has to be able to boot and report the outage through the normal no-eligible path, not crash.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestAssignAccounts_RoundRobinFallsBackWhenNothingIsEligible(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.AllocationStrategy = config.AllocationStrategyRoundRobin
	p := &Pool{cfg: cfg}

	spent := accountWithQuotas("acct_spent", 0, 0)
	spent.Quotas["gemini-weekly"] = auth.QuotaGroup{RemainingPct: 0, IsExhausted: true}
	spent.Quotas["3p-weekly"] = auth.QuotaGroup{RemainingPct: 0, IsExhausted: true}

	got := p.assignAccounts([]auth.Account{spent}, 1)
	if len(got) != 1 {
		t.Fatalf("expected 1 assignment, got %d", len(got))
	}
	if got[0].ID != "acct_spent" {
		t.Fatalf("expected the only account as a last resort, got %s", got[0].ID)
	}
}
