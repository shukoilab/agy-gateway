package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"agy-gateway/internal/config"
)

// writeWorkerCredential installs a credential file for a worker HOME, the same shape the gateway
// writes in materializeLocked.
//
// @purpose Builds the on-disk precondition for the purge tests without an export subprocess.
// @rationale The purge is defined by what is on disk, so the test must produce a real file with a real
// identity field rather than a stub.
// @preconditions root is a directory.
// @param t Standard Go test handle.
// @param root WorkersRootDir.
// @param workerID Worker index.
// @param email Identity to embed.
// @returns The path written.
// @throws None; failures are reported through t.Fatalf.
// @invariants Writes exactly one file at mode 0600.
func writeWorkerCredential(t *testing.T, root string, workerID int, email string) string {
	t.Helper()
	dir := filepath.Join(root, "w"+strconv.Itoa(workerID), ".gemini", "antigravity-cli")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	payload, err := json.Marshal(map[string]any{
		"token":          map[string]any{"access_token": "a", "refresh_token": "r"},
		"auth_method":    "consumer",
		"id_token_email": email,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(dir, "antigravity-oauth-token")
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatalf("write credential: %v", err)
	}
	return path
}

// TestSync_PurgesWorkerCredentialsOfDeletedAccounts pins the on-disk half of deletion.
//
// @purpose Proves the plaintext credential installed for a removed account does not survive a resync.
// @rationale `agy-auth remove` only knows the vault; the per-worker credential copies the gateway
// installed are its own responsibility. Without the purge a deleted account kept a working refresh
// token readable on disk under every worker HOME indefinitely.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants No subprocess beyond the stubbed list call.
func TestSync_PurgesWorkerCredentialsOfDeletedAccounts(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Pool.WorkerCount = 3
	cfg.Storage.WorkersRootDir = t.TempDir()

	doomed := writeWorkerCredential(t, cfg.Storage.WorkersRootDir, 0, "doomed@example.com")
	kept := writeWorkerCredential(t, cfg.Storage.WorkersRootDir, 1, "kept@example.com")

	mgr := NewManager(cfg, nil)
	mgr.SetAccountsForTest([]Account{{ID: "acct_doomed", Email: "doomed@example.com", Enabled: true}})
	// The vault no longer reports the doomed account.
	mgr.listForTest = func(context.Context) ([]byte, error) {
		return []byte(`{"data":{"accounts":[{"id":"acct_kept","email":"kept@example.com","active":true}]}}`), nil
	}

	if err := mgr.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if _, err := os.Stat(doomed); !os.IsNotExist(err) {
		t.Fatalf("credential of a deleted account must be removed, stat err = %v", err)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("credential of a live account must be kept, stat err = %v", err)
	}
	if got := len(mgr.GetAccounts()); got != 1 {
		t.Fatalf("account set after sync = %d, want 1", got)
	}
}

// TestSync_KeepsCredentialsWhenVaultStillKnowsTheAccount is the no-op guard.
//
// @purpose Proves the purge never deletes a credential for an account that still exists.
// @rationale A purge keyed on "not in this response" would wipe every worker on a partial payload;
// only accounts confirmed absent from a successfully parsed list may be purged.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants No subprocess beyond the stubbed list call.
func TestSync_KeepsCredentialsWhenVaultStillKnowsTheAccount(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Pool.WorkerCount = 1
	cfg.Storage.WorkersRootDir = t.TempDir()

	path := writeWorkerCredential(t, cfg.Storage.WorkersRootDir, 0, "alive@example.com")

	mgr := NewManager(cfg, nil)
	mgr.SetAccountsForTest([]Account{{ID: "acct_alive", Email: "alive@example.com", Enabled: true}})
	mgr.listForTest = func(context.Context) ([]byte, error) {
		return []byte(`{"data":{"accounts":[{"id":"acct_alive","email":"alive@example.com","active":true}]}}`), nil
	}

	if err := mgr.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("credential of a live account must never be purged, stat err = %v", err)
	}
}

// TestSync_UnparseableListPurgesNothing pins the fail-safe direction of the purge.
//
// @purpose Proves a malformed vault response cannot delete worker credentials.
// @rationale Sync already refuses to wipe the account set on an unparseable payload; the purge must
// inherit the same rule, otherwise a broken CLI would destroy every worker's credential.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants No subprocess beyond the stubbed list call.
func TestSync_UnparseableListPurgesNothing(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Pool.WorkerCount = 1
	cfg.Storage.WorkersRootDir = t.TempDir()

	path := writeWorkerCredential(t, cfg.Storage.WorkersRootDir, 0, "alive@example.com")

	mgr := NewManager(cfg, nil)
	mgr.SetAccountsForTest([]Account{{ID: "acct_alive", Email: "alive@example.com", Enabled: true}})
	mgr.listForTest = func(context.Context) ([]byte, error) {
		return []byte(`<html>gateway error</html>`), nil
	}

	if err := mgr.Sync(context.Background()); err == nil {
		t.Fatal("Sync must report an unparseable payload")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("an unparseable payload must not purge credentials, stat err = %v", err)
	}
}

// TestQuotaPoller_DropsDeletedAccount pins the steady-state view.
//
// @purpose Proves the background poll notices an account removed through the CLI.
// @rationale The poller only resynced while the account set was empty, so in steady state a deleted
// account stayed HEALTHY in memory forever and rotation kept offering it. Every interval now resyncs.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants No subprocess; both the list and usage seams are stubbed.
func TestQuotaPoller_DropsDeletedAccount(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.MinRemainingQuotaPct = 5
	cfg.Pool.WorkerCount = 1
	cfg.Storage.WorkersRootDir = t.TempDir()

	mgr := newQuotaTestManager(t, cfg, map[string]AccountUsage{
		"acct_live": usageWithBuckets("acct_live", "live@example.com", 90, 90, time.Now().Add(24*time.Hour)),
	}, nil)
	mgr.SetAccountsForTest([]Account{
		{ID: "acct_live", Email: "live@example.com", Enabled: true, Status: StatusHealthy,
			Quotas: map[string]QuotaGroup{}, FamilyCooldowns: map[string]time.Time{}},
		{ID: "acct_doomed", Email: "doomed@example.com", Enabled: true, Status: StatusHealthy,
			Quotas: map[string]QuotaGroup{}, FamilyCooldowns: map[string]time.Time{}},
	})
	mgr.listForTest = func(context.Context) ([]byte, error) {
		return []byte(`{"data":{"accounts":[{"id":"acct_live","email":"live@example.com","active":true}]}}`), nil
	}

	NewQuotaPoller(mgr, cfg, nil).Poll(context.Background())

	accounts := mgr.GetAccounts()
	if len(accounts) != 1 || accounts[0].ID != "acct_live" {
		t.Fatalf("poller must drop the deleted account, got %+v", accounts)
	}
}
