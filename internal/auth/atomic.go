package auth

import (
	"os"

	"agy-gateway/internal/atomicfile"
)

// atomicWriteFile writes data to path durably and atomically.
//
// @purpose Keeps the credential installer's call site unchanged while the implementation lives in one
// shared package instead of a per-package copy.
// @rationale The same tmp+fsync+rename sequence is required by both credential materialisation and
// the worker-binding cache. Duplicating it would let the two copies drift, and a drift in the
// credential path is the defect (a half-written token) this exists to prevent. This delegate keeps
// internal/auth/atomic_test.go meaningful as a behaviour-neutrality guard on the move.
// @preconditions The parent directory of path exists and is writable.
// @param path Final destination path.
// @param data Complete file contents.
// @param perm Permission bits applied to the temporary file before it is renamed into place.
// @returns A wrapped error naming the failing step, or nil on success.
// @throws Never panics; every failure is returned.
// @invariants O(len(data)) in time and space; leaves no temporary file behind on any path; never
// makes a partially written file visible at path. Thread-safe for distinct paths.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	return atomicfile.Write(path, data, perm)
}
