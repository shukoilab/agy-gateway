// Package config provides externalized YAML configuration loading, environment overrides,
// and strict fail-fast validation for the agy-gateway service.
package config

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
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
	YoloMode                      bool   `yaml:"yolo_mode"`
	AcpSessionMode                string `yaml:"acp_session_mode"`
	AutoApprovePermissions        bool   `yaml:"auto_approve_permissions"`
	CleanWorkspaceBetweenRequests bool   `yaml:"clean_workspace_between_requests"`
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

// Load loads the configuration from a YAML file, applies environment overrides,
// and enforces fail-fast validation invariants.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file at %s: %w", path, err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse YAML config: %w", err)
	}

	applyEnvOverrides(cfg)

	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("configuration validation failed: %w", err)
	}

	return cfg, nil
}

func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("AGY_GW_HOST"); v != "" {
		cfg.Server.Host = v
	}
	if v := os.Getenv("AGY_GW_PORT"); v != "" {
		var port int
		if _, err := fmt.Sscanf(v, "%d", &port); err == nil && port > 0 {
			cfg.Server.Port = port
		}
	}
	if v := os.Getenv("AGY_GW_POOL_WORKER_COUNT"); v != "" {
		var count int
		if _, err := fmt.Sscanf(v, "%d", &count); err == nil && count > 0 {
			cfg.Pool.WorkerCount = count
		}
	}
	if v := os.Getenv("AGY_GW_DEFAULT_MODEL"); v != "" {
		cfg.OpenAI.DefaultModel = v
	}

	// Resolve secrets from environment or direct config
	if cfg.Security.AdminAPIKeyEnv != "" {
		cfg.Security.AdminAPIKeySecret = os.Getenv(cfg.Security.AdminAPIKeyEnv)
	}
	if cfg.Security.AdminAPIKeySecret == "" && cfg.Security.AdminAPIKey != "" {
		cfg.Security.AdminAPIKeySecret = cfg.Security.AdminAPIKey
	}
	if cfg.Security.AdminAPIKeySecret == "" {
		if v := os.Getenv("AGY_GW_ADMIN_KEY"); v != "" {
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

	// Fallback global client key from AGY_GW_CLIENT_KEY or AGY_GW_CLIENT_KEY_1 if none matched
	if v := os.Getenv("AGY_GW_CLIENT_KEY"); v != "" {
		cfg.Security.APIKeys = append(cfg.Security.APIKeys, APIKeyConfig{
			ID:           "env-client-key",
			KeySecret:    v,
			RateLimitRPM: 120,
		})
	}

	// Dynamic binary path resolution
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
		cfg.Pool.MaxScannerBufferBytes = 16 * 1024 * 1024 // 16 MiB
	}
	if cfg.MCP.ClientToolCalling.MaxToolArgumentBytes <= 0 {
		cfg.MCP.ClientToolCalling.MaxToolArgumentBytes = 1 * 1024 * 1024 // 1 MiB
	}
	if cfg.MCP.ClientToolCalling.DelimiterTag == "" {
		cfg.MCP.ClientToolCalling.DelimiterTag = "tool_call"
	}

	return nil
}
