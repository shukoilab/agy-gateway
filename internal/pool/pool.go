package pool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"agy-gateway/internal/auth"
	"agy-gateway/internal/config"
)

// Pool coordinates a pool of N isolated warm agy acp workers.
type Pool struct {
	cfg     *config.Config
	authMgr *auth.Manager
	logger  *slog.Logger

	leaseCh chan *Worker
	workers []*Worker
	mu      sync.Mutex
	bgWg    sync.WaitGroup
	closed  bool
}

// NewPool initializes and starts the worker pool.
func NewPool(ctx context.Context, cfg *config.Config, authMgr *auth.Manager, logger *slog.Logger) (*Pool, error) {
	if logger == nil {
		logger = slog.Default()
	}

	p := &Pool{
		cfg:     cfg,
		authMgr: authMgr,
		logger:  logger,
		leaseCh: make(chan *Worker, cfg.Pool.WorkerCount),
		workers: make([]*Worker, cfg.Pool.WorkerCount),
	}

	if err := p.initWorkers(ctx); err != nil {
		p.Close()
		return nil, fmt.Errorf("failed to initialize worker pool: %w", err)
	}

	return p, nil
}

// NewTestPoolWithWorker initializes a pool wrapping a pre-configured worker for integration testing.
func NewTestPoolWithWorker(worker *Worker, cfg *config.Config) *Pool {
	p := &Pool{
		cfg:     cfg,
		leaseCh: make(chan *Worker, 1),
		workers: []*Worker{worker},
	}
	p.leaseCh <- worker
	return p
}

func (p *Pool) log() *slog.Logger {
	if p.logger == nil {
		return slog.Default()
	}
	return p.logger
}

func (p *Pool) initWorkers(ctx context.Context) error {
	accounts := p.authMgr.GetAccounts()

	for i := 0; i < p.cfg.Pool.WorkerCount; i++ {
		homeDir, tmpDir, cwd, err := p.authMgr.ProvisionWorkerHome(i)
		if err != nil {
			return err
		}

		var boundAcc *auth.Account
		if len(accounts) > 0 {
			acc := accounts[i%len(accounts)]
			boundAcc = &acc
		}

		w := NewWorker(i, homeDir, tmpDir, cwd, boundAcc, p.cfg, p.logger)
		p.workers[i] = w

		// Attempt background warm startup
		go func(worker *Worker) {
			startCtx, cancel := context.WithTimeout(context.Background(), p.cfg.Pool.HandshakeTimeout*2)
			defer cancel()
			if err := worker.Start(startCtx); err != nil {
				p.logger.Warn("worker warm start deferred or failed; will retry on lease", "workerID", worker.ID, "err", err)
			}
			p.leaseCh <- worker
		}(w)
	}

	return nil
}

// Acquire retrieves an available worker from the pool with timeout and eligibility checks.
func (p *Pool) Acquire(ctx context.Context, requiresPaidTier bool) (*Worker, error) {
	acquireTimeout := p.cfg.Pool.AcquireTimeout
	if acquireTimeout <= 0 {
		acquireTimeout = 15 * time.Second
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, acquireTimeout)
	defer cancel()

	for {
		select {
		case <-timeoutCtx.Done():
			return nil, errors.New("timeout acquiring worker from pool (queue saturated)")
		case w := <-p.leaseCh:
			// Ensure bound account is refreshed from auth manager if missing or previously expired
			if p.authMgr != nil {
				accounts := p.authMgr.GetAccounts()
				if len(accounts) > 0 {
					if w.Account == nil || w.Account.Status == auth.StatusAuthExpired {
						acc := accounts[w.ID%len(accounts)]
						acc.Status = auth.StatusHealthy
						w.Account = &acc
					}
				}
			}

			// Check if process is alive; if dead or unstarted, restart
			if w.Process == nil || !w.Process.IsRunning() {
				p.log().Info("restarting inactive worker before lease", "workerID", w.ID)
				if p.authMgr != nil {
					_, _, _, _ = p.authMgr.ProvisionWorkerHome(w.ID)
				}
				if err := w.Start(timeoutCtx); err != nil {
					p.log().Error("failed to restart worker", "workerID", w.ID, "err", err)
					// Put back and try another
					p.leaseCh <- w
					time.Sleep(100 * time.Millisecond)
					continue
				}
			}

			// Validate account eligibility if account is bound
			if w.Account != nil && !w.Account.IsEligible(requiresPaidTier, p.cfg.Accounts.MinRemainingQuotaPct) {
				p.log().Debug("worker account not eligible, seeking alternate", "workerID", w.ID)
				// Return to pool and seek next
				p.leaseCh <- w
				time.Sleep(50 * time.Millisecond)
				continue
			}

			w.LastActive = time.Now()
			w.RequestCount.Add(1)
			return w, nil
		}
	}
}

// Release returns a worker back into the pool, recycling it if tainted or expired.
func (p *Pool) Release(w *Worker) {
	if w == nil {
		return
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		w.Close()
		return
	}
	p.bgWg.Add(1)
	p.mu.Unlock()

	go func() {
		defer p.bgWg.Done()

		// Clean transient workspace files
		w.CleanWorkspace()

		// Check if recycling is mandated
		shouldRecycle := w.TaintedForRecycle.Load() ||
			(p.cfg.Pool.MaxRequestsPerWorker > 0 && w.RequestCount.Load() >= int64(p.cfg.Pool.MaxRequestsPerWorker)) ||
			(p.cfg.Pool.MaxWorkerUptime > 0 && time.Since(w.CreatedAt) >= p.cfg.Pool.MaxWorkerUptime)

		if shouldRecycle {
			p.log().Info("proactively recycling worker", "workerID", w.ID, "tainted", w.TaintedForRecycle.Load())
			w.Close()

			// Reset worker state
			w.TaintedForRecycle.Store(false)
			w.RequestCount.Store(0)
			w.CreatedAt = time.Now()

			startCtx, cancel := context.WithTimeout(context.Background(), p.cfg.Pool.HandshakeTimeout*2)
			defer cancel()
			if err := w.Start(startCtx); err != nil {
				p.log().Error("failed to respawn recycled worker", "workerID", w.ID, "err", err)
			}
		}

		p.mu.Lock()
		closed := p.closed
		p.mu.Unlock()

		if closed {
			w.Close()
			return
		}

		select {
		case p.leaseCh <- w:
		default:
			w.Close()
		}
	}()
}

// Close terminates all workers in the pool.
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()

	p.bgWg.Wait()

	p.mu.Lock()
	defer p.mu.Unlock()

	for _, w := range p.workers {
		if w != nil {
			w.Close()
		}
	}
}
