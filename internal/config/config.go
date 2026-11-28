// Package config provides externalized YAML configuration loading, environment overrides,
// and strict fail-fast validation for the agy-gateway service.
package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"agy-gateway/internal/version"

	"gopkg.in/yaml.v3"
)

// Config represents the top-level unified gateway configuration.
type Config struct {
	Server        ServerConfig       `yaml:"server"`
	Security      SecurityConfig     `yaml:"security"`
	Binaries      BinariesConfig     `yaml:"binaries"`
	Execution     ExecutionConfig    `yaml:"execution"`
	Identity      IdentityConfig     `yaml:"identity"`
	Capabilities  CapabilitiesConfig `yaml:"capabilities"`
	Storage       StorageConfig      `yaml:"storage"`
	Pool          PoolConfig         `yaml:"pool"`
	Accounts      AccountsConfig     `yaml:"accounts"`
	Admin         AdminConfig        `yaml:"admin"`
	OpenAI        OpenAIConfig       `yaml:"openai"`
	Anthropic     AnthropicConfig    `yaml:"anthropic"`
	Proxy         ProxyConfig        `yaml:"proxy"`
	AutoUpdate    AutoUpdateConfig   `yaml:"auto_update"`
	MCP           MCPConfig          `yaml:"mcp"`
	Observability ObsConfig          `yaml:"observability"`

	// Compiled regex patterns for zero-runtime-allocation routing & validation
	QueryRegex *regexp.Regexp `yaml:"-"`
	AliasRegex *regexp.Regexp `yaml:"-"`
	binMu      sync.RWMutex   `yaml:"-"`

	// RemovedKeys carries the YAML keys and environment variables that were deleted by the YOLO
	// refactor but are still present in the loaded configuration. Load does not log them itself:
	// it runs before the configured log handler is installed, so an emission here would bypass
	// observability.log_format/level. Callers pass the value to WarnRemovedKeys once their logger is
	// built; every entrypoint that builds a logger does so, and the daemon uses setupLogging.
	RemovedKeys []string `yaml:"-"`
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
	APIKeys            []APIKeyConfig `yaml:"api_keys"`
	AdminAPIKey        string         `yaml:"admin_api_key"`
	AdminAPIKeyEnv     string         `yaml:"admin_api_key_env"`
	AdminAPIKeySecret  string         `yaml:"-"`
	AllowedCORSOrigins []string       `yaml:"allowed_cors_origins"`
}

// BinariesConfig locates the agent and constrains which build the gateway will run against.
//
// @purpose Supplies paths plus the minimum version gate.
// @rationale agy_acp_args was removed with the YOLO refactor: the argv is a product invariant owned by
// acp.SpawnArgs(), so exposing it would let a deployment silently drop the flag that makes the gateway
// a provider endpoint rather than a blocking harness.
type BinariesConfig struct {
	AgyPath       string `yaml:"agy_path"`
	AgyMinVersion string `yaml:"agy_min_version"`
	AgyAuthPath   string `yaml:"agy_auth_path"`
}

// ExecutionConfig controls per-turn agent behaviour.
//
// @purpose Groups the policies that shape how one inference turn is executed.
// @rationale The YOLO refactor removed two knobs that only existed to compensate for a mode or
// auto-approval choice the gateway no longer makes: acp_session_mode (plan/build is the consumer's
// system prompt) and auto_approve_permissions (--dangerously-skip-permissions is now unconditional).
// system_prompt_channel is gone for the same reason: only one delivery path works, so a knob to pick
// another would only exist to be misconfigured.
type ExecutionConfig struct {
	CleanWorkspaceBetweenRequests bool          `yaml:"clean_workspace_between_requests"`
	StrictClientToolDelegation    bool          `yaml:"strict_client_tool_delegation"`
	VisionMode                    string        `yaml:"vision_mode"`
	MaxRetries                    int           `yaml:"max_retries"`
	RetryInitialBackoff           time.Duration `yaml:"retry_initial_backoff"`
	RetryMaxBackoff               time.Duration `yaml:"retry_max_backoff"`
	// MaxHistoryTokens bounds the conversational history forwarded to the agent per turn. Above
	// ~100K estimated tokens the agent stops converging and re-issues the same tool calls, so the
	// budget is enforced by compacting the oldest tool results first (head and tail kept), which
	// preserves every assistant turn — dropping turns is what made a long session forget its own
	// analysis. Whole turns are dropped only when the prose alone exceeds the budget, and then the
	// opening turn and the newest turns are kept. 0 disables the bound.
	MaxHistoryTokens int `yaml:"max_history_tokens"`
	// UnsupportedParameterPolicy decides what happens to provider parameters that ACP cannot express
	// (temperature, top_p, stop, penalties, max_tokens, seed, logit_bias, logprobs, …). ACP exposes
	// only model, thought_level and mode, so those fields can never be honoured; silently accepting
	// them hides that from the caller. "ignore" (default) accepts them and names them in the
	// x-agy-ignored-params response header so a harness can see the truth; "reject" answers with the
	// provider's own invalid_request_error naming the first unsupported parameter.
	UnsupportedParameterPolicy string `yaml:"unsupported_parameter_policy"`
}

// IdentityConfig controls what the gateway announces in `initialize.clientInfo`.
//
// @purpose Lets the operator choose the client fingerprint the agent sees.
// @rationale The reference behaviour (Zed ↔ agy) is what the gateway is a bridge for, so the
// default is the Zed fingerprint: agy branches on the client name for a few vendor paths and its
// logs read "Zed". A deployment that must not impersonate an editor can switch to `gateway`.
//
// @invariants Name/Title/Version are always non-empty after validation; Version is data loaded
// from YAML/env, never a value baked into code, so an operator can match a newer Zed without a
// rebuild.
type IdentityConfig struct {
	// Profile selects the preset: "zed" (default) or "gateway".
	Profile string `yaml:"profile"`
	Name    string `yaml:"name"`
	Title   string `yaml:"title"`
	Version string `yaml:"version"`
}

// CapabilitiesConfig controls which client capabilities the gateway advertises in `initialize`.
//
// @purpose Separates "what we tell the agent we can do" from "what we actually implement".
// @rationale Advertising a capability without its handler makes agy emit requests the gateway
// answers with method-not-found, which stalls the turn. Validation therefore fails startup when a
// profile advertises fs or terminal and the corresponding handlers are not enabled.
//
// @invariants Profile "zed" advertises fs+terminal (all handlers registered by the acp transport);
// "sandbox" advertises neither.
type CapabilitiesConfig struct {
	// Profile selects the preset: "zed" (default) or "sandbox".
	Profile string `yaml:"profile"`
	// FS enables the fs.readTextFile/fs.writeTextFile advertisement.
	FS bool `yaml:"fs"`
	// Terminal enables the terminal capability advertisement.
	Terminal bool `yaml:"terminal"`
	// Elicitation enables the elicitation capability advertisement.
	Elicitation bool `yaml:"elicitation"`
	// SessionMetadata enables session.compaction/configOptions/notices advertisement.
	SessionMetadata bool `yaml:"session_metadata"`
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
	CredentialOpTimeout   time.Duration `yaml:"credential_op_timeout"`
	// AuthExpiredRetryInterval bounds how long a runtime-detected credential fault removes an account
	// from service before it is retried. A vault-reported expiry is permanent (it needs
	// `agy-auth login`), but a materialisation or upstream auth failure must not strand a healthy
	// account forever if the cause was transient.
	AuthExpiredRetryInterval time.Duration `yaml:"auth_expired_retry_interval"`
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
	DefaultModel           string `yaml:"default_model"`
	DefaultReasoningEffort string `yaml:"default_reasoning_effort"`
	ExposeReasoningContent bool   `yaml:"expose_reasoning_content"`
}

type AnthropicConfig struct {
	DefaultModel           string `yaml:"default_model"`
	DefaultMaxTokens       int    `yaml:"default_max_tokens"`
	ExposeReasoningContent bool   `yaml:"expose_reasoning_content"`
}

// ProxyConfig specifies optional outbound HTTP/HTTPS proxy settings for Docker and host environments.
type ProxyConfig struct {
	Enabled    bool   `yaml:"enabled"`
	HTTPProxy  string `yaml:"http_proxy"`
	HTTPSProxy string `yaml:"https_proxy"`
	NoProxy    string `yaml:"no_proxy"`
}

// ExportToEnv synchronizes proxy settings to the current process OS environment.
func (p *ProxyConfig) ExportToEnv() {
	if p.HTTPProxy != "" {
		_ = os.Setenv("HTTP_PROXY", p.HTTPProxy)
		_ = os.Setenv("http_proxy", p.HTTPProxy)
	}
	if p.HTTPSProxy != "" {
		_ = os.Setenv("HTTPS_PROXY", p.HTTPSProxy)
		_ = os.Setenv("https_proxy", p.HTTPSProxy)
	}
	if p.NoProxy != "" {
		_ = os.Setenv("NO_PROXY", p.NoProxy)
		_ = os.Setenv("no_proxy", p.NoProxy)
	}
}

// EnvSlice returns a slice of environment variable strings ("KEY=VALUE") for child subprocesses.
func (p *ProxyConfig) EnvSlice() []string {
	if !p.Enabled && p.HTTPProxy == "" && p.HTTPSProxy == "" {
		return nil
	}
	var envs []string
	if p.HTTPProxy != "" {
		envs = append(envs, "HTTP_PROXY="+p.HTTPProxy, "http_proxy="+p.HTTPProxy)
	}
	if p.HTTPSProxy != "" {
		envs = append(envs, "HTTPS_PROXY="+p.HTTPSProxy, "https_proxy="+p.HTTPSProxy)
	}
	if p.NoProxy != "" {
		envs = append(envs, "NO_PROXY="+p.NoProxy, "no_proxy="+p.NoProxy)
	}
	envs = append(envs, "NODE_USE_ENV_PROXY=1")
	return envs
}

// AutoUpdateConfig governs the automated background update checking and installation engine.
type AutoUpdateConfig struct {
	Enabled        bool                `yaml:"enabled"`
	Interval       time.Duration       `yaml:"interval"`
	CheckOnStartup bool                `yaml:"check_on_startup"`
	BinDir         string              `yaml:"bin_dir"`
	AgyCLI         AgyCLIUpdateConfig  `yaml:"agy_cli"`
	AgyAuth        AgyAuthUpdateConfig `yaml:"agy_auth"`
}

// AgyCLIUpdateConfig defines parameters for updating the Google Antigravity CLI flat binary.
type AgyCLIUpdateConfig struct {
	Enabled     bool   `yaml:"enabled"`
	ManifestURL string `yaml:"manifest_url"`
}

// AgyAuthUpdateConfig defines parameters for updating @fikrilal/agy-auth multi-account CLI.
type AgyAuthUpdateConfig struct {
	Enabled     bool   `yaml:"enabled"`
	Source      string `yaml:"source"`       // "github" (default) or "npm"
	ReleaseRepo string `yaml:"release_repo"` // default: "fikrilal/agy-auth"
}

// GetAgyPath returns the current active path to the agy CLI binary with thread safety.
func (c *Config) GetAgyPath() string {
	c.binMu.RLock()
	defer c.binMu.RUnlock()
	return c.Binaries.AgyPath
}

// SetAgyPath safely updates the active path to the agy CLI binary.
func (c *Config) SetAgyPath(p string) {
	c.binMu.Lock()
	defer c.binMu.Unlock()
	c.Binaries.AgyPath = p
}

// GetAgyAuthPath returns the current active path to the agy-auth binary with thread safety.
func (c *Config) GetAgyAuthPath() string {
	c.binMu.RLock()
	defer c.binMu.RUnlock()
	return c.Binaries.AgyAuthPath
}

// SetAgyAuthPath safely updates the active path to the agy-auth binary.
func (c *Config) SetAgyAuthPath(p string) {
	c.binMu.Lock()
	defer c.binMu.Unlock()
	c.Binaries.AgyAuthPath = p
}

// MCPClientToolCallingConfig controls client tool delegation and interception.
//
// @purpose Bounds and validates every knob that affects the client-tool path.
// @rationale DefaultParallel was removed: OpenAI's `parallel_tool_calls` is the real control surface, and
// multi-call collection is now always enabled, so the knob implied a switch that did nothing.
type MCPClientToolCallingConfig struct {
	Enabled                 bool  `yaml:"enabled"`
	StrictSchemaValidation  bool  `yaml:"strict_schema_validation"`
	MaxToolArgumentBytes    int64 `yaml:"max_tool_argument_bytes"`
	MaxClientTools          int   `yaml:"max_client_tools"`
	MaxInterceptedToolCalls int   `yaml:"max_intercepted_tool_calls"`
	// ToolCallCollectWindow is the idle watchdog window, not a post-call deadline.
	//
	// @purpose Ends a turn only after the agent stops producing output while calls are pending.
	// @rationale It was applied as a one-shot deadline from the moment a tool call arrived, so a
	// reasoning model that kept thinking for more than a window (TTFT reached 63 s in a real session)
	// had its turn cancelled mid-generation; the client saw a truncated turn and retried it, which is
	// the loop this restart prevents. It is now re-armed by every streamed chunk and, when it does
	// fire, sends session/cancel followed by one more window of grace before cancelling the prompt.
	ToolCallCollectWindow time.Duration `yaml:"tool_call_collect_window"`
	// DelimiterTag is retained for configuration backwards-compatibility.
	// All tools are natively handled via ACP and the in-memory MCP loopback server.
	DelimiterTag string `yaml:"delimiter_tag"`
}

type MCPConfig struct {
	ClientToolCalling MCPClientToolCallingConfig `yaml:"client_tool_calling"`
	// Media governs consumer-supplied image references. An API consumer can point an image part at
	// any URL, so this policy is what stops the gateway from being used as an SSRF proxy.
	Media MediaConfig `yaml:"media"`
}

// MediaConfig is the operator-facing image resolution policy.
//
// @purpose Bounds where and how much image data the gateway will ingest.
// @rationale Sensible defaults are enforced by the resolver even when this block is absent, so an
// empty config cannot disable the size ceiling or the host policy.
type MediaConfig struct {
	// AllowRemote permits http(s) image references. When false only inline data: URIs are accepted.
	AllowRemote bool `yaml:"allow_remote"`
	// MaxBytes bounds the decoded payload of one image.
	MaxBytes int64 `yaml:"max_bytes"`
	// FetchTimeout bounds one remote fetch including redirects.
	FetchTimeout time.Duration `yaml:"fetch_timeout"`
	// AllowedMimeTypes is the accepted media type set. Empty means png/jpeg/gif/webp.
	AllowedMimeTypes []string `yaml:"allowed_mime_types"`
	// AllowPrivateHosts permits loopback/private/link-local destinations. Off by default because
	// those are exactly what an SSRF attempt targets.
	AllowPrivateHosts bool `yaml:"allow_private_hosts"`
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
			APIKeys:            make([]APIKeyConfig, 0),
			AllowedCORSOrigins: make([]string, 0),
		},
		Binaries: BinariesConfig{
			AgyPath:       "/usr/local/bin/agy",
			AgyMinVersion: "1.5.16",
			AgyAuthPath:   "/usr/local/bin/agy-auth",
		},
		Execution: ExecutionConfig{
			CleanWorkspaceBetweenRequests: true,
			StrictClientToolDelegation:    true,
			VisionMode:                    VisionModeAuto,
			MaxRetries:                    3,
			RetryInitialBackoff:           500 * time.Millisecond,
			RetryMaxBackoff:               10 * time.Second,
			MaxHistoryTokens:              60000,
			UnsupportedParameterPolicy:    UnsupportedParameterPolicyIgnore,
		},
		Identity: IdentityConfig{
			Profile: IdentityProfileZed,
			Name:    "zed",
			Title:   "Zed",
			Version: DefaultZedClientVersion,
		},
		Capabilities: CapabilitiesConfig{
			Profile:         CapabilitiesProfileZed,
			FS:              true,
			Terminal:        true,
			Elicitation:     true,
			SessionMetadata: true,
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
			DefaultCooldownWeekly: 24 * time.Hour,
			MaxFailoverRetries:    3,
			CredentialOpTimeout:   20 * time.Second,

			AuthExpiredRetryInterval: 5 * time.Minute,
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
			DefaultModel:           "gemini-3.8-flash",
			DefaultReasoningEffort: "medium",
			ExposeReasoningContent: true,
		},
		Anthropic: AnthropicConfig{
			DefaultModel:           "claude-3-5-sonnet",
			DefaultMaxTokens:       4096,
			ExposeReasoningContent: true,
		},
		Proxy: ProxyConfig{
			Enabled:    false,
			HTTPProxy:  "",
			HTTPSProxy: "",
			NoProxy:    "localhost,127.0.0.1,::1,agy-net,agy-gateway",
		},
		AutoUpdate: AutoUpdateConfig{
			Enabled:        true,
			Interval:       6 * time.Hour,
			CheckOnStartup: true,
			BinDir:         "/var/lib/agy-gateway/bin",
			AgyCLI: AgyCLIUpdateConfig{
				Enabled:     true,
				ManifestURL: "https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests",
			},
			AgyAuth: AgyAuthUpdateConfig{
				Enabled:     true,
				Source:      "github",
				ReleaseRepo: "fikrilal/agy-auth",
			},
		},
		MCP: MCPConfig{
			ClientToolCalling: MCPClientToolCallingConfig{
				Enabled:                 true,
				StrictSchemaValidation:  true,
				MaxToolArgumentBytes:    16777216, // 16 MiB (supports large code/document tool outputs)
				MaxClientTools:          128,
				MaxInterceptedToolCalls: 16,
				ToolCallCollectWindow:   time.Second,
				DelimiterTag:            "tool_call",
			},
			Media: MediaConfig{
				AllowRemote:       true,
				MaxBytes:          8 << 20, // 8 MiB per image
				FetchTimeout:      10 * time.Second,
				AllowedMimeTypes:  []string{"image/png", "image/jpeg", "image/gif", "image/webp"},
				AllowPrivateHosts: false,
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
			cfg.RemovedKeys = collectRemovedKeys(data)
		}
	}

	applyEnvOverrides(cfg)
	cfg.RemovedKeys = append(cfg.RemovedKeys, collectRemovedEnvKeys(os.Getenv)...)

	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("configuration validation failed: %w", err)
	}

	return cfg, nil
}

// removedConfigKeys lists YAML keys deleted by the YOLO refactor.
//
// @purpose Lets a stale config file keep working while telling the operator what no longer applies.
// @rationale Load uses plain yaml.Unmarshal, so an unknown key is silently ignored. That is desirable
// for boot safety but unacceptable for auto_approve_permissions=false, which used to disable
// auto-approval: an operator relying on it would otherwise believe a policy still holds.
var removedConfigKeys = []struct{ section, key string }{
	{"execution", "acp_session_mode"},
	{"execution", "auto_approve_permissions"},
	{"execution", "system_prompt_channel"},
	{"binaries", "agy_acp_args"},
	{"openai", "unsupported_param_policy"},
	{"anthropic", "unsupported_param_policy"},
}

// removedEnvKeys lists environment variables deleted by the YOLO refactor.
//
// @purpose Discloses a stale environment configuration, which is how most deployments actually
// configure the gateway (docker-compose, systemd, .env).
// @rationale A YAML-only check is insufficient: an operator who set
// AGY_GW_AUTO_APPROVE_PERMISSIONS=false in .env would never learn that approval can no longer be
// denied.
var removedEnvKeys = []string{
	"AGY_GW_ACP_SESSION_MODE",
	"ACP_SESSION_MODE",
	"AGY_GW_AUTO_APPROVE_PERMISSIONS",
	"AGY_GW_SYSTEM_PROMPT_CHANNEL",
	"AGY_GW_AGY_ACP_ARGS",
	"AGY_GW_UNSUPPORTED_PARAM_POLICY",
	"AGY_GW_ANTHROPIC_UNSUPPORTED_PARAM_POLICY",
}

// removedKeyNotes maps each removed key to the action an operator should take.
//
// @purpose Gives the startup warning something actionable to say.
// @rationale A bare "ignored" leaves the operator guessing whether their intent is still honoured.
// This map is the single source of truth for both the YAML and env key sets; keys absent here emit no
// note, which Validate-free lookups in tests assert against.
var removedKeyNotes = map[string]string{
	"execution.acp_session_mode":         "plan vs build is now decided by the consumer system prompt",
	"execution.auto_approve_permissions": "auto-approval is unconditional (--dangerously-skip-permissions)",
	"execution.system_prompt_channel":    "the consumer system prompt is always sent as the first prompt block",
	"binaries.agy_acp_args":              "the ACP argv is fixed by the gateway",
	"AGY_GW_ACP_SESSION_MODE":            "plan vs build is now decided by the consumer system prompt",
	"ACP_SESSION_MODE":                   "plan vs build is now decided by the consumer system prompt",
	"AGY_GW_AUTO_APPROVE_PERMISSIONS":    "auto-approval is unconditional (--dangerously-skip-permissions)",
	"AGY_GW_SYSTEM_PROMPT_CHANNEL":       "the consumer system prompt is always sent as the first prompt block",
	"AGY_GW_AGY_ACP_ARGS":                "the ACP argv is fixed by the gateway",
	// Both policies were replaced by one key covering every provider:
	// execution.unsupported_parameter_policy / AGY_GW_UNSUPPORTED_PARAMETER_POLICY (ignore|reject).
	"openai.unsupported_param_policy":           "replaced by execution.unsupported_parameter_policy (ignore|reject)",
	"anthropic.unsupported_param_policy":        "replaced by execution.unsupported_parameter_policy (ignore|reject)",
	"AGY_GW_UNSUPPORTED_PARAM_POLICY":           "replaced by AGY_GW_UNSUPPORTED_PARAMETER_POLICY (ignore|reject)",
	"AGY_GW_ANTHROPIC_UNSUPPORTED_PARAM_POLICY": "replaced by AGY_GW_UNSUPPORTED_PARAMETER_POLICY (ignore|reject)",
}

// collectRemovedEnvKeys returns the removed environment variables that are still exported.
//
// @purpose Identifies a stale environment configuration without emitting any log itself.
// @rationale Load runs before the configured slog handler is installed, so logging here would bypass
// observability.log_format/level and could not be indexed by the operator's pipeline.
// @preconditions None.
// @param getenv Lookup function, injected so tests do not mutate the process environment.
// @returns The removed variable names that are set and non-blank, in declaration order.
// @throws None.
// @invariants O(len(removedEnvKeys)); reads each variable exactly once.
func collectRemovedEnvKeys(getenv func(string) string) []string {
	if getenv == nil {
		return nil
	}
	var stale []string
	for _, name := range removedEnvKeys {
		if strings.TrimSpace(getenv(name)) != "" {
			stale = append(stale, name)
		}
	}
	return stale
}

// collectRemovedKeys returns the removed YAML keys still present in the loaded document.
//
// @purpose Identifies a stale YAML configuration without emitting any log itself.
// @rationale Silently ignoring a security-relevant toggle is the defect this avoids; failing the boot
// would punish an operator for an otherwise harmless leftover line. Emission is deferred to the
// entrypoint, after the real handler exists.
// @preconditions data is the YAML document already applied to cfg.
// @param data Raw YAML bytes.
// @returns Dotted key paths present in the document, in declaration order.
// @throws None; parse failures yield nil because Load has already validated the document shape.
// @invariants O(len(removedConfigKeys)).
func collectRemovedKeys(data []byte) []string {
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil || raw == nil {
		return nil
	}
	var stale []string
	for _, removed := range removedConfigKeys {
		section, ok := raw[removed.section].(map[string]any)
		if !ok {
			continue
		}
		if _, present := section[removed.key]; present {
			stale = append(stale, removed.section+"."+removed.key)
		}
	}
	return stale
}

// RemovedKeyNote returns the operator-facing note for a removed configuration key.
//
// @purpose Lets every entrypoint print the same actionable warning for a stale key.
// @param key A value returned by Load in Config.RemovedKeys.
// @returns The explanatory note, or an empty string for an unrecognised key.
// @invariants Pure; O(1).
func RemovedKeyNote(key string) string { return removedKeyNotes[key] }

// WarnRemovedKeys emits one actionable warning per removed configuration key.
//
// @purpose Gives every entrypoint (daemon and CLI subcommands) a single way to disclose a stale
// configuration, so no operator loses the notice depending on which binary they run.
// @rationale Load cannot log: it runs before the configured handler exists, and the CLI subcommands
// each build their own logger. Emitting from here keeps the message identical everywhere and honours
// whatever handler the caller installed.
// @preconditions cfg may be nil; logger may be nil.
// @param logger The caller's configured logger.
// @param cfg Loaded configuration whose RemovedKeys were populated by Load.
// @returns None.
// @throws None.
// @invariants O(len(cfg.RemovedKeys)); one line per key; emits only static key names and notes.
func WarnRemovedKeys(logger *slog.Logger, cfg *Config) {
	if logger == nil || cfg == nil {
		return
	}
	for _, key := range cfg.RemovedKeys {
		logger.Warn("removed configuration key ignored", "key", key, "reason", RemovedKeyNote(key))
	}
}

func applyEnvOverrides(cfg *Config) { // 1. Server Overrides
	if v := getFirstEnv("AGY_GW_HOST", "HOST"); v != "" {
		cfg.Server.Host = v
	}
	if v := getFirstEnv("AGY_GW_PORT", "PORT"); v != "" {
		if p, err := parsePort(v); err == nil {
			cfg.Server.Port = p
		}
	}
	if v := os.Getenv("AGY_GW_READ_HEADER_TIMEOUT"); v != "" {
		cfg.Server.ReadHeaderTimeout = parseDurationEnv(v, cfg.Server.ReadHeaderTimeout)
	}
	if v := os.Getenv("AGY_GW_IDLE_TIMEOUT"); v != "" {
		cfg.Server.IdleTimeout = parseDurationEnv(v, cfg.Server.IdleTimeout)
	}
	if v := os.Getenv("AGY_GW_SHUTDOWN_DRAIN_TIMEOUT"); v != "" {
		cfg.Server.ShutdownDrainTimeout = parseDurationEnv(v, cfg.Server.ShutdownDrainTimeout)
	}
	if v := os.Getenv("AGY_GW_MAX_REQUEST_BODY_BYTES"); v != "" {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			cfg.Server.MaxRequestBodyBytes = n
		}
	}
	if v := os.Getenv("AGY_GW_SSE_KEEPALIVE_INTERVAL"); v != "" {
		cfg.Server.SSEKeepaliveInterval = parseDurationEnv(v, cfg.Server.SSEKeepaliveInterval)
	}

	// 2. Pool Overrides
	if v := getFirstEnv("AGY_GW_POOL_WORKER_COUNT", "WORKER_COUNT"); v != "" {
		var count int
		if _, err := fmt.Sscanf(v, "%d", &count); err == nil && count > 0 {
			cfg.Pool.WorkerCount = count
		}
	}
	if v := os.Getenv("AGY_GW_POOL_ACQUIRE_TIMEOUT"); v != "" {
		cfg.Pool.AcquireTimeout = parseDurationEnv(v, cfg.Pool.AcquireTimeout)
	}
	if v := os.Getenv("AGY_GW_POOL_HANDSHAKE_TIMEOUT"); v != "" {
		cfg.Pool.HandshakeTimeout = parseDurationEnv(v, cfg.Pool.HandshakeTimeout)
	}
	if v := os.Getenv("AGY_GW_POOL_INFERENCE_TIMEOUT"); v != "" {
		cfg.Pool.InferenceTimeout = parseDurationEnv(v, cfg.Pool.InferenceTimeout)
	}
	if v := os.Getenv("AGY_GW_POOL_FIRST_TOKEN_TIMEOUT"); v != "" {
		cfg.Pool.FirstTokenTimeout = parseDurationEnv(v, cfg.Pool.FirstTokenTimeout)
	}
	if v := os.Getenv("AGY_GW_POOL_MAX_REQUESTS_PER_WORKER"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			cfg.Pool.MaxRequestsPerWorker = n
		}
	}
	if v := os.Getenv("AGY_GW_POOL_MAX_WORKER_UPTIME"); v != "" {
		cfg.Pool.MaxWorkerUptime = parseDurationEnv(v, cfg.Pool.MaxWorkerUptime)
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
	if v := os.Getenv("AGY_GW_ALLOCATION_STRATEGY"); v != "" {
		cfg.Accounts.AllocationStrategy = v
	}
	if v := os.Getenv("AGY_GW_CREDENTIAL_OP_TIMEOUT"); v != "" {
		cfg.Accounts.CredentialOpTimeout = parseDurationEnv(v, cfg.Accounts.CredentialOpTimeout)
	}
	if v := os.Getenv("AGY_GW_AUTH_EXPIRED_RETRY_INTERVAL"); v != "" {
		cfg.Accounts.AuthExpiredRetryInterval = parseDurationEnv(v, cfg.Accounts.AuthExpiredRetryInterval)
	}
	if v := os.Getenv("AGY_GW_QUOTA_POLL_INTERVAL"); v != "" {
		cfg.Accounts.QuotaPollInterval = parseDurationEnv(v, cfg.Accounts.QuotaPollInterval)
	}
	if v := os.Getenv("AGY_GW_MIN_REMAINING_QUOTA_PCT"); v != "" {
		var pct float64
		if _, err := fmt.Sscanf(v, "%f", &pct); err == nil && pct >= 0 {
			cfg.Accounts.MinRemainingQuotaPct = pct
		}
	}
	if v := os.Getenv("AGY_GW_DEFAULT_COOLDOWN_5H"); v != "" {
		cfg.Accounts.DefaultCooldown5h = parseDurationEnv(v, cfg.Accounts.DefaultCooldown5h)
	}
	if v := os.Getenv("AGY_GW_DEFAULT_COOLDOWN_WEEKLY"); v != "" {
		cfg.Accounts.DefaultCooldownWeekly = parseDurationEnv(v, cfg.Accounts.DefaultCooldownWeekly)
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
		cfg.Execution.RetryInitialBackoff = parseDurationEnv(v, cfg.Execution.RetryInitialBackoff)
	}
	if v := os.Getenv("AGY_GW_RETRY_MAX_BACKOFF"); v != "" {
		cfg.Execution.RetryMaxBackoff = parseDurationEnv(v, cfg.Execution.RetryMaxBackoff)
	}
	if v := os.Getenv("AGY_GW_STRICT_CLIENT_TOOL_DELEGATION"); v != "" {
		cfg.Execution.StrictClientToolDelegation = parseBoolEnv(v, cfg.Execution.StrictClientToolDelegation)
	}
	if v := os.Getenv("AGY_GW_MAX_HISTORY_TOKENS"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			cfg.Execution.MaxHistoryTokens = n
		}
	}

	// 4b. Identity & capabilities overrides
	if v := os.Getenv("AGY_GW_IDENTITY_PROFILE"); v != "" {
		cfg.Identity.Profile = strings.TrimSpace(v)
	}
	if v := os.Getenv("AGY_GW_IDENTITY_NAME"); v != "" {
		cfg.Identity.Name = strings.TrimSpace(v)
	}
	if v := os.Getenv("AGY_GW_IDENTITY_TITLE"); v != "" {
		cfg.Identity.Title = strings.TrimSpace(v)
	}
	if v := os.Getenv("AGY_GW_IDENTITY_VERSION"); v != "" {
		cfg.Identity.Version = strings.TrimSpace(v)
	}
	if v := os.Getenv("AGY_GW_CAPABILITIES_PROFILE"); v != "" {
		cfg.Capabilities.Profile = strings.TrimSpace(v)
	}
	if v := os.Getenv("AGY_GW_CAPABILITIES_FS"); v != "" {
		cfg.Capabilities.FS = parseBoolEnv(v, cfg.Capabilities.FS)
	}
	if v := os.Getenv("AGY_GW_CAPABILITIES_TERMINAL"); v != "" {
		cfg.Capabilities.Terminal = parseBoolEnv(v, cfg.Capabilities.Terminal)
	}
	if v := os.Getenv("AGY_GW_CAPABILITIES_ELICITATION"); v != "" {
		cfg.Capabilities.Elicitation = parseBoolEnv(v, cfg.Capabilities.Elicitation)
	}
	if v := os.Getenv("AGY_GW_CAPABILITIES_SESSION_METADATA"); v != "" {
		cfg.Capabilities.SessionMetadata = parseBoolEnv(v, cfg.Capabilities.SessionMetadata)
	}

	// 4c. Media policy overrides
	if v := os.Getenv("AGY_GW_MEDIA_ALLOW_REMOTE"); v != "" {
		cfg.MCP.Media.AllowRemote = parseBoolEnv(v, cfg.MCP.Media.AllowRemote)
	}
	if v := os.Getenv("AGY_GW_MEDIA_MAX_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.MCP.Media.MaxBytes = n
		}
	}
	if v := os.Getenv("AGY_GW_MEDIA_FETCH_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.MCP.Media.FetchTimeout = d
		}
	}
	if v := os.Getenv("AGY_GW_MEDIA_ALLOWED_MIME_TYPES"); v != "" {
		parts := strings.Split(v, ",")
		types := make([]string, 0, len(parts))
		for _, p := range parts {
			if t := strings.ToLower(strings.TrimSpace(p)); t != "" {
				types = append(types, t)
			}
		}
		if len(types) > 0 {
			cfg.MCP.Media.AllowedMimeTypes = types
		}
	}
	if v := os.Getenv("AGY_GW_MEDIA_ALLOW_PRIVATE_HOSTS"); v != "" {
		cfg.MCP.Media.AllowPrivateHosts = parseBoolEnv(v, cfg.MCP.Media.AllowPrivateHosts)
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

	// 5b. Anthropic Overrides
	if v := getFirstEnv("AGY_GW_ANTHROPIC_DEFAULT_MODEL", "ANTHROPIC_DEFAULT_MODEL"); v != "" {
		cfg.Anthropic.DefaultModel = v
	}
	if v := os.Getenv("AGY_GW_ANTHROPIC_DEFAULT_MAX_TOKENS"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			cfg.Anthropic.DefaultMaxTokens = n
		}
	}
	if v := os.Getenv("AGY_GW_ANTHROPIC_EXPOSE_REASONING_CONTENT"); v != "" {
		cfg.Anthropic.ExposeReasoningContent = parseBoolEnv(v, cfg.Anthropic.ExposeReasoningContent)
	}
	if v := os.Getenv("AGY_GW_MAX_TOOL_ARGUMENT_BYTES"); v != "" {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			cfg.MCP.ClientToolCalling.MaxToolArgumentBytes = n
		}
	}
	if v := os.Getenv("AGY_GW_STRICT_SCHEMA_VALIDATION"); v != "" {
		cfg.MCP.ClientToolCalling.StrictSchemaValidation = parseBoolEnv(v, cfg.MCP.ClientToolCalling.StrictSchemaValidation)
	}
	if v := os.Getenv("AGY_GW_MAX_CLIENT_TOOLS"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			cfg.MCP.ClientToolCalling.MaxClientTools = n
		}
	}
	if v := os.Getenv("AGY_GW_MAX_INTERCEPTED_TOOL_CALLS"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			cfg.MCP.ClientToolCalling.MaxInterceptedToolCalls = n
		}
	}
	if v := os.Getenv("AGY_GW_TOOL_CALL_COLLECT_WINDOW"); v != "" {
		cfg.MCP.ClientToolCalling.ToolCallCollectWindow = parseDurationEnv(v, cfg.MCP.ClientToolCalling.ToolCallCollectWindow)
	}

	// 5d. Execution Policy Overrides
	// acp_session_mode, auto_approve_permissions and agy_acp_args are no longer read: they are
	// product invariants now (see acp.SpawnArgs).
	if v := os.Getenv("AGY_GW_CLEAN_WORKSPACE"); v != "" {
		cfg.Execution.CleanWorkspaceBetweenRequests = parseBoolEnv(v, cfg.Execution.CleanWorkspaceBetweenRequests)
	}
	if v := os.Getenv("AGY_GW_VISION_MODE"); v != "" {
		cfg.Execution.VisionMode = strings.TrimSpace(v)
	}
	if v := os.Getenv("AGY_GW_UNSUPPORTED_PARAMETER_POLICY"); v != "" {
		cfg.Execution.UnsupportedParameterPolicy = strings.ToLower(strings.TrimSpace(v))
	}
	if v := os.Getenv("AGY_GW_CONFIG_SWITCH_TIMEOUT"); v != "" {
		cfg.Pool.ConfigSwitchTimeout = parseDurationEnv(v, cfg.Pool.ConfigSwitchTimeout)
	}
	if v := os.Getenv("AGY_GW_METRICS_ENABLED"); v != "" {
		cfg.Observability.MetricsEnabled = parseBoolEnv(v, cfg.Observability.MetricsEnabled)
	}
	if v := os.Getenv("AGY_GW_METRICS_PATH"); v != "" {
		if p := strings.TrimSpace(v); strings.HasPrefix(p, "/") {
			cfg.Observability.MetricsPath = p
		}
	}
	if v := os.Getenv("AGY_GW_AGY_MIN_VERSION"); v != "" {
		cfg.Binaries.AgyMinVersion = strings.TrimSpace(v)
	}

	// 5c. Outbound Proxy Overrides (Docker Layer & Host)
	if v := os.Getenv("AGY_GW_PROXY_ENABLED"); v != "" {
		cfg.Proxy.Enabled = parseBoolEnv(v, cfg.Proxy.Enabled)
	}
	if v := getFirstEnv("AGY_GW_PROXY_HTTP_PROXY", "HTTP_PROXY", "http_proxy"); v != "" {
		cfg.Proxy.HTTPProxy = strings.TrimSpace(v)
	}
	if v := getFirstEnv("AGY_GW_PROXY_HTTPS_PROXY", "HTTPS_PROXY", "https_proxy"); v != "" {
		cfg.Proxy.HTTPSProxy = strings.TrimSpace(v)
	}
	if v := getFirstEnv("AGY_GW_PROXY_NO_PROXY", "NO_PROXY", "no_proxy"); v != "" {
		cfg.Proxy.NoProxy = strings.TrimSpace(v)
	}
	// Fallback to ALL_PROXY / all_proxy if specific http/https proxies are not set
	if cfg.Proxy.HTTPProxy == "" && cfg.Proxy.HTTPSProxy == "" {
		if v := getFirstEnv("ALL_PROXY", "all_proxy"); v != "" {
			cfg.Proxy.HTTPProxy = strings.TrimSpace(v)
			cfg.Proxy.HTTPSProxy = strings.TrimSpace(v)
		}
	}
	if cfg.Proxy.HTTPProxy != "" || cfg.Proxy.HTTPSProxy != "" {
		cfg.Proxy.Enabled = true
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

	// 10. Auto-update environment variable overrides
	if v := getFirstEnv("AGY_GW_AUTO_UPDATE_ENABLED", "AUTO_UPDATE_ENABLED"); v != "" {
		cfg.AutoUpdate.Enabled = parseBoolEnv(v, cfg.AutoUpdate.Enabled)
	}
	if v := getFirstEnv("AGY_GW_AUTO_UPDATE_INTERVAL", "AUTO_UPDATE_INTERVAL"); v != "" {
		cfg.AutoUpdate.Interval = parseDurationEnv(v, cfg.AutoUpdate.Interval)
	}
	if v := getFirstEnv("AGY_GW_AUTO_UPDATE_ON_STARTUP", "AUTO_UPDATE_ON_STARTUP"); v != "" {
		cfg.AutoUpdate.CheckOnStartup = parseBoolEnv(v, cfg.AutoUpdate.CheckOnStartup)
	}
	if v := getFirstEnv("AGY_GW_AUTO_UPDATE_BIN_DIR", "AUTO_UPDATE_BIN_DIR"); v != "" {
		cfg.AutoUpdate.BinDir = v
	}
	if v := getFirstEnv("AGY_GW_AUTO_UPDATE_AGY_ENABLED", "AUTO_UPDATE_AGY_ENABLED"); v != "" {
		cfg.AutoUpdate.AgyCLI.Enabled = parseBoolEnv(v, cfg.AutoUpdate.AgyCLI.Enabled)
	}
	if v := getFirstEnv("AGY_GW_AUTO_UPDATE_AGY_AUTH_ENABLED", "AUTO_UPDATE_AGY_AUTH_ENABLED"); v != "" {
		cfg.AutoUpdate.AgyAuth.Enabled = parseBoolEnv(v, cfg.AutoUpdate.AgyAuth.Enabled)
	}

	// 11. Prioritize auto-updated binaries in BinDir if present and executable
	if cfg.AutoUpdate.BinDir != "" {
		overrideAgy := filepath.Join(cfg.AutoUpdate.BinDir, "agy")
		if info, err := os.Stat(overrideAgy); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
			cfg.Binaries.AgyPath = overrideAgy
		}
		overrideAuth := filepath.Join(cfg.AutoUpdate.BinDir, "agy-auth")
		if info, err := os.Stat(overrideAuth); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
			cfg.Binaries.AgyAuthPath = overrideAuth
		}
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

// parseDurationEnv parses duration strings ("1800s", "30m", "1h") or raw integer/float
// numbers ("1800", "60") which are treated as seconds. If parsing fails, defaultVal is returned.
//
// @purpose Provides foolproof, fail-soft duration parsing across 12-factor environment variables.
// @rationale Users frequently omit unit suffixes (e.g. AGY_GW_QUOTA_POLL_INTERVAL=1800 vs 1800s).
// Supporting both eliminates silent configuration failures.
func parseDurationEnv(val string, defaultVal time.Duration) time.Duration {
	s := strings.TrimSpace(val)
	if s == "" {
		return defaultVal
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	if secs, err := strconv.ParseFloat(s, 64); err == nil && secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	return defaultVal
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
	if !validVisionMode(cfg.Execution.VisionMode) {
		return fmt.Errorf("execution.vision_mode must be one of %v, got %q",
			visionModes, cfg.Execution.VisionMode)
	}
	if !validUnsupportedParameterPolicy(cfg.Execution.UnsupportedParameterPolicy) {
		return fmt.Errorf("execution.unsupported_parameter_policy must be one of %v, got %q",
			unsupportedParameterPolicies, cfg.Execution.UnsupportedParameterPolicy)
	}
	if !validIdentityProfile(cfg.Identity.Profile) {
		return fmt.Errorf("identity.profile must be one of %v, got %q",
			identityProfiles, cfg.Identity.Profile)
	}
	if strings.TrimSpace(cfg.Identity.Name) == "" {
		return fmt.Errorf("identity.name cannot be empty")
	}
	if strings.TrimSpace(cfg.Identity.Title) == "" {
		return fmt.Errorf("identity.title cannot be empty")
	}
	if strings.TrimSpace(cfg.Identity.Version) == "" {
		return fmt.Errorf("identity.version cannot be empty")
	}
	if !validCapabilityProfile(cfg.Capabilities.Profile) {
		return fmt.Errorf("capabilities.profile must be one of %v, got %q",
			capabilityProfiles, cfg.Capabilities.Profile)
	}
	// Advertise only what is implemented. The acp transport always registers fs + terminal handlers,
	// so advertising them is honest; the sandbox profile deliberately advertises neither so the
	// agent never offers native file/command tools inside the isolated workspace.
	if cfg.Capabilities.Profile == CapabilitiesProfileSandbox {
		cfg.Capabilities.FS = false
		cfg.Capabilities.Terminal = false
		cfg.Capabilities.Elicitation = false
		cfg.Capabilities.SessionMetadata = false
	}
	// The gateway profile restores the gateway's own fingerprint. Only applied when the operator has
	// not overridden the individual fields, so a deliberate custom name still wins.
	if cfg.Identity.Profile == IdentityProfileGateway {
		if cfg.Identity.Name == "" || cfg.Identity.Name == "zed" {
			cfg.Identity.Name = "agy-gateway"
		}
		if cfg.Identity.Title == "" || cfg.Identity.Title == "Zed" {
			cfg.Identity.Title = "Antigravity Gateway"
		}
		if cfg.Identity.Version == "" || cfg.Identity.Version == DefaultZedClientVersion {
			cfg.Identity.Version = version.Get()
		}
	}
	if cfg.Execution.MaxHistoryTokens < 0 {
		return fmt.Errorf("execution.max_history_tokens must be >= 0, got %d", cfg.Execution.MaxHistoryTokens)
	}
	if cfg.MCP.ClientToolCalling.MaxClientTools <= 0 || cfg.MCP.ClientToolCalling.MaxClientTools > 1024 {
		return fmt.Errorf("mcp.client_tool_calling.max_client_tools must be between 1 and 1024, got %d",
			cfg.MCP.ClientToolCalling.MaxClientTools)
	}
	if cfg.MCP.ClientToolCalling.MaxInterceptedToolCalls <= 0 || cfg.MCP.ClientToolCalling.MaxInterceptedToolCalls > 256 {
		return fmt.Errorf("mcp.client_tool_calling.max_intercepted_tool_calls must be between 1 and 256, got %d",
			cfg.MCP.ClientToolCalling.MaxInterceptedToolCalls)
	}
	if cfg.MCP.ClientToolCalling.ToolCallCollectWindow <= 0 {
		return fmt.Errorf("mcp.client_tool_calling.tool_call_collect_window must be > 0")
	}
	if cfg.Pool.ConfigSwitchTimeout <= 0 {
		return fmt.Errorf("pool.config_switch_timeout must be > 0")
	}
	if !validAllocationStrategy(cfg.Accounts.AllocationStrategy) {
		return fmt.Errorf("accounts.allocation_strategy must be one of %v, got %q",
			allocationStrategies, cfg.Accounts.AllocationStrategy)
	}
	// Normalise after validation so the runtime never sees a legacy alias or an empty string.
	cfg.Accounts.AllocationStrategy = NormalizeAllocationStrategy(cfg.Accounts.AllocationStrategy)
	if cfg.Accounts.MinRemainingQuotaPct < 0 || cfg.Accounts.MinRemainingQuotaPct > 100 {
		return fmt.Errorf("accounts.min_remaining_quota_pct must be between 0 and 100, got %v",
			cfg.Accounts.MinRemainingQuotaPct)
	}
	if cfg.Accounts.DefaultCooldown5h <= 0 {
		return fmt.Errorf("accounts.default_cooldown_5h must be > 0")
	}
	if cfg.Accounts.DefaultCooldownWeekly <= 0 {
		return fmt.Errorf("accounts.default_cooldown_weekly must be > 0")
	}
	if cfg.Accounts.CredentialOpTimeout <= 0 {
		return fmt.Errorf("accounts.credential_op_timeout must be > 0")
	}
	if cfg.Accounts.AuthExpiredRetryInterval <= 0 {
		return fmt.Errorf("accounts.auth_expired_retry_interval must be > 0")
	}
	if cfg.Accounts.QuotaPollInterval > 0 && cfg.Accounts.QuotaPollInterval < 10*time.Second {
		return fmt.Errorf("accounts.quota_poll_interval must be >= 10s, got %v", cfg.Accounts.QuotaPollInterval)
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
	if cfg.Anthropic.DefaultMaxTokens <= 0 {
		cfg.Anthropic.DefaultMaxTokens = 4096
	}
	if cfg.Anthropic.DefaultModel == "" {
		cfg.Anthropic.DefaultModel = "claude-3-5-sonnet"
	}

	// Validate and configure outbound HTTP/HTTPS proxy
	if cfg.Proxy.Enabled || cfg.Proxy.HTTPProxy != "" || cfg.Proxy.HTTPSProxy != "" {
		if cfg.Proxy.HTTPProxy != "" {
			u, err := url.ParseRequestURI(cfg.Proxy.HTTPProxy)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
				return fmt.Errorf("invalid proxy.http_proxy URL %q: scheme must be http or https", cfg.Proxy.HTTPProxy)
			}
		}
		if cfg.Proxy.HTTPSProxy != "" {
			u, err := url.ParseRequestURI(cfg.Proxy.HTTPSProxy)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
				return fmt.Errorf("invalid proxy.https_proxy URL %q: scheme must be http or https", cfg.Proxy.HTTPSProxy)
			}
		}

		// Ensure loopback addresses are included in NoProxy to protect local healthchecks and OAuth callbacks
		noProxyList := strings.Split(cfg.Proxy.NoProxy, ",")
		noProxyMap := make(map[string]bool)
		for _, item := range noProxyList {
			if trimmed := strings.TrimSpace(item); trimmed != "" {
				noProxyMap[trimmed] = true
			}
		}
		for _, required := range []string{"localhost", "127.0.0.1", "::1"} {
			if !noProxyMap[required] {
				noProxyList = append(noProxyList, required)
				noProxyMap[required] = true
			}
		}
		cfg.Proxy.NoProxy = strings.Join(noProxyList, ",")

		// Synchronize proxy settings to the process environment so all standard Go/Node/subprocess calls inherit them
		cfg.Proxy.ExportToEnv()
	}

	return nil
}
