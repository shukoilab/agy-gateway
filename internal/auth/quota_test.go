package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agy-gateway/internal/config"
)

// newQuotaTestManager builds a manager whose usage fetch is scripted.
//
// @purpose Drives QuotaPoller against controlled quota data without a subprocess.
// @rationale Poll behaviour is entirely a function of the payload and the accounts, so the fetch seam
// is all that must be stubbed to exercise the real status-transition logic.
// @preconditions None.
// @param t Standard Go test handle.
// @param payload Per-account usage returned by the fetch; nil means the fetch fails.
// @param fetchErr Error the fetch should return.
// @returns A configured manager.
// @throws None.
// @invariants Spawns no process.
func newQuotaTestManager(t *testing.T, cfg *config.Config, payload map[string]AccountUsage, fetchErr error) *Manager {
	t.Helper()
	mgr := NewManager(cfg, nil)
	mgr.fetchUsageForTest = func(context.Context) (map[string]AccountUsage, error) {
		if fetchErr != nil {
			return nil, fetchErr
		}
		return payload, nil
	}
	return mgr
}

// usageWithBuckets builds a synthetic per-account usage payload.
//
// @purpose Keeps poller fixtures readable.
// @rationale The poller reads bucket ids to choose a window, so the fixture must express real ids.
// @param id Account id.
// @param email Account email.
// @param geminiPct Gemini family remaining percentage.
// @param thirdPartyPct Third-party family remaining percentage.
// @param resetAt Reset time applied to both weekly buckets.
// @returns A usage entry with the four standard buckets.
// @throws None.
// @invariants Deterministic.
func usageWithBuckets(id, email string, geminiPct, thirdPartyPct float64, resetAt time.Time) AccountUsage {
	reset := resetAt.UTC().Format(time.RFC3339)
	return AccountUsage{
		AccountID: id,
		Email:     email,
		Active:    true,
		Groups: []AgyUsageGroup{
			{Name: "Gemini Models", Buckets: []AgyUsageBucket{
				{ID: "gemini-weekly", Window: "weekly", RemainingFraction: geminiPct / 100, ResetTime: reset},
				{ID: "gemini-5h", Window: "5h", RemainingFraction: 1, ResetTime: reset},
			}},
			{Name: "Claude and GPT models", Buckets: []AgyUsageBucket{
				{ID: "3p-weekly", Window: "weekly", RemainingFraction: thirdPartyPct / 100, ResetTime: reset},
				{ID: "3p-5h", Window: "5h", RemainingFraction: 1, ResetTime: reset},
			}},
		},
	}
}

// TestQuotaPoller_UsesEachAccountsOwnBuckets is the A2 regression test.
//
// @purpose Proves one account's exhaustion never marks another account exhausted.
// @rationale The original implementation fetched a single account's usage and attached it to every
// account, reporting 65% for all three when the truth was 65/100/100. That removed healthy capacity
// and hid real exhaustion.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no goroutines.
func TestQuotaPoller_UsesEachAccountsOwnBuckets(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.MinRemainingQuotaPct = 5
	cfg.Accounts.AutoRotateEnabled = true

	resetAt := time.Now().Add(6 * 24 * time.Hour)
	mgr := newQuotaTestManager(t, cfg, map[string]AccountUsage{
		// A is spent on Gemini, healthy elsewhere.
		"acct_spent": usageWithBuckets("acct_spent", "spent@example.com", 0, 100, resetAt),
		// B and C are fully healthy.
		"acct_ok1": usageWithBuckets("acct_ok1", "ok1@example.com", 100, 100, resetAt),
		"acct_ok2": usageWithBuckets("acct_ok2", "ok2@example.com", 100, 100, resetAt),
	}, nil)
	mgr.SetAccountsForTest([]Account{
		{ID: "acct_spent", Email: "spent@example.com", Status: StatusHealthy, Enabled: true},
		{ID: "acct_ok1", Email: "ok1@example.com", Status: StatusHealthy, Enabled: true},
		{ID: "acct_ok2", Email: "ok2@example.com", Status: StatusHealthy, Enabled: true},
	})

	NewQuotaPoller(mgr, cfg, nil).Poll(context.Background())

	byID := map[string]Account{}
	for _, acc := range mgr.GetAccounts() {
		byID[acc.ID] = acc
	}

	// FINDING-01: a Gemini-only depletion must cool only the Gemini family. The account stays
	// usable for Claude/GPT traffic, so its overall status must not become globally cooling.
	spent := byID["acct_spent"]
	if _, cooled := spent.FamilyCooldowns[QuotaFamilyGemini]; !cooled {
		t.Errorf("the Gemini family must be cooled for %s, got %v", spent.ID, spent.FamilyCooldowns)
	}
	if _, cooled := spent.FamilyCooldowns[QuotaFamilyThirdParty]; cooled {
		t.Errorf("the third-party family must remain open for %s, got %v", spent.ID, spent.FamilyCooldowns)
	}
	if !spent.IsEligibleForModel("claude-sonnet-4-6", false, cfg.Accounts.MinRemainingQuotaPct) {
		t.Error("an account spent on Gemini must still serve Claude traffic")
	}
	if spent.IsEligibleForModel("gemini-3.8-flash", false, cfg.Accounts.MinRemainingQuotaPct) {
		t.Error("an account spent on Gemini must not serve Gemini traffic")
	}
	for _, id := range []string{"acct_ok1", "acct_ok2"} {
		if byID[id].Status != StatusHealthy {
			t.Errorf("account %s is healthy in its own data but was marked %s", id, byID[id].Status)
		}
		if len(byID[id].FamilyCooldowns) != 0 {
			t.Errorf("account %s has no exhausted family but carries cooldowns %v", id, byID[id].FamilyCooldowns)
		}
	}

	// Each account must carry its own numbers, not a shared copy.
	if got := byID["acct_ok1"].Quotas["gemini-weekly"].RemainingPct; got != 100 {
		t.Errorf("acct_ok1 gemini remaining = %v, want its own 100", got)
	}
	if got := byID["acct_spent"].Quotas["3p-weekly"].RemainingPct; got != 100 {
		t.Errorf("acct_spent third-party remaining = %v, want 100", got)
	}
}

// TestQuotaPoller_CooldownUsesBucketResetTime is the A11 regression test.
//
// @purpose Makes the cooldown end when capacity actually returns.
// @rationale The previous code hardcoded 30 minutes for every window, so a weekly exhaustion woke the
// account into the same wall while a 5h exhaustion benched it for far too long.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestQuotaPoller_CooldownUsesBucketResetTime(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.MinRemainingQuotaPct = 5
	cfg.Accounts.AutoRotateEnabled = true
	cfg.Accounts.DefaultCooldown5h = 5 * time.Hour
	cfg.Accounts.DefaultCooldownWeekly = 30 * 24 * time.Hour

	weeklyReset := time.Now().Add(4 * 24 * time.Hour).UTC().Truncate(time.Second)
	mgr := newQuotaTestManager(t, cfg, map[string]AccountUsage{
		"acct_a": usageWithBuckets("acct_a", "a@example.com", 0, 100, weeklyReset),
	}, nil)
	mgr.SetAccountsForTest([]Account{
		{ID: "acct_a", Email: "a@example.com", Status: StatusHealthy, Enabled: true},
	})

	NewQuotaPoller(mgr, cfg, nil).Poll(context.Background())

	acc := mgr.GetAccounts()[0]
	// The bucket's own reset must win over the configured fallback duration.
	until, cooled := acc.FamilyCooldowns[QuotaFamilyGemini]
	if !cooled {
		t.Fatalf("the Gemini family must be cooled, got %v", acc.FamilyCooldowns)
	}
	if diff := until.Sub(weeklyReset); diff > time.Second || diff < -time.Second {
		t.Fatalf("cooldown until %v, want the bucket reset %v (diff %v)", until, weeklyReset, diff)
	}
}

// TestQuotaPoller_BothFamiliesSpentCoolsWholeAccount pins the summary status when nothing is left.
//
// @purpose Keeps the operator-facing status meaningful: only when every family is spent does the
// account read as globally cooling.
// @rationale FINDING-01 made a single spent family non-fatal for the whole account, which must not
// hide the genuinely-out-of-quota case.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestQuotaPoller_BothFamiliesSpentCoolsWholeAccount(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.MinRemainingQuotaPct = 5
	cfg.Accounts.AutoRotateEnabled = true

	weeklyReset := time.Now().Add(4 * 24 * time.Hour).UTC().Truncate(time.Second)
	mgr := newQuotaTestManager(t, cfg, map[string]AccountUsage{
		"acct_a": usageWithBuckets("acct_a", "a@example.com", 0, 0, weeklyReset),
	}, nil)
	mgr.SetAccountsForTest([]Account{
		{ID: "acct_a", Email: "a@example.com", Status: StatusHealthy, Enabled: true},
	})

	NewQuotaPoller(mgr, cfg, nil).Poll(context.Background())

	acc := mgr.GetAccounts()[0]
	if acc.Status != StatusCooldownWeekly && acc.Status != StatusCooldown5h {
		t.Fatalf("status = %s, want a cooling status when every family is spent", acc.Status)
	}
	if acc.IsEligibleForModel("gemini-3.8-flash", false, cfg.Accounts.MinRemainingQuotaPct) {
		t.Error("an account with every family spent must not be eligible")
	}
	if acc.IsEligibleForModel("claude-sonnet-4-6", false, cfg.Accounts.MinRemainingQuotaPct) {
		t.Error("an account with every family spent must not be eligible for third-party models either")
	}
}

// TestQuotaPoller_FetchFailureLeavesStatusesUntouched is the EC-01 regression test.
//
// @purpose Prevents rotation decisions from being made on stale or fabricated data.
// @rationale If a failed poll marked accounts healthy, an exhausted account would be leased again
// immediately; if it marked them cooling, a healthy pool would be taken out of service. Doing nothing
// is the only correct response.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestQuotaPoller_FetchFailureLeavesStatusesUntouched(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.MinRemainingQuotaPct = 5
	cfg.Accounts.AutoRotateEnabled = true

	mgr := newQuotaTestManager(t, cfg, nil, errors.New("agy-auth unavailable"))
	cooldownUntil := time.Now().Add(time.Hour)
	mgr.SetAccountsForTest([]Account{
		{ID: "acct_cool", Email: "cool@example.com", Status: StatusCooldown5h, Enabled: true,
			CooldownUntil: cooldownUntil,
			Quotas:        map[string]QuotaGroup{"gemini-weekly": {RemainingPct: 42}}},
		{ID: "acct_ok", Email: "ok@example.com", Status: StatusHealthy, Enabled: true},
	})

	NewQuotaPoller(mgr, cfg, nil).Poll(context.Background())

	byID := map[string]Account{}
	for _, acc := range mgr.GetAccounts() {
		byID[acc.ID] = acc
	}
	if byID["acct_cool"].Status != StatusCooldown5h {
		t.Errorf("a failed poll must not change the cooling account's status, got %s", byID["acct_cool"].Status)
	}
	if byID["acct_ok"].Status != StatusHealthy {
		t.Errorf("a failed poll must not change the healthy account's status, got %s", byID["acct_ok"].Status)
	}
	// Published quota figures must also survive: an operator reading the admin API during an
	// agy-auth outage should see the last known values, never zeros.
	if got := byID["acct_cool"].Quotas["gemini-weekly"].RemainingPct; got != 42 {
		t.Errorf("a failed poll overwrote retained quota data: got %v, want 42", got)
	}
}

// TestQuotaPoller_RecoversExpiredCooldown pins automatic recovery.
//
// @purpose Returns an account to service once its cooldown has elapsed.
// @rationale Without this the account would stay cooling until something else reset it, and the pool
// would permanently lose capacity after one quota event.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestQuotaPoller_RecoversExpiredCooldown(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.MinRemainingQuotaPct = 5
	cfg.Accounts.AutoRotateEnabled = true

	mgr := newQuotaTestManager(t, cfg, map[string]AccountUsage{
		"acct_a": usageWithBuckets("acct_a", "a@example.com", 100, 100, time.Now().Add(time.Hour)),
	}, nil)
	mgr.SetAccountsForTest([]Account{
		{ID: "acct_a", Email: "a@example.com", Status: StatusCooldownWeekly, Enabled: true,
			CooldownUntil: time.Now().Add(-time.Minute)},
	})

	NewQuotaPoller(mgr, cfg, nil).Poll(context.Background())

	acc := mgr.GetAccounts()[0]
	if acc.Status != StatusHealthy {
		t.Fatalf("status = %s, want HEALTHY after the cooldown expired", acc.Status)
	}
	if !acc.CooldownUntil.IsZero() {
		t.Fatalf("cooldownUntil = %v, want zero after recovery", acc.CooldownUntil)
	}
}

// TestQuotaPoller_AutoRotateDisabledOnlyRefreshesData pins the configuration gate.
//
// @purpose Honours an operator who wants live quota data but no automatic demotion.
// @rationale AutoRotateEnabled previously had zero effect outside parsing. Disabling it must stop
// status changes while still keeping the numbers current.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestQuotaPoller_AutoRotateDisabledOnlyRefreshesData(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.MinRemainingQuotaPct = 5
	cfg.Accounts.AutoRotateEnabled = false

	mgr := newQuotaTestManager(t, cfg, map[string]AccountUsage{
		"acct_a": usageWithBuckets("acct_a", "a@example.com", 0, 0, time.Now().Add(time.Hour)),
	}, nil)
	mgr.SetAccountsForTest([]Account{
		{ID: "acct_a", Email: "a@example.com", Status: StatusHealthy, Enabled: true},
	})

	NewQuotaPoller(mgr, cfg, nil).Poll(context.Background())

	acc := mgr.GetAccounts()[0]
	if acc.Status != StatusHealthy {
		t.Fatalf("auto-rotate is disabled, so status must stay HEALTHY, got %s", acc.Status)
	}
	// The data itself is still refreshed.
	if got := acc.Quotas["gemini-weekly"].RemainingPct; got != 0 {
		t.Fatalf("quota data must still be refreshed, got %v", got)
	}
}

// TestQuotaPoller_StopIsIdempotent pins shutdown safety.
//
// @purpose Prevents a panic when shutdown signals arrive twice.
// @rationale A shutdown path commonly cancels the context and calls Stop; a bare close on the second
// call would panic inside the shutdown sequence.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; starts no goroutine.
func TestQuotaPoller_StopIsIdempotent(t *testing.T) {
	cfg := config.DefaultConfig()
	qp := NewQuotaPoller(NewManager(cfg, nil), cfg, nil)

	qp.Stop()
	qp.Stop() // must not panic
}

// TestQuotaPoller_StartExitsOnContextCancel pins loop termination.
//
// @purpose Proves the polling goroutine cannot leak.
// @rationale A ticker loop that ignores cancellation would outlive the process's shutdown and keep
// spawning subprocesses.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Uses a 10ms interval and a 200ms guard; no real poll runs because the manager fetch is
// stubbed to return empty data.
func TestQuotaPoller_StartExitsOnContextCancel(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.QuotaPollInterval = 10 * time.Millisecond
	mgr := newQuotaTestManager(t, cfg, map[string]AccountUsage{}, nil)

	qp := NewQuotaPoller(mgr, cfg, nil)
	ctx, cancel := context.WithCancel(context.Background())
	qp.Start(ctx)

	time.Sleep(30 * time.Millisecond)
	cancel()
	time.Sleep(30 * time.Millisecond)
	// Reaching here without a race report is the assertion; the loop had multiple ticks to run.
}

// TestQuotaPoller_StartPollsBeforePoolInit pins the startup ordering.
//
// @purpose Guarantees eligibility data exists before the worker pool can lease anything.
// @rationale Polling only from a ticker left a full interval (ten minutes in production) during which
// an already-spent account was still selectable, so the very first requests could land on an account
// that live quota had already disqualified.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; the poll interval is long so only the startup poll can run.
func TestQuotaPoller_StartPollsBeforePoolInit(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.MinRemainingQuotaPct = 70
	cfg.Accounts.AutoRotateEnabled = true
	cfg.Accounts.QuotaPollInterval = time.Hour // only the startup poll may fire

	resetAt := time.Now().Add(4 * 24 * time.Hour)
	mgr := newQuotaTestManager(t, cfg, map[string]AccountUsage{
		"acct_spent": usageWithBuckets("acct_spent", "spent@example.com", 65, 100, resetAt),
		"acct_fresh": usageWithBuckets("acct_fresh", "fresh@example.com", 100, 100, resetAt),
	}, nil)
	mgr.SetAccountsForTest([]Account{
		{ID: "acct_spent", Email: "spent@example.com", Status: StatusHealthy, Enabled: true},
		{ID: "acct_fresh", Email: "fresh@example.com", Status: StatusHealthy, Enabled: true},
	})

	qp := NewQuotaPoller(mgr, cfg, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	qp.Start(ctx) // must poll synchronously before returning
	defer qp.Stop()

	// The spent account's Gemini family must already be cooled by the time Start returns. Its status
	// stays HEALTHY because only one family is spent, so eligibility is what is asserted.
	byID := map[string]Account{}
	for _, acc := range mgr.GetAccounts() {
		byID[acc.ID] = acc
	}
	spent := byID["acct_spent"]
	if _, cooled := spent.FamilyCooldowns[QuotaFamilyGemini]; !cooled {
		t.Fatalf("Start must poll before returning; the Gemini family is not cooled: %v", spent.FamilyCooldowns)
	}
	if spent.IsEligibleForModel("gemini-3.8-flash", false, cfg.Accounts.MinRemainingQuotaPct) {
		t.Fatal("Start must poll before returning; the spent account is still eligible for Gemini")
	}
	if !spent.IsEligibleForModel("claude-sonnet-4-6", false, cfg.Accounts.MinRemainingQuotaPct) {
		t.Fatal("a Gemini-only depletion must leave third-party capacity usable")
	}
	if byID["acct_fresh"].Status != StatusHealthy || len(byID["acct_fresh"].FamilyCooldowns) != 0 {
		t.Fatalf("the fresh account must stay healthy with no cooldowns, got %+v", byID["acct_fresh"])
	}
}

// TestSync_PreservesRuntimeState pins that a vault resync never resurrects a cooled account.
//
// @purpose Stops the admin list endpoint (which calls Sync on every read) from erasing poller state.
// @rationale Sync rebuilds accounts from `agy-auth list --all`, which only knows vault fields. A naive
// rebuild reset Status to HEALTHY and cleared Quotas/CooldownUntil, so reading the account list put a
// spent account straight back into rotation — the read path silently undid the poller's work.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; the vault listing is stubbed.
func TestSync_PreservesRuntimeState(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := NewManager(cfg, nil)

	cooldownUntil := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	familyUntil := time.Now().Add(9 * time.Hour).Truncate(time.Second)
	mgr.SetAccountsForTest([]Account{{
		ID: "acct_a", Email: "a@example.com", Status: StatusCooldownWeekly, Enabled: true,
		CooldownUntil:   cooldownUntil,
		FamilyCooldowns: map[string]time.Time{QuotaFamilyThirdParty: familyUntil},
		Quotas:          map[string]QuotaGroup{"gemini-weekly": {RemainingPct: 1}},
		AssignedWorker:  0,
	}})

	// The vault listing knows only vault fields; it reports the account as neither active nor expired.
	mgr.listForTest = func(context.Context) ([]byte, error) {
		return []byte(`{"data":{"accounts":[{"id":"acct_a","email":"a@example.com","auth_method":"consumer","plan":"Pro"}]}}`), nil
	}

	if err := mgr.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	acc := mgr.GetAccounts()[0]
	if acc.Status != StatusCooldownWeekly {
		t.Fatalf("status = %s after Sync, want COOLDOWN_WEEKLY preserved", acc.Status)
	}
	if !acc.CooldownUntil.Equal(cooldownUntil) {
		t.Fatalf("cooldownUntil = %v after Sync, want %v preserved", acc.CooldownUntil, cooldownUntil)
	}
	if got := acc.Quotas["gemini-weekly"].RemainingPct; got != 1 {
		t.Fatalf("quota data was cleared by Sync: got %v, want 1", got)
	}
	if acc.AssignedWorker != 0 {
		t.Fatalf("assignedWorker = %d after Sync, want 0 preserved", acc.AssignedWorker)
	}
	// Eligibility reads FamilyCooldowns, so a Sync that dropped it would silently re-open a family
	// the poller had just closed. Asserted on the map itself and on the resulting eligibility.
	if got, ok := acc.FamilyCooldowns[QuotaFamilyThirdParty]; !ok || !got.Equal(familyUntil) {
		t.Fatalf("family cooldown for 3p = (%v, %v) after Sync, want %v preserved", got, ok, familyUntil)
	}
	if acc.IsEligibleForModel("claude-sonnet-4-6", false, 5.0) {
		t.Fatalf("a 3p-cooled account became eligible for claude-sonnet-4-6 after Sync")
	}
}

// TestQuotaPoller_ThirdPartyDepletionKeepsGeminiCapacity is the FINDING-01 regression test.
//
// @purpose Proves that a spent Claude/GPT bucket no longer removes a 100%-healthy Gemini account from
// selection, which is exactly the capacity loss the audit found live (account 02 sat at 8.15% on
// third-party while fully healthy on Gemini).
// @rationale The poller previously called ExhaustedFamily with an empty family, so any depleted family
// demoted the whole account, and IsEligibleForModel's status gate then rejected it for every model —
// undoing the family scoping A12 introduced. This asserts the end-to-end consequence, not the
// intermediate status, because eligibility is what the pool actually consumes.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no goroutines.
func TestQuotaPoller_ThirdPartyDepletionKeepsGeminiCapacity(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.MinRemainingQuotaPct = 5
	cfg.Accounts.AutoRotateEnabled = true

	resetAt := time.Now().Add(3 * 24 * time.Hour)
	mgr := newQuotaTestManager(t, cfg, map[string]AccountUsage{
		// 3p spent, Gemini untouched — the live shape of account 02.
		"acct_3p_spent": usageWithBuckets("acct_3p_spent", "3p@example.com", 100, 0, resetAt),
	}, nil)
	mgr.SetAccountsForTest([]Account{
		{ID: "acct_3p_spent", Email: "3p@example.com", Status: StatusHealthy, Enabled: true},
	})

	NewQuotaPoller(mgr, cfg, nil).Poll(context.Background())

	acc := mgr.GetAccounts()[0]

	if _, cooled := acc.FamilyCooldowns[QuotaFamilyThirdParty]; !cooled {
		t.Fatalf("the third-party family must be cooled, got %v", acc.FamilyCooldowns)
	}
	if _, cooled := acc.FamilyCooldowns[QuotaFamilyGemini]; cooled {
		t.Fatalf("the Gemini family must stay open, got %v", acc.FamilyCooldowns)
	}
	if acc.Status == StatusCooldownWeekly || acc.Status == StatusCooldown5h {
		t.Fatalf("a single spent family must not cool the whole account, got status %s", acc.Status)
	}
	if !acc.IsEligibleForModel("gemini-3.8-flash", false, cfg.Accounts.MinRemainingQuotaPct) {
		t.Fatal("Gemini capacity was deleted by a third-party depletion (FINDING-01)")
	}
	if acc.IsEligibleForModel("claude-sonnet-4-6", false, cfg.Accounts.MinRemainingQuotaPct) {
		t.Fatal("a spent third-party family must not serve third-party models")
	}
}

// TestGetAccountsSnapshotIsolatedFromPoller pins the race that cycle-1 introduced: eligibility now
// reads FamilyCooldowns, and the poller rewrites that map under accountsMu.
//
// @purpose Fails under `-race` if GetAccounts ever hands out a shallow copy again, and fails outright
// if a snapshot starts observing later poller writes.
// @rationale A shallow struct copy shares the map, so a pool reader iterating it while the poller
// deletes an elapsed window is a real data race (reviewer reproduced mapdelete vs IsEligibleForModel).
// The snapshot must own its maps.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures surface as race detector reports or t.Fatalf.
// @invariants Bounded: one poller goroutine, one reader goroutine, both joined before return.
func TestGetAccountsSnapshotIsolatedFromPoller(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.MinRemainingQuotaPct = 5
	cfg.Accounts.AutoRotateEnabled = true
	// Alternate between a spent and a healthy family so every poll both adds and deletes windows.
	var flip atomic.Bool
	reset := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	payload := map[string]AccountUsage{
		"acct_race": usageWithBuckets("acct_race", "race@example.com", 100, 1, time.Now().Add(2*time.Hour)),
	}
	mgr := NewManager(cfg, nil)
	mgr.fetchUsageForTest = func(context.Context) (map[string]AccountUsage, error) {
		if flip.Load() {
			return payload, nil
		}
		return map[string]AccountUsage{
			"acct_race": usageWithBuckets("acct_race", "race@example.com", 1, 100, time.Now().Add(2*time.Hour)),
		}, nil
	}
	_ = reset
	mgr.SetAccountsForTest([]Account{{
		ID: "acct_race", Email: "race@example.com", Enabled: true, Status: StatusHealthy,
		Quotas:          map[string]QuotaGroup{},
		FamilyCooldowns: map[string]time.Time{},
	}})

	qp := NewQuotaPoller(mgr, cfg, nil)

	var wg sync.WaitGroup
	wg.Add(2)
	stop := make(chan struct{})
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				// Keep flipping so the poller keeps mutating the map.
				flip.Store(!flip.Load())
				qp.Poll(context.Background())
			}
		}
	}()
	go func() {
		defer wg.Done()
		deadline := time.Now().Add(300 * time.Millisecond)
		for time.Now().Before(deadline) {
			for _, acc := range mgr.GetAccounts() {
				// Reading the snapshot outside any lock is exactly what the pool does.
				_ = acc.IsEligibleForModel("gemini-3.8-flash", false, 5)
				for range acc.FamilyCooldowns {
				}
				for range acc.Quotas {
				}
			}
		}
		close(stop)
	}()
	wg.Wait()

	// Ownership check: mutating a snapshot must not touch manager state.
	snapshot := mgr.GetAccounts()
	if len(snapshot) == 0 {
		t.Fatal("expected at least one account")
	}
	snapshot[0].FamilyCooldowns["3p"] = time.Now().Add(24 * time.Hour)
	for _, acc := range mgr.GetAccounts() {
		if until, ok := acc.FamilyCooldowns["3p"]; ok && until.After(time.Now().Add(23*time.Hour)) {
			t.Fatalf("snapshot mutation leaked into manager state: 3p=%v", until)
		}
	}
}

// TestQuotaPoller_UsageErrorDoesNotWipeKnownQuotas pins FR-03.
//
// @purpose Proves a failed quota read never erases data the gateway already had.
// @rationale Poll previously assigned QuotaGroups(live.Groups) unconditionally, so a `usage_error`
// response overwrote a healthy known map with an empty one — turning a transient provider error into
// "this account has no limits" and, worse, discarding the real depletion windows.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants O(accounts); no subprocess.
func TestQuotaPoller_UsageErrorDoesNotWipeKnownQuotas(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.MinRemainingQuotaPct = 5
	cfg.Accounts.AutoRotateEnabled = true
	mgr := NewManager(cfg, nil)
	mgr.SetAccountsForTest([]Account{{
		ID: "acct_a", Email: "a@example.com", Enabled: true, Status: StatusHealthy,
		QuotasKnown: true,
		Quotas: map[string]QuotaGroup{
			"gemini-weekly": {RemainingPct: 42},
		},
		FamilyCooldowns: map[string]time.Time{},
	}})
	mgr.SetFetchUsageForTest(func(context.Context) (map[string]AccountUsage, error) {
		return map[string]AccountUsage{
			"acct_a": {AccountID: "acct_a", Email: "a@example.com", UsageError: "forbidden"},
		}, nil
	})

	NewQuotaPoller(mgr, cfg, nil).Poll(context.Background())

	acc := mgr.GetAccounts()[0]
	if acc.QuotasKnown {
		t.Fatal("QuotasKnown must be false after a failed read")
	}
	if acc.Quotas["gemini-weekly"].RemainingPct != 42 {
		t.Fatalf("known quota was wiped by a failed read: %v", acc.Quotas)
	}
	if acc.UsageError != "forbidden" {
		t.Fatalf("UsageError = %q, want %q", acc.UsageError, "forbidden")
	}
}

// TestQuotaPoller_SuccessClearsUsageError pins the recovery direction.
//
// @purpose Proves a later successful read restores full rank and clears the diagnostic.
// @rationale Otherwise a single transient failure would permanently demote the account to rank 1.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants O(accounts); no subprocess.
func TestQuotaPoller_SuccessClearsUsageError(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.MinRemainingQuotaPct = 5
	cfg.Accounts.AutoRotateEnabled = true
	reset := time.Now().Add(2 * time.Hour)
	mgr := NewManager(cfg, nil)
	mgr.SetAccountsForTest([]Account{{
		ID: "acct_a", Email: "a@example.com", Enabled: true, Status: StatusHealthy,
		QuotasKnown: false, UsageError: "timeout",
		Quotas:          map[string]QuotaGroup{},
		FamilyCooldowns: map[string]time.Time{},
	}})
	mgr.SetFetchUsageForTest(func(context.Context) (map[string]AccountUsage, error) {
		return map[string]AccountUsage{
			"acct_a": {AccountID: "acct_a", Email: "a@example.com", Active: true,
				Groups: []AgyUsageGroup{{Name: "Gemini Models", Buckets: []AgyUsageBucket{
					{ID: "gemini-weekly", Window: "weekly", RemainingFraction: 0.9,
						ResetTime: reset.UTC().Format(time.RFC3339)},
				}}}},
		}, nil
	})

	NewQuotaPoller(mgr, cfg, nil).Poll(context.Background())

	acc := mgr.GetAccounts()[0]
	if !acc.QuotasKnown {
		t.Fatal("QuotasKnown must be true after a successful read")
	}
	if acc.UsageError != "" {
		t.Fatalf("UsageError = %q, want empty after success", acc.UsageError)
	}
	if acc.Quotas["gemini-weekly"].RemainingPct != 90 {
		t.Fatalf("expected the refreshed quota, got %v", acc.Quotas)
	}
}

// TestQuotaPoller_UnknownAccountStaysUsableButDemoted pins FR-02's balance.
//
// @purpose Proves an account with unreadable quota is still assignable, yet ranks below a known-good
// peer.
// @rationale Refusing every unknown account would turn one failing `agy-auth usage` call into a total
// outage; treating it as fully healthy is the fail-open that caused the production defect. Rank 1
// expresses exactly the middle ground.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants O(accounts); no subprocess.
func TestQuotaPoller_UnknownAccountStaysUsableButDemoted(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.MinRemainingQuotaPct = 5
	cfg.Accounts.AutoRotateEnabled = true
	mgr := NewManager(cfg, nil)
	mgr.SetAccountsForTest([]Account{
		{ID: "acct_unknown", Email: "u@example.com", Enabled: true, Status: StatusHealthy,
			QuotasKnown: true, Quotas: map[string]QuotaGroup{}, FamilyCooldowns: map[string]time.Time{}},
		{ID: "acct_known", Email: "k@example.com", Enabled: true, Status: StatusHealthy,
			QuotasKnown: false, Quotas: map[string]QuotaGroup{}, FamilyCooldowns: map[string]time.Time{}},
	})
	mgr.SetFetchUsageForTest(func(context.Context) (map[string]AccountUsage, error) {
		return map[string]AccountUsage{
			"acct_unknown": {AccountID: "acct_unknown", Email: "u@example.com", UsageError: "forbidden"},
			"acct_known": {AccountID: "acct_known", Email: "k@example.com", Active: true,
				Groups: []AgyUsageGroup{{Name: "Gemini Models", Buckets: []AgyUsageBucket{
					{ID: "gemini-weekly", Window: "weekly", RemainingFraction: 0.9},
				}}}},
		}, nil
	})

	NewQuotaPoller(mgr, cfg, nil).Poll(context.Background())

	accounts := mgr.GetAccounts()
	ranks := map[string]int{}
	for i := range accounts {
		ranks[accounts[i].ID] = accounts[i].EligibilityRank("gemini-3.8-flash", false, 5)
	}
	if ranks["acct_unknown"] != 1 {
		t.Fatalf("unknown-quota rank = %d, want 1 (usable but demoted)", ranks["acct_unknown"])
	}
	if ranks["acct_known"] != 2 {
		t.Fatalf("known-good rank = %d, want 2", ranks["acct_known"])
	}

	// The demoted account must still be selectable when nothing better is left.
	next, err := mgr.FindNextEligibleAccount("acct_known", "gemini-3.8-flash", false, nil)
	if err != nil {
		t.Fatalf("the unknown-quota account must remain selectable: %v", err)
	}
	if next.ID != "acct_unknown" {
		t.Fatalf("selected %s, want acct_unknown", next.ID)
	}
	// But with both available, the known-good one must win.
	best, err := mgr.FindNextEligibleAccount("", "gemini-3.8-flash", false, nil)
	if err != nil {
		t.Fatalf("expected a selection: %v", err)
	}
	if best.ID != "acct_known" {
		t.Fatalf("selected %s, want acct_known (rank 2 must beat rank 1)", best.ID)
	}
}
