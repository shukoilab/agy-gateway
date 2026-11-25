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
	Overview       string                `json:"overview,omitempty" example:"Gemini: 70.38% (5h reset in 23m)"`
	LastSynced     time.Time             `json:"last_synced" example:"2026-11-24T10:00:00Z"`
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

	// fetchUsageForTest overrides FetchAllAccountUsage during tests.
	//
	// @purpose Lets poller behaviour be driven by controlled payloads.
	// @rationale Poll logic is a pure function of the payload and account set; stubbing only the fetch
	// keeps the real status-transition code under test.
	// @invariants Assigned only in tests, before concurrent use begins.
	fetchUsageForTest func(ctx context.Context) (map[string]AccountUsage, error)
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
func (m *Manager) Sync(ctx context.Context) error {
	m.vaultMu.Lock()
	defer m.vaultMu.Unlock()

	var out []byte
	var err error
	if m.listForTest != nil {
		out, err = m.listForTest(ctx)
	} else {
		out, err = m.execAgyAuth(ctx, "list", "--all", "--json")
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
		Expired    bool   `json:"expired"`
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
	defer m.accountsMu.Unlock()

	// Preserve runtime state across a resync. Sync reconstructs accounts from the vault, which knows
	// nothing about quota cooldowns, so a naive rebuild would silently reset a cooling account to
	// HEALTHY — making the admin list disagree with the poller and letting a spent account be leased
	// again. Only vault-owned fields are refreshed; runtime-owned fields are carried forward.
	previous := make(map[string]Account, len(m.accounts))
	for _, existing := range m.accounts {
		previous[existing.ID] = existing
	}

	newAccounts := make([]Account, 0, len(rawAccounts))
	for _, raw := range rawAccounts {
		tier := raw.Tier
		if tier == "" && raw.Plan != "" {
			tier = strings.ToLower(raw.Plan)
		}
		status := StatusHealthy
		vaultExpired := false
		if raw.Expired {
			status = StatusAuthExpired
			vaultExpired = true
		}

		acc := Account{
			ID:             raw.ID,
			Email:          raw.Email,
			Alias:          raw.Alias,
			AuthMethod:     raw.AuthMethod,
			Tier:           tier,
			Status:         status,
			Enabled:        true,
			VaultExpired:   vaultExpired,
			AssignedWorker: -1,
			Quotas:         make(map[string]QuotaGroup),
			LastUsed:       time.Now(),
		}

		if prior, ok := previous[raw.ID]; ok {
			// Carry forward everything the vault cannot know: live quota, both the account-wide and
			// the per-family cooldown windows, worker binding, and last-use time. Eligibility reads
			// FamilyCooldowns, so dropping it here would silently re-open a family the poller had
			// just closed — the same class of state loss this carry-forward exists to prevent.
			acc.Quotas = prior.Quotas
			acc.CooldownUntil = prior.CooldownUntil
			acc.FamilyCooldowns = prior.FamilyCooldowns
			acc.AssignedWorker = prior.AssignedWorker
			acc.LastUsed = prior.LastUsed
			switch {
			case status == StatusAuthExpired:
				// The vault itself says this credential is dead: only `agy-auth login` fixes it, so any
				// runtime retry window inherited above must not make it look recoverable.
				acc.CooldownUntil = time.Time{}
				acc.VaultExpired = true
			case prior.Status == StatusAuthExpired && prior.VaultExpired:
				// The vault previously reported this credential expired and now reports it healthy: the
				// operator re-logged in. Keeping the carried-forward AUTH_EXPIRED would make that login
				// invisible until a restart, which is the whole defect this branch exists to prevent.
				m.logger.Info("vault no longer reports the credential expired; restoring account",
					"accountID", acc.ID)
				acc.Status = StatusHealthy
				acc.CooldownUntil = time.Time{}
				acc.VaultExpired = false
			default:
				// Runtime-owned state: keep whatever the fault/retry machinery recorded.
				acc.Status = prior.Status
				acc.VaultExpired = prior.VaultExpired
			}
		}
		newAccounts = append(newAccounts, acc)
	}
	m.accounts = newAccounts
	return nil
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
func (m *Manager) SetAccountsForTest(accounts []Account) {
	m.accountsMu.Lock()
	defer m.accountsMu.Unlock()
	m.accounts = accounts
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
				acc.VaultExpired = false
				acc.FamilyCooldowns = nil
				acc.CooldownUntil = time.Time{}
			case StatusAuthExpired:
				// Bucket windows are irrelevant to a credential fault. The expiry is bounded, not
				// permanent: a runtime-detected failure (a bad export, an upstream 401) may clear on its
				// own, and a permanently-clean account would otherwise disappear from the vault with no
				// recovery path. `AuthExpiredRetryInterval` is how long it stays out of service; a
				// vault-reported expiry is applied by Sync with a zero window and stays permanent.
				// Provenance is runtime, so Sync is free to restore the account once the vault agrees.
				acc.VaultExpired = false
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
				acc.VaultExpired = false
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
	pick := func(allowShared bool) *Account {
		for _, acc := range m.accounts {
			if acc.ID == excludeAccountID {
				continue
			}
			if _, claimed := inUse[acc.ID]; !allowShared && claimed {
				continue
			}
			if acc.IsEligibleForModel(model, requiresPaidTier, m.cfg.Accounts.MinRemainingQuotaPct) {
				// Deep-copy before returning: the caller keeps this value past the lock, and the
				// poller mutates the live maps under accountsMu.
				copyAcc := acc
				copyAcc.Quotas = copyQuotaGroups(acc.Quotas)
				copyAcc.FamilyCooldowns = copyCooldowns(acc.FamilyCooldowns)
				return &copyAcc
			}
		}
		return nil
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

	now := time.Now().UTC()
	result := make([]AccountQuotaInfo, 0, len(accounts))

	for _, acc := range accounts {
		if query != "" &&
			!strings.EqualFold(acc.ID, query) &&
			!strings.EqualFold(acc.Email, query) &&
			!strings.EqualFold(acc.Alias, query) {
			continue
		}

		planDisplay := acc.Tier
		if planDisplay == "" {
			planDisplay = "free"
		}
		planDisplay = strings.ToUpper(planDisplay[:1]) + planDisplay[1:]

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
		if usage, ok := usageByAccount[acc.ID]; ok {
			info.Groups = toQuotaGroupInfos(usage.Groups)
			info.Quotas = QuotaGroups(usage.Groups)
			info.QuotaAvailable = acc.Status == StatusHealthy && acc.Enabled
			for _, q := range info.Quotas {
				if q.IsExhausted {
					info.QuotaAvailable = false
					break
				}
			}
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
	workDir := filepath.Join(m.cfg.Storage.WorkspacesRootDir, fmt.Sprintf("w%d", workerID))

	for _, d := range []string{homeDir, tmpDir, workDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return "", "", "", fmt.Errorf("failed to create directory %s: %w", d, err)
		}
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

// SwitchAccount executes agy-auth switch <query>.
func (m *Manager) SwitchAccount(ctx context.Context, query string) error {
	m.vaultMu.Lock()
	defer m.vaultMu.Unlock()

	_, err := m.execAgyAuth(ctx, "switch", query)
	return err
}

// RotateAccount executes agy-auth rotate.
func (m *Manager) RotateAccount(ctx context.Context) error {
	m.vaultMu.Lock()
	defer m.vaultMu.Unlock()

	_, err := m.execAgyAuth(ctx, "rotate")
	return err
}

// RemoveAccount executes agy-auth remove <query> --yes.
func (m *Manager) RemoveAccount(ctx context.Context, query string) error {
	m.vaultMu.Lock()
	defer m.vaultMu.Unlock()

	_, err := m.execAgyAuth(ctx, "remove", query, "--yes")
	return err
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
