package pool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
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
	// bindingsMu serialises the load/compare/save of the sticky file.
	//
	// @purpose Closes the lost-update window where two concurrent leases each load, compare, and save.
	// @rationale Without it the later save can carry a snapshot older than a SetAccount that landed in
	// between. A dedicated mutex (never held across a subprocess or an account-mutex acquisition)
	// costs one uncontended lock per publish and makes the write a true compare-and-swap.
	// @invariants Never acquired while holding `mu`; never held across IO other than the bindings file.
	bindingsMu sync.Mutex

	// initMu serialises lazy worker bootstrapping for a pool that started with an empty vault.
	//
	// @purpose Lets an account added after boot take effect without restarting the container.
	// @rationale A fresh install boots with no accounts (see initWorkers); the first request that
	// arrives after `agy-accounts add` must create the workers, and exactly one caller may do it.
	// @invariants Never acquired while holding `mu`; initWorkers takes `mu` itself.
	initMu sync.Mutex

	// booted reports that initWorkers created the worker set; false means the vault was empty at boot.
	//
	// @purpose Distinguishes "no workers because the vault is empty" from "no workers because of a fault".
	// @rationale Drives the lazy bootstrap in ensureWorkers without re-provisioning on every request.
	// @invariants Written under initMu; read under initMu.
	booted bool
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

// workerSnapshot returns a copy of the live worker slice under the pool lock.
//
// @purpose Lets callers iterate the worker set without racing a concurrent bootstrap.
// @rationale initWorkers can now run from a request path (see ensureWorkers), which replaces the
// slice wholesale; iterating the field directly would read it while another goroutine swaps it.
// @preconditions None.
// @returns The current workers; an empty slice when the pool has none.
// @throws None.
// @invariants O(len(workers)); the returned slice is never mutated afterwards.
func (p *Pool) workerSnapshot() []*Worker {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.workers == nil {
		return nil
	}
	out := make([]*Worker, len(p.workers))
	copy(out, p.workers)
	return out
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
		// A fresh installation has an empty vault, and the only way to fill it is to run the
		// onboarding CLI inside this container. Failing here made the container exit, so
		// `restart: unless-stopped` turned it into a crash loop and the very command the operator
		// needs (`docker exec -it agy-gateway agy-accounts`) could never be run: the install was
		// permanently unbootable. Boot with zero workers instead; inference answers `no_accounts`
		// (429) and the pool grows itself on the first request after an account is added.
		p.log().Warn("vault holds no accounts; starting in onboarding-only mode: HTTP is up and inference " +
			"answers no_accounts until one is added with `agy-accounts add`")
		// Drop the phantom slots so Availability/readyz report zero workers; ensureWorkers re-allocates
		// when an account appears.
		p.mu.Lock()
		p.workers = make([]*Worker, 0)
		p.mu.Unlock()
		return nil
	}

	p.mu.Lock()
	if len(p.workers) != p.cfg.Pool.WorkerCount {
		p.workers = make([]*Worker, p.cfg.Pool.WorkerCount)
	}
	p.mu.Unlock()

	assignments := p.assignAccounts(accounts, p.cfg.Pool.WorkerCount)

	// Build the set locally and publish it in one assignment: initWorkers can now run from a request
	// path (see ensureWorkers), so a partially filled slice must never be visible to a concurrent
	// reader. Readers still take p.mu; see reconcileBindings and accountsInUse.
	workers := make([]*Worker, p.cfg.Pool.WorkerCount)

	for i := 0; i < p.cfg.Pool.WorkerCount; i++ {
		acc := assignments[i]
		homeDir, tmpDir, cwd, err := p.authMgr.ProvisionWorkerHome(i, acc.ID)
		if err != nil {
			return fmt.Errorf("failed to provision worker %d for account %s: %w", i, acc.ID, err)
		}

		boundAcc := acc
		w := NewWorker(i, homeDir, tmpDir, cwd, &boundAcc, p.cfg, p.logger)
		workers[i] = w
		// The startup binding is reconciled once, after this loop. It must NOT be stamped here:
		// `MarkAccountBinding(acc.ID, i)` per iteration makes the field last-writer-wins, so a shared
		// account boots owned by its HIGHEST holder ({0,2} ⇒ 2) instead of by the reconciled lowest
		// id — the same defect class as FINDING-R7, reached through the boot path.

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

	// Publish the complete set in one assignment, then reconcile: boot-time binding ownership must be
	// lowest-id-wins, which requires the whole set to be visible first.
	p.mu.Lock()
	p.workers = workers
	p.mu.Unlock()

	// One reconciliation for the whole boot: deterministic lowest-id ownership for any account shared
	// by several workers, and `assigned_worker` truthful before the first lease.
	p.reconcileBindings()

	// Unsynchronised on purpose: callers are either the single-threaded boot path or ensureWorkers,
	// which holds initMu for the whole call.
	p.booted = true
	return nil
}

// Bootstrap brings the pool up for a deployment that started with an empty vault.
//
// @purpose Makes an account onboarded after boot usable, including its models, without a restart.
// @rationale The onboarding CLI is a separate process, so the in-memory account set is stale after it
// runs. Re-reading the vault and provisioning the workers here is what lets the first request after
// `agy-accounts add` succeed; provisioning also materialises each worker's credential, which is what
// model discovery reads its token from.
// @preconditions Caller must not hold any pool lock.
// @param ctx Cancellation root for the vault read and worker provisioning.
// @returns True when the vault holds at least one account after bootstrapping.
// @throws None; a failure is logged and retried by the next call.
// @invariants Safe to call on every request; O(1) once the pool has booted.
func (p *Pool) Bootstrap(ctx context.Context) bool {
	if p.authMgr == nil {
		return false
	}
	if len(p.authMgr.GetAccounts()) == 0 {
		// Only re-read while the vault still looks empty: that is the onboarding transition, and it
		// keeps the steady state free of an extra agy-auth subprocess on every request.
		if err := p.authMgr.Sync(ctx); err != nil {
			p.log().Debug("vault re-read failed while awaiting onboarding", "err", err)
		}
	}
	p.ensureWorkers(ctx)
	return len(p.authMgr.GetAccounts()) > 0
}

// ensureWorkers bootstraps the worker set for a pool that booted with an empty vault.
//
// @purpose Makes the first inference after onboarding work without a container restart.
// @rationale initWorkers returns early when the vault is empty, because failing there made a fresh
// install unbootable (see the comment in initWorkers). The workers therefore have to be created
// lazily, once an account actually exists.
// @preconditions Caller must not hold p.mu or initMu.
// @param ctx Cancellation root for provisioning.
// @returns None; a provisioning error is logged and retried on the next call.
// @throws None.
// @invariants At most one bootstrap runs at a time; a pool that already booted does nothing.
func (p *Pool) ensureWorkers(ctx context.Context) {
	p.initMu.Lock()
	defer p.initMu.Unlock()
	if p.booted {
		return
	}
	if len(p.authMgr.GetAccounts()) == 0 {
		return
	}
	if err := p.initWorkers(ctx); err != nil {
		p.log().Error("failed to bootstrap worker pool after an account was added", "err", err)
		return
	}
	p.booted = true
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
// Each strategy now does what its name and documentation claim: round_robin spreads by index,
// least_used is greedy over prior assignment counts (the counter was previously written and never
// read, which collapsed every worker onto the single highest-quota account), and sticky honours
// persisted affinity with an eligibility fallback. Accounts are reused only when no free eligible
// account remains, and every such case is logged because a shared account means two processes can
// race the same refresh token.
// @preconditions accounts may be empty; workerCount is non-negative.
// @param accounts Accounts available for assignment, in vault order.
// @param workerCount Number of workers to assign.
// @returns A slice of length min(workerCount, capacity) — exactly workerCount when accounts is
// non-empty, and empty when the vault holds no accounts.
// @throws None; never indexes an empty slice.
// @invariants O(workerCount × accounts) in the worst case; deterministic for identical inputs;
// performs filesystem IO only for the sticky strategy.
func (p *Pool) assignAccounts(accounts []auth.Account, workerCount int) []auth.Account {
	if workerCount <= 0 || len(accounts) == 0 {
		// Guards the previous `accounts[0]` dereference: an empty vault must produce an empty
		// assignment rather than a panic in the startup path.
		return nil
	}

	strategy := config.NormalizeAllocationStrategy(p.cfg.Accounts.AllocationStrategy)
	assignments := make([]auth.Account, workerCount)
	usedPerAccount := make(map[string]int, len(accounts))
	// assigned[i] records whether slot i has been filled, so sticky can skip honoured bindings.
	assigned := make([]bool, workerCount)
	minPct := p.cfg.Accounts.MinRemainingQuotaPct

	// eligible reports whether an account may be bound at all right now.
	eligible := func(acc auth.Account) bool { return rankForAllocation(acc, minPct) }
	byID := func(id string) (auth.Account, bool) {
		for _, acc := range accounts {
			if acc.ID == id {
				return acc, true
			}
		}
		return auth.Account{}, false
	}

	// greedy fills one slot using the least-used rule: fewest prior assignments, then highest
	// remaining quota, then vault order. Only accounts not already claimed are considered, so the
	// spread is maximised before any reuse happens.
	greedy := func(slot int) bool {
		// Fewest claims first is what spreads the pool evenly on its own: while any account is
		// unused its count is 0, so it beats every used account regardless of quota. Quota only
		// decides between accounts already carrying the same load, and vault order settles the rest.
		bestIdx := -1
		for i := range accounts {
			if !eligible(accounts[i]) {
				continue
			}
			if bestIdx < 0 {
				bestIdx = i
				continue
			}
			cur, best := accounts[i], accounts[bestIdx]
			switch {
			case usedPerAccount[cur.ID] < usedPerAccount[best.ID]:
				bestIdx = i
			case usedPerAccount[cur.ID] == usedPerAccount[best.ID] && quotaScore(cur) > quotaScore(best):
				bestIdx = i
			}
		}
		if bestIdx < 0 {
			return false
		}
		chosen := accounts[bestIdx]
		usedPerAccount[chosen.ID]++
		assignments[slot] = chosen
		assigned[slot] = true
		return true
	}

	switch strategy {
	case config.AllocationStrategySticky:
		p.assignSticky(accounts, assignments, assigned, usedPerAccount, eligible, byID, greedy)
	case config.AllocationStrategyLeastUsed:
		for i := 0; i < workerCount; i++ {
			if !greedy(i) {
				break
			}
		}
	default: // round_robin
		for i := 0; i < workerCount; i++ {
			chosen := accounts[i%len(accounts)]
			usedPerAccount[chosen.ID]++
			assignments[i] = chosen
			assigned[i] = true
		}
	}

	// Any slot left unfilled (fewer eligible accounts than workers) falls back to vault order so the
	// pool still starts with a usable account per worker; materialisation reports a real error later
	// if the credential turns out to be broken.
	for i := 0; i < workerCount; i++ {
		if !assigned[i] {
			chosen := accounts[i%len(accounts)]
			usedPerAccount[chosen.ID]++
			assignments[i] = chosen
		}
	}

	p.warnOnSharedAccounts(assignments, len(accounts))
	return assignments
}

// assignSticky honours persisted worker affinity, then fills the remainder greedily.
//
// @purpose Makes `sticky` keep each worker on its account across restarts.
// @rationale The strategy previously behaved exactly like round_robin: nothing read the value and no
// binding was ever persisted, so a restart silently reshuffled every worker. A binding is honoured
// only when the account still exists, is still usable, and is not already claimed by an earlier
// worker; every other case falls back to the least-used rule rather than failing.
// @preconditions accounts is non-empty and assignments/assigned are sized workerCount.
// @param accounts Available accounts in vault order.
// @param assignments Output slice mutated in place.
// @param assigned Output flags mutated in place.
// @param usedPerAccount Running claim counts, mutated in place.
// @param eligible Predicate for "may be bound at all".
// @param byID Vault lookup by account id.
// @param greedy Fallback filler for one slot.
// @returns None; mutates the output parameters.
// @throws None; a cache failure is logged as a warning and ignored.
// @invariants O(workerCount × accounts); at most one load and one save.
func (p *Pool) assignSticky(
	accounts []auth.Account,
	assignments []auth.Account,
	assigned []bool,
	usedPerAccount map[string]int,
	eligible func(auth.Account) bool,
	byID func(string) (auth.Account, bool),
	greedy func(slot int) bool,
) {
	workerCount := len(assignments)
	path := bindingsPath(p.cfg)

	persisted := map[int]string{}
	if loaded, err := loadBindings(path); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// A cache that cannot be trusted is a warning, never a startup failure.
			p.log().Warn("sticky bindings could not be loaded; allocating fresh",
				"path", path, "err", err)
		}
	} else {
		persisted = loaded
	}

	// First-boot seed: with no usable cache, start worker 0 on the vault's active account so the
	// initial layout matches what the operator sees in `agy-auth current`. Only a seed — the greedy
	// pass still fills every other slot, and a persisted binding always wins over it.
	if len(persisted) == 0 && p.authMgr != nil {
		if activeID := p.authMgr.VaultActiveAccountID(); activeID != "" {
			if acc, ok := byID(activeID); ok && eligible(acc) {
				persisted[0] = activeID
				p.log().Info("seeding sticky affinity from the vault's active account",
					"workerID", 0, "accountID", activeID)
			}
		}
	}

	// Iterate the persisted entries themselves, not an index range bounded by the map's size. A
	// gapped file (e.g. only worker 1 recorded) is a legitimate state after a partial write or a
	// hand-edited file, and a size-bounded range silently dropped every binding past the gap.
	for i := 0; i < workerCount; i++ {
		accountID, present := persisted[i]
		if !present {
			continue
		}
		acc, ok := byID(accountID)
		if !ok {
			p.log().Info("sticky binding references an account that no longer exists; reassigning",
				"workerID", i, "accountID", accountID)
			continue
		}
		if !eligible(acc) {
			p.log().Info("sticky binding references an ineligible account; reassigning",
				"workerID", i, "accountID", accountID)
			continue
		}
		if usedPerAccount[accountID] > 0 {
			p.log().Info("sticky binding duplicates an earlier worker's account; reassigning",
				"workerID", i, "accountID", accountID)
			continue
		}
		assignments[i] = acc
		assigned[i] = true
		usedPerAccount[accountID]++
	}

	for i := 0; i < workerCount; i++ {
		if assigned[i] {
			continue
		}
		if !greedy(i) {
			break
		}
	}

	// Rewrite the file with exactly the live pool: stale worker indices are pruned, and a first boot
	// seeds it so the next restart is stable.
	//
	// Partial allocation must not be persisted. When every account is momentarily ineligible the
	// fallback still fills each slot with a vault-order account, but if even that fails (an empty
	// slice) writing the resulting empty map would erase a perfectly good affinity cache because of a
	// transient fleet-wide cooldown. A cache is only rewritten from a complete, real allocation.
	complete := true
	current := make(map[int]string, workerCount)
	for i := 0; i < workerCount; i++ {
		if !assigned[i] || assignments[i].ID == "" {
			complete = false
			break
		}
		current[i] = assignments[i].ID
	}
	if !complete {
		p.log().Debug("skipping bindings write; allocation was incomplete",
			"path", path, "workerCount", workerCount, "assigned", len(current))
		return
	}
	// `current` is already bounded by workerCount, so no pruning pass is required: iterating 0..n and
	// assigning only filled slots cannot produce a stale index. The former `pruneBindings` helper was
	// unprovable dead code for exactly this reason.
	//
	// Only touch the disk when the content actually changes, and serialise the check against the write
	// so a concurrent lease cannot interleave between them.
	p.bindingsMu.Lock()
	defer p.bindingsMu.Unlock()
	if cached, err := loadBindings(path); err == nil && bindingsEqual(cached, current) {
		return
	}
	if err := saveBindings(path, current); err != nil {
		p.log().Warn("sticky bindings could not be persisted; affinity will not survive a restart",
			"path", path, "err", err)
	}
}

// reconcileBindings republishes every account's owning worker from the live worker set.
//
// @purpose Keeps `assigned_worker` truthful when an account is shared or when a worker rotates away.
// @rationale `AssignedWorker` holds a single id, but the allocator may legitimately place one account
// on several workers. Releasing by "any account naming this worker" therefore left a surviving worker
// leasing an account the admin view reported as unassigned. Reading the truth from the workers makes
// the operation idempotent and independent of which call ran last.
// @preconditions Safe to call with no workers or no accounts.
// @param None.
// @returns None.
// @throws None.
// @invariants O(workers × accounts); takes accountsMu once per account update, never held across IO.
func (p *Pool) reconcileBindings() {
	if p.authMgr == nil {
		return
	}
	// Lowest live worker id per account: deterministic, and never reports a shared account as free.
	workers := p.workerSnapshot()
	owner := make(map[string]int, len(workers))
	for _, w := range workers {
		if w == nil {
			continue
		}
		acc := w.GetAccount()
		if acc == nil || acc.ID == "" {
			continue
		}
		if current, ok := owner[acc.ID]; !ok || w.ID < current {
			owner[acc.ID] = w.ID
		}
	}
	for _, acc := range p.authMgr.GetAccounts() {
		want := -1
		if id, ok := owner[acc.ID]; ok {
			want = id
		}
		if acc.AssignedWorker != want {
			p.authMgr.MarkAccountBinding(acc.ID, want)
		}
	}
}

// bindingsEqual reports whether two worker→account maps are identical.
//
// @purpose Lets the persistence path skip a write when nothing changed.
// @rationale Reconcile runs on every lease; rewriting and fsyncing identical bytes on each one puts a
// disk barrier in the request path for no benefit. Comparing is O(workers) against an in-memory map.
// @preconditions None.
// @param a First map.
// @param b Second map.
// @returns True when both have the same keys and values.
// @throws None.
// @invariants O(len(a)); no allocation.
func bindingsEqual(a, b map[int]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if other, ok := b[k]; !ok || other != v {
			return false
		}
	}
	return true
}

// warnOnSharedAccounts reports every account held by more than one worker.
//
// @purpose Surfaces the refresh-token race before it causes a silent session loss.
// @rationale The previous warning was gated on `workerCount > len(accounts)`, which is false in the
// common balanced case (3 workers, 3 accounts) even when a bad strategy put all three on one
// account. Gating on the real condition — more than one worker on the same account — reports the
// situation that actually matters.
// @preconditions assignments may be empty.
// @param assignments Worker index → account map produced by allocation.
// @param accountCount Number of distinct accounts available, used in the remedy text.
// @returns None; logs a warning per shared account.
// @throws None.
// @invariants O(workerCount); deterministic ordering by account id for stable logs.
func (p *Pool) warnOnSharedAccounts(assignments []auth.Account, accountCount int) {
	if len(assignments) == 0 {
		return
	}
	sharing := make(map[string][]int, len(assignments))
	ids := make([]string, 0, len(assignments))
	for i, acc := range assignments {
		if _, seen := sharing[acc.ID]; !seen {
			ids = append(ids, acc.ID)
		}
		sharing[acc.ID] = append(sharing[acc.ID], i)
	}
	sort.Strings(ids)
	for _, accountID := range ids {
		workerIDs := sharing[accountID]
		if len(workerIDs) < 2 {
			continue
		}
		p.log().Warn("multiple workers share one account; concurrent token refresh can race",
			"accountID", accountID, "workerIDs", workerIDs, "accountsAvailable", accountCount,
			"remedy", "add accounts or reduce worker_count so each worker has its own")
	}
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
	// A pool that booted with an empty vault has no workers and an empty lease channel, so waiting
	// would burn the acquire timeout and answer 503 "saturated" for what is really "not set up yet".
	// Bootstrapping here is also what makes an account added after boot take effect immediately.
	if p.authMgr != nil {
		p.ensureWorkers(ctx)
		if len(p.authMgr.GetAccounts()) == 0 {
			return nil, ErrNoAccounts
		}
	}

	acquireTimeout := p.cfg.Pool.AcquireTimeout
	if acquireTimeout <= 0 {
		acquireTimeout = 15 * time.Second
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, acquireTimeout)
	defer cancel()

	// Each worker is attempted at most once per call so a permanently unusable worker cannot spin.
	rejected := make(map[int]struct{}, len(p.workerSnapshot()))
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
			// Record the lease so `last_used` reports real traffic rather than the last vault sync.
			// Throttled inside the manager to one write per account per second: this is the hot path
			// and every write takes accountsMu.
			if acc := w.GetAccount(); acc != nil {
				p.authMgr.MarkAccountUsed(acc.ID, time.Now())
			}
			return w, nil
		}
	}
}

// accountsInUse reports how many other workers currently hold each account.
//
// @purpose Keeps rotation from binding two workers to one account while an unused account exists, and
// from piling a third worker onto an account two already share while a one-way account is free.
// @rationale Every worker holds its own copy of a refresh token; two workers refreshing the same
// account's token concurrently race and only the first succeeds, leaving the other offline. Knowing
// how CONTENDED each account is lets selection prefer the least-used one. The previous contract
// returned a single owner ID and silently omitted any account held by two or more workers, which made
// the most contended accounts look entirely free.
// @preconditions None; safe to call at any time.
// @param self Worker being reconciled, whose own claim is not counted.
// @returns map[string]int of account ID to the number of OTHER workers bound to it. Only accounts
// held by at least one other worker appear. Never nil.
// @throws None.
// @invariants O(workers); reads each worker's Account pointer once; no locks held on return.
func (p *Pool) accountsInUse(self *Worker) map[string]int {
	workers := p.workerSnapshot()
	inUse := make(map[string]int, len(workers))
	for _, other := range workers {
		if other == nil || other == self {
			continue
		}
		acc := other.GetAccount()
		if acc == nil {
			continue
		}
		inUse[acc.ID]++
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
			// The binding is already truthful for this worker; re-publishing it keeps
			// `assigned_worker` correct for a worker that started before the manager existed and for
			// any path that rebound the account without a rotation.
			p.publishBinding(w.ID)
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
	// Publish only after SetAccount: reporting a binding whose credential failed to install would
	// misattribute quota spend to a worker that never used the account. The old account needs no
	// explicit release — publishBinding reconciles `assigned_worker` from the live workers, so once
	// SetAccount has moved this worker the previous account is already unowned.
	p.publishBinding(w.ID)
	return nil
}

// publishBinding records a worker's account in the manager and, under sticky, on disk.
//
// @purpose Keeps the admin API's `assigned_worker` truthful and the affinity cache current.
// @rationale Nothing ever wrote `AssignedWorker`, so it reported -1 forever. Persisting the affinity
// cache here rather than only at boot is required because a worker recycled in place keeps its
// *Worker and never re-runs allocation, so a boot-only write would go stale after the first rotation.
// The two are deliberately separate from the lease path's error handling: neither can fail a lease.
// @preconditions authMgr may be nil in tests; w is non-nil.
// @param workerID Worker whose bindings are being republished; retained for diagnostics.
// @returns None.
// @throws None; a cache failure is logged as a warning.
// @invariants O(workers) for the snapshot; performs file IO outside accountsMu.
func (p *Pool) publishBinding(workerID int) {
	if p.authMgr == nil {
		return
	}
	// Ownership is derived from the workers themselves, never from an id passed in. An account can
	// legitimately be shared by several workers, so a caller-supplied id cannot be trusted to be the
	// account's owner: writing it would make the field last-publisher-wins and contradict the
	// deterministic `reconcileBindings` rule. Every caller has already run `SetAccount`, which is the
	// authoritative record of what the worker holds.
	p.reconcileBindings()

	if config.NormalizeAllocationStrategy(p.cfg.Accounts.AllocationStrategy) != config.AllocationStrategySticky {
		return
	}
	// Persistence is driven from the live worker bindings rather than from the id being published, so
	// a full pool is written even when this call is a release. File IO happens after
	// MarkAccountBinding released accountsMu, so a slow disk can never serialise credential leasing.
	workers := p.workerSnapshot()
	bindings := make(map[int]string, len(workers))
	for _, other := range workers {
		if other == nil {
			continue
		}
		if acc := other.GetAccount(); acc != nil {
			bindings[other.ID] = acc.ID
		}
	}
	path := bindingsPath(p.cfg)
	// No pruning pass: `bindings` is keyed only from `p.workers`, so it is bounded by the live pool by
	// construction. The former `idx < WorkerCount` filter was unprovable dead code — the same shape as
	// the `pruneBindings` helper deleted for that reason.
	// Same rule as the allocation path: only write a complete, changed snapshot. A partial pool (a
	// worker mid-recycle has no account momentarily) must not overwrite a good cache.
	if len(bindings) < p.cfg.Pool.WorkerCount {
		p.log().Debug("skipping bindings write; pool bindings are incomplete",
			"path", path, "bound", len(bindings), "workerCount", p.cfg.Pool.WorkerCount)
		return
	}
	// Serialise load/compare/save: two concurrent leases otherwise both load, both decide "changed",
	// and the later save writes a snapshot older than an intervening SetAccount.
	p.bindingsMu.Lock()
	defer p.bindingsMu.Unlock()
	if cached, err := loadBindings(path); err == nil && bindingsEqual(cached, bindings) {
		return
	}
	if err := saveBindings(path, bindings); err != nil {
		p.log().Warn("sticky bindings could not be persisted; affinity will not survive a restart",
			"path", path, "err", err)
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
