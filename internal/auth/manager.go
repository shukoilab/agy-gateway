package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	out, err := m.execAgyAuth(ctx, "list", "--all", "--json")
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
	if err := json.Unmarshal(out, &envFormat); err == nil && len(envFormat.Data.Accounts) > 0 {
		rawAccounts = envFormat.Data.Accounts
	} else if err := json.Unmarshal(out, &rawAccounts); err != nil {
		m.logger.Debug("unmarshaling accounts output", "raw", string(out), "err", err)
	}

	m.accountsMu.Lock()
	defer m.accountsMu.Unlock()

	newAccounts := make([]Account, 0, len(rawAccounts))
	for _, raw := range rawAccounts {
		tier := raw.Tier
		if tier == "" && raw.Plan != "" {
			tier = strings.ToLower(raw.Plan)
		}
		status := StatusHealthy
		if raw.Expired {
			status = StatusAuthExpired
		}

		acc := Account{
			ID:             raw.ID,
			Email:          raw.Email,
			Alias:          raw.Alias,
			AuthMethod:     raw.AuthMethod,
			Tier:           tier,
			Status:         status,
			Enabled:        true,
			AssignedWorker: -1,
			Quotas:         make(map[string]QuotaGroup),
			LastUsed:       time.Now(),
		}
		newAccounts = append(newAccounts, acc)
	}
	m.accounts = newAccounts
	return nil
}

// GetAccounts returns a snapshot of all tracked accounts.
func (m *Manager) GetAccounts() []Account {
	m.accountsMu.RLock()
	defer m.accountsMu.RUnlock()

	out := make([]Account, len(m.accounts))
	copy(out, m.accounts)
	return out
}

// SetAccountsForTest injects accounts directly into the manager for unit and integration testing.
func (m *Manager) SetAccountsForTest(accounts []Account) {
	m.accountsMu.Lock()
	defer m.accountsMu.Unlock()
	m.accounts = accounts
}

// MarkAccountStatus updates the status and optional cooldown duration of a specific account by ID.
func (m *Manager) MarkAccountStatus(accountID string, status AccountStatus, cooldown time.Duration) {
	m.accountsMu.Lock()
	defer m.accountsMu.Unlock()

	for i := range m.accounts {
		if m.accounts[i].ID == accountID {
			m.accounts[i].Status = status
			if cooldown > 0 {
				m.accounts[i].CooldownUntil = time.Now().Add(cooldown)
			} else {
				m.accounts[i].CooldownUntil = time.Time{}
			}
			m.logger.Info("marked account status", "accountID", accountID, "status", status, "cooldown", cooldown)
			return
		}
	}
}

// FindNextEligibleAccount locates the next healthy, non-exhausted account excluding excludeAccountID.
func (m *Manager) FindNextEligibleAccount(excludeAccountID string, requiresPaidTier bool) (*Account, error) {
	m.accountsMu.Lock()
	defer m.accountsMu.Unlock()

	now := time.Now()
	// Auto-recover expired cooldown accounts back to healthy
	for i := range m.accounts {
		if !m.accounts[i].CooldownUntil.IsZero() && now.After(m.accounts[i].CooldownUntil) {
			if m.accounts[i].Status == StatusCooldown5h || m.accounts[i].Status == StatusCooldownWeekly {
				m.logger.Info("account cooldown expired; recovering to healthy", "accountID", m.accounts[i].ID)
				m.accounts[i].Status = StatusHealthy
				m.accounts[i].CooldownUntil = time.Time{}
			}
		}
	}

	for _, acc := range m.accounts {
		if acc.ID == excludeAccountID {
			continue
		}
		if acc.IsEligible(requiresPaidTier, m.cfg.Accounts.MinRemainingQuotaPct) {
			copyAcc := acc
			return &copyAcc, nil
		}
	}

	return nil, ErrNoEligibleAccounts
}

// SwitchWorkerAccount switches the active account in agy-auth and syncs credentials into the worker's home sandbox.
func (m *Manager) SwitchWorkerAccount(ctx context.Context, workerID int, targetAccountID string) error {
	m.vaultMu.Lock()
	defer m.vaultMu.Unlock()

	if _, err := m.execAgyAuth(ctx, "switch", targetAccountID); err != nil {
		return fmt.Errorf("failed to switch agy-auth account to %s: %w", targetAccountID, err)
	}

	// Update token mirror in worker home
	homeDir := filepath.Join(m.cfg.Storage.WorkersRootDir, fmt.Sprintf("w%d", workerID))
	dstTokenDir := filepath.Join(homeDir, ".gemini", "antigravity-cli")
	_ = os.MkdirAll(dstTokenDir, 0755)
	dstToken := filepath.Join(dstTokenDir, "antigravity-oauth-token")

	tokenCandidates := m.tokenCandidates()

	for _, tokenPath := range tokenCandidates {
		if _, err := os.Stat(tokenPath); err == nil {
			_ = copyFile(tokenPath, dstToken)
			break
		}
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

// GetQuota returns live, accurate quota metrics from agy -p "/usage" across all accounts or for a specific account.
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

	// Fetch live quota directly via agy -p "/usage" --output-format json
	usageEnv, err := m.FetchAgyUsage(ctx, "")
	if err != nil {
		if len(m.cfg.Storage.WorkersRootDir) > 0 {
			w0Home := filepath.Join(m.cfg.Storage.WorkersRootDir, "w0")
			usageEnv, _ = m.FetchAgyUsage(ctx, w0Home)
		}
	}

	now := time.Now().UTC()
	var parsedGroups []QuotaGroupInfo
	quotasMap := make(map[string]QuotaGroup)
	allAvailable := true

	if usageEnv != nil && len(usageEnv.Command.Data.Groups) > 0 {
		for _, grp := range usageEnv.Command.Data.Groups {
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
				isExh := b.RemainingFraction <= 0
				if isExh {
					allAvailable = false
				}

				bInfo := QuotaBucketInfo{
					ID:                b.ID,
					Name:              b.Name,
					Description:       b.Description,
					Window:            b.Window,
					RemainingFraction: b.RemainingFraction,
					RemainingPct:      pct,
					ResetTime:         resetT,
					IsExhausted:       isExh,
				}
				gInfo.Buckets = append(gInfo.Buckets, bInfo)

				bucketKey := b.ID
				if bucketKey == "" {
					bucketKey = fmt.Sprintf("%s-%s", strings.ToLower(grp.Name), b.Window)
				}
				quotasMap[bucketKey] = QuotaGroup{
					Name:         fmt.Sprintf("%s - %s", grp.Name, b.Name),
					RemainingPct: pct,
					ResetTime:    resetT,
					IsExhausted:  isExh,
				}
			}
			parsedGroups = append(parsedGroups, gInfo)
		}
	}

	overview := ""
	if usageEnv != nil {
		overview = strings.TrimSpace(usageEnv.Response)
	}

	result := make([]AccountQuotaInfo, 0, len(accounts))
	for _, acc := range accounts {
		planDisplay := acc.Tier
		if planDisplay == "" {
			planDisplay = "free"
		}
		if len(planDisplay) > 0 {
			planDisplay = strings.ToUpper(planDisplay[:1]) + planDisplay[1:]
		}

		info := AccountQuotaInfo{
			ID:             acc.ID,
			Email:          acc.Email,
			Alias:          acc.Alias,
			Plan:           planDisplay,
			Active:         acc.Status == StatusHealthy,
			Status:         acc.Status,
			QuotaAvailable: allAvailable && acc.Status == StatusHealthy && acc.Enabled,
			Groups:         parsedGroups,
			Quotas:         quotasMap,
			Overview:       overview,
			LastSynced:     now,
		}
		if !acc.CooldownUntil.IsZero() {
			cd := acc.CooldownUntil
			info.CooldownUntil = &cd
		}

		if query != "" {
			if strings.EqualFold(acc.ID, query) || strings.EqualFold(acc.Email, query) || strings.EqualFold(acc.Alias, query) {
				result = append(result, info)
			}
		} else {
			result = append(result, info)
		}
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

// ProvisionWorkerHome copies central credentials to an isolated worker HOME directory.
func (m *Manager) ProvisionWorkerHome(workerID int) (string, string, string, error) {
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

	// Ensure token mirror is written via switch if accounts exist
	accounts := m.GetAccounts()
	srcToken := m.tokenCandidates()[0]
	if _, err := os.Stat(srcToken); err != nil && len(accounts) > 0 {
		_, _ = m.execAgyAuth(context.Background(), "switch", accounts[0].ID)
	}

	tokenCandidates := m.tokenCandidates()

	dstTokenDir := filepath.Join(homeDir, ".gemini", "antigravity-cli")
	_ = os.MkdirAll(dstTokenDir, 0755)
	dstToken := filepath.Join(dstTokenDir, "antigravity-oauth-token")

	for _, tokenPath := range tokenCandidates {
		if _, err := os.Stat(tokenPath); err == nil {
			_ = copyFile(tokenPath, dstToken)
			break
		}
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

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// tokenCandidates returns the ordered list of filesystem locations that may hold the
// Antigravity OAuth token, highest priority first.
//
// @purpose Removes the hardcoded /home/agygw development path (Zero-Hardcoding invariant).
// @rationale The vault directory is the authoritative location and is derived from configuration;
// HOME is only a last-resort convenience for bare-metal runs.
// @preconditions m.cfg must be non-nil.
// @returns Two or three absolute paths; the first is the preferred source.
// @throws None.
// @invariants O(1), allocation-bounded, no filesystem access.
func (m *Manager) tokenCandidates() []string {
	rel := []string{"gemini", "antigravity-cli", "antigravity-oauth-token"}
	candidates := []string{}
	if m.cfg != nil && m.cfg.Storage.VaultXDGConfigDir != "" {
		candidates = append(candidates, filepath.Join(m.cfg.Storage.VaultXDGConfigDir, filepath.Join(rel...)))
	}
	if home := os.Getenv("HOME"); home != "" {
		candidates = append(candidates, filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token"))
	}
	if len(candidates) == 0 {
		candidates = append(candidates, filepath.Join(os.TempDir(), "agy-gateway-missing-token"))
	}
	return candidates
}
