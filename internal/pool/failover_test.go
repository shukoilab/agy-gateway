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

// TestFailoverCoordinator_HealthyQuotaRefusalDoesNotCool covers S12: a 429 that arrives with no
// output while the account still has measured capacity is a refusal, not exhaustion, so the
// credential must stay usable.
func TestFailoverCoordinator_HealthyQuotaRefusalDoesNotCool(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Execution.RetryInitialBackoff = 10 * time.Millisecond
	cfg.Execution.RetryMaxBackoff = 20 * time.Millisecond
	cfg.Accounts.MinRemainingQuotaPct = 50

	authMgr := auth.NewManager(cfg, nil)
	acc := auth.Account{
		ID: "acc_primary", Email: "primary@example.com", Tier: "payg",
		Status: auth.StatusHealthy, Enabled: true, QuotasKnown: true,
		Quotas: map[string]auth.QuotaGroup{
			"gemini": {Name: "Gemini Models", RemainingPct: 97.4},
		},
	}
	authMgr.SetAccountsForTest([]auth.Account{acc})

	p := &Pool{cfg: cfg, authMgr: authMgr, leaseCh: make(chan *Worker, 1)}
	w0 := createMockWorker(t, 0, &acc, cfg)
	p.leaseCh <- w0

	fc := NewFailoverCoordinator(p, authMgr, nil)
	err := fc.ExecuteWithFailover(context.Background(), "", false, 3, func(ctx context.Context, w *Worker) error {
		return MarkZeroTokens(errors.New("upstream error: RESOURCE_EXHAUSTED (429 Too Many Requests)"))
	})
	if err == nil {
		t.Fatal("a refusal must still surface to the caller")
	}

	for _, a := range authMgr.GetAccounts() {
		if a.ID != "acc_primary" {
			continue
		}
		if a.Status != auth.StatusHealthy {
			t.Fatalf("healthy account must not be cooled, got status %v", a.Status)
		}
		if !a.CooldownUntil.IsZero() {
			t.Fatalf("healthy account must not get a cooldown deadline, got %v", a.CooldownUntil)
		}
	}
}

// TestFailoverCoordinator_UnknownQuotaStillCools covers the complement of S12: when the account has
// no measured quota there is no evidence of capacity, so the pre-existing penalty still applies.
func TestFailoverCoordinator_UnknownQuotaStillCools(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Execution.RetryInitialBackoff = 10 * time.Millisecond
	cfg.Execution.RetryMaxBackoff = 20 * time.Millisecond

	authMgr := auth.NewManager(cfg, nil)
	acc := auth.Account{
		ID: "acc_unknown", Email: "unknown@example.com", Tier: "payg",
		Status: auth.StatusHealthy, Enabled: true, QuotasKnown: false,
	}
	authMgr.SetAccountsForTest([]auth.Account{acc})

	p := &Pool{cfg: cfg, authMgr: authMgr, leaseCh: make(chan *Worker, 1)}
	w0 := createMockWorker(t, 0, &acc, cfg)
	p.leaseCh <- w0

	fc := NewFailoverCoordinator(p, authMgr, nil)
	_ = fc.ExecuteWithFailover(context.Background(), "", false, 1, func(ctx context.Context, w *Worker) error {
		return MarkZeroTokens(errors.New("upstream error: RESOURCE_EXHAUSTED (429 Too Many Requests)"))
	})

	for _, a := range authMgr.GetAccounts() {
		if a.ID != "acc_unknown" {
			continue
		}
		if a.Status == auth.StatusHealthy && a.CooldownUntil.IsZero() {
			t.Fatal("an account with no measured quota must still be cooled on a 429")
		}
	}
}

// TestKnownQuotaPct_ExhaustedBucketReportsZero covers S13 at the seam the guard reads: an exhausted
// bucket must report zero so the threshold comparison cannot treat it as healthy. The pool already
// refuses to lease such an account, so this is asserted on the helper rather than through failover.
func TestKnownQuotaPct_ExhaustedBucketReportsZero(t *testing.T) {
	cfg := config.DefaultConfig()
	authMgr := auth.NewManager(cfg, nil)
	authMgr.SetAccountsForTest([]auth.Account{
		{
			ID: "acc_exhausted", Email: "ex@example.com", Tier: "payg",
			Status: auth.StatusHealthy, Enabled: true, QuotasKnown: true,
			Quotas: map[string]auth.QuotaGroup{
				"gemini": {Name: "Gemini Models", RemainingPct: 80, IsExhausted: true},
			},
		},
		{
			ID: "acc_healthy", Email: "ok@example.com", Tier: "payg",
			Status: auth.StatusHealthy, Enabled: true, QuotasKnown: true,
			Quotas: map[string]auth.QuotaGroup{
				"gemini": {Name: "Gemini Models", RemainingPct: 97.4},
				"3p":     {Name: "Claude Models", RemainingPct: 55.0},
			},
		},
		{ID: "acc_unknown", Email: "u@example.com", Tier: "payg", Status: auth.StatusHealthy, Enabled: true},
	})

	if pct, known := authMgr.KnownQuotaPct("acc_exhausted"); !known || pct != 0 {
		t.Fatalf("exhausted bucket must report (0, true), got (%v, %v)", pct, known)
	}
	if pct, known := authMgr.KnownQuotaPct("acc_healthy"); !known || pct != 55.0 {
		t.Fatalf("must report the worst bucket, got (%v, %v)", pct, known)
	}
	if _, known := authMgr.KnownQuotaPct("acc_unknown"); known {
		t.Fatal("unknown quota must report false so the caller falls back")
	}
	if _, known := authMgr.KnownQuotaPct(""); known {
		t.Fatal("empty account id must report false")
	}
	if _, known := authMgr.KnownQuotaPct("acc_missing"); known {
		t.Fatal("unknown account must report false")
	}
}

// TestMarkZeroTokens_RoundTrip proves the marker preserves the original error for classification.
func TestMarkZeroTokens_RoundTrip(t *testing.T) {
	base := errors.New("boom")
	marked := MarkZeroTokens(base)
	if !IsZeroTokens(marked) {
		t.Fatal("marked error must report zero tokens")
	}
	if !errors.Is(marked, base) {
		t.Fatal("marker must unwrap to the original error")
	}
	if IsZeroTokens(base) {
		t.Fatal("unmarked error must not report zero tokens")
	}
	if MarkZeroTokens(nil) != nil {
		t.Fatal("nil must stay nil")
	}
}
