package pool

import (
	"os"
	"path/filepath"
	"testing"

	"agy-gateway/internal/config"
)

// TestWorker_PurgeAccountCachesLeavesWorkspaceAndToken covers the split that replaced
// CleanWorkspace.
//
// @purpose Proves account-owned caches are purged while the workspace and the credential survive.
// @rationale The old CleanWorkspace deleted the workspace on every Release, which is what made an agent
// rewrite its own files forever. Purging must therefore be limited to data that belongs to the account
// being replaced.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatal.
// @invariants Deterministic; no subprocess.
func TestWorker_PurgeAccountCachesLeavesWorkspaceAndToken(t *testing.T) {
	tempBase := t.TempDir()
	workerHome := filepath.Join(tempBase, "worker_home")
	workerCwd := filepath.Join(tempBase, "workspace")
	workerTmp := filepath.Join(tempBase, "worker_tmp")

	cliDir := filepath.Join(workerHome, ".gemini", "antigravity-cli")
	convDir := filepath.Join(cliDir, "conversations")
	brainDir := filepath.Join(cliDir, "brain", "session-123")
	annDir := filepath.Join(cliDir, "annotations")
	for _, d := range []string{workerCwd, workerTmp, convDir, brainDir, annDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	tokenPath := filepath.Join(cliDir, "antigravity-oauth-token")
	if err := os.WriteFile(tokenPath, []byte(`{"token":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	workspaceFile := filepath.Join(workerCwd, "agent-written.txt")
	if err := os.WriteFile(workspaceFile, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitHead := filepath.Join(workerCwd, ".git", "HEAD")
	if err := os.MkdirAll(filepath.Dir(gitHead), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gitHead, []byte("ref: refs/heads/main"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, f := range []string{
		filepath.Join(convDir, "session-123.db"),
		filepath.Join(convDir, "session-123.db-wal"),
		filepath.Join(convDir, "session-123.db-shm"),
		filepath.Join(cliDir, "conversation_summaries.db"),
		filepath.Join(cliDir, "conversation_summaries.db-wal"),
		filepath.Join(cliDir, "conversation_summaries.db-shm"),
		filepath.Join(brainDir, "task.log"),
		filepath.Join(annDir, "note.pbtxt"),
	} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.Config{}
	worker := NewWorker(1, workerHome, workerTmp, workerCwd, nil, cfg, nil)

	if !worker.PurgeAccountCaches() {
		t.Fatal("PurgeAccountCaches must report that it removed something")
	}

	// The directories themselves stay (agy recreates them); their account-owned contents must not.
	for _, emptied := range []string{convDir, annDir, brainDir} {
		entries, err := os.ReadDir(emptied)
		if err != nil {
			continue // removed entirely is also fine
		}
		if len(entries) != 0 {
			t.Errorf("%s must be emptied with the account, still holds %d entries", emptied, len(entries))
		}
	}
	for _, gone := range []string{
		filepath.Join(cliDir, "conversation_summaries.db"),
		filepath.Join(cliDir, "conversation_summaries.db-wal"),
		filepath.Join(cliDir, "conversation_summaries.db-shm"),
	} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s must be purged with the account, stat err = %v", gone, err)
		}
	}

	// The workspace and the credential are NOT account-owned caches.
	if _, err := os.Stat(workspaceFile); err != nil {
		t.Errorf("agent-written workspace file must survive a cache purge, stat err = %v", err)
	}
	if _, err := os.Stat(gitHead); err != nil {
		t.Errorf(".git inside the workspace must survive, stat err = %v", err)
	}
	if _, err := os.Stat(tokenPath); err != nil {
		t.Errorf("the credential is replaced by materialisation, not purged here, stat err = %v", err)
	}
}

// TestWorker_PurgeWorkspaceKeepsGit covers the explicit operator action.
//
// @purpose Proves the destructive workspace purge exists but is only reachable on purpose.
// @rationale Deleting a workspace is a legitimate operator action; doing it implicitly on every request
// was the loop. Keeping .git usable means the purge cannot destroy a clone's history.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatal.
// @invariants Deterministic; refuses to run when the workspace is not inside the configured root.
func TestWorker_PurgeWorkspaceKeepsGit(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "conv")
	if err := os.MkdirAll(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".git", "HEAD"), []byte("ref"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Storage: config.StorageConfig{WorkspaceDir: root}}
	worker := NewWorker(0, t.TempDir(), t.TempDir(), cwd, nil, cfg, nil)

	if n := worker.PurgeWorkspace(); n != 1 {
		t.Fatalf("PurgeWorkspace removed %d entries, want 1 (a.txt only)", n)
	}
	if _, err := os.Stat(filepath.Join(cwd, "a.txt")); !os.IsNotExist(err) {
		t.Error("a.txt must be removed by an explicit workspace purge")
	}
	if _, err := os.Stat(filepath.Join(cwd, ".git", "HEAD")); err != nil {
		t.Errorf(".git content must be preserved, stat err = %v", err)
	}
}
