package pool

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"agy-gateway/internal/atomicfile"
	"agy-gateway/internal/auth"
	"agy-gateway/internal/config"
)

const (
	// bindingsFileName is the state file's path relative to the configured base data directory.
	//
	// @purpose Names the affinity cache without introducing a new configurable path.
	// @rationale FR-10 forbids new hardcoded locations; deriving from storage.base_data_dir keeps the
	// file inside the one directory the operator already configures and backs up.
	bindingsFileName = "state/worker-bindings.json"

	// bindingsSchemaVersion gates the file's forward compatibility.
	//
	// @purpose Lets a future format change be rejected instead of misread.
	// @rationale The file is a cache: an unrecognised version must fall back to fresh allocation, not
	// attempt a best-effort decode that could pin every worker to the wrong account.
	bindingsSchemaVersion = 1
)

// bindingFile is the on-disk representation of sticky worker affinity.
type bindingFile struct {
	Version  int               `json:"version"`
	Strategy string            `json:"strategy"`
	Bindings map[string]string `json:"bindings"`
}

// bindingsPath derives the affinity cache location from configuration.
//
// @purpose Keeps the state file inside the operator's configured data directory.
// @rationale A literal path or a new environment variable would be a second source of truth for a
// file that must live next to the vault it describes.
// @preconditions cfg is non-nil and cfg.Storage.BaseDataDir is set.
// @param cfg Effective gateway configuration.
// @returns Absolute or base-relative path to the bindings file.
// @throws None.
// @invariants O(1); pure.
func bindingsPath(cfg *config.Config) string {
	return filepath.Join(cfg.Storage.BaseDataDir, bindingsFileName)
}

// loadBindings reads persisted sticky affinity.
//
// @purpose Restores each worker's account across a restart so `sticky` actually sticks.
// @rationale A missing file is the normal first-boot case and must not be an error; a corrupt,
// wrong-version, or wrong-strategy file must fall back to fresh allocation rather than pinning
// workers to accounts from a different policy. Both directions are reported to the caller as an error
// so it can log a WARN, because silently ignoring a corrupt cache would hide a real problem.
// @preconditions path is a filesystem path; the file need not exist.
// @param path Location produced by bindingsPath.
// @returns The recorded worker-index → account-ID map (never nil), or an error describing why the
// cache could not be trusted. A missing file returns an empty map and io.ErrNotExist wrapped in the
// error so the caller can distinguish it.
// @throws Never panics; every failure is returned.
// @invariants O(entries log entries) for the sort; no mutation of the file.
func loadBindings(path string) (map[int]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[int]string{}, err
	}

	var file bindingFile
	if err := json.Unmarshal(data, &file); err != nil {
		return map[int]string{}, fmt.Errorf("bindings file %s is not valid JSON: %w", path, err)
	}
	if file.Version != bindingsSchemaVersion {
		return map[int]string{}, fmt.Errorf(
			"bindings file %s has schema version %d, want %d", path, file.Version, bindingsSchemaVersion)
	}
	if file.Strategy != config.AllocationStrategySticky {
		return map[int]string{}, fmt.Errorf(
			"bindings file %s was written for strategy %q, not %q", path, file.Strategy, config.AllocationStrategySticky)
	}

	out := make(map[int]string, len(file.Bindings))
	for key, accountID := range file.Bindings {
		idx, convErr := strconv.Atoi(key)
		if convErr != nil {
			return map[int]string{}, fmt.Errorf("bindings file %s has a non-numeric worker key %q", path, key)
		}
		if idx < 0 {
			return map[int]string{}, fmt.Errorf("bindings file %s has a negative worker key %q", path, key)
		}
		if accountID == "" {
			return map[int]string{}, fmt.Errorf("bindings file %s maps worker %d to an empty account", path, idx)
		}
		out[idx] = accountID
	}
	return out, nil
}

// saveBindings writes sticky affinity to disk atomically.
//
// @purpose Persists the live worker→account map so a restart resumes the same layout.
// @rationale The parent directory is created here rather than at startup because it does not exist in
// a default deployment (the gateway creates only its five configured directories), and the atomic
// writer fails when the parent is missing. A binding file that is a cache must never be able to fail
// a lease, so the caller downgrades every error from this function to a WARN.
// @preconditions path is a filesystem path whose parent may not exist.
// @param path Location produced by bindingsPath.
// @param m Worker index → account ID map to persist.
// @returns A wrapped error naming the failing step, or nil on success.
// @throws Never panics; every failure is returned.
// @invariants O(entries log entries) for the stable key order; file mode 0600; directory mode 0700;
// never leaves a partially written file visible.
func saveBindings(path string, m map[int]string) error {
	// Worker indices are encoded as strings purely so the JSON object has a stable, sortable key set;
	// encoding/json sorts map[string] keys, which makes the file diff-friendly and idempotent.
	bindings := make(map[string]string, len(m))
	keys := make([]string, 0, len(m))
	for idx, accountID := range m {
		key := strconv.Itoa(idx)
		bindings[key] = accountID
		keys = append(keys, key)
	}
	sort.Strings(keys)

	file := bindingFile{
		Version:  bindingsSchemaVersion,
		Strategy: config.AllocationStrategySticky,
		Bindings: bindings,
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode bindings for %s: %w", path, err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	// 0700: the file holds account IDs and worker indices only (both already exposed by the admin
	// API), but it lives under a data directory with no reason to be world-listable. Matches the
	// credential token directory's precedent.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create bindings directory %s: %w", dir, err)
	}
	if err := atomicfile.Write(path, data, 0o600); err != nil {
		return fmt.Errorf("failed to write bindings file %s: %w", path, err)
	}
	return nil
}

// rankForAllocation reports whether an account may be bound at all, for allocation purposes.
//
// @purpose Gives boot-time allocation the same usability rule the lease path uses.
// @rationale Allocation previously accepted any account in the vault, so a globally expired or
// disabled account could be bound to a worker that would then fail every turn. The model is unknown
// at allocation time, so the empty-model form (every known family must be serviceable) is the
// correct conservative question.
// @preconditions minPct is the configured floor.
// @param acc Account under consideration.
// @param minPct Minimum remaining percentage required per bucket.
// @returns True when the account ranks above zero.
// @throws None.
// @invariants O(buckets); pure.
func rankForAllocation(acc auth.Account, minPct float64) bool {
	return acc.EligibilityRank("", false, minPct) > 0
}
