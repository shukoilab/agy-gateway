package pool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"agy-gateway/internal/auth"
	"agy-gateway/internal/config"
)

// rebindTestPool builds a pool of idle workers that own no processes.
//
// @purpose Lets the switch/rotate orchestration be exercised without spawning agy.
// @rationale Rebind only touches the credential file, the account pointer, the taint flag, and the
// binding record, so a Worker with just an id and directories is the real production object minus its
// child process.
// @preconditions None.
// @param t Standard Go test handle.
// @param cfg Config with WorkersRootDir and WorkspaceDir under a temp dir.
// @param workers Accounts count == workers count.
// @returns The pool and the worker ids in order.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no goroutines, no subprocesses.
func rebindTestPool(t *testing.T, cfg *config.Config, workers int) *Pool {
	t.Helper()
	p := &Pool{cfg: cfg, leaseCh: make(chan *Worker, workers+1)}
	for i := 0; i < workers; i++ {
		home := filepath.Join(cfg.Storage.WorkersRootDir, "w"+itoa(i))
		tmp := filepath.Join(cfg.Storage.WorkersTmpDir, "w"+itoa(i))
		for _, d := range []string{home, tmp} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", d, err)
			}
		}
		p.workers = append(p.workers, &Worker{ID: i, HomeDir: home, TmpDir: tmp, Cwd: cfg.Storage.EffectiveWorkspaceDir(), cfg: cfg})
	}
	return p
}

// itoa renders a small non-negative int without importing strconv into the test's hot path.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// installFakeExport makes credential materialisation write realistic token files.
//
// @purpose Reuses the real atomic install path while keeping the vault out of the test.
// @rationale Materialisation is what actually changes which account a worker authenticates as, so the
// orchestration must be tested through it rather than around it.
// @preconditions None.
// @param mgr Manager under test.
// @returns None.
// @throws None.
// @invariants Writes one 0600 file per call.
func installFakeExport(t *testing.T, mgr *auth.Manager) {
	t.Helper()
	mgr.SetExportForTest(func(_ context.Context, dir, accountID string) error {
		payload := map[string]any{
			"token":          map[string]any{"access_token": "access-" + accountID, "refresh_token": "refresh-" + accountID},
			"auth_method":    "consumer",
			"id_token_email": accountID + "@example.com",
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, accountID+".json"), raw, 0o600)
	})
}

// TestRebindWorkersForAccount_MovesEveryWorkerAndTaints is S15.
//
// @purpose Proves `switch` actually changes which account each worker authenticates as.
// @rationale Measured before the fix: `agy-auth switch` rewrote the global CLI credential while every
// worker kept its own materialised token, so the admin API answered "switched" with zero effect.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants No subprocess.
func TestRebindWorkersForAccount_MovesEveryWorkerAndTaints(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Pool.WorkerCount = 2
	cfg.Storage.WorkersRootDir = t.TempDir()
	cfg.Storage.WorkersTmpDir = t.TempDir()
	cfg.Storage.WorkspacesRootDir = t.TempDir()
	cfg.Accounts.AllocationStrategy = config.AllocationStrategySticky

	mgr := auth.NewManager(cfg, nil)
	mgr.SetAccountsForTest([]auth.Account{
		{ID: "acct_a", Email: "acct_a@example.com", Enabled: true, Status: auth.StatusHealthy},
		{ID: "acct_b", Email: "acct_b@example.com", Enabled: true, Status: auth.StatusHealthy},
	})
	installFakeExport(t, mgr)

	p := rebindTestPool(t, cfg, 2)
	p.authMgr = mgr
	bound := auth.Account{ID: "acct_a", Email: "acct_a@example.com", Enabled: true}
	for _, w := range p.workers {
		acc := bound
		w.SetAccount(&acc)
	}

	res, err := p.RebindWorkersForAccount(context.Background(), "acct_b")
	if err != nil {
		t.Fatalf("RebindWorkersForAccount: %v", err)
	}
	if res.Unchanged {
		t.Fatal("Unchanged must be false when workers moved")
	}
	if len(res.WorkerMoves) != 2 {
		t.Fatalf("moves = %d, want 2", len(res.WorkerMoves))
	}
	for _, mv := range res.WorkerMoves {
		if mv.From != "acct_a" || mv.To != "acct_b" {
			t.Fatalf("move = %+v, want acct_a -> acct_b", mv)
		}
	}
	for _, w := range p.workers {
		if got := w.GetAccount(); got == nil || got.ID != "acct_b" {
			t.Fatalf("worker %d bound to %+v, want acct_b", w.ID, got)
		}
		if !w.TaintedForRecycle.Load() {
			t.Fatalf("worker %d must be tainted so its running process respawns", w.ID)
		}
		// The credential on disk must match the new binding, not just the in-memory pointer.
		raw, readErr := os.ReadFile(filepath.Join(w.HomeDir, ".gemini", "antigravity-cli", "antigravity-oauth-token"))
		if readErr != nil {
			t.Fatalf("worker %d credential unreadable: %v", w.ID, readErr)
		}
		var payload struct {
			Email string `json:"id_token_email"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("worker %d credential unparseable: %v", w.ID, err)
		}
		if payload.Email != "acct_b@example.com" {
			t.Fatalf("worker %d credential email = %q, want acct_b@example.com", w.ID, payload.Email)
		}
	}
}

// TestRebindWorkersForAccount_AlreadyBoundIsUnchanged is S16.
//
// @purpose Proves a switch onto the account every worker already holds reports the truth: nothing moved.
// @rationale The point of the fix is that "switched" is never claimed without an effect; the inverse
// (a genuine no-op being reported as a move) would be the same defect mirrored.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants No subprocess.
func TestRebindWorkersForAccount_AlreadyBoundIsUnchanged(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Pool.WorkerCount = 1
	cfg.Storage.WorkersRootDir = t.TempDir()
	cfg.Storage.WorkersTmpDir = t.TempDir()
	cfg.Storage.WorkspacesRootDir = t.TempDir()

	mgr := auth.NewManager(cfg, nil)
	mgr.SetAccountsForTest([]auth.Account{
		{ID: "acct_a", Email: "acct_a@example.com", Enabled: true, Status: auth.StatusHealthy},
	})
	installFakeExport(t, mgr)

	p := rebindTestPool(t, cfg, 1)
	p.authMgr = mgr
	acc := auth.Account{ID: "acct_a", Email: "acct_a@example.com", Enabled: true}
	p.workers[0].SetAccount(&acc)

	res, err := p.RebindWorkersForAccount(context.Background(), "acct_a")
	if err != nil {
		t.Fatalf("RebindWorkersForAccount: %v", err)
	}
	if !res.Unchanged || len(res.WorkerMoves) != 0 {
		t.Fatalf("expected an honest no-op, got %+v", res)
	}
	if p.workers[0].TaintedForRecycle.Load() {
		t.Fatal("a no-op switch must not taint the worker")
	}
}

// TestRotateWorkersOneStep_ReportsRealTransitions is S17.
//
// @purpose Proves rotate selects the next eligible account per worker and reports each transition.
// @rationale `agy-auth rotate` is 5h-centric and vault-only: it cannot see workers, and it exits 0
// without doing anything when it decides no rotation is needed. The gateway's rotation must be its own.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants No subprocess.
func TestRotateWorkersOneStep_ReportsRealTransitions(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Pool.WorkerCount = 1
	cfg.Accounts.MinRemainingQuotaPct = 5
	cfg.Accounts.AutoRotateEnabled = true
	cfg.Storage.WorkersRootDir = t.TempDir()
	cfg.Storage.WorkersTmpDir = t.TempDir()
	cfg.Storage.WorkspacesRootDir = t.TempDir()

	spent := accountWithQuotas("acct_spent", 0, 0)
	healthy := accountWithQuotas("acct_healthy", 90, 90)

	mgr := auth.NewManager(cfg, nil)
	mgr.SetAccountsForTest([]auth.Account{spent, healthy})
	installFakeExport(t, mgr)

	p := rebindTestPool(t, cfg, 1)
	p.authMgr = mgr
	bound := spent
	p.workers[0].SetAccount(&bound)

	res, err := p.RotateWorkersOneStep(context.Background(), "gemini-3.8-flash", false)
	if err != nil {
		t.Fatalf("RotateWorkersOneStep: %v", err)
	}
	if res.Unchanged {
		t.Fatal("a spent account with a healthy alternative must rotate")
	}
	if len(res.WorkerMoves) != 1 || res.WorkerMoves[0].To != "acct_healthy" {
		t.Fatalf("moves = %+v, want acct_spent -> acct_healthy", res.WorkerMoves)
	}
	if got := p.workers[0].GetAccount(); got == nil || got.ID != "acct_healthy" {
		t.Fatalf("worker bound to %+v, want acct_healthy", got)
	}
}

// TestRotateWorkersOneStep_NoEligibleAlternativeIsUnchanged is S18.
//
// @purpose Proves rotate never fabricates a move, and never expires a healthy account to look busy.
// @rationale A single-account vault cannot rotate; the operator must see "unchanged" rather than a
// success message with no effect.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants No subprocess.
func TestRotateWorkersOneStep_NoEligibleAlternativeIsUnchanged(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Pool.WorkerCount = 1
	cfg.Accounts.MinRemainingQuotaPct = 5
	cfg.Storage.WorkersRootDir = t.TempDir()
	cfg.Storage.WorkersTmpDir = t.TempDir()
	cfg.Storage.WorkspacesRootDir = t.TempDir()

	only := accountWithQuotas("acct_only", 90, 90)
	mgr := auth.NewManager(cfg, nil)
	mgr.SetAccountsForTest([]auth.Account{only})
	installFakeExport(t, mgr)

	p := rebindTestPool(t, cfg, 1)
	p.authMgr = mgr
	bound := only
	p.workers[0].SetAccount(&bound)

	res, err := p.RotateWorkersOneStep(context.Background(), "gemini-3.8-flash", false)
	if err != nil {
		t.Fatalf("RotateWorkersOneStep: %v", err)
	}
	if !res.Unchanged || len(res.WorkerMoves) != 0 {
		t.Fatalf("expected an honest no-op, got %+v", res)
	}
	if got := p.workers[0].GetAccount(); got == nil || got.ID != "acct_only" {
		t.Fatalf("worker must keep its account, got %+v", got)
	}
	if p.workers[0].TaintedForRecycle.Load() {
		t.Fatal("a no-op rotation must not taint the worker")
	}
}

// TestRebindWorkersForAccount_NoWorkersIsAnError pins the empty-pool contract.
//
// @purpose Proves the admin API can distinguish "no workers to move" from "moved".
// @rationale With an empty vault the pool legitimately has zero workers; reporting success would
// describe a move that could not have happened.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants O(1).
func TestRebindWorkersForAccount_NoWorkersIsAnError(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := auth.NewManager(cfg, nil)
	p := &Pool{cfg: cfg, authMgr: mgr, leaseCh: make(chan *Worker, 1)}

	if _, err := p.RebindWorkersForAccount(context.Background(), "acct_a"); err == nil {
		t.Fatal("an empty pool must report an error, not a silent success")
	}
}

// TestRebindWorkersForAccount_PurgesAccountCachesKeepsWorkspace is S19.
//
// @purpose Proves a rotation clears the old account's per-account caches while leaving the shared
// workspace intact.
// @rationale The workspace is the only thing carrying the agent's context across an account switch; if
// it were purged, the agent would rewrite the same files forever — the loop this task exists to kill.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants No subprocess.
func TestRebindWorkersForAccount_PurgesAccountCachesKeepsWorkspace(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Pool.WorkerCount = 1
	cfg.Storage.WorkersRootDir = t.TempDir()
	cfg.Storage.WorkersTmpDir = t.TempDir()
	cfg.Storage.WorkspacesRootDir = t.TempDir()

	mgr := auth.NewManager(cfg, nil)
	mgr.SetAccountsForTest([]auth.Account{
		{ID: "acct_a", Email: "acct_a@example.com", Enabled: true, Status: auth.StatusHealthy},
		{ID: "acct_b", Email: "acct_b@example.com", Enabled: true, Status: auth.StatusHealthy},
	})
	installFakeExport(t, mgr)

	p := rebindTestPool(t, cfg, 1)
	p.authMgr = mgr
	w := p.workers[0]
	bound := auth.Account{ID: "acct_a", Email: "acct_a@example.com", Enabled: true}
	w.SetAccount(&bound)

	// Per-account caches that must go, and workspace files that must stay.
	cliDir := filepath.Join(w.HomeDir, ".gemini", "antigravity-cli")
	conversations := filepath.Join(cliDir, "conversations")
	if err := os.MkdirAll(conversations, 0o700); err != nil {
		t.Fatalf("mkdir conversations: %v", err)
	}
	conversationFile := filepath.Join(conversations, "session.db")
	dbFile := filepath.Join(cliDir, "conversation_summaries.db")
	for _, f := range []string{conversationFile, dbFile} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	workspaceFile := filepath.Join(cfg.Storage.EffectiveWorkspaceDir(), "agent-work.txt")
	if err := os.MkdirAll(cfg.Storage.EffectiveWorkspaceDir(), 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	if err := os.WriteFile(workspaceFile, []byte("important"), 0o644); err != nil {
		t.Fatalf("write workspace file: %v", err)
	}

	if _, err := p.RebindWorkersForAccount(context.Background(), "acct_b"); err != nil {
		t.Fatalf("RebindWorkersForAccount: %v", err)
	}

	for _, gone := range []string{conversationFile, dbFile} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s must be purged on rebind, stat err = %v", gone, err)
		}
	}
	if _, err := os.Stat(workspaceFile); err != nil {
		t.Fatalf("the shared workspace must survive an account switch, stat err = %v", err)
	}
}
