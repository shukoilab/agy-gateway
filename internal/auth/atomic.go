package auth

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// atomicWriteFile writes data to path durably and atomically.
//
// @purpose Replaces the truncate-in-place credential copy that could leave a half-written token
// file if the process died mid-copy, which would strand a worker on an unusable credential.
// @rationale tmp+rename is the only sequence on POSIX that guarantees a reader sees either the
// complete old file or the complete new file. The explicit fsync of both the file and its parent
// directory is what makes the rename survive a crash: without the directory fsync the rename itself
// can be lost even though the data blocks were flushed.
// @preconditions The parent directory of path exists and is writable.
// @param path Final destination path.
// @param data Complete file contents.
// @param perm Permission bits applied to the temporary file before it is renamed into place.
// @returns A wrapped error naming the failing step, or nil on success.
// @throws Never panics; every failure is returned.
// @invariants O(len(data)) in time and space; leaves no temporary file behind on any path, including
// error paths; never makes a partially written file visible at path.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)

	suffix, err := randomSuffix()
	if err != nil {
		return fmt.Errorf("failed to generate temporary suffix for %s: %w", path, err)
	}
	tmpPath := filepath.Join(dir, fmt.Sprintf(".%s.tmp-%s", filepath.Base(path), suffix))

	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return fmt.Errorf("failed to create temporary file %s: %w", tmpPath, err)
	}

	// Every failure past this point must remove the temporary file so no credential fragment is
	// left readable in the worker HOME.
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}

	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("failed to write temporary file %s: %w", tmpPath, err)
	}
	// Chmod explicitly: O_CREATE honours umask, which could widen a credential beyond 0600.
	if err := tmp.Chmod(perm); err != nil {
		cleanup()
		return fmt.Errorf("failed to set permissions on %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("failed to sync temporary file %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to close temporary file %s: %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to install %s: %w", path, err)
	}

	// The rename is only durable once the parent directory entry is flushed.
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("failed to sync directory %s after installing %s: %w", dir, path, err)
	}
	return nil
}

// syncDir flushes a directory entry so a preceding rename survives a crash.
//
// @purpose Makes atomic installation durable rather than merely ordered.
// @rationale Rename atomicity is guaranteed in memory, but the new directory entry reaches stable
// storage only after the directory itself is fsynced.
// @preconditions dir is an existing directory.
// @param dir Directory to flush.
// @returns A wrapped error when the directory cannot be opened or synced.
// @throws Never panics; failures are returned.
// @invariants O(1); always closes the descriptor it opened.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// randomSuffix returns a short random hex string for collision-free temporary names.
//
// @purpose Guarantees concurrent writers targeting the same destination never share a temp path.
// @rationale Two workers materialising credentials at once must not interleave writes; a random
// suffix removes the need to serialise them on a lock for correctness of the write itself.
// @preconditions None.
// @param None.
// @returns Sixteen hex characters, or an error if the system entropy source fails.
// @throws Never panics; entropy failure is returned.
// @invariants O(1); reads exactly 8 bytes from crypto/rand.
func randomSuffix() (string, error) {
	buf := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
