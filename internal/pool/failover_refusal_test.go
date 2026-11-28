package pool

import (
	"context"
	"errors"
	"testing"
	"time"

	"agy-gateway/internal/auth"
	"agy-gateway/internal/config"
	"agy-gateway/internal/retry"
)

// refusalErr builds the typed failure the turn executor raises for an exhausted account.
//
// @purpose Reproduces the production shape without a subprocess.
// @param reset Stated reset window.
// @returns An error matching retry.ErrQuotaRefusal.
// @invariants Deterministic.
func refusalErr(reset time.Duration) error {
	return MarkZeroTokens(&testQuotaRefusal{reset: reset})
}

type testQuotaRefusal struct{ reset time.Duration }

func (e *testQuotaRefusal) Error() string                      { return "Individual quota reached. Please upgrade." }
func (e *testQuotaRefusal) Unwrap() error                      { return retry.ErrQuotaRefusal }
func (e *testQuotaRefusal) ResetWindow() (time.Duration, bool) { return e.reset, e.reset > 0 }

// TestFailover_AllAccountsExhaustedFailsFastWithoutLoop pins the "every account is out" path.
//
// @purpose Proves a total-quota outage answers once instead of retrying accounts that cannot serve.
// @rationale Without this the coordinator slept through its retry budget re-leasing accounts it had
// just cooled, then surfaced a generic 500. The client must instead get a single 429 carrying the
// earliest reset, and the gateway must not burn a subprocess per retry.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no subprocess.
func TestFailover_AllAccountsExhaustedFailsFastWithoutLoop(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Execution.RetryInitialBackoff = 1 * time.Millisecond
	cfg.Execution.RetryMaxBackoff = 2 * time.Millisecond
	cfg.Accounts.MaxFailoverRetries = 5

	authMgr := auth.NewManager(cfg, nil)
	authMgr.SetAccountsForTest([]auth.Account{{
		ID: "acct_only", Email: "only@example.com", Tier: "payg",
		Status: auth.StatusHealthy, Enabled: true,
	}})

	p := &Pool{cfg: cfg, authMgr: authMgr, leaseCh: make(chan *Worker, 1)}
	p.leaseCh <- createMockWorker(t, 0, &auth.Account{ID: "acct_only", Status: auth.StatusHealthy, Enabled: true}, cfg)

	fc := NewFailoverCoordinator(p, authMgr, nil)

	attempts := 0
	err := fc.ExecuteWithFailover(context.Background(), "gemini-4-hydrogen", false, 5,
		func(context.Context, *Worker) error {
			attempts++
			return refusalErr(6 * 24 * time.Hour)
		})

	if err == nil {
		t.Fatal("a total quota outage must fail the request")
	}
	if !errors.Is(err, ErrNoEligibleAccounts) {
		t.Fatalf("want ErrNoEligibleAccounts so the handler answers 429, got %v", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (retrying cooling accounts is pointless work)", attempts)
	}
	// The account must now be cooling with the window the upstream stated, so Retry-After is truthful.
	accounts := authMgr.GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d", len(accounts))
	}
	if accounts[0].CooldownUntil.IsZero() {
		t.Fatal("a stated refusal must set a cooldown so Retry-After has a value")
	}
	if got := fc.NextAvailableIn(time.Now()); got <= 0 {
		t.Fatalf("NextAvailableIn = %v, want a positive wait derived from the stated reset", got)
	}
}

// TestFailover_StatedRefusalCoolsOnlyRequestedFamily pins family-scoped cooling.
//
// @purpose Proves a Gemini refusal does not bench the account's Claude/GPT capacity.
// @rationale The two windows are independent; cooling both on one refusal throws away working quota.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestFailover_StatedRefusalCoolsOnlyRequestedFamily(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Execution.RetryInitialBackoff = 1 * time.Millisecond
	cfg.Execution.RetryMaxBackoff = 2 * time.Millisecond

	authMgr := auth.NewManager(cfg, nil)
	authMgr.SetAccountsForTest([]auth.Account{{
		ID: "acct_a", Email: "a@example.com", Tier: "payg",
		Status: auth.StatusHealthy, Enabled: true,
	}})

	p := &Pool{cfg: cfg, authMgr: authMgr, leaseCh: make(chan *Worker, 1)}
	p.leaseCh <- createMockWorker(t, 0, &auth.Account{ID: "acct_a", Status: auth.StatusHealthy, Enabled: true}, cfg)

	fc := NewFailoverCoordinator(p, authMgr, nil)
	_ = fc.ExecuteWithFailover(context.Background(), "gemini-4-hydrogen", false, 0,
		func(context.Context, *Worker) error { return refusalErr(time.Hour) })

	accounts := authMgr.GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d", len(accounts))
	}
	acc := accounts[0]
	if _, cooled := acc.FamilyCooldowns[auth.QuotaFamilyGemini]; !cooled {
		t.Fatal("the Gemini family must be cooled by a Gemini refusal")
	}
	if _, cooled := acc.FamilyCooldowns[auth.QuotaFamilyThirdParty]; cooled {
		t.Fatal("the Claude/GPT family must stay usable when only Gemini was refused")
	}
	if acc.IsEligibleForModel("claude-sonnet-4-6", false, cfg.Accounts.MinRemainingQuotaPct) != true {
		t.Fatal("a Gemini refusal must not remove the account from Claude/GPT rotation")
	}
	if acc.IsEligibleForModel("gemini-4-hydrogen", false, cfg.Accounts.MinRemainingQuotaPct) {
		t.Fatal("the refused family must be ineligible")
	}
}
