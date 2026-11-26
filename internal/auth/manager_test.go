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

	// Claim counts, not owner ids: acc_a is held by one other worker; acc_b and acc_c are free.
	inUse := map[string]int{"acc_a": 1}

	next, err := mgr.FindNextEligibleAccount("", "", false, inUse)
	if err != nil {
		t.Fatalf("expected an eligible account, got %v", err)
	}
	if next.ID == "acc_a" {
		t.Fatalf("selection returned acc_a, which another worker already holds; want an unclaimed account")
	}

	// With every account claimed, sharing is the only option and must still succeed. The least
	// contended account must win, so rotation never piles onto the account three workers share while a
	// one-way one is available.
	all := map[string]int{"acc_a": 3, "acc_b": 1, "acc_c": 2}
	shared, err := mgr.FindNextEligibleAccount("", "", false, all)
	if err != nil {
		t.Fatalf("expected a shared fallback account, got %v", err)
	}
	if shared.ID != "acc_b" {
		t.Fatalf("shared fallback selected %s, want acc_b (the least contended account)", shared.ID)
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

// TestMarkAccountBinding_SetsAndClears pins FR-08's binding half.
//
// @purpose Proves `assigned_worker` becomes truthful.
// @rationale Nothing ever wrote this field, so the admin API reported -1 for every account no matter
// which worker was actually spending its quota.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants O(accounts); no IO.
func TestMarkAccountBinding_SetsAndClears(t *testing.T) {
	mgr := NewManager(config.DefaultConfig(), nil)
	mgr.SetAccountsForTest([]Account{{
		ID: "acct_a", Email: "a@example.com", Enabled: true, Status: StatusHealthy,
		AssignedWorker: -1, Quotas: map[string]QuotaGroup{}, FamilyCooldowns: map[string]time.Time{},
	}})

	mgr.MarkAccountBinding("acct_a", 2)
	if got := mgr.GetAccounts()[0].AssignedWorker; got != 2 {
		t.Fatalf("AssignedWorker = %d, want 2", got)
	}

	mgr.MarkAccountBinding("acct_a", -1)
	if got := mgr.GetAccounts()[0].AssignedWorker; got != -1 {
		t.Fatalf("AssignedWorker = %d, want -1 after clearing", got)
	}

	// An unknown account must be a no-op, not a panic or a phantom entry.
	mgr.MarkAccountBinding("acct_missing", 0)
	if got := len(mgr.GetAccounts()); got != 1 {
		t.Fatalf("account count = %d, want 1", got)
	}
}

// TestMarkAccountUsed_ThrottlesWrites pins FR-08's usage half.
//
// @purpose Proves `last_used` tracks real leases without writing on every request.
// @rationale Acquire is the hot path and every write takes accountsMu; per-request writes would
// create a lock convoy. One update per second per account carries the same operational meaning.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants O(accounts); no IO.
func TestMarkAccountUsed_ThrottlesWrites(t *testing.T) {
	mgr := NewManager(config.DefaultConfig(), nil)
	mgr.SetAccountsForTest([]Account{{
		ID: "acct_a", Email: "a@example.com", Enabled: true, Status: StatusHealthy,
		Quotas: map[string]QuotaGroup{}, FamilyCooldowns: map[string]time.Time{},
	}})

	base := time.Now()
	mgr.MarkAccountUsed("acct_a", base)
	first := mgr.GetAccounts()[0].LastUsed
	if !first.Equal(base) {
		t.Fatalf("LastUsed = %v, want %v", first, base)
	}

	// Inside the window: no write.
	mgr.MarkAccountUsed("acct_a", base.Add(200*time.Millisecond))
	if got := mgr.GetAccounts()[0].LastUsed; !got.Equal(first) {
		t.Fatalf("LastUsed changed inside the throttle window: %v -> %v", first, got)
	}

	// Past the window: the write happens.
	later := base.Add(2 * time.Second)
	mgr.MarkAccountUsed("acct_a", later)
	if got := mgr.GetAccounts()[0].LastUsed; !got.Equal(later) {
		t.Fatalf("LastUsed = %v, want %v after the window", got, later)
	}
}

// TestSync_PreservesQuotaKnowledge pins the carry-forward extension.
//
// @purpose Stops a resync from promoting every account to "known healthy".
// @rationale Sync rebuilds each account from the vault, which knows nothing about whether the last
// quota read succeeded. Dropping QuotasKnown there would silently undo the fail-open fix on the next
// resync — the same class of state loss the existing carry-forward exists to prevent.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants O(accounts); no subprocess.
func TestSync_PreservesQuotaKnowledge(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := NewManager(cfg, nil)
	mgr.SetAccountsForTest([]Account{{
		ID: "acct_a", Email: "a@example.com", Enabled: true, Status: StatusHealthy,
		QuotasKnown: false, UsageError: "forbidden",
		Quotas:          map[string]QuotaGroup{"gemini-weekly": {RemainingPct: 42}},
		FamilyCooldowns: map[string]time.Time{},
	}})

	mgr.listForTest = func(context.Context) ([]byte, error) {
		return []byte(`{"data":{"accounts":[{"id":"acct_a","email":"a@example.com","expired":false}]}}`), nil
	}
	if err := mgr.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	acc := mgr.GetAccounts()[0]
	if acc.QuotasKnown {
		t.Fatal("QuotasKnown must survive a resync as false")
	}
	if acc.UsageError != "forbidden" {
		t.Fatalf("UsageError = %q, want it preserved across a resync", acc.UsageError)
	}
	if acc.Quotas["gemini-weekly"].RemainingPct != 42 {
		t.Fatalf("known quota was lost across a resync: %v", acc.Quotas)
	}
}

// TestFindNextEligibleAccount_PrefersHigherRank pins FR-01's selection effect.
//
// @purpose Proves a known-healthy account is chosen over an unknown-quota one regardless of vault
// order.
// @rationale The previous first-match loop returned whichever eligible account came first, so an
// unreadable account could be preferred purely because it was listed earlier.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants O(accounts); no IO.
func TestFindNextEligibleAccount_PrefersHigherRank(t *testing.T) {
	mgr := NewManager(config.DefaultConfig(), nil)
	mgr.SetAccountsForTest([]Account{
		{ID: "acct_unknown", Email: "u@example.com", Tier: "payg", Status: StatusHealthy, Enabled: true,
			QuotasKnown: false, Quotas: map[string]QuotaGroup{}, FamilyCooldowns: map[string]time.Time{}},
		{ID: "acct_known", Email: "k@example.com", Tier: "payg", Status: StatusHealthy, Enabled: true,
			QuotasKnown:     true,
			Quotas:          map[string]QuotaGroup{"gemini-weekly": {RemainingPct: 90}},
			FamilyCooldowns: map[string]time.Time{}},
	})

	next, err := mgr.FindNextEligibleAccount("", "gemini-3.8-flash", false, nil)
	if err != nil {
		t.Fatalf("expected a selection: %v", err)
	}
	if next.ID != "acct_known" {
		t.Fatalf("selected %s, want acct_known (rank 2 beats rank 1 despite vault order)", next.ID)
	}
}

// TestGetAccountsQuota_UsageErrorIsNotReportedAvailable pins F-05.
//
// @purpose Proves the admin quota endpoint no longer reports an unread account as available.
// @rationale The account list was fixed to expose the failure, but the quota DETAIL endpoint — the
// one an operator actually reads to debug selection — still reported `quota_available: true` with zero
// buckets, which is the same fail-open at a second surface.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants O(accounts); no subprocess.
func TestGetAccountsQuota_UsageErrorIsNotReportedAvailable(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := NewManager(cfg, nil)
	mgr.SetAccountsForTest([]Account{
		{ID: "acct_bad", Email: "b@example.com", Enabled: true, Status: StatusHealthy,
			QuotasKnown: false, UsageError: "forbidden",
			Quotas: map[string]QuotaGroup{}, FamilyCooldowns: map[string]time.Time{}},
	})
	mgr.SetFetchUsageForTest(func(context.Context) (map[string]AccountUsage, error) {
		return map[string]AccountUsage{
			"acct_bad": {AccountID: "acct_bad", Email: "b@example.com", UsageError: "forbidden"},
		}, nil
	})

	resp, err := mgr.GetQuota(context.Background(), "")
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if len(resp.Accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(resp.Accounts))
	}
	info := resp.Accounts[0]
	if info.QuotaAvailable {
		t.Fatal("an account whose quota read failed must not report quota_available")
	}
	if info.QuotasKnown {
		t.Fatal("quotas_known must be false for an unread account")
	}
	if info.UsageError != "forbidden" {
		t.Fatalf("usage_error = %q, want %q exposed to the operator", info.UsageError, "forbidden")
	}
}

// TestGetQuota_LiveUsageErrorOverridesHealthySnapshot pins FINDING-R4.
//
// @purpose Stops the quota detail endpoint from trusting a stale snapshot over the current fetch.
// @rationale My first F-05 fix read only `acc.QuotasKnown`, so when the last poll succeeded and THIS
// call's fetch came back with `usage_error: "forbidden"`, the endpoint still answered
// `quotas_known=true quota_available=true` with zero buckets — the same fail-open, one call later.
// @preconditions None.
// @param Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants O(accounts); no subprocess.
func TestGetQuota_LiveUsageErrorOverridesHealthySnapshot(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := NewManager(cfg, nil)
	// Snapshot says the last poll succeeded.
	mgr.SetAccountsForTest([]Account{
		{ID: "acct_a", Email: "a@example.com", Enabled: true, Status: StatusHealthy,
			QuotasKnown:     true,
			Quotas:          map[string]QuotaGroup{"gemini-weekly": {RemainingPct: 80}},
			FamilyCooldowns: map[string]time.Time{}},
	})
	// ...but this fetch fails for that account specifically.
	mgr.SetFetchUsageForTest(func(context.Context) (map[string]AccountUsage, error) {
		return map[string]AccountUsage{
			"acct_a": {AccountID: "acct_a", Email: "a@example.com", UsageError: "forbidden"},
		}, nil
	})

	resp, err := mgr.GetQuota(context.Background(), "")
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	info := resp.Accounts[0]
	if info.QuotaAvailable {
		t.Fatal("a live usage_error must not be reported as quota_available, even if the snapshot was healthy")
	}
	if info.QuotasKnown {
		t.Fatal("quotas_known must be false when this call's fetch failed")
	}
	if info.UsageError != "forbidden" {
		t.Fatalf("usage_error = %q, want the live reason", info.UsageError)
	}
	if len(info.Groups) != 0 || len(info.Quotas) != 0 {
		t.Fatalf("a failed read must not serve stale buckets: groups=%v quotas=%v", info.Groups, info.Quotas)
	}

	// A healthy live fetch must still report availability.
	mgr.SetFetchUsageForTest(func(context.Context) (map[string]AccountUsage, error) {
		return map[string]AccountUsage{
			"acct_a": {AccountID: "acct_a", Email: "a@example.com", Groups: []AgyUsageGroup{
				{Name: "Gemini Models", Buckets: []AgyUsageBucket{
					{ID: "gemini-weekly", Window: "weekly", RemainingFraction: 0.8},
				}},
			}},
		}, nil
	})
	resp, err = mgr.GetQuota(context.Background(), "")
	if err != nil {
		t.Fatalf("GetQuota(healthy): %v", err)
	}
	if !resp.Accounts[0].QuotaAvailable || !resp.Accounts[0].QuotasKnown {
		t.Fatalf("a healthy fetch must report available/known: %+v", resp.Accounts[0])
	}
}

// TestSync_MapsVaultActiveFlag pins FINDING-R6 using a captured live payload.
//
// @purpose Proves the vault's `active` flag actually reaches `Account.ActiveInVault`.
// @rationale The only test touching the flag injected it directly via SetAccountsForTest, so forcing
// `ActiveInVault: false` in Sync left both packages green — the payload → field path was untested,
// and F-07's first-boot seed therefore rested on an unverified mapping.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants O(accounts); no subprocess.
func TestSync_MapsVaultActiveFlag(t *testing.T) {
	// Shape captured verbatim from `agy-auth list --all --json` on agy-auth 0.5.3: there is no
	// `expired` key at all, and `groups` is present in the list payload.
	payload := `{"schema_version":1,"command":"list","data":{"accounts":[
		{"id":"acct_a","number":1,"email":"a@example.com","alias":"acct-a","active":false,
		 "description":"","updated_at":"2026-11-26T00:00:00Z","groups":[]},
		{"id":"acct_b","number":2,"email":"b@example.com","alias":"acct-b","active":true,
		 "description":"","updated_at":"2026-11-26T00:00:00Z","groups":[]}
	]}}`

	cfg := config.DefaultConfig()
	mgr := NewManager(cfg, nil)
	mgr.listForTest = func(context.Context) ([]byte, error) { return []byte(payload), nil }
	if err := mgr.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	byID := map[string]Account{}
	for _, acc := range mgr.GetAccounts() {
		byID[acc.ID] = acc
	}
	if len(byID) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(byID))
	}
	if !byID["acct_b"].ActiveInVault {
		t.Fatal("acct_b is flagged active in the payload but ActiveInVault is false")
	}
	if byID["acct_a"].ActiveInVault {
		t.Fatal("acct_a is not flagged active but ActiveInVault is true")
	}
	// The mapping must be what actually drives the seed.
	if got := mgr.VaultActiveAccountID(); got != "acct_b" {
		t.Fatalf("VaultActiveAccountID() = %q, want acct_b", got)
	}
	if got := byID["acct_a"].Status; got != StatusHealthy {
		t.Fatalf("status = %s: the live payload carries no `expired` key, so it must be HEALTHY", got)
	}
}
