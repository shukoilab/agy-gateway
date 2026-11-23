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
)

var ErrAccountNotFound = errors.New("account not found")

// AgyUsageEnvelope mirrors the stdout from agy -p "/usage" --output-format json.
type AgyUsageEnvelope struct {
	ConversationID string `json:"conversation_id"`
	Status         string `json:"status"`
	Response       string `json:"response"`
	Command        struct {
		Name string `json:"name"`
		Data struct {
			Description string          `json:"description"`
			Groups      []AgyUsageGroup `json:"groups"`
		} `json:"data"`
	} `json:"command"`
}

type AgyUsageGroup struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Buckets     []AgyUsageBucket `json:"buckets"`
}

type AgyUsageBucket struct {
	ID                string  `json:"id"`
	Name              string  `json:"name"`
	Description       string  `json:"description"`
	Window            string  `json:"window"` // "weekly" or "5h"
	RemainingFraction float64 `json:"remaining_fraction"`
	ResetTime         string  `json:"reset_time"`
}

// QuotaBucketInfo represents structured quota metrics for an individual window bucket.
type QuotaBucketInfo struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	Description       string    `json:"description,omitempty"`
	Window            string    `json:"window"` // "weekly" or "5h"
	RemainingFraction float64   `json:"remaining_fraction"`
	RemainingPct      float64   `json:"remaining_pct"` // e.g. 95.68
	ResetTime         time.Time `json:"reset_time"`
	IsExhausted       bool      `json:"is_exhausted"`
}

// QuotaGroupInfo groups quota buckets by model category (e.g. Gemini Models, Claude/GPT).
type QuotaGroupInfo struct {
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Buckets     []QuotaBucketInfo `json:"buckets"`
}

// AccountQuotaInfo represents enriched live quota status for an individual account.
type AccountQuotaInfo struct {
	ID             string                `json:"id"`
	Email          string                `json:"email"`
	Alias          string                `json:"alias,omitempty"`
	Plan           string                `json:"plan"`
	Active         bool                  `json:"active"`
	Status         AccountStatus         `json:"status"`
	QuotaAvailable bool                  `json:"quota_available"`
	CooldownUntil  *time.Time            `json:"cooldown_until,omitempty"`
	Groups         []QuotaGroupInfo      `json:"groups"`
	Quotas         map[string]QuotaGroup `json:"quotas,omitempty"`
	Overview       string                `json:"overview,omitempty"`
	LastSynced     time.Time             `json:"last_synced"`
}

// AccountsQuotaResponse wraps the list of accounts quota data.
type AccountsQuotaResponse struct {
	Timestamp time.Time          `json:"timestamp"`
	Total     int                `json:"total"`
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
	srcToken := "/home/agygw/.gemini/antigravity-cli/antigravity-oauth-token"
	if _, err := os.Stat(srcToken); err != nil && len(accounts) > 0 {
		_, _ = m.execAgyAuth(context.Background(), "switch", accounts[0].ID)
	}

	tokenCandidates := []string{
		srcToken,
		filepath.Join(m.cfg.Storage.VaultXDGConfigDir, "gemini", "antigravity-cli", "antigravity-oauth-token"),
		filepath.Join(os.Getenv("HOME"), ".gemini", "antigravity-cli", "antigravity-oauth-token"),
	}

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

// ExecAgy executes the agy CLI binary under the given HOME directory and returns its stdout.
func (m *Manager) ExecAgy(ctx context.Context, homeDir string, args ...string) ([]byte, error) {
	if homeDir == "" {
		homeDir = os.Getenv("HOME")
	}

	cmd := exec.CommandContext(ctx, m.cfg.Binaries.AgyPath, args...)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("HOME=%s", homeDir),
		fmt.Sprintf("XDG_CONFIG_HOME=%s", filepath.Join(homeDir, ".config")),
		"DBUS_SESSION_BUS_ADDRESS=",
		"AGY_AUTH_NO_UPDATE_CHECK=1",
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("agy %v failed (%w): %s", args, err, stderr.String())
	}
	return stdout.Bytes(), nil
}

func (m *Manager) execAgyAuth(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, m.cfg.Binaries.AgyAuthPath, args...)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("XDG_CONFIG_HOME=%s", m.cfg.Storage.VaultXDGConfigDir),
		"AGY_AUTH_NO_UPDATE_CHECK=1",
		"DBUS_SESSION_BUS_ADDRESS=",
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("agy-auth %v failed (%w): %s", args, err, stderr.String())
	}
	return stdout.Bytes(), nil
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
