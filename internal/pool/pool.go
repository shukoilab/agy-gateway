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

// ErrQueueSaturated reports that every worker stayed leased past the acquire timeout.
//
// @purpose Lets the HTTP surfaces distinguish capacity exhaustion from an internal fault.
// @rationale A saturated queue is a transient load condition: returning 500 tells clients to treat
// it as a bug in the gateway, while 503 invites a retry.
// @preconditions None.
// @param None.
// @returns A shared sentinel matched with errors.Is.
// @throws None.
// @invariants Immutable; safe for concurrent use.
var ErrQueueSaturated = errors.New("timeout acquiring worker from pool (queue saturated)")

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

// NewTestPoolWithNoWorkers initializes an empty pool for timeout and cancellation tests.
//
// @purpose Lets a test drive Acquire's timeout branch without spawning real agy workers.
// @rationale NewPool needs an auth manager and starts subprocesses; this exposes the same struct
// with a lease channel nobody feeds, so Acquire's real select loop is exercised verbatim.
// @preconditions cfg must be non-nil; only Pool.AcquireTimeout is read.
// @param cfg (*config.Config) - Config supplying the acquire timeout.
// @returns (*Pool) - Pool with an empty lease channel and no workers.
// @throws None.
// @invariants O(1); the returned pool owns no goroutines or processes.
func NewTestPoolWithNoWorkers(cfg *config.Config) *Pool {
	return &Pool{cfg: cfg, leaseCh: make(chan *Worker)}
}

func (p *Pool) log() *slog.Logger {
	if p.logger == nil {
		return slog.Default()
	}
	return p.logger
}

func (p *Pool) initWorkers(ctx context.Context) error {
	accounts := p.authMgr.GetAccounts()
	if len(accounts) == 0 {
		return fmt.Errorf("no accounts are stored in the vault; run `agy-auth login` before starting the gateway")
	}

	assignments := p.assignAccounts(accounts, p.cfg.Pool.WorkerCount)

	for i := 0; i < p.cfg.Pool.WorkerCount; i++ {
		acc := assignments[i]
		homeDir, tmpDir, cwd, err := p.authMgr.ProvisionWorkerHome(i, acc.ID)
		if err != nil {
			return fmt.Errorf("failed to provision worker %d for account %s: %w", i, acc.ID, err)
		}

		boundAcc := acc
		w := NewWorker(i, homeDir, tmpDir, cwd, &boundAcc, p.cfg, p.logger)
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

// ErrNoEligibleAccounts reports that no stored account can serve the requested model right now.
//
// @purpose Lets the HTTP surfaces answer 429 with an honest retry hint instead of a generic 500.
// @rationale Every account being in cooldown is a capacity condition the client can wait out;
// reporting it as a server fault would invite the wrong reaction.
// @preconditions None.
// @param None.
// @returns A shared sentinel matched with errors.Is.
// @throws None.
// @invariants Immutable; safe for concurrent use.
var ErrNoEligibleAccounts = errors.New("no eligible account can serve the requested model")

// ErrNoAccounts reports that the vault holds no accounts at all.
//
// @purpose Distinguishes a misconfigured deployment from a transient quota condition.
// @rationale An empty vault needs an operator action, not a client retry; conflating the two would
// make a 429 retry hint misleading.
// @preconditions None.
// @param None.
// @returns A shared sentinel matched with errors.Is.
// @throws None.
// @invariants Immutable; safe for concurrent use.
var ErrNoAccounts = errors.New("no accounts are available in the vault")

// assignAccounts maps each worker index to an account according to the configured strategy.
//
// @purpose Decides which account each worker authenticates as at startup.
// @rationale Every worker previously received whatever token happened to exist under the gateway HOME,
// so the assignment was cosmetic. Assigning explicitly is what makes per-worker credentials possible.
// Accounts are reused only when there are more workers than accounts; that is logged because a shared
// account means two processes can race the same refresh token.
// @preconditions accounts is non-empty.
// @param accounts Accounts eligible for assignment, in vault order.
// @param workerCount Number of workers to assign.
// @returns A slice of length workerCount.
// @throws None; never returns fewer entries than workerCount.
// @invariants O(workerCount × accounts) for least_used, O(workerCount) otherwise.
func (p *Pool) assignAccounts(accounts []auth.Account, workerCount int) []auth.Account {
	strategy := config.NormalizeAllocationStrategy(p.cfg.Accounts.AllocationStrategy)
	assignments := make([]auth.Account, 0, workerCount)
	usedPerAccount := make(map[string]int, len(accounts))

	for i := 0; i < workerCount; i++ {
		var chosen auth.Account
		switch strategy {
		case "least_used":
			// Highest remaining quota first; ties keep vault order for determinism.
			chosen = accounts[0]
			best := quotaScore(chosen)
			for _, acc := range accounts[1:] {
				if score := quotaScore(acc); score > best {
					chosen, best = acc, score
				}
			}
		default: // round_robin and sticky both spread one account per worker before reusing
			chosen = accounts[i%len(accounts)]
		}

		usedPerAccount[chosen.ID]++
		assignments = append(assignments, chosen)
	}

	// Surface the sharing condition once, with the remedy, instead of leaving it silent.
	if workerCount > len(accounts) {
		sharing := make(map[string][]int, len(accounts))
		for i, acc := range assignments {
			sharing[acc.ID] = append(sharing[acc.ID], i)
		}
		for accountID, workerIDs := range sharing {
			if len(workerIDs) > 1 {
				p.log().Warn("multiple workers share one account; concurrent token refresh can race",
					"accountID", accountID, "workerIDs", workerIDs,
					"remedy", "configure worker_count <= number of stored accounts")
			}
		}
	}
	return assignments
}

// quotaScore summarizes an account's remaining capacity for comparison.
//
// @purpose Ranks accounts for the least_used strategy.
// @rationale The minimum remaining percentage across buckets is the honest measure of usability: the
// scarcest family is what will stop this account first.
// @preconditions None.
// @param acc Account to score.
// @returns The lowest remaining percentage, or 0 when the account carries no quota data.
// @throws None.
// @invariants O(buckets); no allocation.
func quotaScore(acc auth.Account) float64 {
	if len(acc.Quotas) == 0 {
		return 0
	}
	lowest := 101.0
	for _, q := range acc.Quotas {
		if q.RemainingPct < lowest {
			lowest = q.RemainingPct
		}
	}
	if lowest > 100 {
		return 0
	}
	return lowest
}

// Acquire retrieves a worker whose account can serve the requested model.
//
// @purpose The single entry point that guarantees a leased worker is authenticated as an account with
// sufficient quota for the model about to run.
// @rationale The previous implementation refreshed the bound account only when it was AUTH_EXPIRED, so
// a cooldown changed nothing and a worker stayed wedged on a spent account. Reconciliation now happens
// on every lease, and the model's quota family decides eligibility.
// @preconditions model may be empty when the caller has no model context; then every family is checked.
// @param ctx Cancellation root; also bounds the acquire wait.
// @param model Upstream model id whose quota family governs eligibility.
// @param requiresPaidTier Reject free accounts when the model demands a paid tier.
// @returns A leased worker bound to an eligible account.
// @throws ErrNoAccounts when the vault is empty; ErrNoEligibleAccounts when no account can serve the
// model; ErrQueueSaturated when the wait times out; the context error when the caller cancels.
// @invariants O(workers) attempts per call; never holds Pool.mu across a subprocess; every rejected
// worker is returned to the lease channel exactly once.
func (p *Pool) Acquire(ctx context.Context, model string, requiresPaidTier bool) (*Worker, error) {
	acquireTimeout := p.cfg.Pool.AcquireTimeout
	if acquireTimeout <= 0 {
		acquireTimeout = 15 * time.Second
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, acquireTimeout)
	defer cancel()

	// Each worker is attempted at most once per call so a permanently unusable worker cannot spin.
	rejected := make(map[int]struct{}, len(p.workers))
	var lastEligibilityErr error

	// returnWorker puts a rejected worker back without blocking past the acquire deadline.
	returnWorker := func(worker *Worker) bool {
		select {
		case p.leaseCh <- worker:
			return true
		case <-timeoutCtx.Done():
			return false
		}
	}

	for {
		select {
		case <-timeoutCtx.Done():
			// Distinguish caller cancellation from our own acquire timeout. A client that hung up is
			// not capacity exhaustion; reporting it as a retryable 503 would invite a retry on a
			// connection the caller already abandoned.
			if cause := ctx.Err(); cause != nil {
				return nil, cause
			}
			if lastEligibilityErr != nil {
				return nil, lastEligibilityErr
			}
			return nil, ErrQueueSaturated
		case w := <-p.leaseCh:
			if _, seen := rejected[w.ID]; seen {
				// Every worker this call has already judged ineligible. Re-queueing cannot change that
				// verdict within one acquire, so stop instead of spinning until the deadline: the caller
				// must receive the eligibility reason (429 all_accounts_cooling), not a capacity error.
				if !returnWorker(w) {
					return nil, lastEligibilityErr
				}
				if lastEligibilityErr != nil {
					return nil, lastEligibilityErr
				}
				return nil, ErrQueueSaturated
			}

			if err := p.reconcileWorkerAccount(timeoutCtx, w, model, requiresPaidTier); err != nil {
				if errors.Is(err, ErrNoAccounts) {
					// Nothing can serve anything; fail fast instead of burning the acquire budget.
					return nil, err
				}
				lastEligibilityErr = err
				rejected[w.ID] = struct{}{}
				p.log().Debug("worker account cannot serve model; seeking another worker",
					"workerID", w.ID, "model", model, "err", err)
				if !returnWorker(w) {
					return nil, err
				}
				continue
			}

			// Check if process is alive; if dead or unstarted, restart
			if w.Process == nil || !w.Process.IsRunning() {
				p.log().Info("restarting inactive worker before lease", "workerID", w.ID)
				if err := w.Start(timeoutCtx); err != nil {
					p.log().Error("failed to restart worker", "workerID", w.ID, "err", err)
					rejected[w.ID] = struct{}{}
					if !returnWorker(w) {
						return nil, err
					}
					time.Sleep(100 * time.Millisecond)
					continue
				}
			}

			w.LastActive = time.Now()
			w.RequestCount.Add(1)
			return w, nil
		}
	}
}

// accountsInUse reports which accounts other workers are currently authenticating as.
//
// @purpose Keeps rotation from binding two workers to one account while an unused account exists.
// @rationale Every worker holds its own copy of a refresh token; two workers refreshing the same
// account's token concurrently race and only the first succeeds, leaving the other offline. Knowing
// the current claims lets selection prefer a free account and only share when nothing else is left.
// @preconditions None; safe to call at any time.
// @param self Worker being reconciled, whose own claim is not a conflict.
// @returns map[string]int of account ID to the single owning worker ID. A nil map when no worker is
// bound. Accounts held by more than one worker are omitted: they are already shared state, so
// excluding them would gain nothing.
// @throws None.
// @invariants O(workers); reads each worker's Account pointer once; no locks held on return.
func (p *Pool) accountsInUse(self *Worker) map[string]int {
	claims := make(map[string]map[int]struct{}, len(p.workers))
	for _, other := range p.workers {
		if other == nil || other == self {
			continue
		}
		acc := other.GetAccount()
		if acc == nil {
			continue
		}
		if claims[acc.ID] == nil {
			claims[acc.ID] = make(map[int]struct{}, 1)
		}
		claims[acc.ID][other.ID] = struct{}{}
	}

	inUse := make(map[string]int, len(claims))
	for accountID, owners := range claims {
		if len(owners) != 1 {
			continue
		}
		for ownerID := range owners {
			inUse[accountID] = ownerID
		}
	}
	return inUse
}

// reconcileWorkerAccount ensures the worker is bound to, and authenticated as, an account able to
// serve the requested model.
//
// @purpose Makes the "every leased worker holds an eligible account" invariant true.
// @rationale Two independent defects made this necessary: cooldowns were never consulted when leasing
// (only AUTH_EXPIRED triggered a rebind), and a worker's credential could diverge from its bound
// account because provisioning copied a global token. Reconciling both together is what closes them.
// @preconditions w is leased to this caller.
// @param ctx Cancellation root bounding the credential subprocess.
// @param w Worker to reconcile.
// @param model Upstream model id governing eligibility.
// @param requiresPaidTier Reject free accounts when the model demands a paid tier.
// @returns nil when the worker holds an eligible account, or a sentinel describing why not.
// @throws ErrNoAccounts when the vault is empty; ErrNoEligibleAccounts when no account can serve the
// model; a wrapped materialisation error when the credential could not be installed.
// @invariants O(accounts) plus at most one subprocess; does not hold Pool.mu.
func (p *Pool) reconcileWorkerAccount(ctx context.Context, w *Worker, model string, requiresPaidTier bool) error {
	if p.authMgr == nil {
		return nil
	}

	accounts := p.authMgr.GetAccounts()
	if len(accounts) == 0 {
		return ErrNoAccounts
	}

	minPct := p.cfg.Accounts.MinRemainingQuotaPct

	// Fast path: adopt the manager's fresher view of the bound account, then judge it.
	if bound := w.GetAccount(); bound != nil {
		for i := range accounts {
			if accounts[i].ID == bound.ID {
				fresh := accounts[i]
				w.SetAccount(&fresh)
				bound = &fresh
				break
			}
		}
		if bound.IsEligibleForModel(model, requiresPaidTier, minPct) {
			return nil
		}
	}

	currentID := ""
	if bound := w.GetAccount(); bound != nil {
		currentID = bound.ID
	}

	next, err := p.authMgr.FindNextEligibleAccount(currentID, model, requiresPaidTier, p.accountsInUse(w))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNoEligibleAccounts, err)
	}

	if _, err := p.authMgr.MaterializeWorkerCredential(ctx, w.ID, next.ID); err != nil {
		// Only a failure that proves the stored credential is unusable may expire the account
		// (EC-02/EC-03/EC-15: bad payload, identity mismatch, rotated refresh token). A transient
		// failure — export timeout or a cancelled lease context (EC-16/EC-19) — leaves the vault entry
		// intact, and expiring it would strand a healthy account until an operator intervened.
		if errors.Is(err, auth.ErrCredentialTransient) {
			p.log().Warn("credential materialisation was interrupted; account left eligible for retry",
				"workerID", w.ID, "accountID", next.ID, "err", err)
			return fmt.Errorf("credential materialisation for account %s did not complete: %w", next.ID, err)
		}
		// Bounded, not permanent: the retry window lets a healthy account return to service on its
		// own if the fault was momentary, while a genuinely dead credential simply re-marks itself.
		p.authMgr.MarkAccountStatus(next.ID, auth.StatusAuthExpired, p.cfg.Accounts.AuthExpiredRetryInterval)
		p.log().Warn("credential materialisation failed; marking account auth-expired",
			"workerID", w.ID, "accountID", next.ID, "err", err)
		return fmt.Errorf("failed to install credential for account %s: %w", next.ID, err)
	}

	p.log().Info("account rotation",
		"workerID", w.ID,
		"fromAccount", currentID,
		"toAccount", next.ID,
		"model", model,
		"reason", "lease_eligibility")
	w.SetAccount(next)
	return nil
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

// MarkAllForRecycle flags all workers in the pool for rolling recycling.
// Active workers will respawn upon release; idle workers will be recycled as they are acquired or released.
//
// @purpose Coordinates seamless transition to an updated agy CLI binary without dropping in-flight requests.
// @rationale Tainting workers ensures atomic rolling respawn without race conditions or server restart downtime.
// @preconditions Pool must be initialized.
// - @param: None.
// @returns None.
// @throws None.
// @invariants Thread-safe via worker atomic flags.
func (p *Pool) MarkAllForRecycle() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, w := range p.workers {
		if w != nil {
			w.TaintedForRecycle.Store(true)
		}
	}
	p.log().Info("all pool workers marked for rolling recreation following binary update")
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

// Availability reports how many workers are currently ready to serve a lease.
//
// @purpose Backs the /readyz probe with real pool state instead of a static string.
// @rationale A gateway with zero warm workers accepts connections it cannot serve; failing readiness
// lets the orchestrator stop routing traffic until workers recover.
// @preconditions None. Safe on a nil or closed pool.
// @returns total and available worker counts (available = queued in the lease channel).
// @throws None.
// @invariants O(1) under p.mu; channel length read is non-blocking.
func (p *Pool) Availability() (total int, available int) {
	if p == nil {
		return 0, 0
	}
	p.mu.Lock()
	total = len(p.workers)
	p.mu.Unlock()
	if p.leaseCh != nil {
		available = len(p.leaseCh)
	}
	return total, available
}
