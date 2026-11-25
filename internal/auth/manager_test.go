package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"agy-gateway/internal/config"
)

func TestManager_GetQuota_Mock(t *testing.T) {
	cfg := &config.Config{}
	mgr := NewManager(cfg, nil)

	mgr.accountsMu.Lock()
	mgr.accounts = []Account{
		{
			ID:       "acct_test123",
			Email:    "user1@gmail.com",
			Alias:    "work-account",
			Tier:     "free",
			Status:   StatusHealthy,
			Enabled:  true,
			LastUsed: time.Now(),
		},
		{
			ID:       "acct_test456",
			Email:    "user2@gmail.com",
			Alias:    "personal-account",
			Tier:     "pro",
			Status:   StatusHealthy,
			Enabled:  true,
			LastUsed: time.Now(),
		},
	}
	mgr.accountsMu.Unlock()

	// Query all accounts
	res, err := mgr.GetQuota(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error fetching all quotas: %v", err)
	}

	if res.Total != 2 {
		t.Fatalf("expected 2 accounts, got %d", res.Total)
	}

	// Query specific account by ID
	singleRes, err := mgr.GetQuota(context.Background(), "acct_test123")
	if err != nil {
		t.Fatalf("unexpected error fetching single quota: %v", err)
	}
	if singleRes.Total != 1 || singleRes.Accounts[0].ID != "acct_test123" {
		t.Fatalf("expected account acct_test123, got %+v", singleRes.Accounts)
	}

	// Query specific account by email
	singleEmailRes, err := mgr.GetQuota(context.Background(), "user2@gmail.com")
	if err != nil {
		t.Fatalf("unexpected error fetching single quota by email: %v", err)
	}
	if singleEmailRes.Total != 1 || singleEmailRes.Accounts[0].Email != "user2@gmail.com" {
		t.Fatalf("expected account user2@gmail.com, got %+v", singleEmailRes.Accounts)
	}

	// Query non-existent account
	_, err = mgr.GetQuota(context.Background(), "non_existent")
	if !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("expected ErrAccountNotFound, got %v", err)
	}
}

func TestManager_MarkAndFindEligibleAccount(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := NewManager(cfg, nil)

	mgr.accountsMu.Lock()
	mgr.accounts = []Account{
		{
			ID:      "acc_1",
			Email:   "acc1@example.com",
			Tier:    "free",
			Status:  StatusHealthy,
			Enabled: true,
		},
		{
			ID:      "acc_2",
			Email:   "acc2@example.com",
			Tier:    "pro",
			Status:  StatusHealthy,
			Enabled: true,
		},
	}
	mgr.accountsMu.Unlock()

	// 1. Find next eligible when excluding acc_1
	next, err := mgr.FindNextEligibleAccount("acc_1", "", false, nil)
	if err != nil {
		t.Fatalf("expected next eligible account, got error: %v", err)
	}
	if next.ID != "acc_2" {
		t.Errorf("expected acc_2, got %s", next.ID)
	}

	// 2. Mark acc_2 as in cooldown
	mgr.MarkAccountStatus("acc_2", StatusCooldown5h, 1*time.Hour)
	accs := mgr.GetAccounts()
	if accs[1].Status != StatusCooldown5h || accs[1].CooldownUntil.IsZero() {
		t.Errorf("expected acc_2 to be in cooldown, got %+v", accs[1])
	}

	// 3. Finding next eligible excluding acc_1 now returns ErrNoEligibleAccounts
	_, err = mgr.FindNextEligibleAccount("acc_1", "", false, nil)
	if !errors.Is(err, ErrNoEligibleAccounts) {
		t.Errorf("expected ErrNoEligibleAccounts, got %v", err)
	}

	// 4. Test cooldown auto-recovery. Both the summary window and the per-family windows are moved
	// into the past, which is the state the poller leaves behind once a bucket resets.
	mgr.accountsMu.Lock()
	past := time.Now().Add(-10 * time.Minute)
	mgr.accounts[1].CooldownUntil = past
	for family := range mgr.accounts[1].FamilyCooldowns {
		mgr.accounts[1].FamilyCooldowns[family] = past
	}
	mgr.accountsMu.Unlock()

	recovered, err := mgr.FindNextEligibleAccount("acc_1", "", false, nil)
	if err != nil {
		t.Fatalf("expected auto-recovered account, got err: %v", err)
	}
	if recovered.ID != "acc_2" || recovered.Status != StatusHealthy {
		t.Errorf("expected recovered acc_2 with StatusHealthy, got %+v", recovered)
	}
}

// TestFindNextEligibleAccount_PrefersUnclaimedAccount pins account-exclusivity in rotation.
//
// @purpose Stops rotation from binding a second worker to an account another worker already holds
// while a free eligible account exists.
// @rationale Each worker keeps its own copy of a refresh token; two workers refreshing one account
// concurrently race and only the first succeeds, so the loser silently loses its session. Selection
// must therefore prefer an unclaimed account before falling back to a shared one.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Errorf/t.Fatalf.
// @invariants Deterministic: no time dependence, no subprocess.
func TestFindNextEligibleAccount_PrefersUnclaimedAccount(t *testing.T) {
	mgr := NewManager(config.DefaultConfig(), nil)
	mgr.accountsMu.Lock()
	mgr.accounts = []Account{
		{ID: "acc_a", Email: "a@example.com", Tier: "pro", Status: StatusHealthy, Enabled: true},
		{ID: "acc_b", Email: "b@example.com", Tier: "pro", Status: StatusHealthy, Enabled: true},
		{ID: "acc_c", Email: "c@example.com", Tier: "pro", Status: StatusHealthy, Enabled: true},
	}
	mgr.accountsMu.Unlock()

	// acc_a is claimed by worker 0; acc_b is free.
	inUse := map[string]int{"acc_a": 0}

	next, err := mgr.FindNextEligibleAccount("", "", false, inUse)
	if err != nil {
		t.Fatalf("expected an eligible account, got %v", err)
	}
	if next.ID == "acc_a" {
		t.Fatalf("selection returned acc_a, which another worker already holds; want an unclaimed account")
	}

	// With every account claimed, sharing is the only option and must still succeed.
	all := map[string]int{"acc_a": 0, "acc_b": 1, "acc_c": 2}
	shared, err := mgr.FindNextEligibleAccount("", "", false, all)
	if err != nil {
		t.Fatalf("expected a shared fallback account, got %v", err)
	}
	if shared.ID == "" {
		t.Fatalf("shared fallback returned an empty account")
	}
}

// TestAuthExpired_RecoversAfterRetryWindow pins FINDING-C1-02's second half.
//
// @purpose Stops a runtime credential fault from removing an account forever with no recovery path.
// @rationale MarkAccountStatus(AUTH_EXPIRED, 0) used to be a tombstone: Sync preserved it and no code
// cleared it, so one transient failure left the vault permanently unusable. The state is now bounded
// by AuthExpiredRetryInterval.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants O(accounts); no subprocess or filesystem access.
func TestAuthExpired_RecoversAfterRetryWindow(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := NewManager(cfg, nil)
	mgr.SetAccountsForTest([]Account{
		{ID: "acct_a", Email: "a@example.com", Enabled: true, Status: StatusHealthy,
			Quotas: map[string]QuotaGroup{}, FamilyCooldowns: map[string]time.Time{}},
	})

	mgr.MarkAccountStatus("acct_a", StatusAuthExpired, time.Hour)

	acc := mgr.GetAccounts()[0]
	if acc.Status != StatusAuthExpired {
		t.Fatalf("status = %s, want AUTH_EXPIRED", acc.Status)
	}
	if _, err := mgr.FindNextEligibleAccount("", "gemini-3.8-flash", false, nil); err == nil {
		t.Fatal("an auth-expired account must not be selectable inside its retry window")
	}

	// Simulate the window elapsing rather than sleeping for it.
	mgr.accountsMu.Lock()
	mgr.accounts[0].CooldownUntil = time.Now().Add(-time.Second)
	mgr.accountsMu.Unlock()

	next, err := mgr.FindNextEligibleAccount("", "gemini-3.8-flash", false, nil)
	if err != nil {
		t.Fatalf("expected the account to return to service after the retry window: %v", err)
	}
	if next.ID != "acct_a" {
		t.Fatalf("selected %s, want acct_a", next.ID)
	}
	if got := mgr.GetAccounts()[0].Status; got != StatusHealthy {
		t.Fatalf("status after recovery = %s, want HEALTHY", got)
	}
}

// TestSync_KeepsVaultReportedExpiryPermanent pins the counterpart: only an operator can fix a
// credential the vault itself reports as expired.
//
// @purpose Prevents the bounded retry window from masking a genuinely dead credential.
// @Rationale Sync is authoritative about expiry; a runtime retry window must not survive it.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants O(accounts); no subprocess.
func TestSync_KeepsVaultReportedExpiryPermanent(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := NewManager(cfg, nil)
	mgr.SetAccountsForTest([]Account{
		{ID: "acct_a", Email: "a@example.com", Enabled: true, Status: StatusHealthy,
			Quotas: map[string]QuotaGroup{}, FamilyCooldowns: map[string]time.Time{},
			CooldownUntil: time.Now().Add(time.Hour)},
	})

	// The vault reports this credential as expired; a stale runtime window must not revive it.
	mgr.listForTest = func(context.Context) ([]byte, error) {
		return []byte(`{"data":{"accounts":[{"id":"acct_a","email":"a@example.com","expired":true}]}}`), nil
	}
	if err := mgr.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	acc := mgr.GetAccounts()[0]
	if acc.Status != StatusAuthExpired {
		t.Fatalf("status = %s, want AUTH_EXPIRED", acc.Status)
	}
	if !acc.CooldownUntil.IsZero() {
		t.Fatalf("a vault-reported expiry must be permanent, got cooldown until %v", acc.CooldownUntil)
	}
}

// TestSync_HonoursLegitimatelyEmptyVault pins FINDING-C2-01.
//
// @purpose Lets an operator remove the last account without the gateway refusing to observe it.
// @rationale The cycle-2 parse guard conflated "unparseable payload" with "zero accounts", so a real
// empty vault was rejected and the removed accounts persisted forever.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants O(accounts); no subprocess.
func TestSync_HonoursLegitimatelyEmptyVault(t *testing.T) {
	cases := map[string]string{
		"envelope with empty array": `{"data":{"accounts":[]}}`,
		"bare empty array":          `[]`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			mgr := NewManager(cfg, nil)
			mgr.SetAccountsForTest([]Account{
				{ID: "acct_gone", Email: "gone@example.com", Enabled: true, Status: StatusHealthy,
					Quotas: map[string]QuotaGroup{}, FamilyCooldowns: map[string]time.Time{}},
			})
			mgr.listForTest = func(context.Context) ([]byte, error) {
				return []byte(payload), nil
			}
			if err := mgr.Sync(context.Background()); err != nil {
				t.Fatalf("a legitimately empty vault must not be an error: %v", err)
			}
			if got := len(mgr.GetAccounts()); got != 0 {
				t.Fatalf("accounts after sync = %d, want 0 (removal must be observed)", got)
			}
		})
	}
}

// TestSync_NullPayloadKeepsAccounts pins the LOW from the cycle-3 audit.
//
// @purpose Stops a bare JSON `null` from being read as an empty vault.
// @rationale `json.Unmarshal` succeeds into a nil slice for `null`, so a fail-closed parse must also
// demand a non-nil array; otherwise one malformed response removes every account.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants O(accounts); no subprocess.
func TestSync_NullPayloadKeepsAccounts(t *testing.T) {
	for _, payload := range []string{`null`, `{"data":{"accounts":null}}`} {
		cfg := config.DefaultConfig()
		mgr := NewManager(cfg, nil)
		mgr.SetAccountsForTest([]Account{
			{ID: "acct_a", Email: "a@example.com", Enabled: true, Status: StatusHealthy,
				Quotas: map[string]QuotaGroup{}, FamilyCooldowns: map[string]time.Time{}},
		})
		mgr.listForTest = func(context.Context) ([]byte, error) { return []byte(payload), nil }
		if err := mgr.Sync(context.Background()); err == nil {
			t.Fatalf("payload %q must be rejected, not read as an empty vault", payload)
		}
		if got := len(mgr.GetAccounts()); got != 1 {
			t.Fatalf("payload %q: accounts = %d, want the previous 1 intact", payload, got)
		}
	}
}

// TestSync_UnreadablePayloadKeepsAccounts pins the other half of FINDING-C2-01.
//
// @purpose Keeps one malformed response from taking every worker out of service.
// @Rationale Same guard, opposite direction: garbage is not evidence that the vault is empty.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants O(accounts); no subprocess.
func TestSync_UnreadablePayloadKeepsAccounts(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := NewManager(cfg, nil)
	mgr.SetAccountsForTest([]Account{
		{ID: "acct_a", Email: "a@example.com", Enabled: true, Status: StatusHealthy,
			Quotas: map[string]QuotaGroup{}, FamilyCooldowns: map[string]time.Time{}},
	})
	mgr.listForTest = func(context.Context) ([]byte, error) {
		return []byte(`{"data":{"accounts":`), nil // truncated JSON
	}
	if err := mgr.Sync(context.Background()); err == nil {
		t.Fatal("expected an error for an unparseable payload")
	}
	if got := len(mgr.GetAccounts()); got != 1 {
		t.Fatalf("accounts after a failed sync = %d, want the previous 1 intact", got)
	}
	if got := mgr.GetAccounts()[0].Status; got != StatusHealthy {
		t.Fatalf("status = %s, want HEALTHY preserved", got)
	}
}

// TestSync_RecoversAfterRelogin pins FINDING-C2-02.
//
// @purpose Makes `agy-auth login` actually restore an account instead of requiring a restart.
// @Rationale A vault-reported AUTH_EXPIRED was carried forward even after the vault reported
// `expired:false`, so the operator's fresh credential stayed invisible.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants O(accounts); no subprocess.
func TestSync_RecoversAfterRelogin(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := NewManager(cfg, nil)
	mgr.SetAccountsForTest([]Account{
		{ID: "acct_a", Email: "a@example.com", Enabled: true, Status: StatusHealthy,
			Quotas: map[string]QuotaGroup{}, FamilyCooldowns: map[string]time.Time{}},
	})

	expired := func(context.Context) ([]byte, error) {
		return []byte(`{"data":{"accounts":[{"id":"acct_a","email":"a@example.com","expired":true}]}}`), nil
	}
	healthy := func(context.Context) ([]byte, error) {
		return []byte(`{"data":{"accounts":[{"id":"acct_a","email":"a@example.com","expired":false}]}}`), nil
	}

	mgr.listForTest = expired
	if err := mgr.Sync(context.Background()); err != nil {
		t.Fatalf("Sync(expired): %v", err)
	}
	if got := mgr.GetAccounts()[0].Status; got != StatusAuthExpired {
		t.Fatalf("status = %s, want AUTH_EXPIRED", got)
	}

	// The operator ran `agy-auth login`; the vault now reports the credential as healthy.
	mgr.listForTest = healthy
	if err := mgr.Sync(context.Background()); err != nil {
		t.Fatalf("Sync(healthy): %v", err)
	}
	acc := mgr.GetAccounts()[0]
	if acc.Status != StatusHealthy {
		t.Fatalf("status after re-login = %s, want HEALTHY", acc.Status)
	}
	if acc.VaultExpired {
		t.Fatal("vault_expired must be cleared once the vault reports the credential healthy")
	}
	if _, err := mgr.FindNextEligibleAccount("", "gemini-3.8-flash", false, nil); err != nil {
		t.Fatalf("the re-logged-in account must be selectable: %v", err)
	}
}

// TestSync_KeepsRuntimeAuthExpiredAcrossVaultRefresh pins the guard on the recovery rule.
//
// @purpose Stops a vault refresh from silently reviving an account that a runtime fault removed.
// @Rationale Only a vault-reported expiry is restored by the vault; a runtime fault is owned by the
// bounded retry window, which is the only thing allowed to clear it.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants O(accounts); no subprocess.
func TestSync_KeepsRuntimeAuthExpiredAcrossVaultRefresh(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := NewManager(cfg, nil)
	mgr.SetAccountsForTest([]Account{
		{ID: "acct_a", Email: "a@example.com", Enabled: true, Status: StatusHealthy,
			Quotas: map[string]QuotaGroup{}, FamilyCooldowns: map[string]time.Time{}},
	})
	// Runtime fault: not a vault report, so provenance is false.
	mgr.MarkAccountStatus("acct_a", StatusAuthExpired, time.Hour)

	mgr.listForTest = func(context.Context) ([]byte, error) {
		return []byte(`{"data":{"accounts":[{"id":"acct_a","email":"a@example.com","expired":false}]}}`), nil
	}
	if err := mgr.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := mgr.GetAccounts()[0].Status; got != StatusAuthExpired {
		t.Fatalf("status = %s, want the runtime AUTH_EXPIRED to survive a vault refresh", got)
	}
}
