package pool

import (
	"os"
	"path/filepath"
	"testing"

	"agy-gateway/internal/config"
)

// TestOrphanWorkerHomes_ScansBeyondWorkerCount pins the reason the purge used to miss homes.
//
// @purpose Proves orphan detection is driven by what is on disk, not by `0..WorkerCount-1`.
// @rationale A deleted account's credential survived under `w2`/`w3` while `worker_count` was 1,
// because the old purge iterated the configured slot count. Any test that only places homes inside
// that range would never have caught it.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no filesystem outside t.TempDir.
func TestOrphanWorkerHomes_ScansBeyondWorkerCount(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Storage.WorkersRootDir = t.TempDir()
	cfg.Pool.WorkerCount = 1

	for _, name := range []string{"w0", "w1", "w2", "w3", "notaworker", "wX"} {
		if err := os.MkdirAll(filepath.Join(cfg.Storage.WorkersRootDir, name), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
	}

	orphans := OrphanWorkerHomes(cfg)
	if len(orphans) != 3 {
		t.Fatalf("orphans = %d (%v), want 3 (w1,w2,w3)", len(orphans), orphans)
	}
	for i, want := range []int{1, 2, 3} {
		if orphans[i].Index != want {
			t.Fatalf("orphans[%d] = w%d, want w%d", i, orphans[i].Index, want)
		}
	}

	if err := os.MkdirAll(filepath.Join(cfg.Storage.WorkersRootDir, "w9"), 0o700); err != nil {
		t.Fatalf("mkdir w9: %v", err)
	}
	cfg.Pool.WorkerCount = 10
	if got := OrphanWorkerHomes(cfg); len(got) != 0 {
		t.Fatalf("raising worker_count must clear the orphan set, got %v", got)
	}
}

// TestReapOrphanWorkerHomes_DryRunKeepsEverything pins the preview contract.
//
// @purpose Proves `--dry-run` reports without deleting.
// @rationale This is the only command that can delete agent-visible files; an operator must be able
// to see the plan first.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestReapOrphanWorkerHomes_DryRunKeepsEverything(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Storage.WorkersRootDir = t.TempDir()
	cfg.Pool.WorkerCount = 1

	orphan := filepath.Join(cfg.Storage.WorkersRootDir, "w4")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	planned, err := ReapOrphanWorkerHomes(cfg, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(planned) != 1 {
		t.Fatalf("planned = %v, want one orphan", planned)
	}
	if _, statErr := os.Stat(orphan); statErr != nil {
		t.Fatalf("dry run must not delete anything, stat err = %v", statErr)
	}

	removed, err := ReapOrphanWorkerHomes(cfg, false)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("successful reap must report nothing left, got %v", removed)
	}
	if _, statErr := os.Stat(orphan); !os.IsNotExist(statErr) {
		t.Fatalf("orphan must be gone, stat err = %v", statErr)
	}
}

// TestPurgeSharedWorkspace_KeepsGitAndRefusesDangerousRoots pins both halves of the safety rule.
//
// @purpose Proves the workspace purge removes agent files, keeps `.git`, and refuses a path the
// gateway does not own.
// @rationale The request path no longer deletes anything; this command is now the only destructive
// path, so it has to be both effective and incapable of destroying the workers tree or `/`.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestPurgeSharedWorkspace_KeepsGitAndRefusesDangerousRoots(t *testing.T) {
	cfg := config.DefaultConfig()
	root := t.TempDir()
	cfg.Storage.WorkspacesRootDir = root
	cfg.Storage.WorkspaceDir = filepath.Join(root, "shared")
	ws := cfg.Storage.WorkspaceDir

	if err := os.MkdirAll(filepath.Join(ws, ".git"), 0o750); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	for _, name := range []string{"main.go", "notes.md"} {
		if err := os.WriteFile(filepath.Join(ws, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	if got := SharedWorkspaceEntries(cfg); len(got) != 2 {
		t.Fatalf("entries = %v, want 2 (main.go, notes.md)", got)
	}

	removed, err := PurgeSharedWorkspace(cfg, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("dry run removed = %v, want 2", removed)
	}
	if _, statErr := os.Stat(filepath.Join(ws, "main.go")); statErr != nil {
		t.Fatalf("dry run must not delete, stat err = %v", statErr)
	}

	if _, err := PurgeSharedWorkspace(cfg, false); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(ws, "main.go")); !os.IsNotExist(statErr) {
		t.Fatalf("purge must delete the file, stat err = %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(ws, ".git")); statErr != nil {
		t.Fatalf("purge must keep .git, stat err = %v", statErr)
	}

	// A workspace pointing at the workers tree or outside the workspaces root is refused outright.
	unsafe := config.DefaultConfig()
	unsafe.Storage.WorkspacesRootDir = root
	unsafe.Storage.WorkersRootDir = filepath.Join(root, "workers")
	unsafe.Storage.WorkspaceDir = filepath.Join(root, "workers", "w0")
	if erased, err := PurgeSharedWorkspace(unsafe, false); err != nil || len(erased) != 0 {
		t.Fatalf("a workspace inside the workers tree must be refused, got %v / %v", erased, err)
	}

	escape := config.DefaultConfig()
	escape.Storage.WorkspacesRootDir = root
	escape.Storage.WorkspaceDir = "/tmp/definitely-not-ours"
	if erased, err := PurgeSharedWorkspace(escape, false); err != nil || len(erased) != 0 {
		t.Fatalf("a workspace outside the workspaces root must be refused, got %v / %v", erased, err)
	}
}
