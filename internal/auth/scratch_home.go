package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// WithScratchHome materialises one account's credential into a throwaway HOME and runs fn there.
//
// @purpose Gives every caller that needs to run `agy` as a specific account ONE implementation of
// "borrow a credential for a subprocess", so quota fallback and model discovery cannot drift apart in
// their credential handling, file modes, or cleanup guarantees.
// @rationale `agy` reads its credential from `$HOME/.gemini/antigravity-cli/antigravity-oauth-token`,
// so running it as an account that no worker currently holds requires a HOME that looks like a signed-
// in CLI profile. The exported credential IS plaintext (upstream documents this), which makes the
// directory's lifetime and mode security-relevant: the token is written 0600 inside a 0700 directory
// and the entire tree is removed on every exit path, including cancellation. Never pass the credential
// on argv: only `agy-auth export --dir` writes it, and the copy is made by file read.
// @preconditions accountID exists in the vault.
// @param ctx Cancellation root; propagated to the export and to fn.
// @param accountID Account whose credential should be borrowed.
// @param fn Receives the scratch HOME. Its error is returned unchanged; the directory is already gone
// by then, so the caller must not retain paths into it.
// @returns A wrapped error from export/installation, or fn's own error.
// @throws None directly; all failures are returned.
// @invariants Exactly one export subprocess; O(1) files; the directory is always removed; the token
// file never appears in an error message or log.
func (m *Manager) WithScratchHome(ctx context.Context, accountID string, fn func(homeDir string) error) error {
	if accountID == "" {
		return fmt.Errorf("scratch home requires an account id")
	}

	scratch, err := os.MkdirTemp("", "agy-scratch-")
	if err != nil {
		return fmt.Errorf("failed to create scratch home: %w", err)
	}
	// The credential is plaintext while it exists, so the tree is never group/other accessible.
	if chmodErr := os.Chmod(scratch, 0o700); chmodErr != nil {
		_ = os.RemoveAll(scratch)
		return fmt.Errorf("failed to restrict scratch home: %w", chmodErr)
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	stage, err := os.MkdirTemp("", "agy-scratch-export-")
	if err != nil {
		return fmt.Errorf("failed to create credential staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()

	export := m.exportAccount
	if export == nil {
		export = m.defaultExportAccount
	}
	if err := export(ctx, stage, accountID); err != nil {
		return err
	}
	payload, err := readSingleCredential(stage)
	if err != nil {
		return err
	}
	if payload.Token.AccessToken == "" || payload.Token.RefreshToken == "" {
		return fmt.Errorf("%w: account %s carries no usable access/refresh token",
			ErrCredentialPayloadInvalid, accountID)
	}

	// `agy` only ever reads this one file, so the scratch HOME needs nothing else — measured: no
	// `jetski_state.pbtxt`, `installation_id`, or project cache is required for a `/usage` or `models`.
	tokenDir := filepath.Join(scratch, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(tokenDir, 0o700); err != nil {
		return fmt.Errorf("failed to create scratch token directory: %w", err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to encode credential for account %s: %w", accountID, err)
	}
	tokenPath := filepath.Join(tokenDir, "antigravity-oauth-token")
	if err := atomicWriteFile(tokenPath, raw, 0o600); err != nil {
		return fmt.Errorf("failed to install scratch credential: %w", err)
	}

	return fn(scratch)
}
