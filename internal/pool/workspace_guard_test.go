package pool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agy-gateway/internal/config"
)

// guardTestConfig returns a minimal config sufficient for CleanWorkspace guard testing.
//
// @purpose Supplies the storage root the guard compares against.
// @rationale Keeps each test's intent readable without repeating struct literals.
// @preconditions None.
// @param t Standard Go test handle used for temp directories.
// @returns A config with workspace cleaning enabled and an isolated root.
// @throws None.
// @invariants Allocates only a temp directory.
func guardTestConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Execution: config.ExecutionConfig{CleanWorkspaceBetweenRequests: true},
		Storage:   config.StorageConfig{WorkspacesRootDir: t.TempDir()},
	}
}

// TestValidateWorkspaceRoot_AcceptsProperChild proves the guard allows the normal case of a
// per-worker directory nested under the configured root.
//
// @purpose Regression guard for FR-24: cleaning must keep working when the path is legitimate.
// @rationale Over-tightening the guard would silently disable workspace cleanup entirely.
// @preconditions A temporary directory tree exists.
// @param t Standard Go test handle.
// @returns None.
// @throws Fatal on any mismatch.
// @invariants Creates and removes only its own temp tree.
func TestValidateWorkspaceRoot_AcceptsProperChild(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "w0")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := validateWorkspaceRoot(root, cwd); err != nil {
		t.Fatalf("expected proper child to be accepted, got %v", err)
	}
}

// TestValidateWorkspaceRoot_RefusalMatrix covers every catastrophic path the guard must reject.
//
// @purpose Proves a misconfigured storage root cannot trigger a destructive recursive delete.
// @rationale This is the only barrier between a typo in config and irreversible data loss.
// @preconditions A temporary directory tree exists; HOME may or may not resolve.
// @param t Standard Go test handle.
// @returns None.
// @throws Fatal when a dangerous path is accepted.
// @invariants Read-only apart from its own temp tree.
func TestValidateWorkspaceRoot_RefusalMatrix(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "w0")
	outside := t.TempDir()
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cases := []struct {
		name string
		root string
		cwd  string
	}{
		{name: "filesystem root", root: root, cwd: "/"},
		{name: "equals root", root: root, cwd: root},
		{name: "parent traversal", root: root, cwd: filepath.Join(root, "..", "elsewhere")},
		{name: "sibling outside root", root: root, cwd: outside},
		{name: "empty cwd", root: root, cwd: ""},
		{name: "empty root", root: "", cwd: inside},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateWorkspaceRoot(tc.root, tc.cwd); err == nil {
				t.Fatalf("expected refusal for %s (root=%q cwd=%q)", tc.name, tc.root, tc.cwd)
			}
		})
	}

	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if verr := validateWorkspaceRoot(root, home); verr == nil {
			t.Fatalf("expected refusal for home directory %q", home)
		}
	}
}

// TestValidateWorkspaceRoot_RejectsSymlinkedWorkspace proves a symlinked workspace directory is not
// followed, since deletion through it could escape the root.
//
// @purpose Closes the symlink escape variant of FR-24.
// @rationale A symlinked CWD would make every child path resolve outside the intended root.
// @preconditions A temp tree and a symlink are creatable.
// @param t Standard Go test handle.
// @returns None.
// @throws Fatal on acceptance or setup failure.
// @invariants Only touches its own temp directories.
func TestValidateWorkspaceRoot_RejectsSymlinkedWorkspace(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	link := filepath.Join(root, "w0")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}
	if err := validateWorkspaceRoot(root, link); err == nil {
		t.Fatal("expected symlinked workspace to be refused")
	}
}

// TestSafeRemoveEntry_DoesNotFollowSymlink proves removing a symlinked child deletes only the link.
//
// @purpose Ensures a planted symlink cannot widen the deletion scope beyond the workspace.
// @rationale os.RemoveAll on a symlink removes the link, but the guard must be explicit so a future
// refactor cannot regress into following it.
// @preconditions A temp tree with a symlinked entry exists.
// @param t Standard Go test handle.
// @returns None.
// @throws Fatal when the link target is affected.
// @invariants Only touches its own temp directories.
func TestSafeRemoveEntry_DoesNotFollowSymlink(t *testing.T) {
	root := t.TempDir()
	protected := t.TempDir()
	victim := filepath.Join(protected, "keep.txt")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(protected, link); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}

	if err := safeRemoveEntry(root, "escape"); err != nil {
		t.Fatalf("safeRemoveEntry: %v", err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("symlink target contents were destroyed: %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("expected the link itself to be removed, got %v", err)
	}
}

// TestCleanWorkspace_RefusesDangerousRoot proves the worker-level entry point honours the guard.
//
// @purpose Verifies FR-24 end to end: CleanWorkspace leaves files intact when the root is unsafe.
// @rationale The unit guard is useless if CleanWorkspace bypasses it.
// @preconditions A worker config with a workspace outside the configured root exists.
// @param t Standard Go test handle.
// @returns None.
// @throws Fatal if the sentinel file disappears.
// @invariants Only touches its own temp directories.
func TestCleanWorkspace_RefusesDangerousRoot(t *testing.T) {
	cfg := guardTestConfig(t)

	cwd := t.TempDir() // outside the configured root
	sentinel := filepath.Join(cwd, "precious.txt")
	if err := os.WriteFile(sentinel, []byte("precious"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	w := NewWorker(0, "", "", cwd, nil, cfg, nil)
	w.CleanWorkspace()

	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("CleanWorkspace destroyed a file outside the configured root: %v", err)
	}
}

// TestCleanWorkspace_RemovesInsideRoot proves normal cleanup still works after the guard.
//
// @purpose Prevents a false sense of safety where the guard silently disables all cleanup.
// @rationale Storage growth between requests is a real leak; the guard must not break it.
// @preconditions A worker workspace nested under the configured root contains scratch files.
// @param t Standard Go test handle.
// @returns None.
// @throws Fatal when files survive.
// @invariants Only touches its own temp directories.
func TestCleanWorkspace_RemovesInsideRoot(t *testing.T) {
	cfg := guardTestConfig(t)

	cwd := filepath.Join(cfg.Storage.WorkspacesRootDir, "w0")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	scratch := filepath.Join(cwd, "tmp.txt")
	if err := os.WriteFile(scratch, []byte("x"), 0o600); err != nil {
		t.Fatalf("write scratch: %v", err)
	}
	keep := filepath.Join(cwd, ".git")
	if err := os.MkdirAll(keep, 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}

	w := NewWorker(0, "", "", cwd, nil, cfg, nil)
	w.CleanWorkspace()

	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("expected scratch file removed, got %v", err)
	}
	if info, err := os.Stat(keep); err != nil || !info.IsDir() {
		t.Fatalf(".git must be preserved, got err=%v", err)
	}
}

// TestWorkspaceGuardStringsAreRelativeSafe is a light assertion that the guard never emits a path
// containing a doubled separator, which would confuse Rel on some platforms.
//
// @purpose Cheap invariant check on guard error construction.
// @rationale Cosmetic correctness here keeps error messages actionable in production logs.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws Fatal on a doubled separator.
// @invariants Pure.
func TestWorkspaceGuardStringsAreRelativeSafe(t *testing.T) {
	err := validateWorkspaceRoot("", "/tmp/x")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "//") {
		t.Fatalf("error message contains a doubled separator: %q", err.Error())
	}
}
