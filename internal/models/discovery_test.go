package models

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"agy-gateway/internal/auth"
	"agy-gateway/internal/config"
)

// agyModelsPayload is the exact stdout shape `agy models` produces (tab separated, with a header line).
//
// @purpose Keeps discovery fixtures identical to what the real CLI emits.
// @rationale The parser keys off the `Fetching` prefix and tab separation, so a different fixture shape
// would not prove the production path.
// @preconditions None.
// @param None.
// @returns Raw bytes.
// @throws None.
// @invariants Deterministic.
func agyModelsPayload() []byte {
	return []byte("Fetching available models...\n" +
		"gemini-3.8-flash-high\tGemini 3.8 Flash (High)\n" +
		"gemini-3.8-flash-medium\tGemini 3.8 Flash (Medium)\n" +
		"claude-sonnet-4-6\tClaude Sonnet 4.6 (Thinking)\n")
}

// TestSyncModels_UsesOneExistingWorkerToken is S22.
//
// @purpose Proves one credential is enough and that a worker HOME holding a token is preferred, so no
// export is attempted.
// @rationale Measured: a Free and a Pro account return a byte-identical catalog, so per-account
// discovery would cost ~2 s per account for nothing.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants One stubbed subprocess at most.
func TestSyncModels_UsesOneExistingWorkerToken(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Storage.WorkersRootDir = t.TempDir()
	cfg.Storage.VaultXDGConfigDir = t.TempDir()

	// w3 exists even though WorkerCount is 1: discovery scans directories, not the configured count.
	home := filepath.Join(cfg.Storage.WorkersRootDir, "w3", ".gemini", "antigravity-cli")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "antigravity-oauth-token"), []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	mgr := auth.NewManager(cfg, nil)
	mgr.SetAccountsForTest([]auth.Account{
		{ID: "acct_free", Email: "free@example.com", Enabled: true},
		{ID: "acct_pro", Email: "pro@example.com", Enabled: true},
	})
	calls := 0
	mgr.SetAgyForTest(func(_ context.Context, homeDir string, args []string) ([]byte, error) {
		calls++
		if len(args) == 0 || args[0] != "models" {
			t.Errorf("unexpected agy args: %v", args)
		}
		want := filepath.Join(cfg.Storage.WorkersRootDir, "w3")
		if homeDir != want {
			t.Errorf("discovery ran in %q, want the existing worker home %q", homeDir, want)
		}
		return agyModelsPayload(), nil
	})
	// Any export attempt would be a defect: the worker home already holds a credential.
	mgr.SetExportForTest(func(context.Context, string, string) error {
		t.Error("discovery must not export when a worker home already holds a token")
		return errors.New("unexpected export")
	})

	reg := NewModelRegistry(cfg, mgr, nil)
	if err := reg.SyncModels(context.Background()); err != nil {
		t.Fatalf("SyncModels: %v", err)
	}
	if calls != 1 {
		t.Fatalf("agy models calls = %d, want exactly 1", calls)
	}
	if reg.CatalogEmpty() {
		t.Fatal("catalog must be populated")
	}
	if !reg.HasAccounts() {
		t.Fatal("HasAccounts must be true with a non-empty vault")
	}
}

// TestSyncModels_BorrowsActiveAccountWhenNoWorkerHome is S23.
//
// @purpose Proves an account added with `worker_count=1` and no worker HOME still contributes its
// models.
// @rationale The previous implementation scanned `w0..w{WorkerCount-1}` plus `$HOME`, so a freshly
// onboarded account could be invisible while the catalog silently reflected an older credential.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants One stubbed subprocess.
func TestSyncModels_BorrowsActiveAccountWhenNoWorkerHome(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Storage.WorkersRootDir = t.TempDir()
	cfg.Storage.VaultXDGConfigDir = t.TempDir()
	// Keep discovery away from the operator's real HOME credential.
	t.Setenv("HOME", t.TempDir())

	mgr := auth.NewManager(cfg, nil)
	mgr.SetAccountsForTest([]auth.Account{
		{ID: "acct_new", Email: "new@example.com", Enabled: true, ActiveInVault: true},
	})
	var borrowed string
	mgr.SetAgyForTest(func(_ context.Context, homeDir string, args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "models" {
			// Emulate the real CLI: no credential in this HOME means no answer, which is what makes
			// discovery fall through to the next candidate.
			tokenPath := filepath.Join(homeDir, ".gemini", "antigravity-cli", "antigravity-oauth-token")
			if _, statErr := os.Stat(tokenPath); statErr != nil {
				return nil, errors.New("Error: Please sign in to view available models")
			}
			borrowed = homeDir
			return agyModelsPayload(), nil
		}
		return nil, errors.New("unexpected call")
	})
	mgr.SetExportForTest(func(_ context.Context, dir, accountID string) error {
		if accountID != "acct_new" {
			t.Errorf("exported %q, want the active account", accountID)
		}
		payload := []byte(`{"token":{"access_token":"a","refresh_token":"r"},"id_token_email":"new@example.com"}`)
		return os.WriteFile(filepath.Join(dir, "exported.json"), payload, 0o600)
	})

	reg := NewModelRegistry(cfg, mgr, nil)
	if err := reg.SyncModels(context.Background()); err != nil {
		t.Fatalf("SyncModels: %v", err)
	}
	if borrowed == "" {
		t.Fatal("discovery must borrow the vault account when no worker home holds a token")
	}
	if reg.CatalogEmpty() {
		t.Fatal("catalog must be populated from the borrowed credential")
	}
	if _, err := os.Stat(borrowed); !os.IsNotExist(err) {
		t.Fatalf("scratch HOME must be removed, stat err = %v", err)
	}
}

// TestSyncModels_FailedDiscoveryKeepsPreviousCatalog is S25.
//
// @purpose Proves one failing credential cannot empty a working catalog.
// @rationale An empty catalog turns every valid request into model_not_found, which looks like a client
// typo hunt instead of an operational fault.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Two stubbed subprocesses across two syncs.
func TestSyncModels_FailedDiscoveryKeepsPreviousCatalog(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Storage.WorkersRootDir = t.TempDir()
	cfg.Storage.VaultXDGConfigDir = t.TempDir()
	t.Setenv("HOME", t.TempDir())

	mgr := auth.NewManager(cfg, nil)
	mgr.SetAccountsForTest([]auth.Account{{ID: "acct_a", Email: "a@example.com", Enabled: true}})

	fail := false
	mgr.SetAgyForTest(func(_ context.Context, homeDir string, args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "models" {
			// The real CLI refuses to list models without a credential, so discovery must only succeed
			// on a HOME that actually holds one (here, the borrowed vault credential).
			if _, statErr := os.Stat(filepath.Join(homeDir, ".gemini", "antigravity-cli", "antigravity-oauth-token")); statErr != nil {
				return nil, errors.New("Error: Please sign in to view available models")
			}
			if fail {
				return nil, errors.New("agy models failed: eligibility check failed")
			}
			return agyModelsPayload(), nil
		}
		return nil, errors.New("unexpected call")
	})
	mgr.SetExportForTest(func(_ context.Context, dir, _ string) error {
		payload := []byte(`{"token":{"access_token":"a","refresh_token":"r"},"id_token_email":"a@example.com"}`)
		return os.WriteFile(filepath.Join(dir, "exported.json"), payload, 0o600)
	})

	reg := NewModelRegistry(cfg, mgr, nil)
	if err := reg.SyncModels(context.Background()); err != nil {
		t.Fatalf("first SyncModels: %v", err)
	}
	before := len(reg.ListModels())
	if before == 0 {
		t.Fatal("first discovery must populate the catalog")
	}

	fail = true
	if err := reg.SyncModels(context.Background()); err != nil {
		t.Fatalf("SyncModels must not fail the caller: %v", err)
	}
	if got := len(reg.ListModels()); got != before {
		t.Fatalf("failed discovery changed the catalog: %d -> %d", before, got)
	}
}

// TestDiscoveryUnavailable_ReportsTypedSentinel is S26's model-layer half.
//
// @purpose Proves "accounts exist but nothing could be enumerated" is a distinct reported state.
// @rationale Handlers need to answer an actionable 503 instead of a bare `model_not_found`.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants No subprocess succeeds.
func TestDiscoveryUnavailable_ReportsTypedSentinel(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Storage.WorkersRootDir = t.TempDir()
	cfg.Storage.VaultXDGConfigDir = t.TempDir()
	t.Setenv("HOME", t.TempDir())

	mgr := auth.NewManager(cfg, nil)
	mgr.SetAccountsForTest([]auth.Account{{ID: "acct_a", Email: "a@example.com", Enabled: true}})
	mgr.SetAgyForTest(func(context.Context, string, []string) ([]byte, error) {
		return nil, errors.New("boom")
	})
	mgr.SetExportForTest(func(context.Context, string, string) error {
		return errors.New("credential export failed")
	})

	reg := NewModelRegistry(cfg, mgr, nil)
	if err := reg.SyncModels(context.Background()); err != nil {
		t.Fatalf("SyncModels must not fail the caller: %v", err)
	}
	if !reg.CatalogEmpty() {
		t.Fatal("catalog must stay empty when nothing could be enumerated")
	}
	// The handler distinguishes these two booleans to pick its error code.
	if !reg.HasAccounts() {
		t.Fatal("HasAccounts must be true so the handler reports discovery failure, not a typo")
	}

	// And the sentinel is what discovery actually produced.
	disc := make(map[string]*ModelInfo)
	err := reg.discoverModels(context.Background(), disc)
	if !errors.Is(err, ErrModelDiscoveryUnavailable) {
		t.Fatalf("discoverModels = %v, want ErrModelDiscoveryUnavailable", err)
	}
}

// TestParseAgyModelsOutput_NoPaidTierHeuristic is S11.
//
// @purpose Proves a paid-looking model id no longer carries a fabricated tier requirement.
// @rationale A Free account was measured serving `claude-opus-4-6-thinking` successfully, so the old
// `strings.Contains(id, "opus")` rule would have refused working traffic once Tier was populated.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Deterministic.
func TestParseAgyModelsOutput_NoPaidTierHeuristic(t *testing.T) {
	models := parseAgyModelsOutput([]byte("claude-opus-4-6-thinking\tClaude Opus 4.6 (Thinking)\n"))
	if len(models) != 1 {
		t.Fatalf("parsed %d models, want 1", len(models))
	}
	if models[0].RequiresPaidTier {
		t.Fatal("a model id must not imply a paid-tier requirement; the upstream enforces entitlement")
	}
}
