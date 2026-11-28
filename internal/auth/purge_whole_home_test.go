package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"agy-gateway/internal/config"
)

// TestSync_PurgesWholeWorkerHomeOfDeletedAccount is S1.
//
// @purpose Proves deleting an account removes the entire worker HOME it occupied, not just the token.
// @rationale The previous purge deleted one file per worker and was bounded by `worker_count`, so a
// deleted account kept its refresh token (one file was world-readable), its conversations, its
// summaries database and its brain artifacts on disk indefinitely.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants One stubbed vault read.
func TestSync_PurgesWholeWorkerHomeOfDeletedAccount(t *testing.T) {
	cfg := config.DefaultConfig()
	// Deliberately lower than the number of homes on disk: the purge must not be bounded by this.
	cfg.Pool.WorkerCount = 1
	cfg.Storage.WorkersRootDir = t.TempDir()

	doomedToken := writeWorkerCredential(t, cfg.Storage.WorkersRootDir, 2, "doomed@example.com")
	doomedHome := filepath.Dir(filepath.Dir(filepath.Dir(doomedToken)))

	// Account-owned caches that must disappear with the home.
	cliDir := filepath.Join(doomedHome, ".gemini", "antigravity-cli")
	extra := []string{
		filepath.Join(cliDir, "conversations", "s1.db"),
		filepath.Join(cliDir, "conversation_summaries.db"),
		filepath.Join(cliDir, "brain", "note.txt"),
		filepath.Join(cliDir, "annotations", "a.pbtxt"),
	}
	for _, f := range extra {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(f), err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}

	keptToken := writeWorkerCredential(t, cfg.Storage.WorkersRootDir, 0, "kept@example.com")

	mgr := NewManager(cfg, nil)
	mgr.SetAccountsForTest([]Account{{ID: "acct_doomed", Email: "doomed@example.com", Enabled: true}})
	mgr.listForTest = func(context.Context) ([]byte, error) {
		return []byte(`{"data":{"accounts":[{"id":"acct_kept","email":"kept@example.com","active":true}]}}`), nil
	}

	if err := mgr.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if _, err := os.Stat(doomedHome); !os.IsNotExist(err) {
		t.Fatalf("the whole worker home of a deleted account must be removed, stat err = %v", err)
	}
	for _, f := range extra {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatalf("%s survived the purge", f)
		}
	}
	if _, err := os.Stat(keptToken); err != nil {
		t.Fatalf("a live account's credential must survive, stat err = %v", err)
	}
}

// TestSync_PurgeIgnoresNonWorkerDirectories pins the scan boundary.
//
// @purpose Proves the purge only acts on `w<digits>` directories.
// @rationale The purge removes whole directories, so a loose pattern would let it delete unrelated
// state under the workers root.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants One stubbed vault read.
func TestSync_PurgeIgnoresNonWorkerDirectories(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Storage.WorkersRootDir = t.TempDir()

	other := filepath.Join(cfg.Storage.WorkersRootDir, "scratch", ".gemini", "antigravity-cli")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	otherFile := filepath.Join(other, "keep.txt")
	if err := os.WriteFile(otherFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	mgr := NewManager(cfg, nil)
	mgr.SetAccountsForTest([]Account{{ID: "acct_doomed", Email: "doomed@example.com", Enabled: true}})
	mgr.listForTest = func(context.Context) ([]byte, error) {
		return []byte(`{"data":{"accounts":[{"id":"acct_kept","email":"kept@example.com"}]}}`), nil
	}
	if err := mgr.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if _, err := os.Stat(otherFile); err != nil {
		t.Fatalf("a non-worker directory must never be purged, stat err = %v", err)
	}
}
