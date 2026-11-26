package pool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agy-gateway/internal/acp"
	"agy-gateway/internal/auth"
	"agy-gateway/internal/config"
)

func createMockWorker(t *testing.T, id int, acc *auth.Account, cfg *config.Config) *Worker {
	tmp := t.TempDir()
	w := NewWorker(id, tmp, tmp, tmp, acc, cfg, nil)
	w.StartFn = func(ctx context.Context) error {
		proc, err := acp.SpawnProcess(context.Background(), "sleep", []string{"60"}, nil, tmp, nil)
		if err != nil {
			return err
		}
		w.Process = proc
		return nil
	}
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("failed to start mock worker: %v", err)
	}
	t.Cleanup(func() {
		w.Close()
	})
	return w
}

func TestFailoverCoordinator_SuccessOnFirstAttempt(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Pool.WorkerCount = 1
	p := &Pool{
		cfg:     cfg,
		leaseCh: make(chan *Worker, 1),
	}
	w := createMockWorker(t, 0, nil, cfg)
	p.leaseCh <- w

	fc := NewFailoverCoordinator(p, nil, nil)

	invocations := 0
	err := fc.ExecuteWithFailover(context.Background(), "", false, 3, func(ctx context.Context, w *Worker) error {
		invocations++
		return nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if invocations != 1 {
		t.Errorf("expected 1 invocation, got %d", invocations)
	}
}

func TestFailoverCoordinator_NonRetryableStreamError(t *testing.T) {
	cfg := config.DefaultConfig()
	p := &Pool{
		cfg:     cfg,
		leaseCh: make(chan *Worker, 1),
	}
	w := createMockWorker(t, 0, nil, cfg)
	p.leaseCh <- w

	fc := NewFailoverCoordinator(p, nil, nil)

	invocations := 0
	err := fc.ExecuteWithFailover(context.Background(), "", false, 3, func(ctx context.Context, w *Worker) error {
		invocations++
		return ErrNonRetryableStream
	})

	if !errors.Is(err, ErrNonRetryableStream) {
		t.Fatalf("expected ErrNonRetryableStream, got %v", err)
	}
	if invocations != 1 {
		t.Errorf("expected exactly 1 invocation without retry, got %d", invocations)
	}
}

func TestFailoverCoordinator_AutoSwitchOnQuotaExhausted(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Execution.RetryInitialBackoff = 10 * time.Millisecond
	cfg.Execution.RetryMaxBackoff = 20 * time.Millisecond

	authMgr := auth.NewManager(cfg, nil)

	acc1 := auth.Account{
		ID:      "acc_primary",
		Email:   "primary@example.com",
		Tier:    "payg",
		Status:  auth.StatusHealthy,
		Enabled: true,
	}
	acc2 := auth.Account{
		ID:      "acc_secondary",
		Email:   "secondary@example.com",
		Tier:    "payg",
		Status:  auth.StatusHealthy,
		Enabled: true,
	}

	authMgr.SetAccountsForTest([]auth.Account{acc1, acc2})
	// The install writes into the configured workers root; point it at a temp dir so the real
	// materialisation path (export → validate → copy into the worker HOME) can run unprivileged.
	cfg.Storage.WorkersRootDir = t.TempDir()
	// The switch must genuinely succeed: a failover that publishes a binding for a credential it could
	// not install is exactly the defect under test, so the recovery path is only meaningful when the
	// real install path (export → validate → copy) works. The default export shells out to the real
	// `agy-auth`, so the seam writes a well-formed credential instead.
	// Materialisation verifies the exported identity against the requested account, so the seam must
	// emit each fixture account's real email — the check is the same one that stops the gateway
	// installing the wrong account's token.
	emails := map[string]string{"acc_primary": acc1.Email, "acc_secondary": acc2.Email}
	authMgr.SetExportForTest(func(_ context.Context, dir, accountID string) error {
		payload := `{"token":{"access_token":"a","token_type":"Bearer","refresh_token":"r","expiry":"2026-11-27T00:00:00Z"},"auth_method":"oauth","id_token":"i","id_token_email":"` + emails[accountID] + `"}`
		return os.WriteFile(filepath.Join(dir, "creds.json"), []byte(payload), 0o600)
	})

	p := &Pool{
		cfg:     cfg,
		authMgr: authMgr,
		leaseCh: make(chan *Worker, 2),
	}
	w0 := createMockWorker(t, 0, &acc1, cfg)
	p.leaseCh <- w0

	fc := NewFailoverCoordinator(p, authMgr, nil)

	attempts := 0
	err := fc.ExecuteWithFailover(context.Background(), "", false, 2, func(ctx context.Context, w *Worker) error {
		attempts++
		if attempts == 1 {
			// Simulate quota exhaustion on acc1
			return errors.New("upstream error: RESOURCE_EXHAUSTED (429 Too Many Requests)")
		}
		// On second attempt, verify that worker account switched to acc2
		if w.GetAccount() == nil || w.GetAccount().ID != "acc_secondary" {
			t.Errorf("expected worker to be switched to acc_secondary, got %+v", w.GetAccount())
		}
		return nil
	})

	if err != nil {
		t.Fatalf("expected recovery on retry attempt, got %v", err)
	}
	if attempts != 2 {
		t.Errorf("expected 2 attempts, got %d", attempts)
	}
}

func TestFailoverCoordinator_AuthExpiredCode(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Execution.RetryInitialBackoff = 10 * time.Millisecond
	cfg.Execution.RetryMaxBackoff = 20 * time.Millisecond

	p := &Pool{
		cfg:     cfg,
		leaseCh: make(chan *Worker, 2),
	}
	w0 := createMockWorker(t, 0, nil, cfg)
	p.leaseCh <- w0

	fc := NewFailoverCoordinator(p, nil, nil)

	attempts := 0
	err := fc.ExecuteWithFailover(context.Background(), "", false, 2, func(ctx context.Context, w *Worker) error {
		attempts++
		if attempts == 1 {
			return &acp.ResponseError{Code: acp.ErrCodeAuthRequired, Message: "Authentication required"}
		}
		return nil
	})

	if err != nil {
		t.Fatalf("expected success on second attempt after auth expired, got %v", err)
	}
	if attempts != 2 {
		t.Errorf("expected 2 attempts, got %d", attempts)
	}
}

// TestFailoverCoordinator_FailedInstallIsNotPublished pins FINDING-R2.
//
// @purpose Stops failover from advertising a binding for a credential it never installed.
// @rationale Publication asserts "this worker holds this account". Writing it after a failed
// `agy-auth` export made the admin view and the sticky cache name an account that was never
// materialised, and the next lease's fast path then accepted that binding without re-materialising —
// so the pool could execute a turn on a worker still holding the previous credential.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf or t.Errorf.
// @invariants Bounded to temp dirs; the mock worker process is closed by t.Cleanup.
func TestFailoverCoordinator_FailedInstallIsNotPublished(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Pool.WorkerCount = 1
	cfg.Execution.RetryInitialBackoff = 10 * time.Millisecond
	cfg.Execution.RetryMaxBackoff = 20 * time.Millisecond
	cfg.Storage.WorkersRootDir = t.TempDir()

	authMgr := auth.NewManager(cfg, nil)
	acc1 := auth.Account{ID: "acc_primary", Email: "primary@example.com", Tier: "payg", Status: auth.StatusHealthy, Enabled: true}
	acc2 := auth.Account{ID: "acc_secondary", Email: "secondary@example.com", Tier: "payg", Status: auth.StatusHealthy, Enabled: true}
	authMgr.SetAccountsForTest([]auth.Account{acc1, acc2})
	authMgr.MarkAccountBinding("acc_primary", 0)

	// Every install fails: the export produces nothing to validate.
	authMgr.SetExportForTest(func(context.Context, string, string) error { return nil })

	p := &Pool{cfg: cfg, authMgr: authMgr, leaseCh: make(chan *Worker, 2)}
	p.workers = []*Worker{}
	w0 := createMockWorker(t, 0, &acc1, cfg)
	p.workers = []*Worker{w0}
	p.leaseCh <- w0

	fc := NewFailoverCoordinator(p, authMgr, nil)
	_ = fc.ExecuteWithFailover(context.Background(), "", false, 1, func(context.Context, *Worker) error {
		return errors.New("upstream error: RESOURCE_EXHAUSTED (429 Too Many Requests)")
	})

	// acc_secondary was never installed, so it must not claim the worker.
	for _, acc := range authMgr.GetAccounts() {
		if acc.ID == "acc_secondary" && acc.AssignedWorker != -1 {
			t.Fatalf("a failed credential install must not be published: acc_secondary claims worker %d",
				acc.AssignedWorker)
		}
	}
	// The sticky cache must not record it either.
	if saved, err := loadBindings(bindingsPath(cfg)); err == nil {
		for _, id := range saved {
			if id == "acc_secondary" {
				t.Fatalf("a failed credential install must not reach the sticky cache: %v", saved)
			}
		}
	}
}
