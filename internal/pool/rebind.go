package pool

import (
	"context"
	"fmt"
	"time"

	"agy-gateway/internal/auth"
)

// BindingResult describes what a forced rebind actually did.
//
// @purpose Turns "switch" and "rotate" into operations that report their effect instead of claiming one.
// @rationale Both used to answer success after mutating only the vault's global credential, while every
// worker kept serving its previous account. An operator reading "switched" reasonably concluded traffic
// had moved. Reporting the per-worker transitions makes a no-op visible as a no-op.
// @preconditions None.
// @param None.
// @returns None.
// @throws None.
// @invariants Plain data; safe to serialise.
type BindingResult struct {
	// WorkerMoves lists the workers whose account changed, in worker order.
	WorkerMoves []WorkerMove `json:"worker_moves"`
	// Unchanged reports that no worker's account changed.
	Unchanged bool `json:"unchanged"`
}

// WorkerMove records one worker's account transition.
type WorkerMove struct {
	WorkerID int    `json:"worker_id"`
	From     string `json:"from,omitempty"`
	To       string `json:"to"`
}

// RebindWorkersForAccount installs accountID on every worker.
//
// @purpose Makes `switch <query>` actually change which account serves traffic.
// @rationale The vault's active account is a property of `agy-auth`, not of this process: workers
// authenticate with their own materialised credential, so a vault switch alone changes nothing. This
// forces the requested account onto every worker so the operator's intent becomes observable.
// @preconditions accountID must exist in the vault.
// @param ctx Cancellation root bounding each credential subprocess.
// @param accountID Account to install on all workers.
// @returns The transitions performed.
// @throws ErrNoAccounts when the pool has no workers; a wrapped error when any credential cannot be
// installed, after the workers that succeeded have already moved.
// @invariants O(workers) subprocesses; each worker is tainted so its running process respawns with the
// new credential; no lock is held across a subprocess.
func (p *Pool) RebindWorkersForAccount(ctx context.Context, accountID string) (BindingResult, error) {
	if p.authMgr == nil {
		return BindingResult{Unchanged: true}, nil
	}
	workers := p.workerSnapshot()
	if len(workers) == 0 {
		return BindingResult{Unchanged: true}, ErrNoAccounts
	}

	result := BindingResult{}
	for _, w := range workers {
		if w == nil {
			continue
		}
		from := ""
		if bound := w.GetAccount(); bound != nil {
			from = bound.ID
			if bound.ID == accountID {
				// Already truthful; still publish so the admin view cannot drift.
				p.publishBinding(w.ID)
				continue
			}
		}
		if err := p.authMgr.SwitchWorkerAccount(ctx, w.ID, accountID); err != nil {
			return result, fmt.Errorf("worker %d: %w", w.ID, err)
		}
		acc, err := p.authMgr.AccountByID(accountID)
		if err != nil {
			return result, err
		}
		w.PurgeAccountCaches()
		w.SetAccount(&acc)
		w.TaintedForRecycle.Store(true)
		p.publishBinding(w.ID)
		result.WorkerMoves = append(result.WorkerMoves, WorkerMove{WorkerID: w.ID, From: from, To: accountID})
	}
	result.Unchanged = len(result.WorkerMoves) == 0
	return result, nil
}

// RotateWorkersOneStep advances every worker to the next eligible account.
//
// @purpose Makes `rotate` mean what it says for this process, not just for the vault.
// @rationale `agy-auth rotate` only changes which snapshot is the vault's active account and exits 0
// when it decides nothing needs to move; it has no concept of workers. The gateway's own rotation is
// per worker, so it must select the next eligible account here and report each transition.
// @preconditions None.
// @param ctx Cancellation root bounding each credential subprocess.
// @param model Upstream model id whose quota family governs eligibility; empty checks every family.
// @param requiresPaidTier Reserved for a catalog-sourced tier requirement; pass false.
// @returns The transitions performed; Unchanged is true when no worker could move.
// @throws ErrNoAccounts when the pool has no workers or the vault is empty; a wrapped error when a
// credential cannot be installed.
// @invariants O(workers × accounts) plus one subprocess per moved worker; no lock across a subprocess.
func (p *Pool) RotateWorkersOneStep(ctx context.Context, model string, requiresPaidTier bool) (BindingResult, error) {
	if p.authMgr == nil {
		return BindingResult{Unchanged: true}, nil
	}
	if len(p.authMgr.GetAccounts()) == 0 {
		return BindingResult{Unchanged: true}, ErrNoAccounts
	}
	workers := p.workerSnapshot()
	if len(workers) == 0 {
		return BindingResult{Unchanged: true}, ErrNoAccounts
	}

	minPct := p.cfg.Accounts.MinRemainingQuotaPct
	result := BindingResult{}
	for _, w := range workers {
		if w == nil {
			continue
		}
		from := ""
		if bound := w.GetAccount(); bound != nil {
			from = bound.ID
		}
		next, err := p.authMgr.FindNextEligibleAccount(from, model, requiresPaidTier, p.accountsInUse(w))
		if err != nil || next == nil {
			// No eligible alternative for this worker: leave it where it is rather than expiring a
			// possibly-healthy account. An operator forcing rotation on a single-account vault must see
			// "unchanged", not a fabricated move.
			continue
		}
		if next.ID == from || !next.IsEligibleForModel(model, requiresPaidTier, minPct) {
			continue
		}
		if err := p.authMgr.SwitchWorkerAccount(ctx, w.ID, next.ID); err != nil {
			return result, fmt.Errorf("worker %d: %w", w.ID, err)
		}
		w.PurgeAccountCaches()
		w.SetAccount(next)
		w.TaintedForRecycle.Store(true)
		p.publishBinding(w.ID)
		result.WorkerMoves = append(result.WorkerMoves, WorkerMove{WorkerID: w.ID, From: from, To: next.ID})
	}
	result.Unchanged = len(result.WorkerMoves) == 0
	if result.Unchanged {
		p.log().Info("rotation requested but no worker had an eligible alternative account")
	}
	return result, nil
}

// RecycleTainted respawns every worker marked for recycle within the given budget.
//
// @purpose Makes a rebind take effect without waiting for the next request to release the worker.
// @rationale Rebind installs a credential on disk; the running agy process authenticated at spawn time
// and will not see it. Tainting alone only defers the respawn to Release, so a switch performed while
// all workers are idle would not take effect until traffic arrived.
// @preconditions None.
// @param ctx Cancellation root for the restart handshakes.
// @param budget Upper bound for the whole pass; each worker gets an equal share.
// @returns The number of workers restarted.
// @throws None; a failed restart is logged and the worker stays tainted for the normal path.
// @invariants O(workers); does not hold Pool.mu across a restart.
func (p *Pool) RecycleTainted(ctx context.Context, budget time.Duration) int {
	workers := p.workerSnapshot()
	live := 0
	for _, w := range workers {
		if w != nil && w.TaintedForRecycle.Load() {
			live++
		}
	}
	if live == 0 {
		return 0
	}
	share := budget
	if share <= 0 {
		share = time.Duration(live) * 5 * time.Second
	}
	if per := share / time.Duration(live); per > 0 {
		share = per
	}

	restarted := 0
	for _, w := range workers {
		if w == nil || !w.TaintedForRecycle.Load() {
			continue
		}
		restartCtx, cancel := context.WithTimeout(ctx, share)
		w.Close()
		w.TaintedForRecycle.Store(false)
		w.RequestCount.Store(0)
		w.CreatedAt = time.Now()
		err := w.Start(restartCtx)
		cancel()
		if err != nil {
			// Leave it tainted: the next Release will retry, and a worker that is not running cannot
			// serve a turn anyway.
			w.TaintedForRecycle.Store(true)
			p.log().Error("failed to respawn worker after rebind", "workerID", w.ID, "err", err)
			continue
		}
		restarted++
	}
	return restarted
}

// AccountForWorkerIDs is exported for tests that need to assert a rebind landed.
//
// @purpose Avoids duplicating the worker-snapshot lookup in tests.
// @rationale Tests in other packages cannot reach the unexported slice.
// @preconditions None.
// @param None.
// @returns Worker id → bound account id for every worker that holds an account.
// @throws None.
// @invariants O(workers); takes the pool lock once.
func (p *Pool) AccountForWorkerIDs() map[int]string {
	out := make(map[int]string)
	for _, w := range p.workerSnapshot() {
		if w == nil {
			continue
		}
		if acc := w.GetAccount(); acc != nil {
			out[w.ID] = acc.ID
		}
	}
	return out
}

// ensure auth is referenced even if every function above is compiled out in a future edit.
var _ = auth.Account{}
