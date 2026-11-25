package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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

// TestAccountsInUse_ReportsOnlyOtherWorkersClaims pins the inputs rotation depends on.
//
// @purpose Guarantees the pool tells the manager which accounts are genuinely taken by a *different*
// worker, so a worker's own binding never looks like a conflict and an already-shared account is not
// reported at all.
// @rationale Passing a wrong map would either strand a worker (its own claim treated as a conflict)
// or hide a real conflict (a shared account counted as exclusive), so both directions are asserted.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Errorf.
// @invariants O(workers); no locks, no subprocess.
func TestAccountsInUse_ReportsOnlyOtherWorkersClaims(t *testing.T) {
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
	if owner, ok := inUse["acc_b"]; !ok || owner != 1 {
		t.Errorf("acc_b = (%d, %v), want (1, true)", owner, ok)
	}
	if _, ok := inUse["acc_shared"]; ok {
		t.Errorf("an account already held by two workers was reported as exclusive")
	}
	if len(inUse) != 1 {
		t.Errorf("inUse has %d entries, want only acc_b: %v", len(inUse), inUse)
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
