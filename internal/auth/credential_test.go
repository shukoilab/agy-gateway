package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agy-gateway/internal/config"
)

// newCredentialTestManager builds a Manager whose credential export is scripted.
//
// @purpose Exercises credential materialisation without a vault or a real subprocess.
// @rationale The behaviour under test is identity validation, atomic installation, and rollback —
// none of which needs `agy-auth`. The seam keeps the real installation code path under test.
// @preconditions accounts are injected through SetAccountsForTest.
// @param t Standard Go test handle.
// @param export Replacement export implementation.
// @returns A manager whose WorkersRootDir points into a temporary directory.
// @throws None; fails the test if the temp directory cannot be created.
// @invariants Spawns no process.
func newCredentialTestManager(t *testing.T, export exportAccountFunc) (*Manager, string) {
	t.Helper()

	root := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Storage.WorkersRootDir = root
	cfg.Accounts.CredentialOpTimeout = 5 * time.Second

	mgr := NewManager(cfg, nil)
	mgr.exportAccount = export
	return mgr, root
}

// writeExport emits one credential file into dir.
//
// @purpose Simulates `agy-auth export --dir <dir> <account>`.
// @rationale Keeping the fixture shaped exactly like the real export means the validation path is
// tested against realistic input rather than a convenient struct.
// @preconditions dir exists.
// @param dir Staging directory handed to the export.
// @param email Identity the exported credential should claim.
// @param accessToken Access token value; empty simulates an incomplete credential.
// @param refreshToken Refresh token value; empty simulates an incomplete credential.
// @param files Number of files to emit, so whole-vault behaviour can be simulated.
// @returns An export implementation.
// @throws None.
// @invariants Writes synchronously.
func writeExport(t *testing.T, email, accessToken, refreshToken string, files int) exportAccountFunc {
	return func(_ context.Context, dir, _ string) error {
		payload := map[string]any{
			"token": map[string]any{
				"access_token":  accessToken,
				"token_type":    "Bearer",
				"refresh_token": refreshToken,
				"expiry":        "2026-11-25T20:00:00Z",
			},
			"auth_method":    "consumer",
			"id_token":       "header.payload.signature",
			"id_token_email": email,
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Errorf("marshal fixture: %v", err)
			return err
		}
		for i := 0; i < files; i++ {
			name := filepath.Join(dir, "exported.json")
			if files > 1 {
				name = filepath.Join(dir, "exported-"+string(rune('a'+i))+".json")
			}
			if err := os.WriteFile(name, raw, 0600); err != nil {
				return err
			}
		}
		return nil
	}
}

// readWorkerToken returns the installed credential for a worker.
//
// @purpose Reads back exactly what agy would consume.
// @rationale Verifying the byte content, not just existence, is what proves the identity fix actually
// reached disk.
// @preconditions root is the workers root directory.
// @param t Standard Go test handle.
// @param root Workers root directory.
// @param workerID Worker index.
// @returns The raw installed credential bytes.
// @throws None; fails the test when the file is missing or unreadable.
// @invariants Read-only.
func readWorkerToken(t *testing.T, root string, workerID int) []byte {
	t.Helper()
	path := filepath.Join(root, fmt.Sprintf("w%d", workerID), ".gemini", "antigravity-cli", "antigravity-oauth-token")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed credential: %v", err)
	}
	return raw
}

// TestMaterializeWorkerCredential_InstallsCorrectAccount is the FR-02 regression test.
//
// @purpose Proves each worker receives the credential of the account it was assigned, not a shared
// global token.
// @rationale Defect A1 made every worker authenticate as one account because the first token path
// candidate never existed. This test fails if materialisation ever falls back to a shared source.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no subprocess.
func TestMaterializeWorkerCredential_InstallsCorrectAccount(t *testing.T) {
	mgr, root := newCredentialTestManager(t, writeExport(t, "bob@example.com", "access-bob", "refresh-bob", 1))
	mgr.SetAccountsForTest([]Account{
		{ID: "acct_alice", Email: "alice@example.com", Enabled: true, Status: StatusHealthy},
		{ID: "acct_bob", Email: "bob@example.com", Enabled: true, Status: StatusHealthy},
	})

	if _, err := mgr.MaterializeWorkerCredential(context.Background(), 2, "acct_bob"); err != nil {
		t.Fatalf("MaterializeWorkerCredential: %v", err)
	}

	var installed exportedCredential
	if err := json.Unmarshal(readWorkerToken(t, root, 2), &installed); err != nil {
		t.Fatalf("installed credential is not valid JSON: %v", err)
	}
	if installed.IDTokenEmail != "bob@example.com" {
		t.Errorf("installed email = %q, want bob@example.com (the chosen account, not a shared token)",
			installed.IDTokenEmail)
	}
	if installed.Token.AccessToken != "access-bob" ||
		installed.Token.RefreshToken != "refresh-bob" {
		t.Errorf("installed tokens do not match the exported account: %+v", installed.Token)
	}

	info, err := os.Stat(filepath.Join(root, "w2", ".gemini", "antigravity-cli", "antigravity-oauth-token"))
	if err != nil {
		t.Fatalf("stat installed credential: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("installed credential permissions = %04o, want 0600", perm)
	}
}

// TestMaterializeWorkerCredential_RejectsIdentityMismatch covers the wrong-account guard.
//
// @purpose Prevents installing a well-formed credential for an account other than the requested one.
// @rationale A mismatched credential would leave the pool's bookkeeping and reality disagreeing again,
// which is exactly the class of defect this task removes.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no subprocess.
func TestMaterializeWorkerCredential_RejectsIdentityMismatch(t *testing.T) {
	// The vault says bob; the export hands back alice.
	mgr, root := newCredentialTestManager(t, writeExport(t, "alice@example.com", "a", "r", 1))
	mgr.SetAccountsForTest([]Account{
		{ID: "acct_bob", Email: "bob@example.com", Enabled: true, Status: StatusHealthy},
	})

	_, err := mgr.MaterializeWorkerCredential(context.Background(), 0, "acct_bob")
	if !errors.Is(err, ErrCredentialIdentityMatch) {
		t.Fatalf("expected ErrCredentialIdentityMatch, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "w0")); statErr == nil {
		// The credential must not exist; a stray token file would be worse than a clean failure.
		if _, readErr := os.Stat(filepath.Join(root, "w0", ".gemini", "antigravity-cli", "antigravity-oauth-token")); readErr == nil {
			t.Fatal("a mismatched credential must not be installed")
		}
	}
}

// TestMaterializeWorkerCredential_RejectsIncompletePayload covers the missing-token guard.
//
// @purpose Refuses a credential agy cannot use, instead of installing it and failing at turn time.
// @rationale An access token without a refresh token works until it expires and then strands the
// worker with no way to recover; failing at install time is the only place the error is actionable.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no subprocess.
func TestMaterializeWorkerCredential_RejectsIncompletePayload(t *testing.T) {
	for _, tc := range []struct{ name, access, refresh string }{
		{"missing refresh token", "access", ""},
		{"missing access token", "", "refresh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr, _ := newCredentialTestManager(t, writeExport(t, "bob@example.com", tc.access, tc.refresh, 1))
			mgr.SetAccountsForTest([]Account{
				{ID: "acct_bob", Email: "bob@example.com", Enabled: true, Status: StatusHealthy},
			})

			_, err := mgr.MaterializeWorkerCredential(context.Background(), 0, "acct_bob")
			if !errors.Is(err, ErrCredentialPayloadInvalid) {
				t.Fatalf("expected ErrCredentialPayloadInvalid, got %v", err)
			}
		})
	}
}

// TestMaterializeWorkerCredential_RejectsWholeVaultExport covers the file-count guard.
//
// @purpose Guarantees a per-account export is what gets installed.
// @rationale If the account selector were ever ignored, silently installing one of several files
// would recreate the shared-credential defect without any signal.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no subprocess.
func TestMaterializeWorkerCredential_RejectsWholeVaultExport(t *testing.T) {
	mgr, _ := newCredentialTestManager(t, writeExport(t, "bob@example.com", "a", "r", 3))
	mgr.SetAccountsForTest([]Account{
		{ID: "acct_bob", Email: "bob@example.com", Enabled: true, Status: StatusHealthy},
	})

	_, err := mgr.MaterializeWorkerCredential(context.Background(), 0, "acct_bob")
	if !errors.Is(err, ErrCredentialPayloadInvalid) {
		t.Fatalf("expected ErrCredentialPayloadInvalid for a 3-file export, got %v", err)
	}
}

// TestMaterializeWorkerCredential_PreservesPreviousOnFailure covers rollback.
//
// @purpose Keeps a worker usable when a rotation attempt fails mid-flight.
// @rationale A failed rotation that wiped the existing credential would turn a recoverable quota event
// into a dead worker; the previous token must survive every failure path.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no subprocess.
func TestMaterializeWorkerCredential_PreservesPreviousOnFailure(t *testing.T) {
	mgr, root := newCredentialTestManager(t, writeExport(t, "bob@example.com", "access-bob", "refresh-bob", 1))
	mgr.SetAccountsForTest([]Account{
		{ID: "acct_bob", Email: "bob@example.com", Enabled: true, Status: StatusHealthy},
	})

	if _, err := mgr.MaterializeWorkerCredential(context.Background(), 0, "acct_bob"); err != nil {
		t.Fatalf("initial materialisation: %v", err)
	}
	before := readWorkerToken(t, root, 0)

	// Simulate the next export failing outright.
	mgr.exportAccount = func(context.Context, string, string) error {
		return ErrCredentialExportFailed
	}
	if _, err := mgr.MaterializeWorkerCredential(context.Background(), 0, "acct_bob"); err == nil {
		t.Fatal("expected the failing export to surface an error")
	}

	after := readWorkerToken(t, root, 0)
	if string(before) != string(after) {
		t.Fatal("a failed rotation must leave the previous credential untouched")
	}
}

// TestMaterializeWorkerCredential_UnknownAccount covers the not-found path.
//
// @purpose Turns an unknown account id into a typed error rather than an empty credential.
// @rationale Rotation derives the id from live quota data; a mismatch means the vault changed
// underneath, which must be reported instead of papered over.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no subprocess.
func TestMaterializeWorkerCredential_UnknownAccount(t *testing.T) {
	mgr, _ := newCredentialTestManager(t, writeExport(t, "bob@example.com", "a", "r", 1))
	mgr.SetAccountsForTest([]Account{
		{ID: "acct_bob", Email: "bob@example.com", Enabled: true, Status: StatusHealthy},
	})

	_, err := mgr.MaterializeWorkerCredential(context.Background(), 0, "acct_missing")
	if !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("expected ErrAccountNotFound, got %v", err)
	}
}

// TestMaterializeWorkerCredential_LeavesNoStagingDirectory verifies the temporary export is cleaned up.
//
// @purpose Prevents decrypted credentials accumulating in the system temp directory.
// @rationale The staging directory holds a plaintext refresh token; leaking one per rotation would
// quietly leave usable credentials on disk outside the worker HOME.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic; no subprocess.
func TestMaterializeWorkerCredential_LeavesNoStagingDirectory(t *testing.T) {
	var stagingDir string
	mgr, _ := newCredentialTestManager(t, func(ctx context.Context, dir, accountID string) error {
		stagingDir = dir
		return writeExport(t, "bob@example.com", "a", "r", 1)(ctx, dir, accountID)
	})
	mgr.SetAccountsForTest([]Account{
		{ID: "acct_bob", Email: "bob@example.com", Enabled: true, Status: StatusHealthy},
	})

	if _, err := mgr.MaterializeWorkerCredential(context.Background(), 0, "acct_bob"); err != nil {
		t.Fatalf("MaterializeWorkerCredential: %v", err)
	}
	if stagingDir == "" {
		t.Fatal("export was never invoked; the test cannot verify cleanup")
	}
	if _, err := os.Stat(stagingDir); !os.IsNotExist(err) {
		t.Fatalf("staging directory %s still exists after materialisation (err=%v)", stagingDir, err)
	}
}

// TestDefaultExportAccount_TimeoutIsTransient pins that an infrastructure hiccup never expires an
// account.
//
// @purpose Keeps EC-16 (export hangs) from being reported as a dead credential.
// @rationale The reviewer's probe showed every materialise error was marked AUTH_EXPIRED, so a single
// timeout removed the only account permanently. A timeout says nothing about the stored credential.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Bounded by the configured timeout (50ms); spawns one process group, reaped on timeout.
func TestDefaultExportAccount_TimeoutIsTransient(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Storage.WorkersRootDir = t.TempDir()
	// A binary that blocks forever: the timeout, not the child, decides the outcome.
	sleeper := filepath.Join(t.TempDir(), "slow-agy-auth")
	if err := os.WriteFile(sleeper, []byte("#!/bin/sh\nsleep 30\n"), 0755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	cfg.Binaries.AgyAuthPath = sleeper
	cfg.Accounts.CredentialOpTimeout = 50 * time.Millisecond
	mgr := NewManager(cfg, nil)

	err := mgr.defaultExportAccount(context.Background(), t.TempDir(), "acct_any")
	if !errors.Is(err, ErrCredentialTransient) {
		t.Fatalf("expected ErrCredentialTransient on timeout, got %v", err)
	}
	if errors.Is(err, ErrCredentialExportFailed) {
		t.Fatalf("a timeout must not be classified as a permanent export failure: %v", err)
	}
}

// TestDefaultExportAccount_CancelledContextIsTransient pins EC-19 (shutdown mid-materialise).
//
// @purpose A shutdown must not leave the account marked expired for the next boot.
// @rationale ctx cancellation is not evidence about the credential; only the process failed.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Terminates immediately; no goroutine or child left behind.
func TestDefaultExportAccount_CancelledContextIsTransient(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Storage.WorkersRootDir = t.TempDir()
	sleeper := filepath.Join(t.TempDir(), "slow-agy-auth")
	if err := os.WriteFile(sleeper, []byte("#!/bin/sh\nsleep 30\n"), 0755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	cfg.Binaries.AgyAuthPath = sleeper
	cfg.Accounts.CredentialOpTimeout = 5 * time.Second
	mgr := NewManager(cfg, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	err := mgr.defaultExportAccount(ctx, t.TempDir(), "acct_any")
	if !errors.Is(err, ErrCredentialTransient) {
		t.Fatalf("expected ErrCredentialTransient on cancellation, got %v", err)
	}
}
