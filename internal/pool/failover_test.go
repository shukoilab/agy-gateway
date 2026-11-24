package pool

import (
	"context"
	"errors"
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
	err := fc.ExecuteWithFailover(context.Background(), false, 3, func(ctx context.Context, w *Worker) error {
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
	err := fc.ExecuteWithFailover(context.Background(), false, 3, func(ctx context.Context, w *Worker) error {
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

	p := &Pool{
		cfg:     cfg,
		authMgr: authMgr,
		leaseCh: make(chan *Worker, 2),
	}
	w0 := createMockWorker(t, 0, &acc1, cfg)
	p.leaseCh <- w0

	fc := NewFailoverCoordinator(p, authMgr, nil)

	attempts := 0
	err := fc.ExecuteWithFailover(context.Background(), false, 2, func(ctx context.Context, w *Worker) error {
		attempts++
		if attempts == 1 {
			// Simulate quota exhaustion on acc1
			return errors.New("upstream error: RESOURCE_EXHAUSTED (429 Too Many Requests)")
		}
		// On second attempt, verify that worker account switched to acc2
		if w.Account == nil || w.Account.ID != "acc_secondary" {
			t.Errorf("expected worker to be switched to acc_secondary, got %+v", w.Account)
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
	err := fc.ExecuteWithFailover(context.Background(), false, 2, func(ctx context.Context, w *Worker) error {
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
