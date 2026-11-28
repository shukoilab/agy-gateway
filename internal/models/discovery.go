package models

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

// ErrModelDiscoveryUnavailable reports that no credential could be borrowed to enumerate models.
//
// @purpose Lets the HTTP surfaces answer "the gateway knows the vault but could not enumerate models"
// instead of the misleading `model_not_found`, which implies the caller typed the wrong model id.
// @rationale `NeedsOnboarding` only fires when the vault is EMPTY, so a vault with accounts whose
// credentials all fail to export produced a bare 404 naming the model. Distinguishing the two turns a
// confusing typo hunt into an actionable operational report.
// @preconditions None.
// @param None.
// @returns None.
// @throws None.
// @invariants Immutable sentinel; match with errors.Is.
var ErrModelDiscoveryUnavailable = errors.New("could not enumerate models: no usable account credential")

// discoverModels fills discovered using exactly one borrowed credential.
//
// @purpose Implements D13: one credential is enough because every account yields the same catalog.
// @rationale Cheap candidates first (an existing worker HOME already holds a token file, so it costs
// nothing to try), then a scratch HOME built from the vault's own active account (~143 ms export plus
// ~2 s for `agy models`). Every candidate is tried so one broken credential cannot empty the catalog.
// @preconditions mr.mu is held by the caller.
// @param ctx Cancellation root for the subprocesses.
// @param discovered Output map, mutated in place.
// @returns ErrModelDiscoveryUnavailable when no candidate produced a catalog.
// @throws None directly.
// @invariants At most one `agy models` run per candidate that actually has a credential; no lock is
// taken on the manager; every scratch HOME is removed before returning.
func (mr *ModelRegistry) discoverModels(ctx context.Context, discovered map[string]*ModelInfo) error {
	// 1. Any worker HOME that already holds a token. Scanning the directory (not `0..WorkerCount-1`)
	// means a home left over from a previous, larger configuration is still usable.
	for _, home := range workerHomesWithToken(mr.cfg.Storage.WorkersRootDir) {
		if mr.runModelQuery(ctx, home, discovered) {
			return nil
		}
	}

	// 2. The gateway's own HOME, which is where `agy-auth` mirrors the active credential.
	if home := os.Getenv("HOME"); home != "" {
		if mr.runModelQuery(ctx, home, discovered) {
			return nil
		}
	}

	// 3. Borrow the vault's active account (else the first one) into a scratch HOME.
	if mr.authMgr != nil {
		accounts := mr.authMgr.GetAccounts()
		candidates := make([]string, 0, len(accounts))
		for _, acc := range accounts {
			if acc.ActiveInVault {
				candidates = append(candidates, acc.ID)
			}
		}
		for _, acc := range accounts {
			if !acc.ActiveInVault {
				candidates = append(candidates, acc.ID)
			}
		}
		for _, accountID := range candidates {
			var out []byte
			borrowErr := mr.authMgr.WithScratchHome(ctx, accountID, func(homeDir string) error {
				res, err := mr.authMgr.ExecAgy(ctx, homeDir, "models")
				if err != nil {
					return err
				}
				out = res
				return nil
			})
			if borrowErr != nil {
				mr.logger.Debug("could not borrow a credential for model discovery",
					"accountID", accountID, "err", borrowErr)
				continue
			}
			if mr.mergeModels(out, discovered) {
				return nil
			}
		}
	}

	return ErrModelDiscoveryUnavailable
}

// runModelQuery runs `agy models` in one existing HOME and merges the result.
//
// @purpose Isolates the "try a home, merge if it worked" step so candidate ordering stays readable.
// @rationale A missing token file is expected for most candidates; it is a silent skip, not a warning,
// because discovery is expected to succeed on the first usable candidate.
// @preconditions mr.mu is held.
// @param ctx Cancellation root.
// @param home Home directory to query.
// @param discovered Output map.
// @returns True when at least one model was merged.
// @throws None.
// @invariants One subprocess at most.
func (mr *ModelRegistry) runModelQuery(ctx context.Context, home string, discovered map[string]*ModelInfo) bool {
	out, err := mr.authMgr.ExecAgy(ctx, home, "models")
	if err != nil {
		mr.logger.Debug("failed to query agy models from home", "homeDir", home, "err", err)
		return false
	}
	return mr.mergeModels(out, discovered)
}

// mergeModels parses one payload into the catalog and reports whether it carried anything.
//
// @purpose Keeps the parse-and-merge rule identical for every candidate source.
// @rationale Duplicating it per source is how the two lists drifted apart before.
// @preconditions mr.mu is held.
// @param out Raw stdout of `agy models`.
// @param discovered Output map.
// @returns True when at least one model was added or merged.
// @throws None.
// @invariants O(models).
func (mr *ModelRegistry) mergeModels(out []byte, discovered map[string]*ModelInfo) bool {
	models := parseAgyModelsOutput(out)
	for _, m := range models {
		if existing, exists := discovered[m.ID]; exists {
			for _, acc := range m.AccountIDs {
				if !contains(existing.AccountIDs, acc) {
					existing.AccountIDs = append(existing.AccountIDs, acc)
				}
			}
			continue
		}
		discovered[m.ID] = m
	}
	return len(models) > 0
}

// workerHomesWithToken lists `wN` directories that currently hold a credential.
//
// @purpose Makes discovery independent of `worker_count`, which previously decided which homes were
// even considered.
// @rationale The directory name encodes the worker index; anything else at that level is ignored.
// @preconditions root may not exist.
// @param root Configured workers_root_dir.
// @returns Absolute home paths, sorted by numeric worker id.
// @throws None; an unreadable root yields an empty slice.
// @invariants O(entries).
func workerHomesWithToken(root string) []string {
	if root == "" {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	type home struct {
		id   int
		path string
	}
	var homes []home
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) < 2 || name[0] != 'w' {
			continue
		}
		id, convErr := strconv.Atoi(name[1:])
		if convErr != nil || id < 0 {
			continue
		}
		path := filepath.Join(root, name)
		tokenPath := filepath.Join(path, ".gemini", "antigravity-cli", "antigravity-oauth-token")
		if _, statErr := os.Stat(tokenPath); statErr != nil {
			continue
		}
		homes = append(homes, home{id: id, path: path})
	}
	sort.Slice(homes, func(i, j int) bool { return homes[i].id < homes[j].id })
	out := make([]string, 0, len(homes))
	for _, h := range homes {
		out = append(out, h.path)
	}
	return out
}
