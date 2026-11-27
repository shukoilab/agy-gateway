package pool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"agy-gateway/internal/auth"
	"agy-gateway/internal/config"
)

// TestReconcileWorkerAccount_RebindsWhenBoundAccountWasDeleted pins the rotation-to-deleted-account bug.
//
// @purpose Proves a worker bound to an account that no longer exists is rebound to a live one.
// @rationale The fast path adopted the manager's view of the bound account by matching ID, but when
// no ID matched it kept the STALE snapshot and judged that instead. A deleted account was still
// HEALTHY in that stale copy, so the lease was granted against an account the vault no longer had:
// rotation kept landing on a deleted account until a restart cleared the binding. An absent binding
// must be treated as unusable so the rotation path runs.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; one stubbed credential install, no subprocess.
func TestReconcileWorkerAccount_RebindsWhenBoundAccountWasDeleted(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.MinRemainingQuotaPct = 5
	cfg.Storage.WorkersRootDir = t.TempDir()

	live := accountWithQuotas("acct_live", 90, 90)
	authMgr := auth.NewManager(cfg, nil)
	authMgr.SetAccountsForTest([]auth.Account{live})
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

	p := &Pool{cfg: cfg, authMgr: authMgr, leaseCh: make(chan *Worker, 1), workers: make([]*Worker, 1)}

	// The worker still holds the snapshot of an account the operator has since deleted.
	deleted := accountWithQuotas("acct_deleted", 90, 90)
	w := &Worker{ID: 1}
	w.SetAccount(&deleted)
	p.workers[0] = w

	if err := p.reconcileWorkerAccount(context.Background(), w, "gemini-3.8-flash", false); err != nil {
		t.Fatalf("reconcileWorkerAccount: %v", err)
	}
	got := w.GetAccount()
	if got == nil || got.ID != "acct_live" {
		t.Fatalf("a worker bound to a deleted account must be rebound to a live one, got %+v", got)
	}
	if !w.TaintedForRecycle.Load() {
		t.Fatal("a rotated worker must be recycled so the running process picks up the new credential")
	}
}

// TestReconcileWorkerAccount_FailsWhenEveryAccountWasDeleted pins the honest failure.
//
// @purpose Proves a deleted account cannot keep a worker leased when nothing live can serve the model.
// @rationale The stale-snapshot fast path returned nil (lease granted) for a deleted account even when
// every live account was spent, so requests were served by a credential the operator had removed.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no subprocess.
func TestReconcileWorkerAccount_FailsWhenEveryAccountWasDeleted(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.MinRemainingQuotaPct = 5

	spent := accountWithQuotas("acct_spent", 1, 1)
	authMgr := auth.NewManager(cfg, nil)
	authMgr.SetAccountsForTest([]auth.Account{spent})

	p := &Pool{cfg: cfg, authMgr: authMgr, leaseCh: make(chan *Worker, 1), workers: make([]*Worker, 1)}
	deleted := accountWithQuotas("acct_deleted", 90, 90)
	w := &Worker{ID: 0}
	w.SetAccount(&deleted)
	p.workers[0] = w

	err := p.reconcileWorkerAccount(context.Background(), w, "gemini-3.8-flash", false)
	if !errors.Is(err, ErrNoEligibleAccounts) {
		t.Fatalf("a deleted account must not satisfy the lease; want ErrNoEligibleAccounts, got %v", err)
	}
	if got := w.GetAccount(); got == nil || got.ID != "acct_deleted" {
		t.Fatalf("a failed rotation must leave the binding untouched, got %+v", got)
	}
	if w.TaintedForRecycle.Load() {
		t.Fatal("a failed rotation must not taint the worker")
	}
}
