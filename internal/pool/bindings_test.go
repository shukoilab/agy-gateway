package pool

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agy-gateway/internal/auth"
	"agy-gateway/internal/config"
)

// stickyPool builds a pool whose base data dir is a temp directory.
//
// @purpose Gives each sticky test an isolated bindings file.
// @rationale The bindings path derives from storage.base_data_dir, so a temp dir is the only way to
// exercise persistence without touching a real deployment.
// @preconditions None.
// @param t Standard Go test handle.
// @returns A pool configured for the sticky strategy with a temp data dir.
// @throws None.
// @invariants Bounded to the test's temp directory.
func stickyPool(t *testing.T) *Pool {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Accounts.AllocationStrategy = config.AllocationStrategySticky
	cfg.Storage.BaseDataDir = t.TempDir()
	return &Pool{cfg: cfg}
}

// TestBindings_RoundTrip pins the persistence format.
//
// @purpose Proves a saved map reloads identically, so affinity survives a restart.
// @rationale Without a faithful round trip `sticky` cannot work at all.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded to a temp directory.
func TestBindings_RoundTrip(t *testing.T) {
	p := stickyPool(t)
	path := bindingsPath(p.cfg)
	want := map[int]string{0: "acct_a", 1: "acct_b", 2: "acct_c"}

	if err := saveBindings(path, want); err != nil {
		t.Fatalf("saveBindings: %v", err)
	}
	got, err := loadBindings(path)
	if err != nil {
		t.Fatalf("loadBindings: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("loaded %d entries, want %d: %v", len(got), len(want), got)
	}
	for idx, accountID := range want {
		if got[idx] != accountID {
			t.Fatalf("worker %d = %q, want %q", idx, got[idx], accountID)
		}
	}
}

// TestBindings_MissingFileIsNotAnError pins EC-10.
//
// @purpose Proves a first boot (no file) is distinguishable from a corrupt file.
// @rationale The caller must log nothing on a first boot but warn on corruption; conflating the two
// would either spam every fresh deployment or hide a real problem.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded to a temp directory.
func TestBindings_MissingFileIsNotAnError(t *testing.T) {
	p := stickyPool(t)
	path := bindingsPath(p.cfg)

	got, err := loadBindings(path)
	if !os.IsNotExist(err) {
		t.Fatalf("expected a not-exist error, got %v", err)
	}
	if got == nil {
		t.Fatal("loadBindings must return an empty map, never nil")
	}
	if len(got) != 0 {
		t.Fatalf("expected an empty map, got %v", got)
	}
}

// TestBindings_CorruptAndMismatchedFilesAreRejected pins EC-11/EC-12/EC-13.
//
// @purpose Proves every untrustworthy cache shape falls back to fresh allocation instead of pinning
// workers to stale or foreign bindings.
// @rationale Honouring a corrupt file would bind workers to arbitrary accounts; honouring a file
// written under a different strategy would silently apply a policy the operator turned off.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded to a temp directory.
func TestBindings_CorruptAndMismatchedFilesAreRejected(t *testing.T) {
	cases := map[string]string{
		"corrupt json":      `{"version":1,`,
		"wrong version":     `{"version":99,"strategy":"sticky","bindings":{"0":"acct_a"}}`,
		"wrong strategy":    `{"version":1,"strategy":"round_robin","bindings":{"0":"acct_a"}}`,
		"non-numeric key":   `{"version":1,"strategy":"sticky","bindings":{"zero":"acct_a"}}`,
		"negative key":      `{"version":1,"strategy":"sticky","bindings":{"-1":"acct_a"}}`,
		"empty account":     `{"version":1,"strategy":"sticky","bindings":{"0":""}}`,
		"not an object":     `[]`,
		"bindings null":     `{"version":1,"strategy":"sticky","bindings":null}`,
		"unknown strategy":  `{"version":1,"strategy":"least_used","bindings":{}}`,
		"version as string": `{"version":"1","strategy":"sticky","bindings":{}}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			p := stickyPool(t)
			path := bindingsPath(p.cfg)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			got, err := loadBindings(path)
			if err == nil {
				// The only acceptable success is "no bindings", never a partial honouring.
				if len(got) != 0 {
					t.Fatalf("payload %q must be rejected, got %v", payload, got)
				}
				return
			}
			if os.IsNotExist(err) {
				t.Fatalf("payload %q exists; a not-exist error is wrong: %v", payload, err)
			}
		})
	}
}

// TestSaveBindings_CreatesMissingStateDirectory pins G3.
//
// @purpose Proves persistence works on a deployment where the state directory does not exist.
// @rationale The gateway creates only its five configured directories; `state/` is not among them, and
// the atomic writer fails when the parent is missing. Without the MkdirAll the first sticky boot never
// persists anything and affinity silently never works.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded to a temp directory.
func TestSaveBindings_CreatesMissingStateDirectory(t *testing.T) {
	p := stickyPool(t)
	path := bindingsPath(p.cfg)

	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("precondition: state dir should not exist yet, stat err = %v", err)
	}
	if err := saveBindings(path, map[int]string{0: "acct_a"}); err != nil {
		t.Fatalf("saveBindings must create the state directory: %v", err)
	}
	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("state dir missing after save: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("state dir perm = %o, want 700", perm)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatalf("bindings file missing after save: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("bindings file perm = %o, want 600", perm)
	}
}

// TestSaveBindings_UnwritableDirectoryReturnsError pins EC-18.
//
// @purpose Proves a failed write is an error, not a panic.
// @rationale The caller downgrades it to a WARN; a panic would take the gateway down on a cache write.
// @preconditions The test runs as a user that cannot write the read-only directory; skipped as root.
// @param t Standard Go test handle.
// @returns None.
// @throws None; the case is skipped when permissions cannot be enforced.
// @invariants Bounded to a temp directory.
func TestSaveBindings_UnwritableDirectoryReturnsError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions are not enforced")
	}
	p := stickyPool(t)
	base := p.cfg.Storage.BaseDataDir
	blocked := filepath.Join(base, "state")

	if err := os.MkdirAll(blocked, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := saveBindings(filepath.Join(blocked, "worker-bindings.json"), map[int]string{0: "acct_a"}); err == nil {
		t.Fatal("expected an error writing into a read-only directory")
	}
}

// TestAssignAccounts_StickyHonoursPersistedBindings pins FR-05's core promise.
//
// @purpose Proves a restart resumes the same layout rather than reshuffling every worker.
// @rationale `sticky` previously did nothing: it was an alias for round_robin and nothing was
// persisted, so an operator choosing affinity got churn on every restart.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded to a temp directory.
func TestAssignAccounts_StickyHonoursPersistedBindings(t *testing.T) {
	p := stickyPool(t)
	accounts := []auth.Account{
		accountWithQuotas("acct_a", 80, 80),
		accountWithQuotas("acct_b", 80, 80),
		accountWithQuotas("acct_c", 80, 80),
	}

	// Simulate a previous run that bound worker 0 to the LAST account in vault order. Honouring the
	// binding must override the greedy rule, which would otherwise prefer acct_a.
	if err := saveBindings(bindingsPath(p.cfg), map[int]string{0: "acct_c"}); err != nil {
		t.Fatalf("seed bindings: %v", err)
	}

	got := p.assignAccounts(accounts, 3)
	if len(got) != 3 {
		t.Fatalf("expected 3 assignments, got %d", len(got))
	}
	if got[0].ID != "acct_c" {
		t.Fatalf("worker 0 = %s, want the persisted binding acct_c", got[0].ID)
	}
	// The remaining workers must not duplicate the honoured account.
	present := map[string]int{}
	for _, acc := range got {
		present[acc.ID]++
	}
	if present["acct_c"] != 1 {
		t.Fatalf("acct_c assigned %d times, want exactly 1", present["acct_c"])
	}
	if len(present) != 3 {
		t.Fatalf("expected 3 distinct accounts, got %v", present)
	}

	// The rewritten file must describe the full pool, so the next restart is stable.
	saved, err := loadBindings(bindingsPath(p.cfg))
	if err != nil {
		t.Fatalf("reload bindings: %v", err)
	}
	if len(saved) != 3 {
		t.Fatalf("persisted %d bindings, want 3: %v", len(saved), saved)
	}
	for i := 0; i < 3; i++ {
		if saved[i] != got[i].ID {
			t.Fatalf("persisted worker %d = %q, want %q", i, saved[i], got[i].ID)
		}
	}
}

// TestAssignAccounts_StickyFallsBackWhenAccountRemoved pins EC-14.
//
// @purpose Proves a binding to a deleted account does not strand the worker.
// @rationale `agy-auth remove` is a normal operator action; refusing to allocate would leave that
// worker permanently unusable.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded to a temp directory.
func TestAssignAccounts_StickyFallsBackWhenAccountRemoved(t *testing.T) {
	p := stickyPool(t)
	accounts := []auth.Account{
		accountWithQuotas("acct_a", 80, 80),
		accountWithQuotas("acct_b", 80, 80),
	}
	if err := saveBindings(bindingsPath(p.cfg), map[int]string{0: "acct_gone", 1: "acct_b"}); err != nil {
		t.Fatalf("seed bindings: %v", err)
	}

	got := p.assignAccounts(accounts, 2)
	if len(got) != 2 {
		t.Fatalf("expected 2 assignments, got %d", len(got))
	}
	if got[1].ID != "acct_b" {
		t.Fatalf("worker 1 = %s, want its surviving binding acct_b", got[1].ID)
	}
	if got[0].ID != "acct_a" {
		t.Fatalf("worker 0 = %s, want the only remaining free account acct_a", got[0].ID)
	}
}

// TestAssignAccounts_StickyFallsBackWhenAccountIneligible pins the eligibility fallback.
//
// @purpose Proves affinity never overrides usability.
// @rationale Keeping a worker on an exhausted account would fail every turn on that worker; the
// binding is a preference, not a guarantee.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded to a temp directory.
func TestAssignAccounts_StickyFallsBackWhenAccountIneligible(t *testing.T) {
	p := stickyPool(t)
	spent := accountWithQuotas("acct_spent", 0, 0)
	spent.Quotas["gemini-weekly"] = auth.QuotaGroup{RemainingPct: 0, IsExhausted: true}
	spent.Quotas["3p-weekly"] = auth.QuotaGroup{RemainingPct: 0, IsExhausted: true}
	accounts := []auth.Account{spent, accountWithQuotas("acct_ok", 90, 90)}

	if err := saveBindings(bindingsPath(p.cfg), map[int]string{0: "acct_spent"}); err != nil {
		t.Fatalf("seed bindings: %v", err)
	}

	got := p.assignAccounts(accounts, 1)
	if len(got) != 1 {
		t.Fatalf("expected 1 assignment, got %d", len(got))
	}
	if got[0].ID != "acct_ok" {
		t.Fatalf("worker 0 = %s, want acct_ok (the exhausted binding must not be honoured)", got[0].ID)
	}
}

// TestAssignAccounts_StickyCorruptFileDoesNotFailStartup pins EC-13 end to end.
//
// @purpose Proves a corrupt cache degrades to fresh allocation instead of aborting startup.
// @rationale The file is a cache, not a source of truth; a bad byte in it must never prevent the
// gateway from serving traffic.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded to a temp directory.
func TestAssignAccounts_StickyCorruptFileDoesNotFailStartup(t *testing.T) {
	p := stickyPool(t)
	path := bindingsPath(p.cfg)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	accounts := []auth.Account{
		accountWithQuotas("acct_a", 80, 80),
		accountWithQuotas("acct_b", 80, 80),
	}
	got := p.assignAccounts(accounts, 2)
	if len(got) != 2 {
		t.Fatalf("expected 2 assignments despite a corrupt cache, got %d", len(got))
	}
	if got[0].ID == got[1].ID {
		t.Fatalf("allocation must still spread accounts, got %s twice", got[0].ID)
	}
	// The corrupt file must have been replaced by a valid one.
	if _, err := loadBindings(path); err != nil {
		t.Fatalf("cache should have been rewritten after fallback: %v", err)
	}
}

// TestAssignAccounts_StickyPrunesRemovedWorkers pins the prune-on-save path.
//
// @purpose Proves reducing worker_count rewrites the file with only live entries.
// @rationale A stale entry would be resurrected if the count grew again, binding a new worker to an
// account another worker had since taken.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded to a temp directory.
func TestAssignAccounts_StickyPrunesRemovedWorkers(t *testing.T) {
	p := stickyPool(t)
	accounts := []auth.Account{
		accountWithQuotas("acct_a", 80, 80),
		accountWithQuotas("acct_b", 80, 80),
		accountWithQuotas("acct_c", 80, 80),
	}
	if err := saveBindings(bindingsPath(p.cfg), map[int]string{0: "acct_a", 1: "acct_b", 2: "acct_c"}); err != nil {
		t.Fatalf("seed bindings: %v", err)
	}

	// Pool shrinks to two workers.
	got := p.assignAccounts(accounts, 2)
	if len(got) != 2 {
		t.Fatalf("expected 2 assignments, got %d", len(got))
	}
	saved, err := loadBindings(bindingsPath(p.cfg))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(saved) != 2 {
		t.Fatalf("persisted %d bindings, want 2 after shrinking the pool: %v", len(saved), saved)
	}
	for idx := range saved {
		if idx > 1 {
			t.Fatalf("stale worker %d survived pruning: %v", idx, saved)
		}
	}
}

// TestBindings_EmptyAccountMapRoundTrips pins the degenerate persistence case.
//
// @purpose Proves saving no bindings produces a readable file rather than an error.
// @rationale Allocation can legitimately produce no bindings (every account ineligible); that must
// not turn into a WARN loop or an unreadable file.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded to a temp directory.
func TestBindings_EmptyAccountMapRoundTrips(t *testing.T) {
	p := stickyPool(t)
	path := bindingsPath(p.cfg)

	if err := saveBindings(path, map[int]string{}); err != nil {
		t.Fatalf("saveBindings(empty): %v", err)
	}
	got, err := loadBindings(path)
	if err != nil {
		t.Fatalf("loadBindings: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no bindings, got %v", got)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), `"strategy": "sticky"`) {
		t.Fatalf("file must record its strategy, got %s", raw)
	}
}

// TestAssignAccounts_StickyHonoursGappedBindings pins F-01.
//
// @purpose Proves a binding file with a missing worker index still honours the entries after the gap.
// @rationale The load loop was bounded by the map's size, so a file like `{"1": ...}` made the loop
// run once with i=0, look up a non-existent worker 0, and silently drop worker 1's affinity. Gaps are
// a legitimate state after a partial write, so a size-bounded range is the wrong iteration.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded to a temp directory.
func TestAssignAccounts_StickyHonoursGappedBindings(t *testing.T) {
	p := stickyPool(t)
	accounts := []auth.Account{
		accountWithQuotas("acct_a", 80, 80),
		accountWithQuotas("acct_b", 80, 80),
		accountWithQuotas("acct_c", 80, 80),
	}
	// Only worker 2 is recorded; indices 0 and 1 are absent. The binding deliberately points at the
	// FIRST account in vault order, which greedy would otherwise hand to worker 0 — so a size-bounded
	// loop (which would run once, miss worker 0, and drop the entry) yields a visibly different result
	// instead of coincidentally agreeing with the greedy spread.
	if err := saveBindings(bindingsPath(p.cfg), map[int]string{2: "acct_a"}); err != nil {
		t.Fatalf("seed bindings: %v", err)
	}

	got := p.assignAccounts(accounts, 3)
	if len(got) != 3 {
		t.Fatalf("expected 3 assignments, got %d", len(got))
	}
	if got[2].ID != "acct_a" {
		t.Fatalf("worker 2 = %s, want its persisted binding acct_a despite the gap", got[2].ID)
	}
	seen := map[string]int{}
	for _, acc := range got {
		seen[acc.ID]++
	}
	if len(seen) != 3 {
		t.Fatalf("expected 3 distinct accounts, got %v", seen)
	}
}

// TestAssignAccounts_StickySeedsFromVaultActiveAccount pins F-07 / FR-05's first-boot seed.
//
// @purpose Proves the vault's own active account drives the initial layout.
// @rationale The `active` flag was parsed by Sync into a field with no consumer, so an operator's
// choice in `agy-auth current` had no effect on which worker held which account.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded to a temp directory.
func TestAssignAccounts_StickySeedsFromVaultActiveAccount(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.AllocationStrategy = config.AllocationStrategySticky
	cfg.Storage.BaseDataDir = t.TempDir()

	// Deliberately list the active account LAST so vault order alone would not pick it.
	active := accountWithQuotas("acct_active", 50, 50)
	active.ActiveInVault = true
	accounts := []auth.Account{
		accountWithQuotas("acct_a", 90, 90),
		accountWithQuotas("acct_b", 90, 90),
		active,
	}

	mgr := auth.NewManager(cfg, nil)
	mgr.SetAccountsForTest(accounts)
	p := &Pool{cfg: cfg, authMgr: mgr, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	got := p.assignAccounts(accounts, 2)
	if len(got) != 2 {
		t.Fatalf("expected 2 assignments, got %d", len(got))
	}
	if got[0].ID != "acct_active" {
		t.Fatalf("worker 0 = %s, want the vault's active account acct_active", got[0].ID)
	}
	// The seed must not break the spread for the remaining slot.
	if got[1].ID == "acct_active" {
		t.Fatal("the seeded account must not be handed to a second worker")
	}
}

// TestPublishBinding_ReleaseClearsOldAccount pins F-02's telemetry half.
//
// @purpose Proves a rotated worker no longer leaves the previous account claiming it.
// @rationale `MarkAccountBinding` was only ever called for the NEW account, so after a rotation both
// the old and the new account reported the same `assigned_worker` and the admin view could not say
// which one was actually in use.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded to a temp directory.
func TestPublishBinding_ReleaseClearsOldAccount(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.AllocationStrategy = config.AllocationStrategyRoundRobin
	cfg.Storage.BaseDataDir = t.TempDir()
	mgr := auth.NewManager(cfg, nil)
	mgr.SetAccountsForTest([]auth.Account{
		{ID: "acct_old", Email: "o@example.com", Enabled: true, Status: auth.StatusHealthy,
			AssignedWorker: -1, Quotas: map[string]auth.QuotaGroup{}, FamilyCooldowns: map[string]time.Time{}},
		{ID: "acct_new", Email: "n@example.com", Enabled: true, Status: auth.StatusHealthy,
			AssignedWorker: -1, Quotas: map[string]auth.QuotaGroup{}, FamilyCooldowns: map[string]time.Time{}},
	})
	p := &Pool{cfg: cfg, authMgr: mgr, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	w := NewWorker(0, t.TempDir(), t.TempDir(), t.TempDir(),
		&auth.Account{ID: "acct_old", Enabled: true}, cfg, p.logger)
	p.workers = []*Worker{w}

	// Mirror the real rotation sequence: the worker's credential is swapped FIRST, then the binding is
	// published. Publication reconciles from the live workers, so the old ownership must be gone.
	w.SetAccount(&auth.Account{ID: "acct_old", Enabled: true})
	p.publishBinding(0)

	w.SetAccount(&auth.Account{ID: "acct_new", Enabled: true})
	p.publishBinding(0)

	byID := map[string]int{}
	for _, acc := range mgr.GetAccounts() {
		byID[acc.ID] = acc.AssignedWorker
	}
	if byID["acct_old"] != -1 {
		t.Fatalf("acct_old still claims worker %d, want -1 after release", byID["acct_old"])
	}
	if byID["acct_new"] != 0 {
		t.Fatalf("acct_new = %d, want 0", byID["acct_new"])
	}
}

// TestPublishBinding_SkipsWriteWhenUnchanged pins F-04.
//
// @purpose Proves an unchanged binding does not rewrite the file.
// @rationale Reconcile runs on every lease, so an unconditional write fsynced identical bytes on each
// request, putting a disk barrier in the request path for no information gain.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded to a temp directory.
func TestPublishBinding_SkipsWriteWhenUnchanged(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.AllocationStrategy = config.AllocationStrategySticky
	cfg.Pool.WorkerCount = 1
	cfg.Storage.BaseDataDir = t.TempDir()
	mgr := auth.NewManager(cfg, nil)
	mgr.SetAccountsForTest([]auth.Account{
		{ID: "acct_a", Email: "a@example.com", Enabled: true, Status: auth.StatusHealthy,
			AssignedWorker: -1, Quotas: map[string]auth.QuotaGroup{}, FamilyCooldowns: map[string]time.Time{}},
	})
	p := &Pool{cfg: cfg, authMgr: mgr, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	w := NewWorker(0, t.TempDir(), t.TempDir(), t.TempDir(),
		&auth.Account{ID: "acct_a", Enabled: true}, cfg, p.logger)
	p.workers = []*Worker{w}

	p.publishBinding(0)
	path := bindingsPath(cfg)
	first, err := os.Stat(path)
	if err != nil {
		t.Fatalf("bindings file missing after first publish: %v", err)
	}

	// Sleep past filesystem timestamp granularity so a rewrite would be observable.
	time.Sleep(20 * time.Millisecond)

	for i := 0; i < 10; i++ {
		p.publishBinding(0)
	}
	second, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !second.ModTime().Equal(first.ModTime()) {
		t.Fatalf("unchanged bindings were rewritten: mtime %v -> %v",
			first.ModTime(), second.ModTime())
	}
}

// TestPublishBinding_DoesNotEraseCacheWhenPoolIncomplete pins F-03.
//
// @purpose Proves a transient empty binding set cannot wipe a good affinity cache.
// @rationale A fleet-wide cooldown makes every account momentarily unassigned; persisting that state
// destroyed the cache that was about to become valid again.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded to a temp directory.
func TestPublishBinding_DoesNotEraseCacheWhenPoolIncomplete(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.AllocationStrategy = config.AllocationStrategySticky
	cfg.Pool.WorkerCount = 2
	cfg.Storage.BaseDataDir = t.TempDir()
	mgr := auth.NewManager(cfg, nil)
	mgr.SetAccountsForTest([]auth.Account{
		{ID: "acct_a", Email: "a@example.com", Enabled: true, Status: auth.StatusHealthy,
			AssignedWorker: -1, Quotas: map[string]auth.QuotaGroup{}, FamilyCooldowns: map[string]time.Time{}},
		{ID: "acct_b", Email: "b@example.com", Enabled: true, Status: auth.StatusHealthy,
			AssignedWorker: -1, Quotas: map[string]auth.QuotaGroup{}, FamilyCooldowns: map[string]time.Time{}},
	})
	p := &Pool{cfg: cfg, authMgr: mgr, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	path := bindingsPath(cfg)

	// A complete, good cache.
	if err := saveBindings(path, map[int]string{0: "acct_a", 1: "acct_b"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Both workers momentarily hold no account (fleet-wide cooldown).
	w0 := NewWorker(0, t.TempDir(), t.TempDir(), t.TempDir(), nil, cfg, p.logger)
	w1 := NewWorker(1, t.TempDir(), t.TempDir(), t.TempDir(), nil, cfg, p.logger)
	p.workers = []*Worker{w0, w1}

	p.publishBinding(0)

	saved, err := loadBindings(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(saved) != 2 {
		t.Fatalf("cache was destroyed by a transient empty binding: %v", saved)
	}
}

// TestPublishBinding_ReconcileIsLowestIdWins pins FINDING-R1 and FINDING-R7.
//
// @purpose Proves ownership of a shared account is deterministic and not last-publisher-wins.
// @rationale `AssignedWorker` holds one id but one account may be leased by several workers. The
// original defect released every account naming the publishing worker, so the account was reported
// `-1` while a co-tenant still held it. My first fix then let the caller's id overwrite the reconciled
// owner, making the field last-publisher-wins — a second defect, and a test that rotated only the
// first worker could not distinguish the two. This pins the contract that actually governs: the
// lowest live worker id owns a shared account, and no publish call can change that.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded to a temp directory.
func TestPublishBinding_ReconcileIsLowestIdWins(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.AllocationStrategy = config.AllocationStrategyRoundRobin
	cfg.Storage.BaseDataDir = t.TempDir()

	// Two accounts, three workers: account A is shared by workers 0 and 2, account B is held by 1.
	// "Lowest id wins" ⇒ A is owned by 0. "Last publisher wins" ⇒ whatever ran last, here 2.
	shared := auth.Account{ID: "acct_shared", Email: "s@example.com", Enabled: true,
		Status: auth.StatusHealthy, AssignedWorker: -1,
		Quotas: map[string]auth.QuotaGroup{}, FamilyCooldowns: map[string]time.Time{}}
	other := auth.Account{ID: "acct_other", Email: "o@example.com", Enabled: true,
		Status: auth.StatusHealthy, AssignedWorker: -1,
		Quotas: map[string]auth.QuotaGroup{}, FamilyCooldowns: map[string]time.Time{}}

	mgr := auth.NewManager(cfg, nil)
	mgr.SetAccountsForTest([]auth.Account{shared, other})
	p := &Pool{cfg: cfg, authMgr: mgr, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	mk := func(id int, accountID string) *Worker {
		return NewWorker(id, t.TempDir(), t.TempDir(), t.TempDir(),
			&auth.Account{ID: accountID, Enabled: true}, cfg, p.logger)
	}
	w0 := mk(0, "acct_shared")
	w1 := mk(1, "acct_other")
	w2 := mk(2, "acct_shared")
	p.workers = []*Worker{w0, w1, w2}

	// Worker 2 publishes LAST: a last-publisher-wins implementation therefore reports 2, while the
	// contract ("lowest live worker id owns a shared account") requires 0. Publishing in id order is
	// what makes the two distinguishable — an earlier version published in reverse, so the final
	// publisher was worker 0 and both rules agreed, which is why the mutation went unnoticed.
	p.publishBinding(0)
	p.publishBinding(1)
	p.publishBinding(2)

	if got := workerOf(mgr, "acct_shared"); got != 0 {
		t.Fatalf("acct_shared = %d, want 0 (lowest live worker id owns a shared account)", got)
	}
	if got := workerOf(mgr, "acct_other"); got != 1 {
		t.Fatalf("acct_other = %d, want 1", got)
	}

	// A co-tenant rotating away must not free the account for the survivor.
	w0.SetAccount(&auth.Account{ID: "acct_other", Enabled: true})
	p.publishBinding(0)
	if got := workerOf(mgr, "acct_shared"); got != 2 {
		t.Fatalf("acct_shared = %d, want 2: worker 2 still leases it after worker 0 rotated away", got)
	}
}

// workerOf reads one account's recorded owner, failing when the account is unknown.
//
// @purpose Keeps the ownership assertions above readable.
// @rationale Several tests need the same lookup; formatting the failure here keeps each assertion one
// line and its message uniform.
// @preconditions mgr is non-nil.
// @param mgr Manager snapshot source.
// @param accountID Account to look up.
// @returns The recorded worker id, or -2 when the account is absent.
// @throws None.
// @invariants O(accounts).
func workerOf(mgr *auth.Manager, accountID string) int {
	for _, acc := range mgr.GetAccounts() {
		if acc.ID == accountID {
			return acc.AssignedWorker
		}
	}
	return -2
}

// TestPublishBinding_UnsharedAccountFreedOnRotation pins the other half of R1.
//
// @purpose Proves a non-shared account is released when its only worker rotates away.
// @rationale The reconciliation must not be a no-op, or the cycle-0 F-02 defect returns.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded to a temp directory.
func TestPublishBinding_UnsharedAccountFreedOnRotation(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.AllocationStrategy = config.AllocationStrategyRoundRobin
	cfg.Pool.WorkerCount = 1
	cfg.Storage.BaseDataDir = t.TempDir()
	mgr := auth.NewManager(cfg, nil)
	mgr.SetAccountsForTest([]auth.Account{
		{ID: "acct_a", Email: "a@example.com", Enabled: true, Status: auth.StatusHealthy,
			AssignedWorker: -1, Quotas: map[string]auth.QuotaGroup{}, FamilyCooldowns: map[string]time.Time{}},
		{ID: "acct_b", Email: "b@example.com", Enabled: true, Status: auth.StatusHealthy,
			AssignedWorker: -1, Quotas: map[string]auth.QuotaGroup{}, FamilyCooldowns: map[string]time.Time{}},
	})
	p := &Pool{cfg: cfg, authMgr: mgr, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	w := NewWorker(0, t.TempDir(), t.TempDir(), t.TempDir(), &auth.Account{ID: "acct_a", Enabled: true}, cfg, p.logger)
	p.workers = []*Worker{w}

	p.publishBinding(0)
	w.SetAccount(&auth.Account{ID: "acct_b", Enabled: true})
	p.publishBinding(0)

	for _, acc := range mgr.GetAccounts() {
		if acc.ID == "acct_a" && acc.AssignedWorker != -1 {
			t.Fatalf("acct_a = %d, want -1 once its only worker rotated away", acc.AssignedWorker)
		}
	}
}

// TestPublishBinding_ConcurrentPublishesKeepFileConsistent pins FINDING-R3.
//
// @purpose Proves concurrent publishes never leave the sticky file describing a layout that no worker
// holds.
// @rationale The snapshot is rebuilt from live workers (so a write is never partial), but two leases
// could each load, compare, and save; the later save could then carry a snapshot older than an
// intervening SetAccount. Serialising the load/compare/save closes that window.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Two goroutines, joined; writes confined to a temp directory.
func TestPublishBinding_ConcurrentPublishesKeepFileConsistent(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.AllocationStrategy = config.AllocationStrategySticky
	cfg.Pool.WorkerCount = 3
	cfg.Storage.BaseDataDir = t.TempDir()

	ids := []string{"acct_a", "acct_b", "acct_c"}
	accs := make([]auth.Account, 0, 3)
	for _, id := range ids {
		accs = append(accs, auth.Account{ID: id, Email: id + "@example.com", Enabled: true,
			Status: auth.StatusHealthy, AssignedWorker: -1,
			Quotas: map[string]auth.QuotaGroup{}, FamilyCooldowns: map[string]time.Time{}})
	}
	mgr := auth.NewManager(cfg, nil)
	mgr.SetAccountsForTest(accs)
	p := &Pool{cfg: cfg, authMgr: mgr, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	ws := make([]*Worker, 0, 3)
	for i := range ids {
		ws = append(ws, NewWorker(i, t.TempDir(), t.TempDir(), t.TempDir(),
			&auth.Account{ID: ids[i], Enabled: true}, cfg, p.logger))
	}
	p.workers = ws

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for n := 0; n < 20; n++ {
				p.publishBinding(idx)
			}
		}(i)
	}
	wg.Wait()

	saved, err := loadBindings(bindingsPath(cfg))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(saved) != 3 {
		t.Fatalf("bindings file incomplete after concurrent publishes: %v", saved)
	}
	for idx, id := range saved {
		if idx < 0 || idx > 2 || ids[idx] != id {
			t.Fatalf("binding %d = %q, want %q (file disagrees with live workers: %v)",
				idx, id, ids[idx], saved)
		}
	}
}
