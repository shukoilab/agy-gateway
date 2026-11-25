package pool

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// validateWorkspaceRoot proves a worker working directory is a dedicated, disposable scratch
// directory before any recursive deletion is allowed to touch it.
//
// @purpose Bounds the blast radius of CleanWorkspace so a misconfigured root cannot wipe a real
// repository, a home directory, or the filesystem root.
// @rationale CleanWorkspace removes every entry under w.Cwd except .git on each turn. That is safe
// only when w.Cwd is a subdirectory of storage.workspaces_root_dir that this process created.
// @preconditions root is the configured workspaces root; cwd is the candidate directory.
// @param root Absolute path of storage.workspaces_root_dir.
// @param cwd Absolute path of the per-worker workspace.
// @returns nil when deletion is safe, or a typed error explaining the refusal.
// @throws None; all failures are returned as values.
// @invariants O(n) in path component count; no filesystem mutation; safe for concurrent use.
func validateWorkspaceRoot(root, cwd string) error {
	if cwd == "" {
		return fmt.Errorf("workspace directory is empty")
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return fmt.Errorf("workspace path %q cannot be resolved: %w", cwd, err)
	}
	cleanCwd := filepath.Clean(absCwd)

	// Refuse locations whose recursive deletion is catastrophic by definition.
	if cleanCwd == string(filepath.Separator) {
		return fmt.Errorf("workspace %q refuses the filesystem root", cleanCwd)
	}
	if home, herr := os.UserHomeDir(); herr == nil && home != "" {
		if filepath.Clean(home) == cleanCwd {
			return fmt.Errorf("workspace %q refuses the home directory", cleanCwd)
		}
	}

	if root == "" {
		return fmt.Errorf("storage.workspaces_root_dir is not configured; refusing to clean %q", cleanCwd)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("workspaces root %q cannot be resolved: %w", root, err)
	}
	cleanRoot := filepath.Clean(absRoot)
	if cleanRoot == cleanCwd {
		return fmt.Errorf("workspace %q equals the workspaces root; refusing to delete its contents", cleanCwd)
	}

	rel, err := filepath.Rel(cleanRoot, cleanCwd)
	if err != nil {
		return fmt.Errorf("workspace %q is not comparable to root %q: %w", cleanCwd, cleanRoot, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("workspace %q escapes workspaces root %q", cleanCwd, cleanRoot)
	}

	info, err := os.Lstat(cleanCwd)
	if err != nil {
		return fmt.Errorf("workspace %q is not accessible: %w", cleanCwd, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("workspace %q is a symlink; refusing to follow it for deletion", cleanCwd)
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace %q is not a directory", cleanCwd)
	}
	return nil
}

// safeRemoveEntry deletes one direct child of a validated workspace root without traversing
// symlinks that point outside it.
//
// @purpose Removes a single workspace entry while guaranteeing the deletion cannot escape the root
// through a symlinked child.
// @rationale os.RemoveAll follows nothing on the final component but recurses into directories;
// resolving the entry first makes the escape check explicit and auditable.
// @preconditions root and name come from a directory listing of an already validated root.
// @param root Validated absolute workspace root.
// @param name Base name of the entry to remove.
// @returns nil on success or when the entry is already gone.
// @throws None; errors are returned.
// @invariants O(1) path work; never follows a symlink target outside root.
func safeRemoveEntry(root, name string) error {
	entryPath := filepath.Join(root, name)
	info, err := os.Lstat(entryPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		// Remove the link itself; never recurse into whatever it points at.
		return os.Remove(entryPath)
	}
	return os.RemoveAll(entryPath)
}
