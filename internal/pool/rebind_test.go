package pool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agy-gateway/internal/auth"
	"agy-gateway/internal/config"
)

// accountWithQuotas builds an account carrying one bucket per family.
//
// @purpose Keeps allocation and rotation fixtures readable.
// @rationale Allocation decisions depend on bucket data, so every fixture needs a realistic quota
// map rather than a bare account.
// @preconditions None.
// @param id Account id.
// @param geminiPct Remaining percentage for the Gemini family.
// @param thirdPartyPct Remaining percentage for the third-party family.
// @returns A healthy, enabled, paid-tier account.
// @throws None.
// @invariants Deterministic.
func accountWithQuotas(id string, geminiPct, thirdPartyPct float64) auth.Account {
	return auth.Account{
		ID:      id,
		Email:   id + "@example.com",
		Tier:    "payg",
		Status:  auth.StatusHealthy,
		Enabled: true,
		Quotas: map[string]auth.QuotaGroup{
			"gemini-weekly": {Name: "Gemini Models", RemainingPct: geminiPct},
			"3p-weekly":     {Name: "Claude and GPT models", RemainingPct: thirdPartyPct},
		},
	}
}

// TestAssignAccounts_DedicatedPerWorker pins the one-account-per-worker invariant.
//
// @purpose Proves distinct accounts are handed to distinct workers, which is the prerequisite for
// per-worker credential materialisation.
// @rationale Defect A1 meant every worker authenticated as one account regardless of the binding.
// The allocation must therefore never collapse several accounts onto one worker when there are enough
// to go around.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestAssignAccounts_DedicatedPerWorker(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.AllocationStrategy = "round_robin"
	p := &Pool{cfg: cfg}

	accounts := []auth.Account{
		accountWithQuotas("acct_a", 90, 90),
		accountWithQuotas("acct_b", 90, 90),
		accountWithQuotas("acct_c", 90, 90),
	}

	got := p.assignAccounts(accounts, 3)
	if len(got) != 3 {
		t.Fatalf("expected 3 assignments, got %d", len(got))
	}

	seen := make(map[string]int)
	for _, acc := range got {
		seen[acc.ID]++
	}
	if len(seen) != 3 {
		t.Fatalf("expected 3 distinct accounts for 3 workers, got %v", seen)
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("account %s assigned %d times; expected exactly once", id, count)
		}
	}
}

// TestAssignAccounts_FewerAccountsThanWorkers pins the documented sharing fallback.
//
// @purpose Keeps the pool functional when the vault holds fewer accounts than workers, while making
// the consequence explicit rather than silent.
// @rationale Concurrent refresh with a shared refresh token races (only the first succeeds), so
// sharing is a configuration fault. Allocation must still be deterministic and total.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestAssignAccounts_FewerAccountsThanWorkers(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.AllocationStrategy = "round_robin"
	p := &Pool{cfg: cfg}

	accounts := []auth.Account{
		accountWithQuotas("acct_a", 90, 90),
		accountWithQuotas("acct_b", 90, 90),
	}

	got := p.assignAccounts(accounts, 4)
	if len(got) != 4 {
		t.Fatalf("expected 4 assignments, got %d", len(got))
	}

	// Round robin must still spread evenly: two accounts across four workers.
	counts := map[string]int{}
	for _, acc := range got {
		counts[acc.ID]++
	}
	if counts["acct_a"] != 2 || counts["acct_b"] != 2 {
		t.Fatalf("expected an even 2/2 split, got %v", counts)
	}
}

// TestAssignAccounts_LeastUsedPrefersHighestQuota pins the least_used strategy.
//
// @purpose Makes the strategy setting behave as documented.
// @rationale The setting previously had zero effect outside configuration parsing, so an operator
// choosing least_used got round robin. The chosen account must be the one with the most headroom.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestAssignAccounts_LeastUsedPrefersHighestQuota(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.AllocationStrategy = "least_used"
	p := &Pool{cfg: cfg}

	accounts := []auth.Account{
		accountWithQuotas("acct_low", 20, 90),
		accountWithQuotas("acct_high", 95, 95),
		accountWithQuotas("acct_mid", 60, 60),
	}

	got := p.assignAccounts(accounts, 1)
	if len(got) != 1 {
		t.Fatalf("expected 1 assignment, got %d", len(got))
	}
	if got[0].ID != "acct_high" {
		t.Fatalf("least_used must pick the account with the most headroom, got %s", got[0].ID)
	}
}

// TestQuotaScore_UsesScarcestBucket pins the scoring rule.
//
// @purpose Documents that an account is rated by its scarcest bucket.
// @rationale The scarcest family is what will stop the account first, so averaging would overstate
// capacity.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestQuotaScore_UsesScarcestBucket(t *testing.T) {
	acc := accountWithQuotas("acct", 80, 15)
	if got := quotaScore(acc); got != 15 {
		t.Fatalf("quotaScore = %v, want 15 (the scarcest bucket)", got)
	}
	if got := quotaScore(auth.Account{}); got != 0 {
		t.Fatalf("quotaScore without data = %v, want 0", got)
	}
}

// TestReconcileWorkerAccount_RotatesOnModelFamilyExhaustion is the A12/A4 regression test.
//
// @purpose Proves a worker is rebound to another account when its own account cannot serve the
// requested model's quota family.
// @rationale Two defects combined here: eligibility ignored the model family, and Acquire only
// rebound on AUTH_EXPIRED, so a cooldown or a spent third-party bucket never moved a worker.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no subprocess beyond the stub.
func TestReconcileWorkerAccount_RotatesOnModelFamilyExhaustion(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.MinRemainingQuotaPct = 5

	authMgr := auth.NewManager(cfg, nil)
	spent := accountWithQuotas("acct_spent", 90, 1)  // Gemini fine, third-party spent.
	fresh := accountWithQuotas("acct_fresh", 90, 90) // Everything fine.
	authMgr.SetAccountsForTest([]auth.Account{spent, fresh})

	// Credential installation is stubbed: this test is about the rotation decision, not the export.
	cfg.Storage.WorkersRootDir = t.TempDir()
	authMgr.SetExportForTest(func(_ context.Context, dir, accountID string) error {
		payload := map[string]any{
			"token": map[string]any{
				"access_token":  "access-" + accountID,
				"refresh_token": "refresh-" + accountID,
			},
			"auth_method":    "consumer",
			"id_token_email": accountID + "@example.com",
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, accountID+".json"), raw, 0600)
	})
	p := &Pool{cfg: cfg, authMgr: authMgr, leaseCh: make(chan *Worker, 1)}

	// The worker is bound to the account whose third-party bucket is spent.
	bound := spent
	w := &Worker{ID: 1}
	w.SetAccount(&bound)

	if err := p.reconcileWorkerAccount(context.Background(), w, "claude-sonnet-4-6", false); err != nil {
		t.Fatalf("reconcileWorkerAccount: %v", err)
	}
	if w.GetAccount() == nil || w.GetAccount().ID != "acct_fresh" {
		t.Fatalf("expected rotation to acct_fresh, got %+v", w.GetAccount())
	}

	// The same worker must NOT be rotated for a Gemini model: its own account is fine there.
	bound = spent
	w2 := &Worker{ID: 2}
	w2.SetAccount(&bound)
	if err := p.reconcileWorkerAccount(context.Background(), w2, "gemini-3.8-flash", false); err != nil {
		t.Fatalf("reconcileWorkerAccount(gemini): %v", err)
	}
	if w2.GetAccount() == nil || w2.GetAccount().ID != "acct_spent" {
		t.Fatalf("a depleted third-party bucket must not force rotation for a Gemini model, got %+v", w2.GetAccount())
	}
}

// TestReconcileWorkerAccount_NoAccounts pins the empty-vault fast failure.
//
// @purpose Turns an empty vault into a typed error so handlers can answer without burning the acquire
// budget.
// @rationale Retrying an impossible condition for 15 seconds delays every request for no benefit.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestReconcileWorkerAccount_NoAccounts(t *testing.T) {
	cfg := config.DefaultConfig()
	authMgr := auth.NewManager(cfg, nil)
	authMgr.SetAccountsForTest(nil)

	p := &Pool{cfg: cfg, authMgr: authMgr, leaseCh: make(chan *Worker, 1)}
	w := &Worker{ID: 0}

	err := p.reconcileWorkerAccount(context.Background(), w, "gemini-3.8-flash", false)
	if !errors.Is(err, ErrNoAccounts) {
		t.Fatalf("expected ErrNoAccounts, got %v", err)
	}
}

// TestReconcileWorkerAccount_NoEligibleAccount pins the all-exhausted verdict.
//
// @purpose Distinguishes "no capacity for this model" from "no accounts configured".
// @rationale The HTTP layer maps the two to different codes, and an operator needs to tell a
// configuration fault from a quota event.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestReconcileWorkerAccount_NoEligibleAccount(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.MinRemainingQuotaPct = 5

	authMgr := auth.NewManager(cfg, nil)
	// Both accounts are fine on Gemini but spent on the third-party family.
	authMgr.SetAccountsForTest([]auth.Account{
		accountWithQuotas("acct_a", 90, 0),
		accountWithQuotas("acct_b", 90, 2),
	})

	p := &Pool{cfg: cfg, authMgr: authMgr, leaseCh: make(chan *Worker, 1)}
	bound := accountWithQuotas("acct_a", 90, 0)
	w := &Worker{ID: 0}
	w.SetAccount(&bound)

	err := p.reconcileWorkerAccount(context.Background(), w, "claude-sonnet-4-6", false)
	if !errors.Is(err, ErrNoEligibleAccounts) {
		t.Fatalf("expected ErrNoEligibleAccounts, got %v", err)
	}
}

// TestReconcileWorkerAccount_AdoptsFresherManagerView pins that a cooled-down account is re-judged.
//
// @purpose Proves the pool no longer trusts a stale copy of the account it bound at startup.
// @rationale Defect A4 was that Acquire refreshed the bound account only on AUTH_EXPIRED, so a poller
// cooldown was invisible forever. The worker's cached copy must be replaced by the manager's view.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestReconcileWorkerAccount_AdoptsFresherManagerView(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.MinRemainingQuotaPct = 5

	authMgr := auth.NewManager(cfg, nil)
	cooled := accountWithQuotas("acct_cooled", 90, 90)
	cooled.Status = auth.StatusCooldownWeekly
	cooled.CooldownUntil = time.Now().Add(time.Hour)
	usable := accountWithQuotas("acct_usable", 90, 90)
	authMgr.SetAccountsForTest([]auth.Account{cooled, usable})

	// Credential installation is stubbed: this test is about the rotation decision, not the export.
	cfg.Storage.WorkersRootDir = t.TempDir()
	authMgr.SetExportForTest(func(_ context.Context, dir, accountID string) error {
		payload := map[string]any{
			"token": map[string]any{
				"access_token":  "access-" + accountID,
				"refresh_token": "refresh-" + accountID,
			},
			"auth_method":    "consumer",
			"id_token_email": accountID + "@example.com",
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, accountID+".json"), raw, 0600)
	})
	p := &Pool{cfg: cfg, authMgr: authMgr, leaseCh: make(chan *Worker, 1)}

	// The worker's stale copy claims HEALTHY; the manager knows it is cooling down.
	stale := accountWithQuotas("acct_cooled", 90, 90)
	w := &Worker{ID: 0}
	w.SetAccount(&stale)

	if err := p.reconcileWorkerAccount(context.Background(), w, "gemini-3.8-flash", false); err != nil {
		t.Fatalf("reconcileWorkerAccount: %v", err)
	}
	if w.GetAccount() == nil || w.GetAccount().ID != "acct_usable" {
		t.Fatalf("expected the cooled-down account to be replaced, got %+v", w.GetAccount())
	}
}

// TestMaterializeOnRotation_WritesAccountIdentity proves rotation reaches disk.
//
// @purpose Ties the pool's rotation decision to the credential actually installed for the worker.
// @rationale Rebinding the in-memory account without rewriting the token was the original divergence:
// the pool believed one thing while agy authenticated as another.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no real subprocess (export seam stubbed).
func TestMaterializeOnRotation_WritesAccountIdentity(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.MinRemainingQuotaPct = 5
	cfg.Accounts.CredentialOpTimeout = 5 * time.Second
	root := t.TempDir()
	cfg.Storage.WorkersRootDir = root

	authMgr := auth.NewManager(cfg, nil)
	authMgr.SetAccountsForTest([]auth.Account{
		accountWithQuotas("acct_spent", 90, 0),
		accountWithQuotas("acct_fresh", 90, 90),
	})
	authMgr.SetExportForTest(func(_ context.Context, dir, accountID string) error {
		payload := map[string]any{
			"token": map[string]any{
				"access_token":  "access-" + accountID,
				"token_type":    "Bearer",
				"refresh_token": "refresh-" + accountID,
				"expiry":        "2026-11-25T20:00:00Z",
			},
			"auth_method":    "consumer",
			"id_token":       "h.p.s",
			"id_token_email": accountID + "@example.com",
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, accountID+".json"), raw, 0600)
	})

	p := &Pool{cfg: cfg, authMgr: authMgr, leaseCh: make(chan *Worker, 1)}
	bound := accountWithQuotas("acct_spent", 90, 0)
	w := &Worker{ID: 0}
	w.SetAccount(&bound)

	if err := p.reconcileWorkerAccount(context.Background(), w, "claude-sonnet-4-6", false); err != nil {
		t.Fatalf("reconcileWorkerAccount: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(root, "w0", ".gemini", "antigravity-cli", "antigravity-oauth-token"))
	if err != nil {
		t.Fatalf("read installed credential: %v", err)
	}
	var installed struct {
		Token struct {
			AccessToken string `json:"access_token"`
		} `json:"token"`
	}
	if err := json.Unmarshal(raw, &installed); err != nil {
		t.Fatalf("installed credential is not valid JSON: %v", err)
	}
	if installed.Token.AccessToken != "access-acct_fresh" {
		t.Fatalf("installed credential belongs to %q, want the rotated account acct_fresh",
			installed.Token.AccessToken)
	}
}

// TestAccountsInUse_ReportsClaimCounts pins the inputs rotation depends on.
//
// @purpose Guarantees the pool reports how many OTHER workers hold each account, so a worker's own
// binding never looks like a conflict and a heavily-shared account looks as contended as it is.
// @rationale Passing a wrong map would either strand a worker (its own claim treated as a conflict) or
// misrank rotation. The previous contract returned a single owner id and OMITTED any account held by
// two or more workers, which made the most contended accounts look entirely free — so rotation piled
// onto them while a one-way account sat idle. Claim counts remove that blind spot.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Errorf.
// @invariants O(workers); no locks, no subprocess.
func TestAccountsInUse_ReportsClaimCounts(t *testing.T) {
	cfg := config.DefaultConfig()
	p := NewTestPoolWithNoWorkers(cfg)

	self := &Worker{ID: 0}
	self.SetAccount(&auth.Account{ID: "acc_a"})
	other := &Worker{ID: 1}
	other.SetAccount(&auth.Account{ID: "acc_b"})
	shared1 := &Worker{ID: 2}
	shared1.SetAccount(&auth.Account{ID: "acc_shared"})
	shared2 := &Worker{ID: 3}
	shared2.SetAccount(&auth.Account{ID: "acc_shared"})
	unbound := &Worker{ID: 4}

	p.workers = []*Worker{self, other, shared1, shared2, unbound}

	inUse := p.accountsInUse(self)

	if _, ok := inUse["acc_a"]; ok {
		t.Errorf("own account reported as claimed; a worker must not conflict with itself")
	}
	if got := inUse["acc_b"]; got != 1 {
		t.Errorf("acc_b count = %d, want 1", got)
	}
	if got := inUse["acc_shared"]; got != 2 {
		t.Errorf("acc_shared count = %d, want 2 (both holders must be counted)", got)
	}
	if len(inUse) != 2 {
		t.Errorf("inUse has %d entries, want acc_b and acc_shared: %v", len(inUse), inUse)
	}
	if inUse == nil {
		t.Error("accountsInUse must never return a nil map")
	}
}

// TestAcquire_AllAccountsCoolingReturnsEligibilityError pins FINDING-03.
//
// @purpose Guarantees an exhausted pool reports why it cannot serve, so handlers can answer
// 429 all_accounts_cooling with Retry-After instead of 503 pool_saturated.
// @rationale The acquire loop re-queued an already-rejected worker and kept spinning until the acquire
// deadline, which surfaced as a capacity error and told clients the gateway was overloaded when in
// fact every account was cooling. Re-queueing cannot change a verdict reached in the same call.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; the acquire timeout bounds the run.
func TestAcquire_AllAccountsCoolingReturnsEligibilityError(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Pool.AcquireTimeout = 300 * time.Millisecond
	cfg.Accounts.MinRemainingQuotaPct = 5

	authMgr := auth.NewManager(cfg, nil)
	authMgr.SetAccountsForTest([]auth.Account{{
		ID:             "acct_cooled",
		Email:          "cooled@example.com",
		Tier:           "pro",
		Status:         auth.StatusCooldownWeekly,
		Enabled:        true,
		AssignedWorker: 0,
	}})

	p := &Pool{cfg: cfg, authMgr: authMgr, leaseCh: make(chan *Worker, 1)}

	bound := auth.Account{ID: "acct_cooled", Email: "cooled@example.com", Tier: "pro",
		Status: auth.StatusCooldownWeekly, Enabled: true}
	w := &Worker{ID: 0}
	w.SetAccount(&bound)
	p.workers = []*Worker{w}
	p.leaseCh <- w

	start := time.Now()
	_, err := p.Acquire(context.Background(), "gemini-3.8-flash", false)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error when every account is cooling")
	}
	if !errors.Is(err, ErrNoEligibleAccounts) {
		t.Fatalf("err = %v, want ErrNoEligibleAccounts so handlers answer 429, not 503", err)
	}
	if errors.Is(err, ErrQueueSaturated) {
		t.Fatalf("cooling accounts must not be reported as queue saturation: %v", err)
	}
	// The verdict cannot change within one call, so the pool must not burn the whole acquire budget
	// re-queueing the same worker. Returning near-instantly is what distinguishes the fix from a
	// spin-then-timeout that happens to reach the same error.
	if elapsed > cfg.Pool.AcquireTimeout/2 {
		t.Fatalf("acquire took %v of a %v budget; an already-rejected worker must not be re-queued",
			elapsed, cfg.Pool.AcquireTimeout)
	}
}

// TestReconcileWorkerAccount_MaterialiseFailureMarksAuthExpired pins FINDING-04.
//
// @purpose Ensures a credential that cannot be installed removes that account from selection instead
// of being retried on every lease.
// @rationale A wrong identity, a corrupt export or a revoked refresh token needs an operator to run
// `agy-auth login`. Without this marking, every lease would pick the same broken account, burn a
// subprocess, and fail — and EC-02/EC-03/EC-15's promised transition would never occur.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; the export seam is faked.
func TestReconcileWorkerAccount_MaterialiseFailureMarksAuthExpired(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.MinRemainingQuotaPct = 5
	cfg.Storage.WorkersRootDir = t.TempDir()
	cfg.Accounts.CredentialOpTimeout = 5 * time.Second

	authMgr := auth.NewManager(cfg, nil)
	authMgr.SetAccountsForTest([]auth.Account{
		{ID: "acct_broken", Email: "broken@example.com", Tier: "pro", Status: auth.StatusHealthy, Enabled: true},
		{ID: "acct_broken2", Email: "broken2@example.com", Tier: "pro", Status: auth.StatusHealthy, Enabled: true},
	})
	// Every export fails, so no account can be materialised.
	authMgr.SetExportForTest(func(ctx context.Context, dir, accountID string) error {
		return errors.New("simulated export failure")
	})

	// The bound account is a third one whose quota is spent, forcing a rotation that must materialise.
	spent := auth.Account{
		ID: "acct_spent", Email: "spent@example.com", Tier: "pro",
		Status: auth.StatusCooldownWeekly, Enabled: true,
	}
	p := &Pool{cfg: cfg, authMgr: authMgr, leaseCh: make(chan *Worker, 1)}
	w := &Worker{ID: 0}
	w.SetAccount(&spent)
	p.workers = []*Worker{w}

	err := p.reconcileWorkerAccount(context.Background(), w, "gemini-3.8-flash", false)
	if err == nil {
		t.Fatal("expected reconcile to fail when every export fails")
	}

	marked := false
	for _, acc := range authMgr.GetAccounts() {
		if acc.Status == auth.StatusAuthExpired {
			marked = true
			break
		}
	}
	if !marked {
		t.Fatalf("no account was marked AUTH_EXPIRED after a materialisation failure: %+v",
			authMgr.GetAccounts())
	}
}

// TestReconcileWorkerAccount_TransientFailureLeavesAccountEligible pins FINDING-C1-02's first half.
//
// @purpose Stops an export timeout or a cancelled lease from marking the account AUTH_EXPIRED.
// @rationale The reviewer's probe marked every materialise error expired, so one timeout stranded the
// only account permanently. Only a fault that proves the credential is unusable may expire it.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants O(accounts); no subprocess, no filesystem.
func TestReconcileWorkerAccount_TransientFailureLeavesAccountEligible(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Storage.WorkersRootDir = t.TempDir()
	cfg.Accounts.MinRemainingQuotaPct = 5
	cfg.Accounts.AuthExpiredRetryInterval = time.Hour

	mgr := auth.NewManager(cfg, nil)
	// acct_a is bound to the worker but its Gemini family is spent, so reconcile must rotate to acct_b;
	// installing acct_b's credential then fails transiently.
	mgr.SetAccountsForTest([]auth.Account{
		{ID: "acct_a", Email: "a@example.com", Enabled: true, Status: auth.StatusHealthy,
			Quotas:          map[string]auth.QuotaGroup{},
			FamilyCooldowns: map[string]time.Time{auth.QuotaFamilyGemini: time.Now().Add(time.Hour)}},
		{ID: "acct_b", Email: "b@example.com", Enabled: true, Status: auth.StatusHealthy,
			Quotas: map[string]auth.QuotaGroup{}, FamilyCooldowns: map[string]time.Time{}},
	})
	mgr.SetExportForTest(func(context.Context, string, string) error {
		return fmt.Errorf("%w: context deadline exceeded", auth.ErrCredentialTransient)
	})

	p := &Pool{cfg: cfg, authMgr: mgr, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	bound := auth.Account{ID: "acct_a", Email: "a@example.com", Enabled: true, Status: auth.StatusHealthy,
		Quotas:          map[string]auth.QuotaGroup{},
		FamilyCooldowns: map[string]time.Time{auth.QuotaFamilyGemini: time.Now().Add(time.Hour)}}
	w := NewWorker(0, t.TempDir(), t.TempDir(), t.TempDir(), &bound, cfg, p.logger)
	p.workers = []*Worker{w}

	err := p.reconcileWorkerAccount(context.Background(), w, "gemini-3.8-flash", false)
	if err == nil {
		t.Fatal("expected a materialisation error")
	}
	for _, acc := range mgr.GetAccounts() {
		if acc.Status == auth.StatusAuthExpired && !acc.CooldownUntil.IsZero() &&
			acc.CooldownUntil.After(time.Now().Add(30*time.Minute)) {
			t.Fatalf("a transient export failure must not expire account %s for the full retry window (until %v)",
				acc.ID, acc.CooldownUntil)
		}
	}
	// acct_b must remain selectable, which is the user-visible contract.
	if _, findErr := mgr.FindNextEligibleAccount("acct_a", "gemini-3.8-flash", false, nil); findErr != nil {
		t.Fatalf("account must stay eligible after a transient failure: %v", findErr)
	}
}

// TestQuotaPoller_DoesNotResurrectAuthExpired pins the poller guard.
//
// @purpose Keeps live quota data from overwriting a credential-level fault.
// @rationale Polling refreshes buckets for every account; without the guard it wrote a quota summary
// over AUTH_EXPIRED and put a dead credential back into service.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants O(accounts); no subprocess.
func TestQuotaPoller_DoesNotResurrectAuthExpired(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.MinRemainingQuotaPct = 5
	reset := time.Now().Add(2 * time.Hour)
	mgr := auth.NewManager(cfg, nil)
	mgr.SetAccountsForTest([]auth.Account{
		{ID: "acct_dead", Email: "d@example.com", Enabled: true, Status: auth.StatusAuthExpired,
			Quotas: map[string]auth.QuotaGroup{}, FamilyCooldowns: map[string]time.Time{},
			CooldownUntil: time.Now().Add(time.Hour)},
	})
	mgr.SetFetchUsageForTest(func(context.Context) (map[string]auth.AccountUsage, error) {
		return map[string]auth.AccountUsage{
			"acct_dead": {
				AccountID: "acct_dead", Email: "d@example.com", Active: true,
				// Both families are spent, so a poll would summarise this account as COOLDOWN_WEEKLY and
				// overwrite AUTH_EXPIRED unless the credential-level guard holds.
				Groups: []auth.AgyUsageGroup{
					{Name: "Gemini Models", Buckets: []auth.AgyUsageBucket{
						{ID: "gemini-weekly", Window: "weekly", RemainingFraction: 0, ResetTime: reset.UTC().Format(time.RFC3339)},
						{ID: "gemini-5h", Window: "5h", RemainingFraction: 1, ResetTime: reset.UTC().Format(time.RFC3339)},
					}},
					{Name: "Claude and GPT models", Buckets: []auth.AgyUsageBucket{
						{ID: "3p-weekly", Window: "weekly", RemainingFraction: 0, ResetTime: reset.UTC().Format(time.RFC3339)},
						{ID: "3p-5h", Window: "5h", RemainingFraction: 1, ResetTime: reset.UTC().Format(time.RFC3339)},
					}},
				},
			},
		}, nil
	})

	auth.NewQuotaPoller(mgr, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).Poll(context.Background())

	acc := mgr.GetAccounts()[0]
	if acc.Status != auth.StatusAuthExpired {
		t.Fatalf("status = %s, want AUTH_EXPIRED preserved through a poll", acc.Status)
	}
}

// TestAssignAccounts_LeastUsedSpreadsAcrossWorkers is the test that would have caught D1.
//
// @purpose Proves least_used distributes distinct accounts instead of collapsing every worker onto
// the single highest-quota account.
// @rationale The strategy computed the highest score independently per worker and never read the
// assignment counter it maintained, so with scores 99/81/8 and three workers all three rows were the
// 99 account. An operator who set least_used to spread load got the exact opposite. The old test used
// workerCount=1, which is why the defect survived review.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no filesystem, no subprocess.
func TestAssignAccounts_LeastUsedSpreadsAcrossWorkers(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.AllocationStrategy = config.AllocationStrategyLeastUsed
	p := &Pool{cfg: cfg}

	accounts := []auth.Account{
		accountWithQuotas("acct_best", 99, 99),
		accountWithQuotas("acct_mid", 81, 81),
		accountWithQuotas("acct_worst", 8, 8),
	}

	got := p.assignAccounts(accounts, 3)
	if len(got) != 3 {
		t.Fatalf("expected 3 assignments, got %d", len(got))
	}

	seen := map[string]int{}
	for _, acc := range got {
		seen[acc.ID]++
	}
	if len(seen) != 3 {
		t.Fatalf("least_used collapsed workers onto %d account(s) (%v); want 3 distinct", len(seen), seen)
	}
	// The highest-quota account must still be the first pick, so the strategy remains quota-aware and
	// does not merely degenerate into round_robin.
	if got[0].ID != "acct_best" {
		t.Fatalf("first assignment = %s, want acct_best (highest remaining quota)", got[0].ID)
	}
}

// TestAssignAccounts_LeastUsedFallsBackToReuse pins the behaviour when workers outnumber accounts.
//
// @purpose Proves the strategy reuses accounts evenly rather than failing or starving a worker.
// @rationale Refusing to allocate would leave a configured worker permanently unusable; the correct
// behaviour is the most even split available, with a logged warning.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestAssignAccounts_LeastUsedFallsBackToReuse(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.AllocationStrategy = config.AllocationStrategyLeastUsed
	p := &Pool{cfg: cfg}

	accounts := []auth.Account{
		accountWithQuotas("acct_a", 90, 90),
		accountWithQuotas("acct_b", 50, 50),
	}

	got := p.assignAccounts(accounts, 5)
	if len(got) != 5 {
		t.Fatalf("expected 5 assignments, got %d", len(got))
	}
	counts := map[string]int{}
	for _, acc := range got {
		counts[acc.ID]++
	}
	// 5 slots over 2 accounts: 3/2. The exact split matters less than the spread being even and both
	// accounts used.
	if counts["acct_a"] < 2 || counts["acct_b"] < 2 {
		t.Fatalf("expected both accounts reused, got %v", counts)
	}
	if diff := counts["acct_a"] - counts["acct_b"]; diff < -1 || diff > 1 {
		t.Fatalf("reuse is uneven (%v); no account may carry two more workers than another", counts)
	}
}

// TestAssignAccounts_EmptyVaultReturnsEmpty pins EC-01.
//
// @purpose Proves an empty account list yields an empty assignment instead of a panic.
// @rationale The previous implementation indexed accounts[0] for least_used, so an empty vault was a
// nil-dereference in the startup path rather than an actionable error.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no allocation.
func TestAssignAccounts_EmptyVaultReturnsEmpty(t *testing.T) {
	for _, strategy := range []string{
		config.AllocationStrategyRoundRobin,
		config.AllocationStrategyLeastUsed,
		config.AllocationStrategySticky,
	} {
		cfg := config.DefaultConfig()
		cfg.Accounts.AllocationStrategy = strategy
		p := &Pool{cfg: cfg}

		if got := p.assignAccounts(nil, 3); len(got) != 0 {
			t.Fatalf("%s: expected no assignments for an empty vault, got %d", strategy, len(got))
		}
	}
}

// TestAssignAccounts_SharingWarningFiresWhenCountsMatch pins FR-06.
//
// @purpose Proves the sharing warning reports the real condition instead of a count comparison.
// @rationale The warning was gated on `workerCount > len(accounts)`, which is false in the common
// balanced case (3 workers, 3 accounts) even when a strategy put all three on one account, so the
// operator saw a silent, fully-shared pool. This drives the real `assignAccounts` path — an earlier
// version called `warnOnSharedAccounts` directly, which let the gate be reintroduced without the test
// noticing, making the claimed mutation proof false.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; uses a captured log buffer.
func TestAssignAccounts_SharingWarningFiresWhenCountsMatch(t *testing.T) {
	var buf lockedBuffer
	cfg := config.DefaultConfig()
	cfg.Accounts.AllocationStrategy = config.AllocationStrategyLeastUsed
	p := &Pool{cfg: cfg, logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))}

	// The case the old `workerCount > len(accounts)` gate missed: EQUAL counts (2 workers, 2 accounts)
	// where one account is ineligible, so both workers necessarily share the surviving one.
	spent := accountWithQuotas("acct_spent", 0, 0)
	spent.Quotas["gemini-weekly"] = auth.QuotaGroup{RemainingPct: 0, IsExhausted: true}
	spent.Quotas["3p-weekly"] = auth.QuotaGroup{RemainingPct: 0, IsExhausted: true}
	accounts := []auth.Account{spent, accountWithQuotas("acct_ok", 90, 90)}

	got := p.assignAccounts(accounts, 2)
	if len(got) != 2 {
		t.Fatalf("expected 2 assignments, got %d", len(got))
	}
	if got[0].ID != got[1].ID {
		t.Fatalf("fixture must force sharing, got %s and %s", got[0].ID, got[1].ID)
	}

	out := buf.String()
	if !strings.Contains(out, "multiple workers share one account") {
		t.Fatalf("expected a sharing warning with equal worker/account counts, got %q", out)
	}
	if !strings.Contains(out, "acct_ok") {
		t.Fatalf("warning must name the shared account, got %q", out)
	}
}

// TestAssignAccounts_NoSharingWarningWhenOnePerWorker pins the warning's negative case.
//
// @purpose Proves a healthy one-account-per-worker pool produces no warning.
// @rationale A warning that fires unconditionally would be noise an operator learns to ignore.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestAssignAccounts_NoSharingWarningWhenOnePerWorker(t *testing.T) {
	var buf lockedBuffer
	cfg := config.DefaultConfig()
	cfg.Accounts.AllocationStrategy = config.AllocationStrategyRoundRobin
	p := &Pool{cfg: cfg, logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))}

	accounts := []auth.Account{
		accountWithQuotas("acct_a", 90, 90),
		accountWithQuotas("acct_b", 90, 90),
		accountWithQuotas("acct_c", 90, 90),
	}
	got := p.assignAccounts(accounts, 3)
	if len(got) != 3 {
		t.Fatalf("expected 3 assignments, got %d", len(got))
	}
	if out := buf.String(); strings.Contains(out, "multiple workers share") {
		t.Fatalf("a one-account-per-worker pool must not warn, got %q", out)
	}
}

// lockedBuffer is a concurrency-safe bytes.Buffer for log assertions.
//
// @purpose Lets a test capture slog output without racing the handler goroutine.
// @rationale slog may write from another goroutine; a bare bytes.Buffer would be a data race under
// -race even when only one goroutine is expected.
// @preconditions None.
// @param None.
// @returns None.
// @throws None.
// @invariants O(len(p)) per write.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestInitWorkers_BootBindingIsLowestIdWins pins FINDING-R9.
//
// @purpose Drives the real boot path and proves `assigned_worker` is not stamped in loop order.
// @rationale `initWorkers` called `MarkAccountBinding(acc.ID, i)` per iteration, so when one account
// was assigned to several workers the field ended up owned by the HIGHEST holder ({0,2} ⇒ 2) until a
// later publish corrected it — the same last-writer-wins defect as FINDING-R7, through boot. An
// earlier version of this test called `reconcileBindings` itself, so removing the call from
// `initWorkers` left it green: it has to run the real function.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Spawns no ACP process; the export seam is stubbed so provisioning is filesystem-only.
func TestInitWorkers_BootBindingIsLowestIdWins(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.AllocationStrategy = config.AllocationStrategyRoundRobin
	cfg.Pool.WorkerCount = 2
	cfg.Storage.BaseDataDir = t.TempDir()
	cfg.Storage.WorkersRootDir = t.TempDir()
	cfg.Storage.WorkersTmpDir = t.TempDir()
	cfg.Storage.WorkspacesRootDir = t.TempDir()

	mgr := auth.NewManager(cfg, nil)
	// ONE account for TWO workers: round_robin must share it, which is the shape that exposes the bug.
	mgr.SetAccountsForTest([]auth.Account{
		{ID: "acct_shared", Email: "shared@example.com", Enabled: true, Status: auth.StatusHealthy,
			AssignedWorker: -1, Quotas: map[string]auth.QuotaGroup{}, FamilyCooldowns: map[string]time.Time{}},
	})
	mgr.SetExportForTest(func(_ context.Context, dir, _ string) error {
		payload := `{"token":{"access_token":"a","token_type":"Bearer","refresh_token":"r","expiry":"2026-11-27T00:00:00Z"},"auth_method":"oauth","id_token":"i","id_token_email":"shared@example.com"}`
		return os.WriteFile(filepath.Join(dir, "creds.json"), []byte(payload), 0o600)
	})

	// leaseCh is required: the warm-start goroutine ends with `p.leaseCh <- worker`, which blocks
	// forever on a nil channel and leaks one goroutine per boot for the rest of the test binary's life.
	p := &Pool{cfg: cfg, authMgr: mgr, workers: make([]*Worker, cfg.Pool.WorkerCount),
		leaseCh: make(chan *Worker, cfg.Pool.WorkerCount),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := p.initWorkers(context.Background()); err != nil {
		t.Fatalf("initWorkers: %v", err)
	}
	t.Cleanup(func() {
		for _, w := range p.workers {
			if w != nil {
				w.Close()
			}
		}
	})

	for _, acc := range mgr.GetAccounts() {
		if acc.ID == "acct_shared" && acc.AssignedWorker != 0 {
			t.Fatalf("boot binding = %d, want 0: a shared account must not be owned by its highest holder",
				acc.AssignedWorker)
		}
	}
}
