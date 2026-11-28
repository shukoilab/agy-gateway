package pool

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agy-gateway/internal/config"
)

// SharedWorkspaceEntries lists what an explicit workspace purge would remove.
//
// @purpose Gives `agy-accounts clean --workspace --dry-run` something truthful to print before
// anything is deleted, so an operator can see the blast radius of a destructive command.
// @rationale The workspace is durable by design (nothing removes it on the request path), which
// makes an explicit purge the only way it ever shrinks. A destructive operation that cannot be
// previewed is not acceptable for a directory that may hold a user's repository.
// @preconditions cfg is non-nil.
// @param cfg Loaded configuration supplying the effective workspace directory.
// @returns Entry names that would be removed, sorted, excluding `.git`; nil when the directory does
// not exist or cannot be read.
// @throws None; an unreadable directory yields nil entries and is reported by the error return of
// PurgeSharedWorkspace instead.
// @invariants O(entries); never follows a symlink out of the workspace.
func SharedWorkspaceEntries(cfg *config.Config) []string {
	dir, ok := purgeableWorkspaceDir(cfg)
	if !ok {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// PurgeSharedWorkspace removes the contents of the shared workspace, keeping `.git`.
//
// @purpose Performs the only sanctioned deletion of agent-visible files.
// @rationale Every automatic purge was removed from the request path because deleting files the
// agent had just written is what made long agentic sessions rewrite the same files forever. The
// replacement is an operator-invoked, previewable purge that preserves version-control metadata.
// @preconditions cfg is non-nil and its effective workspace directory passes purgeableWorkspaceDir.
// @param cfg Loaded configuration.
// @param dryRun When true, reports without deleting.
// @returns The entry names removed (or that would be removed under dryRun).
// @throws A wrapped error when the workspace directory exists but cannot be read, or when an entry
// cannot be removed. A missing directory is not an error.
// @invariants O(entries); `.git` is never touched; never operates outside the configured workspace.
func PurgeSharedWorkspace(cfg *config.Config, dryRun bool) ([]string, error) {
	dir, ok := purgeableWorkspaceDir(cfg)
	if !ok {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("could not read workspace %q: %w", dir, err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		names = append(names, e.Name())
		if dryRun {
			continue
		}
		if err := safeRemoveEntry(dir, e.Name()); err != nil {
			return names, fmt.Errorf("could not remove workspace entry %q: %w", e.Name(), err)
		}
	}
	sort.Strings(names)
	return names, nil
}

// purgeableWorkspaceDir resolves the workspace directory and proves it is safe to empty.
//
// @purpose Refuses every workspace path whose deletion would destroy something the gateway does not
// own: the filesystem root, a home directory, the workers tree (which holds credentials), or any
// path outside the configured workspaces root.
// @rationale The shared workspace is a legitimate place to delete files; `/var/lib/agy-gateway` and
// `~` are not. Because the path is operator-configurable, the check must not rely on the default.
// @preconditions cfg is non-nil.
// @param cfg Loaded configuration.
// @returns The cleaned absolute directory and true when it is safe; "" and false otherwise.
// @throws None; every rejection is a false return rather than an error so callers can treat an
// unusable configuration as "nothing to do".
// @invariants O(1); no filesystem mutation.
func purgeableWorkspaceDir(cfg *config.Config) (string, bool) {
	if cfg == nil {
		return "", false
	}
	dir := strings.TrimSpace(cfg.Storage.EffectiveWorkspaceDir())
	if dir == "" {
		return "", false
	}
	clean := filepath.Clean(dir)
	if !filepath.IsAbs(clean) || clean == string(filepath.Separator) {
		return "", false
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && filepath.Clean(home) == clean {
		return "", false
	}
	for _, forbidden := range []string{cfg.Storage.WorkersRootDir, cfg.Storage.BaseDataDir} {
		if forbidden == "" {
			continue
		}
		f := filepath.Clean(forbidden)
		if clean == f || strings.HasPrefix(clean, f+string(filepath.Separator)) {
			return "", false
		}
	}
	root := strings.TrimSpace(cfg.Storage.WorkspacesRootDir)
	if root == "" {
		return "", false
	}
	cleanRoot := filepath.Clean(root)
	if clean != cleanRoot && !strings.HasPrefix(clean, cleanRoot+string(filepath.Separator)) {
		return "", false
	}
	return clean, true
}
