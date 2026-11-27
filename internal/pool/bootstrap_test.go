package pool

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agy-gateway/internal/auth"
	"agy-gateway/internal/config"
)

// TestInitWorkers_EmptyVaultBootsInsteadOfFailing pins the fresh-install crash loop.
//
// @purpose Proves an empty vault yields a bootable, zero-worker pool instead of an error.
// @rationale initWorkers returned an error when the vault held no accounts; main exits on that error
// and `restart: unless-stopped` restarted it forever, so a fresh install could never run the
// onboarding CLI the README tells the operator to run inside the container.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Spawns no ACP process and touches no real vault.
func TestInitWorkers_EmptyVaultBootsInsteadOfFailing(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Pool.WorkerCount = 2
	cfg.Storage.BaseDataDir = t.TempDir()
	cfg.Storage.WorkersRootDir = t.TempDir()
	cfg.Storage.WorkersTmpDir = t.TempDir()
	cfg.Storage.WorkspacesRootDir = t.TempDir()

	mgr := auth.NewManager(cfg, nil)
	mgr.SetAccountsForTest(nil)

	p := &Pool{cfg: cfg, authMgr: mgr, workers: make([]*Worker, cfg.Pool.WorkerCount),
		leaseCh: make(chan *Worker, cfg.Pool.WorkerCount),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil))}

	if err := p.initWorkers(context.Background()); err != nil {
		t.Fatalf("initWorkers must not fail on an empty vault, got: %v", err)
	}
	total, available := p.Availability()
	if total != 0 || available != 0 {
		t.Fatalf("empty vault must report zero workers, got total=%d available=%d", total, available)
	}

	// Inference must say "not set up yet", not "busy".
	if _, err := p.Acquire(context.Background(), "", false); !errors.Is(err, ErrNoAccounts) {
		t.Fatalf("Acquire on an empty vault = %v, want ErrNoAccounts", err)
	}
}

// TestEnsureWorkers_BootstrapsAfterOnboarding pins the no-restart requirement.
//
// @purpose Proves the pool grows itself once an account exists, so onboarding needs no container restart.
// @rationale The onboarding CLI runs as a separate process; the pool booted with zero workers must pick
// the new account up rather than staying permanently unusable.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Runs the real bootstrap path; the credential export seam is stubbed.
func TestEnsureWorkers_BootstrapsAfterOnboarding(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Pool.WorkerCount = 1
	cfg.Accounts.AllocationStrategy = config.AllocationStrategyRoundRobin
	cfg.Storage.BaseDataDir = t.TempDir()
	cfg.Storage.WorkersRootDir = t.TempDir()
	cfg.Storage.WorkersTmpDir = t.TempDir()
	cfg.Storage.WorkspacesRootDir = t.TempDir()

	mgr := auth.NewManager(cfg, nil)
	mgr.SetAccountsForTest(nil)
	mgr.SetExportForTest(func(_ context.Context, dir, accountID string) error {
		payload := `{"token":{"access_token":"a","token_type":"Bearer","refresh_token":"r","expiry":"2026-11-27T00:00:00Z"},"auth_method":"oauth","id_token":"i","id_token_email":"late@example.com"}`
		return os.WriteFile(filepath.Join(dir, accountID+".json"), []byte(payload), 0o600)
	})

	p := &Pool{cfg: cfg, authMgr: mgr, workers: make([]*Worker, cfg.Pool.WorkerCount),
		leaseCh: make(chan *Worker, cfg.Pool.WorkerCount),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := p.initWorkers(context.Background()); err != nil {
		t.Fatalf("initWorkers: %v", err)
	}
	if total, _ := p.Availability(); total != 0 {
		t.Fatalf("precondition: pool must boot with zero workers, got %d", total)
	}

	// The account is added out of band, past any cache in this process.
	mgr.SetAccountsForTest([]auth.Account{{
		ID: "acct_late", Email: "late@example.com", Enabled: true, Status: auth.StatusHealthy,
		AssignedWorker: -1, Quotas: map[string]auth.QuotaGroup{}, FamilyCooldowns: map[string]time.Time{},
	}})

	p.ensureWorkers(context.Background())

	if total, _ := p.Availability(); total != cfg.Pool.WorkerCount {
		t.Fatalf("pool did not bootstrap after onboarding: total=%d want %d", total, cfg.Pool.WorkerCount)
	}
	t.Cleanup(func() { p.Close() })

	// Idempotent: a second call must not provision again.
	p.ensureWorkers(context.Background())
	if total, _ := p.Availability(); total != cfg.Pool.WorkerCount {
		t.Fatalf("ensureWorkers provisioned twice: total=%d", total)
	}
}
