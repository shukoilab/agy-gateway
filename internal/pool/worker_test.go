package pool

import (
	"os"
	"path/filepath"
	"testing"

	"agy-gateway/internal/config"
)

func TestWorker_CleanWorkspacePurgesConversationsAndBrain(t *testing.T) {
	tempBase := t.TempDir()
	workerHome := filepath.Join(tempBase, "worker_home")
	workerCwd := filepath.Join(tempBase, "worker_cwd")
	workerTmp := filepath.Join(tempBase, "worker_tmp")

	if err := os.MkdirAll(workerHome, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workerCwd, 0755); err != nil {
		t.Fatal(err)
	}

	// Create CWD transient files and .git
	if err := os.WriteFile(filepath.Join(workerCwd, "temp.txt"), []byte("temp"), 0644); err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(workerCwd, ".git")
	if err := os.MkdirAll(gitDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create ephemeral ACP session database and brain files
	convDir := filepath.Join(workerHome, ".gemini", "antigravity-cli", "conversations")
	brainDir := filepath.Join(workerHome, ".gemini", "antigravity-cli", "brain", "session-123")
	if err := os.MkdirAll(convDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(brainDir, 0755); err != nil {
		t.Fatal(err)
	}
	dbFile := filepath.Join(convDir, "session-123.db")
	dbWal := filepath.Join(convDir, "session-123.db-wal")
	dbShm := filepath.Join(convDir, "session-123.db-shm")
	if err := os.WriteFile(dbFile, []byte("sqlite-db-data"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbWal, []byte("sqlite-wal-data"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbShm, []byte("sqlite-shm-data"), 0644); err != nil {
		t.Fatal(err)
	}
	brainFile := filepath.Join(brainDir, "task.log")
	if err := os.WriteFile(brainFile, []byte("log-content"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Execution: config.ExecutionConfig{
			CleanWorkspaceBetweenRequests: true,
		},
	}

	worker := NewWorker(1, workerHome, workerTmp, workerCwd, nil, cfg, nil)
	worker.CleanWorkspace()

	// Verify CWD files
	if _, err := os.Stat(filepath.Join(workerCwd, "temp.txt")); !os.IsNotExist(err) {
		t.Errorf("expected temp.txt to be removed from CWD")
	}
	if _, err := os.Stat(filepath.Join(gitDir, "HEAD")); err != nil {
		t.Errorf("expected .git/HEAD to be preserved")
	}

	// Verify ephemeral ACP session state was purged
	if _, err := os.Stat(dbFile); !os.IsNotExist(err) {
		t.Errorf("expected session-123.db to be removed from conversations")
	}
	if _, err := os.Stat(dbWal); !os.IsNotExist(err) {
		t.Errorf("expected session-123.db-wal to be removed from conversations")
	}
	if _, err := os.Stat(dbShm); !os.IsNotExist(err) {
		t.Errorf("expected session-123.db-shm to be removed from conversations")
	}
	if _, err := os.Stat(brainDir); !os.IsNotExist(err) {
		t.Errorf("expected brain/session-123 to be removed from brain directory")
	}
}
