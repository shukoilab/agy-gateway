package pool

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"agy-gateway/internal/config"
)

// OrphanWorkerHome is one worker home directory that no live worker slot uses.
//
// @purpose Gives the operator a name and a reason before anything is deleted.
// @rationale Lowering `worker_count` never removed the surplus homes, so credentials, conversation
// caches and annotation files for accounts the operator had already removed kept sitting on disk
// indefinitely — including one token file found world-readable. A number that is not a live slot is
// the only definition of an orphan the reaper needs.
// @preconditions None.
// @invariants Immutable value.
type OrphanWorkerHome struct {
	Index int    `json:"index" example:"3"`
	Path  string `json:"path" example:"/var/lib/agy-gateway/workers/w3"`
}

// OrphanWorkerHomes lists worker home directories beyond the configured worker count.
//
// @purpose Detects worker homes that no live worker slot owns.
// @rationale Scans the directory instead of assuming `0..WorkerCount-1` exist: `worker_count` can be
// lowered, and a previous build created every home it was ever asked for. Both directions must be
// visible, so the scan is driven by what is on disk.
// @preconditions cfg is non-nil.
// @param cfg Loaded configuration supplying the workers root and worker count.
// @returns Orphan homes sorted by index; nil when the root is absent or holds none.
// @throws None; an unreadable root yields nil.
// @invariants O(directories); only names matching `w<digits>` are considered.
func OrphanWorkerHomes(cfg *config.Config) []OrphanWorkerHome {
	if cfg == nil {
		return nil
	}
	root := strings.TrimSpace(cfg.Storage.WorkersRootDir)
	if root == "" {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	liveSlots := cfg.Pool.WorkerCount
	if liveSlots < 0 {
		liveSlots = 0
	}
	var orphans []OrphanWorkerHome
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) < 2 || name[0] != 'w' {
			continue
		}
		idx, convErr := strconv.Atoi(name[1:])
		if convErr != nil || idx < 0 || idx < liveSlots {
			continue
		}
		orphans = append(orphans, OrphanWorkerHome{Index: idx, Path: filepath.Join(root, name)})
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].Index < orphans[j].Index })
	return orphans
}

// ReapOrphanWorkerHomes removes worker homes no live slot owns.
//
// @purpose Makes `worker_count` reductions and account deletions leave nothing behind.
// @rationale The credential of a deleted account was found still on disk under a surplus home
// because the previous purge iterated `0..WorkerCount-1`. Reaping by directory closes that class
// rather than the single observed instance.
// @preconditions cfg is non-nil.
// @param cfg Loaded configuration.
// @param dryRun When true, reports without deleting.
// @returns The orphans removed (or that would be removed under dryRun).
// @throws A wrapped error when an orphan exists but cannot be removed; the remaining ones are still
// attempted and reported.
// @invariants O(directories); a home is removed whole, never partially.
func ReapOrphanWorkerHomes(cfg *config.Config, dryRun bool) ([]OrphanWorkerHome, error) {
	orphans := OrphanWorkerHomes(cfg)
	if dryRun || len(orphans) == 0 {
		return orphans, nil
	}
	var firstErr error
	kept := orphans[:0]
	for _, o := range orphans {
		if err := os.RemoveAll(o.Path); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("could not remove orphan worker home %q: %w", o.Path, err)
			}
			kept = append(kept, o)
		}
	}
	if len(kept) == 0 {
		return nil, firstErr
	}
	return kept, firstErr
}
