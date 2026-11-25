package auth

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAtomicWriteFile_InstallsCompleteContent pins the durability and completeness contract.
//
// @purpose Proves a credential file is never observable in a partially written state.
// @rationale Defect A7 was a swallowing `_ = copyFile(...)` that could leave a truncated token. The
// replacement must install the complete payload, with the requested permissions, in one step.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Uses only a temporary directory; no network or subprocess.
func TestAtomicWriteFile_InstallsCompleteContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "antigravity-oauth-token")
	payload := []byte(`{"token":{"access_token":"a","refresh_token":"r"}}`)

	if err := atomicWriteFile(path, payload, 0600); err != nil {
		t.Fatalf("atomicWriteFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed file: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("installed content = %q, want %q", got, payload)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat installed file: %v", err)
	}
	// A credential readable by group or others is a security regression, not a style issue.
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("permissions = %04o, want 0600", perm)
	}

	// No temporary file may survive a successful write.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(path) {
			t.Fatalf("unexpected leftover file in destination directory: %s", e.Name())
		}
	}
}

// TestAtomicWriteFile_OverwritesAtomically verifies an existing credential is replaced, not appended.
//
// @purpose Prevents rotation from corrupting a worker's token by writing a shorter payload over a
// longer one without truncation.
// @rationale The old copy path opened the destination in place; a shorter credential would leave a
// tail of the previous one, producing invalid JSON that agy would reject at an unpredictable time.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Uses only a temporary directory.
func TestAtomicWriteFile_OverwritesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "antigravity-oauth-token")

	long := []byte(`{"token":{"access_token":"LONG-TOKEN-VALUE-HERE","refresh_token":"r1"}}`)
	short := []byte(`{"token":{"access_token":"a","refresh_token":"r2"}}`)

	if err := atomicWriteFile(path, long, 0600); err != nil {
		t.Fatalf("first write: %v", err)
	}
	// Widen the permissions deliberately: a copy-style rewrite would keep them, because os.WriteFile
	// only applies its mode argument when it creates the file.
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := atomicWriteFile(path, short, 0600); err != nil {
		t.Fatalf("second write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(short) {
		t.Fatalf("content = %q, want exactly %q (no tail from the previous write)", got, short)
	}

	// Every rotation must re-assert 0600 on the installed credential, whatever the previous mode was.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("permissions = %04o after rewrite, want 0600", perm)
	}
}

// TestAtomicWriteFile_FailsOnMissingDirectory verifies errors are returned rather than swallowed.
//
// @purpose Proves a failed credential install surfaces to the caller instead of being ignored.
// @rationale Defect A7 discarded the copy error entirely, so a worker could be leased with no valid
// credential and fail hours later. Every write failure must be reported.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Uses only a temporary directory; asserts no panic.
func TestAtomicWriteFile_FailsOnMissingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does", "not", "exist", "token")
	if err := atomicWriteFile(missing, []byte("x"), 0600); err == nil {
		t.Fatal("expected an error writing into a missing directory, got nil")
	}
}
