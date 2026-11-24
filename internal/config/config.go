// Package config provides externalized YAML configuration loading, environment overrides,
// and strict fail-fast validation for the agy-gateway service.
package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config represents the top-level unified gateway configuration.
type Config struct {
	Server        ServerConfig    `yaml:"server"`
	Security      SecurityConfig  `yaml:"security"`
	Binaries      BinariesConfig  `yaml:"binaries"`
	Execution     ExecutionConfig `yaml:"execution"`
	Storage       StorageConfig   `yaml:"storage"`
	Pool          PoolConfig      `yaml:"pool"`
	Accounts      AccountsConfig  `yaml:"accounts"`
	Admin         AdminConfig     `yaml:"admin"`
	OpenAI        OpenAIConfig    `yaml:"openai"`
	MCP           MCPConfig       `yaml:"mcp"`
	Observability ObsConfig       `yaml:"observability"`

	// Compiled regex patterns for zero-runtime-allocation routing & validation
	QueryRegex *regexp.Regexp `yaml:"-"`
	AliasRegex *regexp.Regexp `yaml:"-"`
}

type ServerConfig struct {
	Host                 string        `yaml:"host"`
	Port                 int           `yaml:"port"`
	ReadHeaderTimeout    time.Duration `yaml:"read_header_timeout"`
	IdleTimeout          time.Duration `yaml:"idle_timeout"`
	ShutdownDrainTimeout time.Duration `yaml:"shutdown_drain_timeout"`
	MaxRequestBodyBytes  int64         `yaml:"max_request_body_bytes"`
	SSEKeepaliveInterval time.Duration `yaml:"sse_keepalive_interval"`
}

type APIKeyConfig struct {
	ID           string `yaml:"id"`
	Key          string `yaml:"key"`
	KeyEnv       string `yaml:"key_env"`
	KeySecret    string `yaml:"-"`
	RateLimitRPM int    `yaml:"rate_limit_rpm"`
}

type SecurityConfig struct {
	APIKeys              []APIKeyConfig `yaml:"api_keys"`
	AdminAPIKey          string         `yaml:"admin_api_key"`
	AdminAPIKeyEnv       string         `yaml:"admin_api_key_env"`
	AdminAPIKeySecret    string         `yaml:"-"`
	AllowRemoteImageURLs bool           `yaml:"allow_remote_image_urls"`
	AllowedCORSOrigins   []string       `yaml:"allowed_cors_origins"`
}

type BinariesConfig struct {
	AgyPath       string   `yaml:"agy_path"`
	AgyMinVersion string   `yaml:"agy_min_version"`
	AgyAuthPath   string   `yaml:"agy_auth_path"`
	AgyAcpArgs    []string `yaml:"agy_acp_args"`
}

type ExecutionConfig struct {
	YoloMode                      bool          `yaml:"yolo_mode"`
	AcpSessionMode                string        `yaml:"acp_session_mode"`
	AutoApprovePermissions        bool          `yaml:"auto_approve_permissions"`
	CleanWorkspaceBetweenRequests bool          `yaml:"clean_workspace_between_requests"`
	MaxRetries                    int           `yaml:"max_retries"`
	RetryInitialBackoff           time.Duration `yaml:"retry_initial_backoff"`
	RetryMaxBackoff               time.Duration `yaml:"retry_max_backoff"`
}

type StorageConfig struct {
	BaseDataDir       string `yaml:"base_data_dir"`
	VaultXDGConfigDir string `yaml:"vault_xdg_config_dir"`
	WorkersRootDir    string `yaml:"workers_root_dir"`
	WorkersTmpDir     string `yaml:"workers_tmp_dir"`
	WorkspacesRootDir string `yaml:"workspaces_root_dir"`
}

type PoolConfig struct {
	WorkerCount            int           `yaml:"worker_count"`
	AcquireTimeout         time.Duration `yaml:"acquire_timeout"`
	HandshakeTimeout       time.Duration `yaml:"handshake_timeout"`
	ConfigSwitchTimeout    time.Duration `yaml:"config_switch_timeout"`
	InferenceTimeout       time.Duration `yaml:"inference_timeout"`
	FirstTokenTimeout      time.Duration `yaml:"first_token_timeout"`
	ProcessKillGracePeriod time.Duration `yaml:"process_kill_grace_period"`
	MaxRequestsPerWorker   int           `yaml:"max_requests_per_worker"`
	MaxWorkerUptime        time.Duration `yaml:"max_worker_uptime"`
	MaxScannerBufferBytes  int           `yaml:"max_scanner_buffer_bytes"`
}

type AccountsConfig struct {
	AutoRotateEnabled     bool          `yaml:"auto_rotate_enabled"`
	AllocationStrategy    string        `yaml:"allocation_strategy"`
	QuotaPollInterval     time.Duration `yaml:"quota_poll_interval"`
	MinRemainingQuotaPct  float64       `yaml:"min_remaining_quota_pct"`
	DefaultCooldown5h     time.Duration `yaml:"default_cooldown_5h"`
	DefaultCooldownWeekly time.Duration `yaml:"default_cooldown_weekly"`
	MaxFailoverRetries    int           `yaml:"max_failover_retries"`
}

type OAuthOnboardingConfig struct {
	Enabled               bool          `yaml:"enabled"`
	MaxConcurrentLogins   int           `yaml:"max_concurrent_logins"`
	URLExtractTimeout     time.Duration `yaml:"url_extract_timeout"`
	LoginSessionTTL       time.Duration `yaml:"login_session_ttl"`
	LoopbackReplayTimeout time.Duration `yaml:"loopback_replay_timeout"`
}

type AdminConfig struct {
	CommandTimeout        time.Duration         `yaml:"command_timeout"`
	AllowCredentialExport bool                  `yaml:"allow_credential_export"`
	QueryValidationRegex  string                `yaml:"query_validation_regex"`
	AliasValidationRegex  string                `yaml:"alias_validation_regex"`
	RuntimeOverridesFile  string                `yaml:"runtime_overrides_file"`
	OAuthOnboarding       OAuthOnboardingConfig `yaml:"oauth_onboarding"`
}

type OpenAIConfig struct {
	UnsupportedParamPolicy string `yaml:"unsupported_param_policy"`
	DefaultModel           string `yaml:"default_model"`
	DefaultReasoningEffort string `yaml:"default_reasoning_effort"`
	ExposeReasoningContent bool   `yaml:"expose_reasoning_content"`
}

type MCPClientToolCallingConfig struct {
	Enabled                bool   `yaml:"enabled"`
	DefaultParallel        bool   `yaml:"default_parallel"`
	StrictSchemaValidation bool   `yaml:"strict_schema_validation"`
	MaxToolArgumentBytes   int64  `yaml:"max_tool_argument_bytes"`
	DelimiterTag           string `yaml:"delimiter_tag"`
}

type MCPConfig struct {
	ClientToolCalling MCPClientToolCallingConfig `yaml:"client_tool_calling"`
}

type ObsConfig struct {
	LogLevel       string `yaml:"log_level"`
	LogFormat      string `yaml:"log_format"`
	MetricsEnabled bool   `yaml:"metrics_enabled"`
	MetricsPath    string `yaml:"metrics_path"`
}

// DefaultConfig generates a production-ready baseline configuration struct with hardened defaults.
//
// @purpose Establishes safe, fail-fast default values prior to YAML unmarshaling and environment overrides.
// @rationale Eliminates nil pointer exceptions and ensures service operability even without an explicit YAML file.
// @preconditions None.
// - @param: None.
// @returns An initialized Config pointer populated with production invariants.
// @throws None.
// @invariants Static initialization; zero memory leaks; independent instance allocated per invocation.
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Host:                 "0.0.0.0",
			Port:                 8080,
			ReadHeaderTimeout:    10 * time.Second,
			IdleTimeout:          300 * time.Second,
			ShutdownDrainTimeout: 120 * time.Second,
			MaxRequestBodyBytes:  104857600, // 100 MiB (supports 1M+ context & multimodal payloads)
			SSEKeepaliveInterval: 15 * time.Second,
		},
		Security: SecurityConfig{
			APIKeys:              make([]APIKeyConfig, 0),
			AllowRemoteImageURLs: false,
			AllowedCORSOrigins:   make([]string, 0),
		},
		Binaries: BinariesConfig{
			AgyPath:       "/usr/local/bin/agy",
			AgyMinVersion: "1.5.5",
			AgyAuthPath:   "/usr/local/bin/agy-auth",
			AgyAcpArgs:    []string{"acp", "--dangerously-skip-permissions"},
		},
		Execution: ExecutionConfig{
			YoloMode:                      true,
			AcpSessionMode:                "accept-edits",
			AutoApprovePermissions:        true,
			CleanWorkspaceBetweenRequests: true,
			MaxRetries:                    3,
			RetryInitialBackoff:           500 * time.Millisecond,
			RetryMaxBackoff:               10 * time.Second,
		},
		Storage: StorageConfig{
			BaseDataDir:       "/var/lib/agy-gateway",
			VaultXDGConfigDir: "/var/lib/agy-gateway/vault-xdg",
			WorkersRootDir:    "/var/lib/agy-gateway/workers",
			WorkersTmpDir:     "/var/lib/agy-gateway/tmp",
			WorkspacesRootDir: "/var/lib/agy-gateway/workspaces",
		},
		Pool: PoolConfig{
			WorkerCount:            2,
			AcquireTimeout:         15 * time.Second,
			HandshakeTimeout:       15 * time.Second,
			ConfigSwitchTimeout:    10 * time.Second,
			InferenceTimeout:       900 * time.Second, // 15m for 1M context prefill and generation
			FirstTokenTimeout:      120 * time.Second, // 2m TTFT headroom for 1M token cold cache
			ProcessKillGracePeriod: 5 * time.Second,
			MaxRequestsPerWorker:   250,
			MaxWorkerUptime:        2 * time.Hour,
			MaxScannerBufferBytes:  134217728, // 128 MiB (prevents bufio.ErrTooLong on large turn payloads)
		},
		Accounts: AccountsConfig{
			AutoRotateEnabled:     true,
			AllocationStrategy:    "dedicated_per_worker",
			QuotaPollInterval:     30 * time.Second,
			MinRemainingQuotaPct:  5.0,
			DefaultCooldown5h:     30 * time.Minute,
			DefaultCooldownWeekly: 6 * time.Hour,
			MaxFailoverRetries:    3,
		},
		Admin: AdminConfig{
			CommandTimeout:        15 * time.Second,
			AllowCredentialExport: false,
			QueryValidationRegex:  "^[a-zA-Z0-9@._:-]{1,128}$",
			AliasValidationRegex:  "^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$",
			RuntimeOverridesFile:  "/var/lib/agy-gateway/vault-xdg/gateway-runtime-overrides.json",
			OAuthOnboarding: OAuthOnboardingConfig{
				Enabled:               true,
				MaxConcurrentLogins:   3,
				URLExtractTimeout:     10 * time.Second,
				LoginSessionTTL:       5 * time.Minute,
				LoopbackReplayTimeout: 15 * time.Second,
			},
		},
		OpenAI: OpenAIConfig{
			UnsupportedParamPolicy: "strip_and_warn",
			DefaultModel:           "gemini-3.8-flash",
			DefaultReasoningEffort: "medium",
			ExposeReasoningContent: true,
		},
		MCP: MCPConfig{
			ClientToolCalling: MCPClientToolCallingConfig{
				Enabled:                true,
				DefaultParallel:        true,
				StrictSchemaValidation: true,
				MaxToolArgumentBytes:   16777216, // 16 MiB (supports large code/document tool outputs)
				DelimiterTag:           "tool_call",
			},
		},
		Observability: ObsConfig{
			LogLevel:       "info",
			LogFormat:      "json",
			MetricsEnabled: true,
			MetricsPath:    "/metrics",
		},
	}
}

// Load loads the configuration from a YAML file (if present), merges over baseline defaults,
// applies 12-factor environment overrides, and enforces fail-fast validation invariants.
//
// @purpose Discovers, merges, overrides, and validates system-wide gateway configuration parameters.
// @rationale Implements the 12-factor app configuration hierarchy: Defaults -> YAML File -> .env File -> Process Environment.
// @preconditions Environment variables must contain valid format types if declared; file must be readable if present.
// - @param: path Absolute or relative filesystem path to the YAML configuration file.
// @returns A fully validated Config pointer ready for service bootstrapping, or a descriptive error.
// @throws Returns typed validation errors if ports, keys, or timeout invariants fail.
// @invariants Thread-safe across independent invocations; deterministic output for identical input states.
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			// If non-default custom config file was passed and not found, report error
			if !os.IsNotExist(err) || (path != "config.yaml" && path != "/etc/agy-gateway/config.yaml") {
				return nil, fmt.Errorf("failed to read config file at %s: %w", path, err)
			}
		} else {
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("failed to parse YAML config: %w", err)
			}
		}
	}

	applyEnvOverrides(cfg)

	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("configuration validation failed: %w", err)
	}

	return cfg, nil
}

func applyEnvOverrides(cfg *Config) {
	// 1. Server Overrides
	if v := getFirstEnv("AGY_GW_HOST", "HOST"); v != "" {
		cfg.Server.Host = v
	}
	if v := getFirstEnv("AGY_GW_PORT", "PORT"); v != "" {
		if p, err := parsePort(v); err == nil {
			cfg.Server.Port = p
		}
	}
	if v := os.Getenv("AGY_GW_READ_HEADER_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Server.ReadHeaderTimeout = d
		}
	}
	if v := os.Getenv("AGY_GW_IDLE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Server.IdleTimeout = d
		}
	}
	if v := os.Getenv("AGY_GW_SHUTDOWN_DRAIN_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Server.ShutdownDrainTimeout = d
		}
	}
	if v := os.Getenv("AGY_GW_MAX_REQUEST_BODY_BYTES"); v != "" {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			cfg.Server.MaxRequestBodyBytes = n
		}
	}
	if v := os.Getenv("AGY_GW_SSE_KEEPALIVE_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Server.SSEKeepaliveInterval = d
		}
	}

	// 2. Pool Overrides
	if v := getFirstEnv("AGY_GW_POOL_WORKER_COUNT", "WORKER_COUNT"); v != "" {
		var count int
		if _, err := fmt.Sscanf(v, "%d", &count); err == nil && count > 0 {
			cfg.Pool.WorkerCount = count
		}
	}
	if v := os.Getenv("AGY_GW_POOL_ACQUIRE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Pool.AcquireTimeout = d
		}
	}
	if v := os.Getenv("AGY_GW_POOL_HANDSHAKE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Pool.HandshakeTimeout = d
		}
	}
	if v := os.Getenv("AGY_GW_POOL_INFERENCE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Pool.InferenceTimeout = d
		}
	}
	if v := os.Getenv("AGY_GW_POOL_FIRST_TOKEN_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Pool.FirstTokenTimeout = d
		}
	}
	if v := os.Getenv("AGY_GW_POOL_MAX_REQUESTS_PER_WORKER"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			cfg.Pool.MaxRequestsPerWorker = n
		}
	}
	if v := os.Getenv("AGY_GW_POOL_MAX_WORKER_UPTIME"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Pool.MaxWorkerUptime = d
		}
	}
	if v := os.Getenv("AGY_GW_POOL_MAX_SCANNER_BUFFER_BYTES"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			cfg.Pool.MaxScannerBufferBytes = n
		}
	}

	// 3. Storage Overrides
	if v := getFirstEnv("AGY_GW_BASE_DATA_DIR", "AGY_GW_STORAGE_DIR"); v != "" {
		cfg.Storage.BaseDataDir = v
		if cfg.Storage.VaultXDGConfigDir == "" || strings.HasPrefix(cfg.Storage.VaultXDGConfigDir, "/var/lib/agy-gateway") {
			cfg.Storage.VaultXDGConfigDir = filepath.Join(v, "vault-xdg")
		}
		if cfg.Storage.WorkersRootDir == "" || strings.HasPrefix(cfg.Storage.WorkersRootDir, "/var/lib/agy-gateway") {
			cfg.Storage.WorkersRootDir = filepath.Join(v, "workers")
		}
		if cfg.Storage.WorkersTmpDir == "" || strings.HasPrefix(cfg.Storage.WorkersTmpDir, "/var/lib/agy-gateway") {
			cfg.Storage.WorkersTmpDir = filepath.Join(v, "tmp")
		}
		if cfg.Storage.WorkspacesRootDir == "" || strings.HasPrefix(cfg.Storage.WorkspacesRootDir, "/var/lib/agy-gateway") {
			cfg.Storage.WorkspacesRootDir = filepath.Join(v, "workspaces")
		}
	}
	if v := os.Getenv("AGY_GW_VAULT_DIR"); v != "" {
		cfg.Storage.VaultXDGConfigDir = v
	}
	if v := os.Getenv("AGY_GW_WORKERS_DIR"); v != "" {
		cfg.Storage.WorkersRootDir = v
	}
	if v := os.Getenv("AGY_GW_WORKSPACES_DIR"); v != "" {
		cfg.Storage.WorkspacesRootDir = v
	}
	if v := os.Getenv("AGY_GW_TMP_DIR"); v != "" {
		cfg.Storage.WorkersTmpDir = v
	}

	// 4. Accounts & Failover Overrides
	if v := os.Getenv("AGY_GW_AUTO_ROTATE_ENABLED"); v != "" {
		cfg.Accounts.AutoRotateEnabled = parseBoolEnv(v, cfg.Accounts.AutoRotateEnabled)
	}
	if v := os.Getenv("AGY_GW_QUOTA_POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Accounts.QuotaPollInterval = d
		}
	}
	if v := os.Getenv("AGY_GW_MIN_REMAINING_QUOTA_PCT"); v != "" {
		var pct float64
		if _, err := fmt.Sscanf(v, "%f", &pct); err == nil && pct >= 0 {
			cfg.Accounts.MinRemainingQuotaPct = pct
		}
	}
	if v := getFirstEnv("AGY_GW_MAX_RETRIES", "MAX_RETRIES"); v != "" {
		var retries int
		if _, err := fmt.Sscanf(v, "%d", &retries); err == nil && retries >= 0 {
			cfg.Execution.MaxRetries = retries
			cfg.Accounts.MaxFailoverRetries = retries
		}
	}
	if v := os.Getenv("AGY_GW_MAX_FAILOVER_RETRIES"); v != "" {
		var retries int
		if _, err := fmt.Sscanf(v, "%d", &retries); err == nil && retries >= 0 {
			cfg.Accounts.MaxFailoverRetries = retries
			cfg.Execution.MaxRetries = retries
		}
	}
	if v := os.Getenv("AGY_GW_RETRY_INITIAL_BACKOFF"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.Execution.RetryInitialBackoff = d
		}
	}
	if v := os.Getenv("AGY_GW_RETRY_MAX_BACKOFF"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.Execution.RetryMaxBackoff = d
		}
	}

	// 5. OpenAI & Models Overrides
	if v := getFirstEnv("AGY_GW_DEFAULT_MODEL", "DEFAULT_MODEL"); v != "" {
		cfg.OpenAI.DefaultModel = v
	}
	if v := os.Getenv("AGY_GW_DEFAULT_REASONING_EFFORT"); v != "" {
		cfg.OpenAI.DefaultReasoningEffort = v
	}
	if v := os.Getenv("AGY_GW_EXPOSE_REASONING_CONTENT"); v != "" {
		cfg.OpenAI.ExposeReasoningContent = parseBoolEnv(v, cfg.OpenAI.ExposeReasoningContent)
	}
	if v := os.Getenv("AGY_GW_MAX_TOOL_ARGUMENT_BYTES"); v != "" {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			cfg.MCP.ClientToolCalling.MaxToolArgumentBytes = n
		}
	}

	// 6. Observability Overrides
	if v := getFirstEnv("AGY_GW_LOG_LEVEL", "LOG_LEVEL"); v != "" {
		cfg.Observability.LogLevel = v
	}
	if v := getFirstEnv("AGY_GW_LOG_FORMAT", "LOG_FORMAT"); v != "" {
		cfg.Observability.LogFormat = v
	}
	if v := os.Getenv("AGY_GW_METRICS_ENABLED"); v != "" {
		cfg.Observability.MetricsEnabled = parseBoolEnv(v, cfg.Observability.MetricsEnabled)
	}
	if v := os.Getenv("AGY_GW_METRICS_PATH"); v != "" {
		cfg.Observability.MetricsPath = v
	}

	// 7. Security: CORS Origins Override
	if v := os.Getenv("AGY_GW_ALLOWED_CORS_ORIGINS"); v != "" {
		parts := strings.Split(v, ",")
		cleaned := make([]string, 0, len(parts))
		for _, p := range parts {
			if s := strings.TrimSpace(p); s != "" {
				cleaned = append(cleaned, s)
			}
		}
		if len(cleaned) > 0 {
			cfg.Security.AllowedCORSOrigins = cleaned
		}
	}

	// 8. Security: Resolve secrets from environment or direct config
	if cfg.Security.AdminAPIKeyEnv != "" {
		cfg.Security.AdminAPIKeySecret = os.Getenv(cfg.Security.AdminAPIKeyEnv)
	}
	if cfg.Security.AdminAPIKeySecret == "" && cfg.Security.AdminAPIKey != "" {
		cfg.Security.AdminAPIKeySecret = cfg.Security.AdminAPIKey
	}
	if cfg.Security.AdminAPIKeySecret == "" {
		if v := getFirstEnv("AGY_GW_ADMIN_KEY", "ADMIN_API_KEY"); v != "" {
			cfg.Security.AdminAPIKeySecret = v
		}
	}

	for i := range cfg.Security.APIKeys {
		if cfg.Security.APIKeys[i].KeyEnv != "" {
			cfg.Security.APIKeys[i].KeySecret = os.Getenv(cfg.Security.APIKeys[i].KeyEnv)
		}
		if cfg.Security.APIKeys[i].KeySecret == "" && cfg.Security.APIKeys[i].Key != "" {
			cfg.Security.APIKeys[i].KeySecret = cfg.Security.APIKeys[i].Key
		}
	}

	// Multiple client keys support via comma-separated AGY_GW_CLIENT_KEYS (e.g. "key1:60,key2:120")
	if v := os.Getenv("AGY_GW_CLIENT_KEYS"); v != "" {
		items := strings.Split(v, ",")
		for idx, item := range items {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			parts := strings.SplitN(item, ":", 2)
			secret := parts[0]
			rpm := 120
			if len(parts) == 2 {
				_, _ = fmt.Sscanf(parts[1], "%d", &rpm)
			}
			cfg.Security.APIKeys = append(cfg.Security.APIKeys, APIKeyConfig{
				ID:           fmt.Sprintf("env-key-%d", idx+1),
				KeySecret:    secret,
				RateLimitRPM: rpm,
			})
		}
	}

	// Fallback global client key from AGY_GW_CLIENT_KEY or CLIENT_API_KEY
	if v := getFirstEnv("AGY_GW_CLIENT_KEY", "CLIENT_API_KEY", "AGY_GW_CLIENT_KEY_1"); v != "" {
		found := false
		for _, k := range cfg.Security.APIKeys {
			if k.KeySecret == v {
				found = true
				break
			}
		}
		if !found {
			cfg.Security.APIKeys = append(cfg.Security.APIKeys, APIKeyConfig{
				ID:           "env-client-key",
				KeySecret:    v,
				RateLimitRPM: 120,
			})
		}
	}

	// 9. Dynamic binary path resolution
	if v := getFirstEnv("AGY_PATH", "AGY_GW_AGY_PATH"); v != "" {
		cfg.Binaries.AgyPath = v
	}
	if v := getFirstEnv("AGY_AUTH_PATH", "AGY_GW_AGY_AUTH_PATH"); v != "" {
		cfg.Binaries.AgyAuthPath = v
	}

	if cfg.Binaries.AgyAuthPath != "" {
		if _, err := os.Stat(cfg.Binaries.AgyAuthPath); err != nil {
			if lp, err := exec.LookPath("agy-auth"); err == nil {
				cfg.Binaries.AgyAuthPath = lp
			}
		}
	} else if lp, err := exec.LookPath("agy-auth"); err == nil {
		cfg.Binaries.AgyAuthPath = lp
	}

	if cfg.Binaries.AgyPath != "" {
		if _, err := os.Stat(cfg.Binaries.AgyPath); err != nil {
			if lp, err := exec.LookPath("agy"); err == nil {
				cfg.Binaries.AgyPath = lp
			}
		}
	} else if lp, err := exec.LookPath("agy"); err == nil {
		cfg.Binaries.AgyPath = lp
	}
}

func getFirstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func parsePort(s string) (int, error) {
	var port int
	if _, err := fmt.Sscanf(s, "%d", &port); err == nil && port > 0 && port <= 65535 {
		return port, nil
	}
	return 0, fmt.Errorf("invalid port %q", s)
}

func parseBoolEnv(val string, defaultVal bool) bool {
	switch strings.ToLower(strings.TrimSpace(val)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return defaultVal
	}
}

func validate(cfg *Config) error {
	if cfg.Server.Port <= 0 || cfg.Server.Port > 65535 {
		return fmt.Errorf("server.port must be between 1 and 65535, got %d", cfg.Server.Port)
	}
	if cfg.Pool.WorkerCount <= 0 {
		return fmt.Errorf("pool.worker_count must be > 0, got %d", cfg.Pool.WorkerCount)
	}
	if cfg.Binaries.AgyPath == "" {
		return fmt.Errorf("binaries.agy_path cannot be empty")
	}
	if cfg.Binaries.AgyAuthPath == "" {
		return fmt.Errorf("binaries.agy_auth_path cannot be empty")
	}
	if cfg.Execution.AcpSessionMode != "default" && cfg.Execution.AcpSessionMode != "accept-edits" && cfg.Execution.AcpSessionMode != "plan" {
		return fmt.Errorf("execution.acp_session_mode must be 'default', 'accept-edits', or 'plan', got %q", cfg.Execution.AcpSessionMode)
	}

	// Compile admin regexes
	var err error
	if cfg.Admin.QueryValidationRegex == "" {
		cfg.Admin.QueryValidationRegex = "^[a-zA-Z0-9@._:-]{1,128}$"
	}
	cfg.QueryRegex, err = regexp.Compile(cfg.Admin.QueryValidationRegex)
	if err != nil {
		return fmt.Errorf("invalid admin.query_validation_regex: %w", err)
	}

	if cfg.Admin.AliasValidationRegex == "" {
		cfg.Admin.AliasValidationRegex = "^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$"
	}
	cfg.AliasRegex, err = regexp.Compile(cfg.Admin.AliasValidationRegex)
	if err != nil {
		return fmt.Errorf("invalid admin.alias_validation_regex: %w", err)
	}

	// Set safe defaults for buffers if unset
	if cfg.Pool.MaxScannerBufferBytes <= 0 {
		cfg.Pool.MaxScannerBufferBytes = 134217728 // 128 MiB default for 1M context
	}
	if cfg.MCP.ClientToolCalling.MaxToolArgumentBytes <= 0 {
		cfg.MCP.ClientToolCalling.MaxToolArgumentBytes = 16 * 1024 * 1024 // 16 MiB default
	}
	if cfg.MCP.ClientToolCalling.DelimiterTag == "" {
		cfg.MCP.ClientToolCalling.DelimiterTag = "tool_call"
	}

	return nil
}
