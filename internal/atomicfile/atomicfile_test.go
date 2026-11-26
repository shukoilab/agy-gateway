package atomicfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWrite_InstallsCompleteContent pins the basic contract.
//
// @purpose Proves the file appears with exactly the requested bytes.
// @rationale Every durability guarantee is worthless if the content is wrong.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded to a temp directory.
func TestWrite_InstallsCompleteContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.json")
	want := []byte(`{"hello":"world"}`)

	if err := Write(path, want, 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

// TestWrite_OverwritesAtomically pins that an existing file is replaced, not truncated in place.
//
// @purpose Proves a shorter second write leaves no tail of the previous longer content.
// @rationale Truncate-in-place could expose a partial file to a concurrent reader; rename cannot.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded to a temp directory.
func TestWrite_OverwritesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.json")

	if err := Write(path, []byte(strings.Repeat("A", 4096)), 0o644); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := Write(path, []byte("short"), 0o600); err != nil {
		t.Fatalf("second write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "short" {
		t.Fatalf("content = %q, want %q (no residue of the longer write)", got, "short")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm = %o, want 600 (the write must tighten, not preserve)", perm)
	}
}

// TestWrite_FailsOnMissingDirectory pins that a missing parent is an error, not a panic.
//
// @purpose Makes the MkdirAll requirement explicit at the callee boundary.
// @rationale The bindings store relies on this being a returned error so it can downgrade it to a
// WARN; a panic would take the gateway down on a cache write.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded to a temp directory; leaves nothing behind.
func TestWrite_FailsOnMissingDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "absent", "nested", "target.json")

	if err := Write(path, []byte("x"), 0o600); err == nil {
		t.Fatal("expected an error when the parent directory is missing")
	}
	if _, err := os.Stat(filepath.Join(dir, "absent")); !os.IsNotExist(err) {
		t.Fatal("Write must not create the parent directory implicitly")
	}
}

// TestWrite_LeavesNoTemporaryFile pins the cleanup invariant on the success path.
//
// @purpose Guarantees no `.target.json.tmp-*` residue accumulates next to the destination.
// @rationale The temp file lives in the same directory so the rename is atomic; a leak would fill a
// worker HOME with stale credential fragments.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded to a temp directory.
func TestWrite_LeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.json")

	if err := Write(path, []byte("data"), 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Fatalf("directory has %d entries, want exactly the target", len(entries))
	}
}
