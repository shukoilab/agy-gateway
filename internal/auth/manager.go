package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"agy-gateway/internal/config"
	"agy-gateway/internal/retry"
)

var (
	ErrAccountNotFound    = errors.New("account not found")
	ErrNoEligibleAccounts = errors.New("no eligible accounts available in vault")
)

// AgyUsageEnvelope mirrors the stdout from agy -p "/usage" --output-format json.
type AgyUsageEnvelope struct {
	ConversationID string `json:"conversation_id" example:"conv-1234"`
	Status         string `json:"status" example:"SUCCESS"`
	Response       string `json:"response" example:"Gemini Models\tWeekly Limit Remaining\t44%\t2026-11-28T08:57:41Z"`
	Command        struct {
		Name string `json:"name" example:"usage"`
		Data struct {
			Description string          `json:"description" example:"Within each group, models share a weekly limit and a 5-hour limit."`
			Groups      []AgyUsageGroup `json:"groups"`
		} `json:"data"`
	} `json:"command"`
}

type AgyUsageGroup struct {
	Name        string           `json:"name" example:"Gemini Models"`
	Description string           `json:"description" example:"Models within this group: Gemini 3.8 Flash"`
	Buckets     []AgyUsageBucket `json:"buckets"`
}

type AgyUsageBucket struct {
	ID                string  `json:"id" example:"gemini-weekly"`
	Name              string  `json:"name" example:"Weekly Limit Remaining"`
	Description       string  `json:"description" example:"You have used some of your weekly limit."`
	Window            string  `json:"window" example:"weekly"` // "weekly" or "5h"
	RemainingFraction float64 `json:"remaining_fraction" example:"0.4436"`
	ResetTime         string  `json:"reset_time" example:"2026-11-28T08:57:41Z"`
}

// QuotaBucketInfo represents structured quota metrics for an individual window bucket.
type QuotaBucketInfo struct {
	ID                string    `json:"id" example:"gemini-5h"`
	Name              string    `json:"name" example:"Five Hour Limit Remaining"`
	Description       string    `json:"description,omitempty" example:"You have used some of your 5-hour limit."`
	Window            string    `json:"window" example:"5h"` // "weekly" or "5h"
	RemainingFraction float64   `json:"remaining_fraction" example:"0.7038"`
	RemainingPct      float64   `json:"remaining_pct" example:"70.38"` // e.g. 95.68
	ResetTime         time.Time `json:"reset_time" example:"2026-11-24T17:47:05Z"`
	IsExhausted       bool      `json:"is_exhausted" example:"false"`
}

// QuotaGroupInfo groups quota buckets by model category (e.g. Gemini Models, Claude/GPT).
type QuotaGroupInfo struct {
	Name        string            `json:"name" example:"Gemini Models"`
	Description string            `json:"description,omitempty" example:"Gemini 3.8 Flash models pool"`
	Buckets     []QuotaBucketInfo `json:"buckets"`
}

// AccountQuotaInfo represents enriched live quota status for an individual account.
type AccountQuotaInfo struct {
	ID             string                `json:"id" example:"acc_9f3b18e2"`
	Email          string                `json:"email" example:"dev@shukoilab.com"`
	Alias          string                `json:"alias,omitempty" example:"primary-account"`
	Plan           string                `json:"plan" example:"payg"`
	Active         bool                  `json:"active" example:"true"`
	Status         AccountStatus         `json:"status" example:"HEALTHY"`
	QuotaAvailable bool                  `json:"quota_available" example:"true"`
	CooldownUntil  *time.Time            `json:"cooldown_until,omitempty"`
	Groups         []QuotaGroupInfo      `json:"groups"`
	Quotas         map[string]QuotaGroup `json:"quotas,omitempty"`
	// QuotasKnown mirrors the account-level flag so a client can tell "no limits" from "could not ask".
	//
	// @purpose Stops the admin endpoint from being the last place that fails open.
	// @rationale The account list already exposes this, but the quota detail endpoint is what an
	// operator actually reads to debug selection; it reported `quota_available: true` with zero
	// buckets for an account whose read had failed, which is the same defect at a different surface.
	// @invariants Mirrors Account.QuotasKnown at snapshot time.
	QuotasKnown bool `json:"quotas_known"`
	// UsageError carries the provider's reason when the quota read failed.
	//
	// @purpose Lets an operator see `forbidden`/`timeout` instead of an empty bucket list.
	// @rationale Diagnosis only; never parsed by the gateway.
	// @invariants Empty exactly when the corresponding read succeeded.
	UsageError string    `json:"usage_error,omitempty"`
	Overview   string    `json:"overview,omitempty" example:"Gemini: 70.38% (5h reset in 23m)"`
	LastSynced time.Time `json:"last_synced" example:"2026-11-24T10:00:00Z"`
}

// AccountsQuotaResponse wraps the list of accounts quota data.
type AccountsQuotaResponse struct {
	Timestamp time.Time          `json:"timestamp" example:"2026-11-24T10:00:00Z"`
	Total     int                `json:"total" example:"2"`
	Accounts  []AccountQuotaInfo `json:"accounts"`
}

// Manager coordinates agy-auth execution, central credential vault mutations,
// and worker sandbox HOME environment provisioning.
type Manager struct {
	cfg     *config.Config
	logger  *slog.Logger
	vaultMu sync.Mutex

	accountsMu sync.RWMutex
	accounts   []Account

	// exportAccount is the credential-extraction seam.
	//
	// @purpose Lets tests supply a fixture export without a vault or subprocess.
	// @rationale Materialisation correctness is identity validation, atomicity, and rollback, none of
	// which require the real CLI. Nil means defaultExportAccount.
	// @invariants Assigned only during construction and in tests, before concurrent use begins.
	exportAccount exportAccountFunc

	// listForTest overrides the `agy-auth list` invocation during tests.
	//
	// @purpose Lets Sync's merge behaviour be tested without a vault.
	// @rationale Sync's contract is about preserving runtime state across a rebuild, which needs a
	// controlled vault listing rather than a real subprocess.
	// @invariants Assigned only in tests, before concurrent use begins.
	listForTest func(ctx context.Context) ([]byte, error)

	// listPlanForTest overrides the `agy-auth list --json` plan query during tests.
	//
	// @purpose Keeps RefreshPlan testable without a vault.
	// @rationale The plan read is a separate command from the vault read, so it needs its own seam.
	// @invariants Assigned only in tests, before concurrent use begins.
	listPlanForTest func(ctx context.Context) ([]byte, error)

	// fetchUsageForTest overrides FetchAllAccountUsage during tests.
	//
	// @purpose Lets poller behaviour be driven by controlled payloads.
	// @rationale Poll logic is a pure function of the account set and payload; stubbing only the fetch
	// keeps the real status-transition code under test.
	// @invariants Assigned only in tests, before concurrent use begins.
	fetchUsageForTest func(ctx context.Context) (map[string]AccountUsage, error)

	// agyForTest overrides the `agy` subprocess for tests.
	//
	// @purpose Lets the `agy /usage` quota fallback and `agy models` discovery be exercised without a
	// real binary, while the surrounding credential-borrowing and decoding code still runs for real.
	// @rationale Both paths share ExecAgy, so one seam covers them and keeps a single place where the
	// child environment is described. Stubbing the binary path instead would require writing an
	// executable per test.
	// @invariants Assigned only in tests, before concurrent use begins; nil means "spawn for real".
	agyForTest func(ctx context.Context, homeDir string, args []string) ([]byte, error)

	// agyAuthForTest overrides the `agy-auth` subprocess for tests.
	//
	// @purpose Lets error-shape handling (for example `ambiguous_query` with candidates) be tested
	// without a real vault.
	// @rationale The vault error contract is what the CLI and admin API surface verbatim, so the test
	// must be able to produce the exact payload.
	// @invariants Assigned only in tests, before concurrent use begins; nil means "spawn for real".
	agyAuthForTest func(ctx context.Context, args []string) ([]byte, error)

	// plans caches the human plan label per account id, for reporting only.
	//
	// @purpose Separates the expensive-but-informative `plan` read from the cheap vault truth read.
	// @rationale `list --json --no-usage` (155 ms) omits `plan`, while only `list --json` (≈1.9 s,
	// because it also fetches quota) carries it. Running the expensive command on every poll would
	// double the poll cost for a display string, so it is refreshed at boot, after add, and after
	// switch/rotate, and read from here in between.
	// @invariants Guarded by vaultMu; empty means "unknown", which must render as unknown and never
	// as "free" (upstream omits `plan` when quota is unavailable). Never consulted by eligibility.
	plans map[string]string
}

// NewManager creates an initialized Account Manager.
func NewManager(cfg *config.Config, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		cfg:      cfg,
		logger:   logger,
		accounts: make([]Account, 0),
	}
}

// Sync queries agy-auth and updates the internal account list.
//
// @purpose Rebuilds the account set from the vault, the only authority on which credentials exist.
// @rationale The command matters as much as the parse. `list --all --json` (measured 1960 ms) omits
// `auth_method`, `expired`, `stored`, and `plan`, so every field the decoder asked for beyond
// id/email/alias arrived empty: `Tier` was always "" (which made a Pro account render as Free) and
// the `expired` branch was unreachable dead code. `list --json --no-usage` (measured 155 ms) is the
// cheap command that actually emits those fields. `plan` is deliberately NOT read here — it lives in
// the separate cache refreshed by RefreshPlan, because only `list --json` (with usage) carries it and
// that command costs ~1.9 s.
// @preconditions m.cfg is configured.
// @param ctx Cancellation root for the subprocess.
// @returns A wrapped error when the vault cannot be read or the payload cannot be parsed. A parse
// failure never mutates the previous set.
// @throws None directly; subprocess and parse errors are wrapped.
// @invariants O(accounts); at most one subprocess; holds vaultMu for the duration; never puts `plan`
// or `usage` in the returned set (reporting-only, see D3).
func (m *Manager) Sync(ctx context.Context) error {
	m.vaultMu.Lock()
	defer m.vaultMu.Unlock()

	var out []byte
	var err error
	if m.listForTest != nil {
		out, err = m.listForTest(ctx)
	} else {
		out, err = m.execAgyAuth(ctx, "list", "--json", "--no-usage")
	}
	if err != nil {
		return fmt.Errorf("agy-auth list failed: %w", err)
	}

	type rawAccountItem struct {
		ID         string `json:"id"`
		Email      string `json:"email"`
		Alias      string `json:"alias"`
		AuthMethod string `json:"auth_method"`
		Plan       string `json:"plan"`
		Tier       string `json:"tier"`
		Active     bool   `json:"active"`
		// Expired is the refresher's own state for the access token, NOT a dead credential.
		//
		// @purpose Documents why this field is parsed but never mapped to AUTH_EXPIRED.
		// @rationale `agy-auth list --json` reports `expired: true` for an access token that has
		// simply passed its short lifetime while the refresh token is still valid — `agy -p` answers
		// normally with that same credential (measured). Treating it as a fault made the gateway
		// refuse an account that works, so expiry is now a non-event; only `Stored == false` (the
		// snapshot is gone) removes an account.
		Expired bool `json:"expired"`
		// Stored is false only when the vault keeps a registry row whose snapshot file is missing.
		//
		// @purpose Distinguishes "an account exists" from "an account is merely remembered".
		// @rationale agy-auth emits `stored: false` for registry rows without a snapshot; such a row
		// cannot be exported, so advertising it as a usable account only causes a failing lease.
		// A pointer is used because the field is absent in other payload shapes: an absent `stored`
		// must mean "stored" (the pre-existing behaviour), never "missing".
		Stored *bool `json:"stored"`
	}

	var rawAccounts []rawAccountItem

	var envFormat struct {
		Data struct {
			Accounts []rawAccountItem `json:"accounts"`
		} `json:"data"`
	}
	// Distinguish "parsed and genuinely empty" from "could not parse at all". An empty vault is a real
	// state (an operator removed the last account) and must be honoured; an unreadable payload is not
	// evidence of anything and must leave the previous set intact.
	parsed := false
	if err := json.Unmarshal(out, &envFormat); err == nil && envFormat.Data.Accounts != nil {
		rawAccounts = envFormat.Data.Accounts
		parsed = true
	} else if err := json.Unmarshal(out, &rawAccounts); err == nil && rawAccounts != nil {
		// A bare `null` unmarshals into a nil slice without error; treating that as an empty vault
		// would let a malformed response remove every account. Only a real JSON array counts.
		parsed = true
	}
	if !parsed {
		// A payload we cannot parse must not wipe the account set: one malformed response would take
		// every worker out of service.
		return fmt.Errorf("failed to parse agy-auth account list: %q", truncateForError(out))
	}

	m.accountsMu.Lock()

	// Preserve runtime state across a resync. Sync reconstructs accounts from the vault, which knows
	// nothing about quota cooldowns, so a naive rebuild would silently reset a cooling account to
	// HEALTHY — making the admin list disagree with the poller and letting a spent account be leased
	// again. Only vault-owned fields are refreshed; runtime-owned fields are carried forward.
	previous := make(map[string]Account, len(m.accounts))
	for _, existing := range m.accounts {
		previous[existing.ID] = existing
	}

	newAccounts := make([]Account, 0, len(rawAccounts))
	newAccountIDs := make(map[string]struct{}, len(rawAccounts))
	for _, raw := range rawAccounts {
		// A registry row without a stored snapshot cannot be exported, so it can never serve a turn.
		// Dropping it here keeps the account set equal to the set of usable credentials. An absent
		// `stored` field is not evidence of absence: only an explicit `false` excludes the account.
		if raw.Stored != nil && !*raw.Stored && raw.ID != "" {
			m.logger.Info("ignoring vault entry with no stored snapshot", "accountID", raw.ID, "email", raw.Email)
			continue
		}
		newAccountIDs[raw.ID] = struct{}{}
		tier := raw.Tier
		if tier == "" && raw.Plan != "" {
			tier = strings.ToLower(raw.Plan)
		}

		acc := Account{
			ID:             raw.ID,
			ActiveInVault:  raw.Active,
			Email:          raw.Email,
			Alias:          raw.Alias,
			AuthMethod:     raw.AuthMethod,
			Tier:           tier,
			Status:         StatusHealthy,
			Enabled:        true,
			AssignedWorker: -1,
			Quotas:         make(map[string]QuotaGroup),
			// Deliberately zero, not time.Now(): this field reports the last LEASE, and a freshly
			// synced account has never served one. Stamping the sync time made `last_used` an alias of
			// "last poll" and hid which accounts were actually carrying traffic.
		}

		if prior, ok := previous[raw.ID]; ok {
			// Carry forward everything the vault cannot know: live quota, both the account-wide and
			// the per-family cooldown windows, worker binding, and last-use time. Eligibility reads
			// FamilyCooldowns, so dropping it here would silently re-open a family the poller had
			// just closed — the same class of state loss this carry-forward exists to prevent.
			acc.Quotas = prior.Quotas
			// Quota knowledge is runtime-owned: the vault knows credentials, not whether the last quota
			// read succeeded. Dropping this would silently promote every account to "known healthy"
			// on the next resync, which is the fail-open FR-02 exists to close.
			acc.QuotasKnown = prior.QuotasKnown
			acc.UsageError = prior.UsageError
			acc.CooldownUntil = prior.CooldownUntil
			acc.FamilyCooldowns = prior.FamilyCooldowns
			acc.AssignedWorker = prior.AssignedWorker
			acc.LastUsed = prior.LastUsed

			// Runtime-owned state: keep whatever the fault/retry machinery recorded. A vault-reported
			// expiry is no longer mapped to AUTH_EXPIRED (see rawAccountItem.Expired), so the only
			// way Status is AUTH_EXPIRED here is a runtime credential fault — which the bounded retry
			// window owns.
			acc.Status = prior.Status
		}
		newAccounts = append(newAccounts, acc)
	}
	m.accounts = newAccounts
	m.accountsMu.Unlock()

	// Purge credential copies that belong to accounts the vault no longer reports. Sync is the only
	// place that learns an account disappeared, so it is also the only place that can clean up the
	// plaintext refresh tokens this gateway installed for it. Without this a deleted account kept a
	// working credential on disk under every worker HOME forever.
	removed := make([]string, 0)
	for id, prior := range previous {
		if _, still := newAccountIDs[id]; !still {
			removed = append(removed, prior.Email)
		}
	}
	m.purgeWorkerCredentialsFor(removed)
	return nil
}

// RefreshPlan re-reads each account's plan label for display.
//
// @purpose Keeps the Pro/Free label truthful without paying its cost on every poll.
// @rationale `list --json --no-usage` is the cheap vault read but carries no `plan`; only
// `list --json` does, and it also performs a quota lookup (≈1.9 s). Calling it at boot, after `add`,
// and after `switch`/`rotate` is enough because a plan changes only when the account does.
// @preconditions m.cfg is configured.
// @param ctx Cancellation root for the subprocess.
// @returns A wrapped error when the subprocess fails or the payload cannot be parsed; the previously
// cached labels are then retained.
// @throws None directly.
// @invariants One subprocess at most; never consults or mutates eligibility state; a missing `plan`
// for an account is stored as empty so the reader can render it as unknown rather than guessing.
func (m *Manager) RefreshPlan(ctx context.Context) error {
	var out []byte
	var err error
	if m.listPlanForTest != nil {
		out, err = m.listPlanForTest(ctx)
	} else {
		out, err = m.execAgyAuth(ctx, "list", "--json")
	}
	if err != nil {
		return fmt.Errorf("agy-auth list --json failed: %w", err)
	}

	type rawPlanItem struct {
		ID   string `json:"id"`
		Plan string `json:"plan"`
	}
	var envFormat struct {
		Data struct {
			Accounts []rawPlanItem `json:"accounts"`
		} `json:"data"`
	}
	var accounts []rawPlanItem
	if err := json.Unmarshal(out, &envFormat); err == nil && envFormat.Data.Accounts != nil {
		accounts = envFormat.Data.Accounts
	} else if err := json.Unmarshal(out, &accounts); err != nil || accounts == nil {
		return fmt.Errorf("failed to parse agy-auth plan payload: %q", truncateForError(out))
	}

	plans := make(map[string]string, len(accounts))
	for _, acc := range accounts {
		if acc.ID == "" {
			continue
		}
		plans[acc.ID] = strings.ToLower(strings.TrimSpace(acc.Plan))
	}

	m.vaultMu.Lock()
	m.plans = plans
	m.accountsMu.Lock()
	// Reconcile the cached label into the account set so readers do not have to consult two places.
	// Tier is reporting-only: nothing in eligibility reads it.
	for i := range m.accounts {
		m.accounts[i].Tier = plans[m.accounts[i].ID]
	}
	m.accountsMu.Unlock()
	m.vaultMu.Unlock()
	return nil
}

// planLabelFor returns the cached display label for an account id.
//
// @purpose Gives the admin API and CLI one place that decides how an unknown plan is rendered.
// @rationale Reporting `free` for an account whose plan was never read is a factual claim the vault
// did not make. Upstream omits `plan` entirely when quota is unavailable, so absence must render as
// unknown.
// @preconditions None.
// @param accountID Account to look up.
// @returns The capitalised plan name, or "Unknown" when no plan has been read for it.
// @throws None.
// @invariants O(1) map read under vaultMu; never mutates state.
func (m *Manager) planLabelFor(accountID string) string {
	m.vaultMu.Lock()
	plan := m.plans[accountID]
	m.vaultMu.Unlock()
	plan = strings.TrimSpace(plan)
	if plan == "" {
		return "Unknown"
	}
	return strings.ToUpper(plan[:1]) + plan[1:]
}

// purgeWorkerCredentialsFor deletes the worker HOME of every account that no longer exists.
//
// @purpose Leaves nothing behind when an account is deleted: not the plaintext refresh token, not the
// agent's conversation cache, not the brain artifacts, not an orphan home that the next boot would
// reuse for a different account.
// @rationale Each worker HOME holds its own credential copy (that is what makes per-account quota
// real). Those copies are never touched by `agy-auth remove`, which only knows the vault, so without
// this they outlive the account indefinitely. The previous version also two narrower bugs: it was
// bounded by `cfg.Pool.WorkerCount`, so lowering the count left `w2`/`w3` holding deleted
// credentials forever (one of them world-readable), and it deleted only the token file, leaving the
// deleted account's `conversations/`, `conversation_summaries.db*` and `brain/` on disk.
// @preconditions Caller must not hold accountsMu or vaultMu.
// @param emails Display emails of the removed accounts; empty is a no-op.
// @returns The worker indices whose HOME was removed. Removal is best-effort per directory: a
// failure is logged and the rest still proceed.
// @throws None; every failure is logged.
// @invariants O(worker directories on disk), NOT O(WorkerCount) — the scan is over `wN` entries
// actually present, so a stale home from a previous, larger configuration is still found. Never logs
// credential material.
func (m *Manager) purgeWorkerCredentialsFor(emails []string) []int {
	if len(emails) == 0 || m.cfg == nil || m.cfg.Storage.WorkersRootDir == "" {
		return nil
	}
	gone := make(map[string]struct{}, len(emails))
	for _, e := range emails {
		if e != "" {
			gone[strings.ToLower(e)] = struct{}{}
		}
	}
	if len(gone) == 0 {
		return nil
	}

	homes, err := workerHomeDirs(m.cfg.Storage.WorkersRootDir)
	if err != nil {
		m.logger.Debug("could not enumerate worker homes for purge", "err", err)
		return nil
	}

	var removed []int
	for _, h := range homes {
		tokenPath := filepath.Join(h.path, ".gemini", "antigravity-cli", "antigravity-oauth-token")
		raw, err := os.ReadFile(tokenPath)
		if err != nil {
			continue
		}
		var payload exportedCredential
		if err := json.Unmarshal(raw, &payload); err != nil {
			continue
		}
		if _, bad := gone[strings.ToLower(payload.IDTokenEmail)]; !bad {
			continue
		}
		if err := os.RemoveAll(h.path); err != nil {
			m.logger.Warn("could not remove the worker home of a deleted account",
				"workerID", h.id, "err", err)
			continue
		}
		m.logger.Info("removed worker home of a deleted account", "workerID", h.id,
			"email", payload.IDTokenEmail)
		removed = append(removed, h.id)
	}
	return removed
}

// workerHome is one `wN` directory found under the workers root.
type workerHome struct {
	id   int
	path string
}

// workerHomeDirs lists every `wN` worker HOME that exists on disk.
//
// @purpose Removes the "only the first WorkerCount directories matter" assumption that left stale
// homes holding deleted credentials after an operator lowered the count.
// @rationale The directory name encodes the worker index and nothing else in the tree matches `^w\d+$`
// at the top level, so a directory listing is the authoritative inventory. It is derived from the
// filesystem, not from configuration, because configuration is exactly what changed.
// @preconditions root is the configured workers root; it may not exist.
// @param root WorkersRootDir.
// @returns Every `wN` directory, sorted by numeric id for deterministic logs. A missing root is an
// empty slice, not an error.
// @throws A wrapped error only when the directory exists but cannot be read.
// @invariants O(entries); ignores files and any name that is not `w<digits>`.
func workerHomeDirs(root string) ([]workerHome, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	homes := make([]workerHome, 0, len(entries))
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
		homes = append(homes, workerHome{id: id, path: filepath.Join(root, name)})
	}
	sort.Slice(homes, func(i, j int) bool { return homes[i].id < homes[j].id })
	return homes, nil
}

// truncateForError bounds a raw payload quoted in an error message.
//
// @purpose Keeps a malformed `agy-auth` response from dumping unbounded output into logs, while still
// showing enough for an operator to diagnose the shape.
// @rationale The payload contains no credential material, but it can be large; 256 bytes identifies
// the shape without flooding.
// @preconditions None.
// @param b Raw output; may be any length.
// @returns At most 256 bytes as a string, suffixed with "…" when truncated.
// @throws None.
// @invariants O(1) memory beyond the source slice; no allocation when b is short.
func truncateForError(b []byte) string {
	const max = 256
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…"
}

// GetAccounts returns a deep snapshot of all tracked accounts.
//
// @purpose Hands the pool an account view it can consult without holding any lock.
// @rationale The poller rewrites Quotas and FamilyCooldowns while holding accountsMu, so a shallow
// copy would share those maps with live state and produce a data race as soon as a lock-free reader
// iterates them (confirmed: mapdelete in the poller against IsEligibleForModel in pool rotation).
// Copying the two maps is what makes lock-free eligibility checks safe.
// @preconditions None.
// @param None.
// @returns A new slice of account values whose Quotas and FamilyCooldowns maps are independent of
// manager state; mutating them cannot affect the manager.
// @throws None.
// @invariants O(accounts + total buckets); acquires accountsMu for the duration; no subprocess.
func (m *Manager) GetAccounts() []Account {
	m.accountsMu.RLock()
	defer m.accountsMu.RUnlock()

	out := make([]Account, len(m.accounts))
	copy(out, m.accounts)
	for i := range out {
		out[i].Quotas = copyQuotaGroups(out[i].Quotas)
		out[i].FamilyCooldowns = copyCooldowns(out[i].FamilyCooldowns)
	}
	return out
}

// copyQuotaGroups clones a bucket map so a reader cannot observe later mutation.
//
// @purpose Removes sharing between live account state and snapshots handed to the pool.
// @rationale The poller rewrites this map under accountsMu; a shallow copy would race with it.
// Bucket values are plain structs, so only the map has to be cloned.
// @preconditions None.
// @param in Source map, possibly nil.
// @returns A new map with the same entries, or nil when in is nil.
// @throws None.
// @invariants O(len(in)); allocates one map of the same capacity.
func copyQuotaGroups(in map[string]QuotaGroup) map[string]QuotaGroup {
	if in == nil {
		return nil
	}
	out := make(map[string]QuotaGroup, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// copyCooldowns clones a per-family cooldown map so a reader cannot observe later mutation.
//
// @purpose Same isolation for the map eligibility now depends on.
// @rationale The poller deletes elapsed windows and adds new ones while holding accountsMu; a
// shallow copy would let IsEligibleForModel iterate the live map during a delete.
// @preconditions None.
// @param in Source map, possibly nil.
// @returns A new map with the same entries, or nil when in is nil.
// @throws None.
// @invariants O(len(in)); allocates one map of the same capacity.
func copyCooldowns(in map[string]time.Time) map[string]time.Time {
	if in == nil {
		return nil
	}
	out := make(map[string]time.Time, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// SetFetchUsageForTest replaces the per-account usage fetch for tests.
//
// @purpose Lets another package drive the quota poller without a real vault or subprocess.
// @rationale The poller is exercised from `pool` too (proving it does not resurrect an auth-expired
// account), and that package cannot reach an unexported seam.
// @preconditions fn must be safe for concurrent use; it is called from the poller goroutine.
// @param fn Replacement fetch returning per-account usage or an error.
// @returns None.
// @throws None.
// @invariants Stores one function pointer under accountsMu.
func (m *Manager) SetFetchUsageForTest(fn func(ctx context.Context) (map[string]AccountUsage, error)) {
	m.accountsMu.Lock()
	defer m.accountsMu.Unlock()
	m.fetchUsageForTest = fn
}

// SetAccountsForTest injects accounts directly into the manager for unit and integration testing.
// SetAgyForTest replaces the `agy` subprocess for tests.
//
// @purpose Lets the quota fallback and model discovery be exercised without a real binary.
// @rationale Both paths route through ExecAgy, so one seam covers them and the child environment stays
// described in exactly one place.
// @preconditions Only call from tests, before concurrent use begins.
// @param fn Replacement runner; nil restores real process execution.
// @returns None.
// @throws None.
// @invariants Stores one function pointer; never spawns anything itself.
func (m *Manager) SetAgyForTest(fn func(ctx context.Context, homeDir string, args []string) ([]byte, error)) {
	m.accountsMu.Lock()
	defer m.accountsMu.Unlock()
	m.agyForTest = fn
}

// SetAgyAuthForTest replaces the `agy-auth` subprocess for tests.
//
// @purpose Lets vault error shapes (for example `ambiguous_query`) be produced exactly.
// @rationale The CLI and admin API surface the vault error verbatim, so the test must be able to
// generate the real payload.
// @preconditions Only call from tests, before concurrent use begins.
// @param fn Replacement runner; nil restores real process execution.
// @returns None.
// @throws None.
// @invariants Stores one function pointer; never spawns anything itself.
func (m *Manager) SetAgyAuthForTest(fn func(ctx context.Context, args []string) ([]byte, error)) {
	m.vaultMu.Lock()
	defer m.vaultMu.Unlock()
	m.agyAuthForTest = fn
}

func (m *Manager) SetAccountsForTest(accounts []Account) {
	m.accountsMu.Lock()
	defer m.accountsMu.Unlock()
	m.accounts = accounts
}

// VaultActiveAccountID reports which account the vault itself considers active.
//
// @purpose Seeds sticky affinity so the gateway's first-boot layout matches what an operator sees in
// `agy-auth current`, instead of depending on vault ordering.
// @rationale The vault's `active` flag was parsed by Sync and then discarded — FR-05 asked for it to
// drive the first-boot seed, and `AccountUsage.Active` had no consumer at all. An operator who set an
// account "current" expects the gateway to start with a worker on it.
// @preconditions None; safe when no account is flagged.
// @param None.
// @returns The active account id, or "" when the vault flags none (or several, which is ambiguous).
// @throws None.
// @invariants O(accounts); takes accountsMu; no IO.
func (m *Manager) VaultActiveAccountID() string {
	m.accountsMu.Lock()
	defer m.accountsMu.Unlock()

	found := ""
	for i := range m.accounts {
		if !m.accounts[i].ActiveInVault {
			continue
		}
		if found != "" {
			// More than one flag is a vault inconsistency; refuse to guess rather than pick arbitrarily.
			return ""
		}
		found = m.accounts[i].ID
	}
	return found
}

// MarkAccountBinding records which worker currently holds an account's credential.
//
// @purpose Makes the admin API's `assigned_worker` field truthful instead of permanently -1.
// @rationale The field existed but nothing ever wrote it, so an operator inspecting the gateway could
// not tell which worker was spending which account's quota. The pool is the only component that
// knows this, and it must publish it after the credential is actually installed — not before, or the
// report would claim a binding that failed to materialise.
// @preconditions None; safe to call with an unknown account id.
// @param accountID Account whose binding changed.
// @param workerID Worker index now holding the account; -1 clears the binding.
// @returns None; mutates manager state.
// @throws None; an unknown account is logged at debug and ignored.
// @invariants O(accounts); takes accountsMu briefly; performs no IO, so it never serialises a lease
// behind a disk operation.
func (m *Manager) MarkAccountBinding(accountID string, workerID int) {
	m.accountsMu.Lock()
	defer m.accountsMu.Unlock()

	for i := range m.accounts {
		if m.accounts[i].ID != accountID {
			continue
		}
		m.accounts[i].AssignedWorker = workerID
		return
	}
	m.logger.Debug("binding reported for an unknown account; ignoring", "accountID", accountID, "workerID", workerID)
}

// MarkAccountUsed records that an account just served a lease.
//
// @purpose Makes `last_used` mean "last leased" rather than "last vault sync".
// @rationale Sync previously stamped every account on every resync, so the field reported the poll
// interval instead of real traffic. Per-lease writes are throttled because Acquire is on the hot path
// and every write takes accountsMu; one update per second per account carries the same operational
// meaning without creating a lock convoy.
// @preconditions None.
// @param accountID Account that served a lease.
// @param now Reference instant, injected so the throttle is testable without sleeping.
// @returns None; mutates manager state when the throttle permits.
// @throws None.
// @invariants O(accounts); takes accountsMu; at most one write per account per second.
func (m *Manager) MarkAccountUsed(accountID string, now time.Time) {
	m.accountsMu.Lock()
	defer m.accountsMu.Unlock()

	const throttle = time.Second
	for i := range m.accounts {
		if m.accounts[i].ID != accountID {
			continue
		}
		acc := &m.accounts[i]
		// A backwards clock must not freeze the field forever: only a value inside the throttle window
		// suppresses the write, so a negative delta refreshes it.
		if !acc.LastUsed.IsZero() && now.Sub(acc.LastUsed) >= 0 && now.Sub(acc.LastUsed) < throttle {
			return
		}
		acc.LastUsed = now
		return
	}
}

// MarkFamilyCooldown cools exactly one quota family of an account.
//
// @purpose Applies a refusal the gateway could attribute to a specific family.
// @rationale A quota refusal names no family, but the gateway knows which model was requested. Cooling
// both families would take a whole account out of rotation because one of its two independent windows
// ran out — the exact over-cooling the family split exists to prevent.
// @preconditions None; an unknown account id or an unknown family (empty string) is a no-op.
// @param accountID Account to cool.
// @param family Quota family to cool; "" is ignored because it cannot be attributed.
// @param until Absolute end of the cooldown; a past time clears the family instead.
// @returns None; mutates manager state.
// @throws None.
// @invariants O(accounts); takes accountsMu; never spawns a subprocess.
func (m *Manager) MarkFamilyCooldown(accountID, family string, until time.Time) {
	if m == nil || accountID == "" || family == "" {
		return
	}
	m.accountsMu.Lock()
	defer m.accountsMu.Unlock()

	for i := range m.accounts {
		acc := &m.accounts[i]
		if acc.ID != accountID {
			continue
		}
		if acc.FamilyCooldowns == nil {
			acc.FamilyCooldowns = make(map[string]time.Time, 2)
		}
		if !time.Now().Before(until) {
			delete(acc.FamilyCooldowns, family)
			summarizeCooldown(acc)
			return
		}
		acc.FamilyCooldowns[family] = until
		// Folding the family map into Status/CooldownUntil here is what makes the operator-facing view
		// and the Retry-After hint agree with eligibility. Without it a family-cooled account kept
		// reporting HEALTHY with no cooldown, so `NextAvailableIn` returned 0 and the client was told to
		// "retry later" with no hint of when.
		summarizeCooldown(acc)
		return
	}
}

// MarkAccountStatus updates an account's status and its derived cooldown state.
//
// @purpose Applies a credential- or quota-level verdict to one account atomically.
// @rationale Eligibility reads FamilyCooldowns, not Status, so writing only Status would let a
// "cooled" account keep being selected. This function is the single place that keeps the two in
// sync: a quota status cools both families (the split is not observable from a turn error), a
// healthy status clears them, and a credential fault clears them because the whole account is gone.
// @preconditions None.
// @param accountID Account to update; an unknown id is a no-op.
// @param status New status.
// @param cooldown Duration applied for quota statuses; ignored otherwise. Non-positive is allowed and
// yields an already-expired window.
// @returns None; mutates manager state.
// @throws None.
// @invariants O(accounts); takes accountsMu; never spawns a subprocess.
func (m *Manager) MarkAccountStatus(accountID string, status AccountStatus, cooldown time.Duration) {
	m.accountsMu.Lock()
	defer m.accountsMu.Unlock()

	for i := range m.accounts {
		if m.accounts[i].ID == accountID {
			acc := &m.accounts[i]
			acc.Status = status
			switch status {
			case StatusCooldown5h, StatusCooldownWeekly:
				// A quota failure observed during a turn is not family-scoped at this point, so both
				// families are cooled conservatively. The poller later refines this per family from
				// live bucket data, which is the only place the split is actually observable.
				until := time.Now().Add(cooldown)
				if acc.FamilyCooldowns == nil {
					acc.FamilyCooldowns = make(map[string]time.Time, 2)
				}
				acc.FamilyCooldowns[QuotaFamilyGemini] = until
				acc.FamilyCooldowns[QuotaFamilyThirdParty] = until
				acc.CooldownUntil = until
			case StatusHealthy:
				acc.FamilyCooldowns = nil
				acc.CooldownUntil = time.Time{}
			case StatusAuthExpired:
				// Bucket windows are irrelevant to a credential fault. The expiry is bounded, not
				// permanent: a runtime-detected failure (a bad export, an upstream 401) may clear on its
				// own, and a permanently-clean account would otherwise disappear from the vault with no
				// recovery path. `AuthExpiredRetryInterval` is how long it stays out of service.
				acc.FamilyCooldowns = nil
				if cooldown > 0 {
					acc.CooldownUntil = time.Now().Add(cooldown)
				} else {
					acc.CooldownUntil = time.Time{}
				}
			default:
				// DISABLED is an operator decision; it carries no retry window.
				acc.FamilyCooldowns = nil
				acc.CooldownUntil = time.Time{}
			}
			m.logger.Info("marked account status", "accountID", accountID, "status", status, "cooldown", cooldown)
			return
		}
	}
}

// FindNextEligibleAccount locates the next healthy account able to serve one model, excluding
// excludeAccountID.
//
// @purpose Selects the account a worker should be rebound to after a cooldown, quota failure, or
// proactive rotation.
// @rationale Eligibility is evaluated per quota family: an account whose third-party bucket is nearly
// spent must still be selectable for Gemini traffic. Expired cooldowns are recovered here so a window
// that has already reset does not keep removing capacity until the next poll.
// @preconditions None.
// @param excludeAccountID Account to skip; empty skips nothing.
// @param model Upstream model id whose quota family governs eligibility; empty checks all families.
// @param requiresPaidTier Reject free accounts when the model demands a paid tier.
// @param inUse Maps an account ID to the worker currently authenticating as it; nil or empty means
// nothing is claimed. Accounts absent from the map are shared freely.
// @returns A copy of the first eligible account, preferring one no other worker is using.
// @throws ErrNoEligibleAccounts when every other account is disabled, cooling, or out of quota.
// @invariants O(accounts) with two linear passes; takes accountsMu; no subprocess, no filesystem.
func (m *Manager) findNextEligibleAccountLocked(excludeAccountID string, model string, requiresPaidTier bool, inUse map[string]int) (*Account, error) {
	m.accountsMu.Lock()
	defer m.accountsMu.Unlock()

	now := time.Now()
	// Auto-recover expired cooldown accounts back to healthy
	for i := range m.accounts {
		if !m.accounts[i].CooldownUntil.IsZero() && now.After(m.accounts[i].CooldownUntil) {
			acc := &m.accounts[i]
			if acc.Status == StatusAuthExpired {
				// The bounded retry window elapsed: let the account be tried again. If the credential is
				// genuinely dead the next materialisation fails and re-marks it for another interval.
				m.logger.Info("auth-expired retry window elapsed; returning account to service",
					"accountID", acc.ID)
				acc.Status = StatusHealthy
				acc.CooldownUntil = time.Time{}
				acc.FamilyCooldowns = nil
				continue
			}
			// Recover every family whose window has elapsed; clearing the map is what makes the
			// account selectable again, since eligibility reads the map rather than the status.
			for family, until := range acc.FamilyCooldowns {
				if now.After(until) {
					delete(acc.FamilyCooldowns, family)
				}
			}
			if len(acc.FamilyCooldowns) == 0 &&
				(acc.Status == StatusCooldown5h || acc.Status == StatusCooldownWeekly) {
				m.logger.Info("account cooldown expired; recovering to healthy", "accountID", acc.ID)
				acc.Status = StatusHealthy
				acc.CooldownUntil = time.Time{}
			}
		}
	}

	// Two passes: prefer an account no other worker holds, then fall back to a shared one. Sharing is
	// not free — concurrent refreshes against one stored refresh token race, and only the first wins —
	// so a shared binding is a capacity-preserving last resort and is logged as such by the caller.
	//
	// Within a pass the choice is a total ordering, not first-match: (rank desc, claimCount asc, vault
	// order asc). First-match would ignore that a later account may be known-healthy while an earlier
	// one only has unknown quota, and would keep piling rotation onto an account three workers already
	// share while a one-way account sat free.
	pick := func(allowShared bool) *Account {
		bestIdx := -1
		bestRank := -1
		bestClaims := 0
		for i := range m.accounts {
			acc := &m.accounts[i]
			if acc.ID == excludeAccountID {
				continue
			}
			claims := inUse[acc.ID]
			if !allowShared && claims > 0 {
				continue
			}
			rank := acc.EligibilityRank(model, requiresPaidTier, m.cfg.Accounts.MinRemainingQuotaPct)
			if rank == 0 {
				continue
			}
			// Strictly-better comparisons only, so earlier vault order wins every exact tie and the
			// result stays deterministic regardless of map iteration.
			if rank > bestRank || (rank == bestRank && claims < bestClaims) {
				bestIdx, bestRank, bestClaims = i, rank, claims
			}
		}
		if bestIdx < 0 {
			return nil
		}
		// Deep-copy before returning: the caller keeps this value past the lock, and the poller
		// mutates the live maps under accountsMu.
		copyAcc := m.accounts[bestIdx]
		copyAcc.Quotas = copyQuotaGroups(copyAcc.Quotas)
		copyAcc.FamilyCooldowns = copyCooldowns(copyAcc.FamilyCooldowns)
		return &copyAcc
	}

	if acc := pick(false); acc != nil {
		return acc, nil
	}
	if acc := pick(true); acc != nil {
		return acc, nil
	}

	return nil, ErrNoEligibleAccounts
}

// FindNextEligibleAccount locates the next healthy account able to serve one model.
//
// @purpose Public entry point for account selection during rotation.
// @rationale Keeps the lock discipline in the manager: callers must not hold accountsMu themselves,
// and a copy of the account is returned so pool state cannot alias manager state.
// @preconditions None.
// @param excludeAccountID Account to skip; empty skips nothing.
// @param model Upstream model id whose quota family governs eligibility; empty checks all families.
// @param requiresPaidTier Reject free accounts when the model demands a paid tier.
// @param inUse Maps an account ID to the worker using it, so an unclaimed account is preferred.
// @returns A copy of the first eligible account.
// @throws ErrNoEligibleAccounts when no other account can serve the model.
// @invariants O(accounts); acquires accountsMu internally.
func (m *Manager) FindNextEligibleAccount(excludeAccountID string, model string, requiresPaidTier bool, inUse map[string]int) (*Account, error) {
	return m.findNextEligibleAccountLocked(excludeAccountID, model, requiresPaidTier, inUse)
}

// SwitchWorkerAccount installs targetAccountID's credential into one worker's HOME.
//
// @purpose Rebinds a single worker to another account.
// @rationale This previously ran `agy-auth switch`, which mutates the GLOBAL active account and then
// mirrored a token read from the gateway's own HOME. Two consequences: the worker still did not
// necessarily receive the requested account, and every other worker's next provisioning inherited the
// change. Installing the credential directly is the only version that affects exactly one worker.
// @preconditions workerID addresses an existing worker; targetAccountID exists in the vault.
// @param ctx Cancellation root for the export subprocess.
// @param workerID Worker to rebind.
// @param targetAccountID Account to install.
// @returns A wrapped error when the account is unknown or the credential cannot be installed.
// @throws ErrAccountNotFound, ErrCredentialExportFailed, ErrCredentialPayloadInvalid,
// ErrCredentialIdentityMatch.
// @invariants O(1) plus one subprocess; the worker keeps its previous credential on any error.
func (m *Manager) SwitchWorkerAccount(ctx context.Context, workerID int, targetAccountID string) error {
	if _, err := m.MaterializeWorkerCredential(ctx, workerID, targetAccountID); err != nil {
		return fmt.Errorf("failed to install credential for account %s on worker %d: %w",
			targetAccountID, workerID, err)
	}
	m.logger.Info("switched worker account successfully", "workerID", workerID, "accountID", targetAccountID)
	return nil
}

// FetchAgyUsage executes agy -p "/usage" --output-format json to pull live quota directly from Google Antigravity.
func (m *Manager) FetchAgyUsage(ctx context.Context, homeDir string) (*AgyUsageEnvelope, error) {
	out, err := m.ExecAgy(ctx, homeDir, "-p", "/usage", "--output-format", "json")
	if err != nil {
		return nil, fmt.Errorf("agy /usage failed: %w", err)
	}

	var env AgyUsageEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, fmt.Errorf("failed to unmarshal agy usage output: %w", err)
	}

	return &env, nil
}

// KnownQuotaPct reports the lowest remaining percentage across an account's known quota buckets.
//
// @purpose Lets the failover path distinguish a real quota exhaustion from a refusal that arrived
// while the account still has ample capacity.
// @rationale An upstream 429 is not always exhaustion: the agent returns one for policy refusals
// too. Cooling an account on that signal removes a working credential for the full cooldown window
// and recycles its worker for nothing. The snapshot this reads is the same one `GetAccounts` serves,
// so no subprocess runs on the failover hot path.
// @preconditions None; safe on an unknown id.
// @param accountID Account to inspect.
// @returns The minimum remaining percentage and true when at least one bucket is known, otherwise
// (0, false) so the caller falls back to the pre-existing penalty behaviour.
// @throws None.
// @invariants O(buckets); takes a read lock; never spawns a process.
func (m *Manager) KnownQuotaPct(accountID string) (float64, bool) {
	if m == nil || accountID == "" {
		return 0, false
	}
	for _, acc := range m.GetAccounts() {
		if acc.ID != accountID {
			continue
		}
		if !acc.QuotasKnown || len(acc.Quotas) == 0 {
			return 0, false
		}
		worst := -1.0
		for _, q := range acc.Quotas {
			if q.IsExhausted {
				return 0, true
			}
			if worst < 0 || q.RemainingPct < worst {
				worst = q.RemainingPct
			}
		}
		if worst < 0 {
			return 0, false
		}
		return worst, true
	}
	return 0, false
}

// GetQuota returns live per-account quota, optionally narrowed to one account.
//
// @purpose Gives operators and the pool the true quota of each account.
// @rationale The previous implementation asked `agy -p "/usage"` for whichever account the gateway
// process was authenticated as, then attached that single result to EVERY account in the response.
// With three accounts holding 65%/100%/100% the API reported 65/65/65, which made IsEligible and the
// admin surface act on fabricated numbers. `agy-auth usage --all --json` carries an identity per
// account, so the data can finally be attributed correctly.
// @preconditions m.cfg and the agy-auth binary path are configured.
// @param ctx Cancellation root for the subprocess.
// @param query Optional account id, email, or alias to narrow the result; empty returns all accounts.
// @returns Per-account quota in vault order; an empty response when no accounts are stored.
// @throws ErrAccountNotFound when query matches nothing; a wrapped error when the live fetch fails so
// the caller can distinguish "no data" from "empty vault".
// @invariants O(accounts × buckets); exactly one agy-auth subprocess; no fabricated cross-account data.
func (m *Manager) GetQuota(ctx context.Context, query string) (*AccountsQuotaResponse, error) {
	_ = m.Sync(ctx)

	accounts := m.GetAccounts()
	if len(accounts) == 0 {
		return &AccountsQuotaResponse{
			Timestamp: time.Now().UTC(),
			Total:     0,
			Accounts:  []AccountQuotaInfo{},
		}, nil
	}

	usageByAccount, usageErr := m.FetchAllAccountUsage(ctx)
	if usageErr != nil {
		// Report the fetch failure instead of silently fabricating percentages. Callers that can
		// tolerate stale data already hold the last polled status on the account itself.
		m.logger.Warn("failed to fetch per-account quota", "err", usageErr)
	}

	// Accounts `agy-auth usage` refuses to report on would otherwise render as unreadable here while
	// the poller (which does run the fallback) knows their real buckets — the admin view and the
	// rotation decisions then disagree. Resolve the same fallback so one call answers with the same
	// truth the scheduler acts on.
	fallbacks := m.ResolveQuotaFallbacks(ctx, usageByAccount)

	now := time.Now().UTC()
	result := make([]AccountQuotaInfo, 0, len(accounts))

	for _, acc := range accounts {
		if query != "" &&
			!strings.EqualFold(acc.ID, query) &&
			!strings.EqualFold(acc.Email, query) &&
			!strings.EqualFold(acc.Alias, query) {
			continue
		}

		planDisplay := m.planLabelFor(acc.ID)

		info := AccountQuotaInfo{
			ID:         acc.ID,
			Email:      acc.Email,
			Alias:      acc.Alias,
			Plan:       planDisplay,
			Active:     acc.Status == StatusHealthy,
			Status:     acc.Status,
			LastSynced: now,
		}
		if !acc.CooldownUntil.IsZero() {
			cd := acc.CooldownUntil
			info.CooldownUntil = &cd
		}

		// Only this account's own buckets may populate its entry. When the live fetch failed the
		// entry carries the last known status but no invented percentages.
		//
		// The knowledge flag must come from THIS call's fetch, not only from the account snapshot.
		// Reading the snapshot alone reproduces the fail-open whenever the last poll succeeded and the
		// current fetch is the one that failed: the snapshot still says "known", so the endpoint would
		// answer `quotas_known: true, quota_available: true` with zero buckets for an account the
		// provider just refused to report on.
		info.QuotasKnown = acc.QuotasKnown
		info.UsageError = acc.UsageError
		if usage, ok := usageByAccount[acc.ID]; ok {
			// A per-account usage_error in a successful fetch means this account's groups are absent,
			// which is the same unreadable state as a failed snapshot — treat it identically.
			if usage.UsageError != "" {
				if groups, recovered := fallbacks[acc.ID]; recovered {
					info.Groups = toQuotaGroupInfos(groups)
					info.Quotas = QuotaGroups(groups)
					info.QuotasKnown = true
					info.UsageError = ""
					info.QuotaAvailable = acc.Status == StatusHealthy && acc.Enabled
					for _, q := range info.Quotas {
						if q.IsExhausted {
							info.QuotaAvailable = false
							break
						}
					}
				} else {
					info.Groups = nil
					info.Quotas = nil
					info.QuotasKnown = false
					info.UsageError = usage.UsageError
					info.QuotaAvailable = false
				}
			} else {
				info.Groups = toQuotaGroupInfos(usage.Groups)
				info.Quotas = QuotaGroups(usage.Groups)
				info.QuotasKnown = true
				info.UsageError = ""
				// An unreadable quota is not "available": claiming it is what let an account the
				// provider refuses to report on look like it had unlimited capacity.
				info.QuotaAvailable = acc.Status == StatusHealthy && acc.Enabled
				for _, q := range info.Quotas {
					if q.IsExhausted {
						info.QuotaAvailable = false
						break
					}
				}
			}
		} else if !acc.QuotasKnown {
			// No live entry and no trusted snapshot: nothing is known about this account.
			info.QuotaAvailable = false
		}

		result = append(result, info)
	}

	if query != "" && len(result) == 0 {
		return nil, ErrAccountNotFound
	}

	return &AccountsQuotaResponse{
		Timestamp: now,
		Total:     len(result),
		Accounts:  result,
	}, nil
}

// toQuotaGroupInfos flattens wire groups into the API's presentation shape.
//
// @purpose Keeps the response type stable while the data source changed.
// @rationale The API contract exposes per-bucket detail; deriving it here means the handler does not
// need to know about the wire types.
// @preconditions groups came from agy-auth.
// @param groups Wire quota groups for one account.
// @returns One entry per group, each carrying its buckets.
// @throws None.
// @invariants O(groups × buckets); returns a non-nil slice.
func toQuotaGroupInfos(groups []AgyUsageGroup) []QuotaGroupInfo {
	out := make([]QuotaGroupInfo, 0, len(groups))
	for _, grp := range groups {
		gInfo := QuotaGroupInfo{
			Name:        grp.Name,
			Description: grp.Description,
			Buckets:     make([]QuotaBucketInfo, 0, len(grp.Buckets)),
		}
		for _, b := range grp.Buckets {
			var resetT time.Time
			if b.ResetTime != "" {
				resetT, _ = time.Parse(time.RFC3339, b.ResetTime)
			}
			pct := math.Round(b.RemainingFraction*10000) / 100
			gInfo.Buckets = append(gInfo.Buckets, QuotaBucketInfo{
				ID:                b.ID,
				Name:              b.Name,
				Description:       b.Description,
				Window:            b.Window,
				RemainingFraction: b.RemainingFraction,
				RemainingPct:      pct,
				ResetTime:         resetT,
				IsExhausted:       b.RemainingFraction <= 0,
			})
		}
		out = append(out, gInfo)
	}
	return out
}

// ProvisionWorkerHome creates an isolated worker HOME tree and installs that worker's credential.
//
// @purpose Prepares everything one worker needs before agy is spawned: three directories plus the
// credential for the account that worker is bound to.
// @rationale The previous implementation copied whichever token happened to exist under the gateway's
// own HOME, which made every worker authenticate as the same account no matter what the pool believed.
// It also performed a GLOBAL account switch as a side effect, so provisioning worker 3 could silently
// change the account used by every other worker. Credential installation is now explicit and bound to
// the worker's own account.
// @preconditions workerID addresses a worker index; accountID may be empty when the caller wants the
// directory tree only. m.cfg.Storage paths are configured.
// @param workerID Worker index whose directories are created.
// @param accountID Account whose credential is installed; empty leaves any existing credential alone.
// @returns The absolute home, temp, and workspace directories for this worker.
// @throws A wrapped mkdir error, ErrAccountNotFound for an unknown accountID, or any materialisation
// error. On failure the created directories remain but no partial credential is installed.
// @invariants O(1) filesystem work plus at most one subprocess; serialised by vaultMu so two workers
// cannot interleave credential installation.
func (m *Manager) ProvisionWorkerHome(workerID int, accountID string) (string, string, string, error) {
	m.vaultMu.Lock()
	defer m.vaultMu.Unlock()

	homeDir := filepath.Join(m.cfg.Storage.WorkersRootDir, fmt.Sprintf("w%d", workerID))
	tmpDir := filepath.Join(m.cfg.Storage.WorkersTmpDir, fmt.Sprintf("w%d", workerID))
	// Every worker shares ONE workspace root so the agent's own files survive an account rotation, a
	// worker recycle, and a restart. The per-worker directory that used to live here is what destroyed
	// context on every switch and fed the rewrite loop.
	workDir := m.cfg.Storage.EffectiveWorkspaceDir()
	if workDir == "" {
		return "", "", "", fmt.Errorf("storage.workspace_dir is not configured")
	}

	for _, d := range []string{homeDir, tmpDir, workDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return "", "", "", fmt.Errorf("failed to create directory %s: %w", d, err)
		}
	}
	// The shared workspace is where an external harness mounts its own files, so it must be traversable
	// by the mounted uid while remaining writable by this process only where the operator allows it.
	// 0755 is deliberate: the container image runs agy as a non-root uid and the host bind-mount must
	// be usable, so restricting it further would break external file CRUD.
	// The worker's TMPDIR holds the per-session MCP socket, and the directory is that socket's access
	// control: since TMPDIR is inherited by the child agent as its temp dir, it must not be readable
	// or traversable by another uid in the container, or any local process could invoke the consumer's
	// tools. Tightened after creation so an existing 0755 directory from an older build is fixed too.
	if err := os.Chmod(tmpDir, 0o700); err != nil {
		return "", "", "", fmt.Errorf("failed to restrict %s: %w", tmpDir, err)
	}

	if accountID == "" {
		// Directory-only provisioning: the caller installs a credential separately, so an existing
		// one must not be disturbed here.
		return homeDir, tmpDir, workDir, nil
	}

	if _, err := m.materializeLocked(context.Background(), workerID, accountID); err != nil {
		return "", "", "", err
	}

	return homeDir, tmpDir, workDir, nil
}

// SwitchAccount sets the vault's active account and makes every worker use it.
//
// @purpose Turns "switch" into an operation with an observable effect on traffic.
// @rationale `agy-auth switch` rewrites the global CLI credential and the keyring, which no worker
// reads: each worker authenticates with its own materialised token. Calling it alone therefore changed
// nothing about who serves a request, while the CLI and the admin API both reported success. The vault
// call is still made, because that is what `agy-auth list` reports as active and what an operator sees
// in `agy-auth current`; the worker rebind is what makes it real.
// @preconditions The vault holds an account matching query.
// @param ctx Cancellation root for the vault call and each credential install.
// @param query Account id, alias, email, or unique prefix as `agy-auth` accepts.
// @returns The resolved account id and the per-worker transitions. A resolution failure or an
// `ambiguous_query` from the vault is returned as an error and nothing is rebound.
// @throws The wrapped vault error; a worker rebind error after the vault already changed.
// @invariants At most one vault subprocess plus one per worker; never holds vaultMu across them.
func (m *Manager) SwitchAccount(ctx context.Context, query string) (string, error) {
	m.vaultMu.Lock()
	_, err := m.execAgyAuth(ctx, "switch", query)
	m.vaultMu.Unlock()
	if err != nil {
		return "", err
	}
	// Sync picks up the new `active` flag and any plan data, so the resolved id is known before
	// anything is installed on a worker.
	if syncErr := m.Sync(ctx); syncErr != nil {
		m.logger.Warn("vault resync after account switch failed", "err", syncErr)
	}
	_ = m.RefreshPlan(ctx)
	target := m.activeAccountID()
	return target, nil
}

// activeAccountID reports which vault account is flagged active, or "" when none is.
//
// @purpose Gives switch a concrete account to install on workers.
// @rationale `agy-auth switch` reports success without naming the resolved account, and a query can be
// an alias or a prefix, so the id has to be read back from the account set.
// @preconditions None.
// @param None.
// @returns The active account id, or "" when the vault has no active flag.
// @throws None.
// @invariants O(accounts); takes a read lock.
func (m *Manager) activeAccountID() string {
	for _, acc := range m.GetAccounts() {
		if acc.ActiveInVault {
			return acc.ID
		}
	}
	return ""
}

// RotateAccount changes the vault's active account.
//
// @purpose Keeps the vault coherent with the operator's intent.
// @rationale The worker-level rotation is performed by the pool, which knows eligibility and bindings.
// This only advances the vault so `agy-auth current` and the admin account list agree with what the
// workers are actually doing. A "no rotation needed" answer from the vault is not an error.
// @preconditions None.
// @param ctx Cancellation root for the subprocess.
// @returns None.
// @throws The wrapped vault error.
// @invariants One subprocess; never holds vaultMu across the resync.
func (m *Manager) RotateAccount(ctx context.Context) error {
	m.vaultMu.Lock()
	_, err := m.execAgyAuth(ctx, "rotate")
	m.vaultMu.Unlock()
	if err != nil {
		return err
	}
	if syncErr := m.Sync(ctx); syncErr != nil {
		m.logger.Warn("vault resync after account rotation failed", "err", syncErr)
	}
	_ = m.RefreshPlan(ctx)
	return nil
}

// RemoveAccount deletes an account from the vault and purges what this gateway installed for it.
//
// @purpose Makes deletion complete from the gateway's point of view, not just in the vault.
// @rationale `agy-auth remove` alone leaves two copies of the credential alive: the live mirror token
// under the gateway's HOME (unless `--logout` is passed) and the per-worker copy under each worker
// HOME. It also leaves this process's account set stale, so rotation kept offering the deleted
// account as a healthy candidate until a restart. Removing with `--logout` and resyncing immediately
// closes all three.
// @preconditions The vault holds an account matching query.
// @param ctx Cancellation root for the removal and the follow-up resync.
// @param query Account id, alias, or email as `agy-auth` accepts.
// @returns A wrapped error when the removal subprocess fails. A failed resync is logged, not returned:
// the account is already gone and reporting failure would invite a second removal attempt.
// @throws None directly; the subprocess error is wrapped.
// @invariants Two subprocesses at most; never holds vaultMu across the resync.
func (m *Manager) RemoveAccount(ctx context.Context, query string) error {
	m.vaultMu.Lock()
	_, err := m.execAgyAuth(ctx, "remove", query, "--yes", "--logout")
	m.vaultMu.Unlock()
	if err != nil {
		return err
	}

	// Sync must run outside vaultMu (it takes the same lock) and is what purges the worker credential
	// copies of the account that just disappeared.
	if syncErr := m.Sync(ctx); syncErr != nil {
		m.logger.Warn("vault resync after account removal failed; the next poll will retry",
			"err", syncErr)
	}
	return nil
}

// SetAlias executes agy-auth alias set <query> <alias>.
func (m *Manager) SetAlias(ctx context.Context, query, alias string) error {
	m.vaultMu.Lock()
	defer m.vaultMu.Unlock()

	_, err := m.execAgyAuth(ctx, "alias", "set", query, alias)
	return err
}

// ClearAlias executes agy-auth alias clear <query>.
func (m *Manager) ClearAlias(ctx context.Context, query string) error {
	m.vaultMu.Lock()
	defer m.vaultMu.Unlock()

	_, err := m.execAgyAuth(ctx, "alias", "clear", query)
	return err
}

// Status executes agy-auth status --json.
func (m *Manager) Status(ctx context.Context) ([]byte, error) {
	m.vaultMu.Lock()
	defer m.vaultMu.Unlock()

	return m.execAgyAuth(ctx, "status", "--json")
}

// Usage executes agy -p "/usage" --output-format json and returns the live output directly from Antigravity.
func (m *Manager) Usage(ctx context.Context) ([]byte, error) {
	out, err := m.ExecAgy(ctx, "", "-p", "/usage", "--output-format", "json")
	if err != nil {
		if len(m.cfg.Storage.WorkersRootDir) > 0 {
			w0Home := filepath.Join(m.cfg.Storage.WorkersRootDir, "w0")
			return m.ExecAgy(ctx, w0Home, "-p", "/usage", "--output-format", "json")
		}
		return nil, err
	}
	return out, nil
}

// Clean executes agy-auth clean [--dry-run].
func (m *Manager) Clean(ctx context.Context, dryRun bool) ([]byte, error) {
	m.vaultMu.Lock()
	defer m.vaultMu.Unlock()

	args := []string{"clean"}
	if dryRun {
		args = append(args, "--dry-run")
	}
	return m.execAgyAuth(ctx, args...)
}

// ExecAgy executes the agy CLI binary under the given HOME directory and returns its stdout,
// applying transient retries with exponential backoff on SQLite lock contention or fork exhaustion.
func (m *Manager) ExecAgy(ctx context.Context, homeDir string, args ...string) ([]byte, error) {
	if homeDir == "" {
		homeDir = os.Getenv("HOME")
	}
	if m.agyForTest != nil {
		return m.agyForTest(ctx, homeDir, args)
	}

	maxRetries := m.cfg.Execution.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		cmd := exec.CommandContext(ctx, m.cfg.Binaries.AgyPath, args...)
		env := append(os.Environ(),
			fmt.Sprintf("HOME=%s", homeDir),
			fmt.Sprintf("XDG_CONFIG_HOME=%s", filepath.Join(homeDir, ".config")),
			"DBUS_SESSION_BUS_ADDRESS=",
			"AGY_AUTH_NO_UPDATE_CHECK=1",
		)
		if proxyEnvs := m.cfg.Proxy.EnvSlice(); len(proxyEnvs) > 0 {
			env = append(env, proxyEnvs...)
		}
		cmd.Env = env

		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		err := cmd.Run()
		if err == nil {
			return stdout.Bytes(), nil
		}

		runErr := fmt.Errorf("agy %v failed (%w): %s", args, err, stderr.String())
		lastErr = runErr

		if retry.ClassifyFailure(runErr) == retry.FailureClassTransientCLI && attempt < maxRetries {
			delay := retry.ComputeBackoff(attempt, 100*time.Millisecond, 2*time.Second)
			m.logger.Warn("agy execution encountered transient lock; retrying", "attempt", attempt+1, "delay", delay, "err", runErr)
			if sleepErr := retry.SleepWithContext(ctx, delay); sleepErr != nil {
				return nil, sleepErr
			}
			continue
		}

		return nil, runErr
	}

	return nil, lastErr
}

func (m *Manager) execAgyAuth(ctx context.Context, args ...string) ([]byte, error) {
	if m.agyAuthForTest != nil {
		return m.agyAuthForTest(ctx, args)
	}
	maxRetries := m.cfg.Execution.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		cmd := exec.CommandContext(ctx, m.cfg.Binaries.AgyAuthPath, args...)
		env := append(os.Environ(),
			fmt.Sprintf("XDG_CONFIG_HOME=%s", m.cfg.Storage.VaultXDGConfigDir),
			"AGY_AUTH_NO_UPDATE_CHECK=1",
			"DBUS_SESSION_BUS_ADDRESS=",
		)
		if proxyEnvs := m.cfg.Proxy.EnvSlice(); len(proxyEnvs) > 0 {
			env = append(env, proxyEnvs...)
		}
		cmd.Env = env

		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		err := cmd.Run()
		if err == nil {
			return stdout.Bytes(), nil
		}

		runErr := fmt.Errorf("agy-auth %v failed (%w): %s", args, err, stderr.String())
		lastErr = runErr

		if retry.ClassifyFailure(runErr) == retry.FailureClassTransientCLI && attempt < maxRetries {
			delay := retry.ComputeBackoff(attempt, 100*time.Millisecond, 2*time.Second)
			m.logger.Warn("agy-auth execution encountered transient lock; retrying", "attempt", attempt+1, "delay", delay, "err", runErr)
			if sleepErr := retry.SleepWithContext(ctx, delay); sleepErr != nil {
				return nil, sleepErr
			}
			continue
		}

		return nil, runErr
	}

	return nil, lastErr
}
