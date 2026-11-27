package auth

import (
	"context"
	"testing"

	"agy-gateway/internal/config"
)

// TestQuotaPoller_ResyncsVaultWhileEmpty pins the no-restart onboarding path.
//
// @purpose Proves the poller notices an account added out of band after booting with an empty vault.
// @rationale The onboarding CLI runs in a separate process, so nothing in this process sees the new
// vault entry until it is re-read. Without this, a fresh install stayed in no_accounts until a
// restart even after `agy-accounts add` succeeded.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Spawns no subprocess; the list and usage seams are both stubbed.
func TestQuotaPoller_ResyncsVaultWhileEmpty(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := newQuotaTestManager(t, cfg, nil, nil)
	mgr.SetAccountsForTest(nil)

	listCalls := 0
	mgr.listForTest = func(context.Context) ([]byte, error) {
		listCalls++
		// The account appears only on the second read, as if the operator had just onboarded.
		if listCalls == 1 {
			return []byte(`{"data":{"accounts":[]}}`), nil
		}
		return []byte(`{"data":{"accounts":[{"id":"acct_new","email":"new@example.com","active":true}]}}`), nil
	}

	poller := NewQuotaPoller(mgr, cfg, nil)
	poller.Poll(context.Background())
	if got := len(mgr.GetAccounts()); got != 0 {
		t.Fatalf("precondition: first poll must see an empty vault, got %d accounts", got)
	}

	poller.Poll(context.Background())
	if got := len(mgr.GetAccounts()); got != 1 {
		t.Fatalf("poller did not pick up the onboarded account: %d accounts", got)
	}
	if mgr.GetAccounts()[0].ID != "acct_new" {
		t.Fatalf("picked up the wrong account: %q", mgr.GetAccounts()[0].ID)
	}
}
