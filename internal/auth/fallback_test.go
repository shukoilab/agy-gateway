package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agy-gateway/internal/config"
)

// freeQuotaEnvelope is the payload shape `agy -p /usage --output-format json` returns for an account
// whose quota `agy-auth usage` refuses to read.
//
// @purpose Gives the fallback tests the exact wire shape the production decoder consumes.
// @rationale The fallback's job is to decode this envelope; a synthetic struct would not prove the
// `command.data.groups` path is right.
// @preconditions None.
// @param None.
// @returns Raw JSON bytes.
// @throws None.
// @invariants Deterministic.
func freeQuotaEnvelope() []byte {
	return []byte(`{"command":{"name":"usage","data":{"groups":[
		{"name":"Gemini Models","buckets":[{"id":"gemini-weekly","window":"weekly","remaining_fraction":0.71,"reset_time":"2026-12-05T00:00:00Z"}]},
		{"name":"Claude and GPT models","buckets":[{"id":"3p-weekly","window":"weekly","remaining_fraction":0.95,"reset_time":"2026-12-05T00:00:00Z"}]}
	]}}}`)
}

// TestQuotaPoller_FreeAccountQuotaRecoveredViaAgy is S21 + S21b.
//
// @purpose Proves an account that `agy-auth usage` answers `forbidden` for still ends up with real
// quota, so it can be cooled and rotated away from.
// @rationale Measured upstream: `agy-auth usage` reports `forbidden` for a Free account whose quota
// `agy -p /usage` answers in full (71% Gemini, 95% Claude/GPT). Without the fallback the account stayed
// `QuotasKnown=false` forever, ranking as usable-unproven and never cooling.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants No real subprocess: both the usage fetch and the `agy` call are stubbed.
func TestQuotaPoller_FreeAccountQuotaRecoveredViaAgy(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Accounts.MinRemainingQuotaPct = 5
	cfg.Accounts.AutoRotateEnabled = true
	cfg.Storage.WorkersRootDir = t.TempDir()

	mgr := newQuotaTestManager(t, cfg, map[string]AccountUsage{
		"acct_free": {
			AccountID:  "acct_free",
			Email:      "free@example.com",
			UsageError: "forbidden",
		},
	}, nil)
	mgr.SetAccountsForTest([]Account{{
		ID: "acct_free", Email: "free@example.com", Enabled: true, Status: StatusHealthy,
		Quotas: map[string]QuotaGroup{}, FamilyCooldowns: map[string]time.Time{},
	}})

	var agyHomes []string
	mgr.agyForTest = func(_ context.Context, homeDir string, args []string) ([]byte, error) {
		if len(args) == 0 || args[0] != "-p" {
			t.Errorf("unexpected agy invocation: %v", args)
		}
		agyHomes = append(agyHomes, homeDir)
		// S21b: the scratch HOME must actually hold the borrowed credential, mode 0600.
		tokenPath := filepath.Join(homeDir, ".gemini", "antigravity-cli", "antigravity-oauth-token")
		info, statErr := os.Stat(tokenPath)
		if statErr != nil {
			t.Errorf("scratch HOME holds no credential: %v", statErr)
		} else if info.Mode().Perm() != 0o600 {
			t.Errorf("scratch credential mode = %o, want 600", info.Mode().Perm())
		}
		return freeQuotaEnvelope(), nil
	}
	mgr.exportAccount = writeExport(t, "free@example.com", "access", "refresh", 1)

	NewQuotaPoller(mgr, cfg, nil).Poll(context.Background())

	if len(agyHomes) != 1 {
		t.Fatalf("expected exactly one agy /usage fallback, got %d", len(agyHomes))
	}
	// The scratch HOME is removed on every path, so the borrowed credential cannot outlive the poll.
	if _, err := os.Stat(agyHomes[0]); !os.IsNotExist(err) {
		t.Fatalf("scratch HOME was not removed, stat err = %v", err)
	}

	accounts := mgr.GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("account set = %d, want 1", len(accounts))
	}
	acc := accounts[0]
	if !acc.QuotasKnown {
		t.Fatal("quota must be marked known after a successful agy fallback")
	}
	if acc.UsageError != "" {
		t.Fatalf("usage error must be cleared after recovery, got %q", acc.UsageError)
	}
	if got := acc.Quotas["gemini-weekly"].RemainingPct; got < 70 || got > 72 {
		t.Fatalf("gemini-weekly remaining = %v, want ~71", got)
	}
	if got := acc.Quotas["3p-weekly"].RemainingPct; got < 94 || got > 96 {
		t.Fatalf("3p-weekly remaining = %v, want ~95", got)
	}
}

// TestQuotaPoller_CredentialFaultsSpawnNoFallback is S34.
//
// @purpose Proves the fallback is confined to the error classes `agy` can actually improve on.
// @rationale `missing_auth`, `auth` and `refresh_failed` describe an unusable token; a second
// subprocess would fail identically and only add latency, while `forbidden`/`unsupported`/
// `invalid_response` describe a tool that cannot see the quota.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants No subprocess at all.
func TestQuotaPoller_CredentialFaultsSpawnNoFallback(t *testing.T) {
	for _, code := range []string{"missing_auth", "auth", "refresh_failed", "timeout", "network", "http_500"} {
		t.Run(code, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.Storage.WorkersRootDir = t.TempDir()

			mgr := newQuotaTestManager(t, cfg, map[string]AccountUsage{
				"acct_x": {AccountID: "acct_x", Email: "x@example.com", UsageError: code},
			}, nil)
			mgr.SetAccountsForTest([]Account{{
				ID: "acct_x", Email: "x@example.com", Enabled: true, Status: StatusHealthy,
				Quotas: map[string]QuotaGroup{}, FamilyCooldowns: map[string]time.Time{},
			}})
			called := 0
			mgr.agyForTest = func(context.Context, string, []string) ([]byte, error) {
				called++
				return freeQuotaEnvelope(), nil
			}
			mgr.exportAccount = writeExport(t, "x@example.com", "a", "r", 1)

			NewQuotaPoller(mgr, cfg, nil).Poll(context.Background())

			if called != 0 {
				t.Fatalf("usage_error %q must not spawn an agy fallback, got %d calls", code, called)
			}

			acc := mgr.GetAccounts()[0]
			if acc.QuotasKnown {
				t.Fatal("a credential fault must leave the quota unknown")
			}
			if acc.UsageError != code {
				t.Fatalf("usage error = %q, want %q preserved for the operator", acc.UsageError, code)
			}
		})
	}
}

// TestQuotaPoller_FallbackFailureLeavesUnknown pins the honest failure direction.
//
// @purpose Proves a failing fallback does not invent quota data.
// @rationale The fallback is best-effort; if `agy` also fails the account must stay unknown so ranking
// demotes it, rather than inheriting the empty bucket map as "unlimited".
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants One stubbed subprocess.
func TestQuotaPoller_FallbackFailureLeavesUnknown(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Storage.WorkersRootDir = t.TempDir()

	mgr := newQuotaTestManager(t, cfg, map[string]AccountUsage{
		"acct_f": {AccountID: "acct_f", UsageError: "forbidden"},
	}, nil)
	mgr.SetAccountsForTest([]Account{{
		ID: "acct_f", Email: "f@example.com", Enabled: true, Status: StatusHealthy,
		Quotas: map[string]QuotaGroup{}, FamilyCooldowns: map[string]time.Time{},
	}})
	mgr.agyForTest = func(context.Context, string, []string) ([]byte, error) {
		return nil, fmt.Errorf("agy /usage failed: exit status 1")
	}
	mgr.exportAccount = writeExport(t, "f@example.com", "a", "r", 1)

	NewQuotaPoller(mgr, cfg, nil).Poll(context.Background())

	acc := mgr.GetAccounts()[0]
	if acc.QuotasKnown {
		t.Fatal("a failed fallback must not mark the quota known")
	}
	if !strings.Contains(acc.UsageError, "forbidden") {
		t.Fatalf("original usage error must be retained, got %q", acc.UsageError)
	}
}

// TestRefreshPlan_LabelsAndUnknownFallback is S35.
//
// @purpose Proves a plan label is only ever reported when the vault actually said it.
// @rationale Upstream omits `plan` together with `usage` when quota is unavailable, so the previous
// `if tier == "" → "free"` fallback asserted something the vault never claimed (a Pro account with a
// failed quota read rendered as Free).
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants No subprocess.
func TestRefreshPlan_LabelsAndUnknownFallback(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := NewManager(cfg, nil)
	mgr.SetAccountsForTest([]Account{
		{ID: "acct_pro", Email: "pro@example.com", Enabled: true},
		{ID: "acct_unk", Email: "unk@example.com", Enabled: true},
	})
	mgr.listPlanForTest = func(context.Context) ([]byte, error) {
		// acct_pro reports a plan; acct_unk omits it entirely, exactly as upstream does when quota is
		// unreadable.
		return []byte(`{"data":{"accounts":[{"id":"acct_pro","plan":"Pro"},{"id":"acct_unk"}]}}`), nil
	}

	if err := mgr.RefreshPlan(context.Background()); err != nil {
		t.Fatalf("RefreshPlan: %v", err)
	}

	res, err := mgr.GetQuota(context.Background(), "")
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	byID := map[string]AccountQuotaInfo{}
	for _, info := range res.Accounts {
		byID[info.ID] = info
	}
	if got := byID["acct_pro"].Plan; got != "Pro" {
		t.Fatalf("plan = %q, want Pro", got)
	}
	if got := byID["acct_unk"].Plan; got != "Unknown" {
		t.Fatalf("absent plan must render as Unknown, got %q", got)
	}
}

// TestRefreshPlan_UnparseablePayloadKeepsPreviousLabels pins the fail-safe read.
//
// @purpose Proves a broken payload cannot relabel every account as unknown mid-flight.
// @rationale The plan read is display-only; discarding good labels because one response was malformed
// would make the admin list flap.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants No subprocess.
func TestRefreshPlan_UnparseablePayloadKeepsPreviousLabels(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := NewManager(cfg, nil)
	mgr.SetAccountsForTest([]Account{{ID: "acct_pro", Email: "pro@example.com", Enabled: true}})

	mgr.listPlanForTest = func(context.Context) ([]byte, error) {
		return []byte(`{"data":{"accounts":[{"id":"acct_pro","plan":"Pro"}]}}`), nil
	}
	if err := mgr.RefreshPlan(context.Background()); err != nil {
		t.Fatalf("first RefreshPlan: %v", err)
	}

	mgr.listPlanForTest = func(context.Context) ([]byte, error) {
		return []byte(`<html>gateway error</html>`), nil
	}
	if err := mgr.RefreshPlan(context.Background()); err == nil {
		t.Fatal("an unparseable plan payload must be reported")
	}
	if got := mgr.planLabelFor("acct_pro"); got != "Pro" {
		t.Fatalf("previous label must survive a failed refresh, got %q", got)
	}
}

// TestSwitchAccount_AmbiguousQueryIsSurfaced is S33.
//
// @purpose Proves an ambiguous selector is reported, never guessed.
// @rationale `agy-auth` returns `ambiguous_query` plus candidates under `--json`; picking one would
// silently install the wrong account on every worker.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants No subprocess.
func TestSwitchAccount_AmbiguousQueryIsSurfaced(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := NewManager(cfg, nil)
	mgr.SetAccountsForTest([]Account{{ID: "acct_a", Email: "a@example.com", Enabled: true}})
	mgr.agyAuthForTest = func(_ context.Context, args []string) ([]byte, error) {
		payload := map[string]any{
			"schema_version": 1,
			"command":        "switch",
			"error": map[string]any{
				"code":    "ambiguous_query",
				"message": "query matches several accounts",
				"candidates": []any{
					map[string]any{"id": "acct_a", "email": "a@example.com"},
					map[string]any{"id": "acct_b", "email": "b@example.com"},
				},
			},
		}
		raw, _ := json.Marshal(payload)
		return raw, fmt.Errorf("agy-auth %v failed (exit status 1): %s", args, raw)
	}

	id, err := mgr.SwitchAccount(context.Background(), "acct")
	if err == nil {
		t.Fatal("an ambiguous query must be an error, not a guess")
	}
	if id != "" {
		t.Fatalf("ambiguous query must resolve no account, got %q", id)
	}
	if !strings.Contains(err.Error(), "ambiguous_query") {
		t.Fatalf("the vault error must be surfaced verbatim, got %v", err)
	}
	if !strings.Contains(err.Error(), "acct_b") {
		t.Fatalf("candidates must reach the caller, got %v", err)
	}
}

// TestSwitchAccount_ResolvesActiveAccountFromVault pins the read-back.
//
// @purpose Proves switch reports the id the vault actually activated, not the query string.
// @rationale `agy-auth switch` accepts an alias or a prefix and never prints the resolved id, so the
// id has to be read from the account set; reporting the query would make the admin payload wrong.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants No subprocess; Sync is stubbed through the same seam.
func TestSwitchAccount_ResolvesActiveAccountFromVault(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := NewManager(cfg, nil)
	mgr.SetAccountsForTest([]Account{{ID: "acct_a", Email: "a@example.com", Enabled: true}})
	mgr.agyAuthForTest = func(_ context.Context, args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "list" {
			return []byte(`{"data":{"accounts":[{"id":"acct_b","email":"b@example.com","active":true}]}}`), nil
		}
		return nil, nil
	}

	id, err := mgr.SwitchAccount(context.Background(), "b")
	if err != nil {
		t.Fatalf("SwitchAccount: %v", err)
	}
	if id != "acct_b" {
		t.Fatalf("resolved account = %q, want acct_b", id)
	}
}

// TestSync_UsesCheapVaultRead pins D1 at the seam.
//
// @purpose Proves Sync reads the vault with the command that actually carries the fields it maps.
// @rationale `list --all --json` omits `plan`, `expired` and `auth_method`, which is why every account
// used to look Free and the expiry branch was dead code.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants No subprocess.
func TestSync_UsesCheapVaultRead(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := NewManager(cfg, nil)

	var gotArgs []string
	mgr.agyAuthForTest = func(_ context.Context, args []string) ([]byte, error) {
		gotArgs = append([]string(nil), args...)
		return []byte(`{"data":{"accounts":[
			{"id":"acct_a","email":"a@example.com","auth_method":"consumer","stored":true,"expired":true,"active":true}
		]}}`), nil
	}

	if err := mgr.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	want := []string{"list", "--json", "--no-usage"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Fatalf("Sync ran %v, want %v", gotArgs, want)
	}

	acc := mgr.GetAccounts()[0]
	if acc.AuthMethod != "consumer" {
		t.Fatalf("auth_method = %q, want consumer", acc.AuthMethod)
	}
	if acc.Status != StatusHealthy {
		t.Fatalf("expired:true must stay HEALTHY (refresher state), got %s", acc.Status)
	}
}

// TestGetQuota_ReportsFallbackBuckets pins the admin view against the poller.
//
// @purpose Proves `/admin/v1/accounts/quota` does not contradict what the scheduler knows.
// @rationale Found on the live container: the poller logged `quota recovered via agy /usage fallback`
// with 2 groups while the admin endpoint still answered `known=False, err=forbidden, groups=0` for the
// same account. The endpoint only read `agy-auth usage`, so the fallback existed for scheduling but was
// invisible to the operator.
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants No real subprocess.
func TestGetQuota_ReportsFallbackBuckets(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Storage.WorkersRootDir = t.TempDir()

	mgr := newQuotaTestManager(t, cfg, map[string]AccountUsage{
		"acct_free": {AccountID: "acct_free", Email: "free@example.com", UsageError: "forbidden"},
	}, nil)
	mgr.SetAccountsForTest([]Account{{
		ID: "acct_free", Email: "free@example.com", Enabled: true, Status: StatusHealthy,
		Quotas: map[string]QuotaGroup{}, FamilyCooldowns: map[string]time.Time{},
	}})
	mgr.listForTest = func(context.Context) ([]byte, error) {
		return []byte(`{"data":{"accounts":[{"id":"acct_free","email":"free@example.com","active":true}]}}`), nil
	}
	mgr.agyForTest = func(context.Context, string, []string) ([]byte, error) {
		return freeQuotaEnvelope(), nil
	}
	mgr.exportAccount = writeExport(t, "free@example.com", "a", "r", 1)

	res, err := mgr.GetQuota(context.Background(), "")
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if len(res.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(res.Accounts))
	}
	got := res.Accounts[0]
	if !got.QuotasKnown {
		t.Fatal("the admin view must report the fallback buckets the poller already uses")
	}
	if got.UsageError != "" {
		t.Fatalf("usage error must be cleared, got %q", got.UsageError)
	}
	if len(got.Groups) != 2 {
		t.Fatalf("groups = %d, want 2 (Gemini + Claude/GPT)", len(got.Groups))
	}
	if !got.QuotaAvailable {
		t.Fatal("a healthy account with recovered quota must be reported available")
	}
}
