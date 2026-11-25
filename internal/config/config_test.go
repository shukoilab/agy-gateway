package config

import (
	"os"
	"testing"
	"time"
)

func TestConfigLoadAndValidation(t *testing.T) {
	tmpDir := t.TempDir()
	configFile := tmpDir + "/test_config.yaml"

	content := `
server:
  host: "127.0.0.1"
  port: 9090
  read_header_timeout: "5s"
  idle_timeout: "60s"
  shutdown_drain_timeout: "10s"
  max_request_body_bytes: 1048576
  sse_keepalive_interval: "5s"

binaries:
  agy_path: "/usr/local/bin/agy"
  agy_min_version: "1.5.5"
  agy_auth_path: "/usr/local/bin/agy-auth"
  agy_acp_args: ["acp", "--dangerously-skip-permissions"]

execution:
  yolo_mode: true
  acp_session_mode: "accept-edits"

pool:
  worker_count: 2

openai:
  default_model: "gemini-3.8-flash"
`
	if err := os.WriteFile(configFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	cfg, err := Load(configFile)
	if err != nil {
		t.Fatalf("failed to load valid config: %v", err)
	}

	if cfg.Server.Port != 9090 {
		t.Errorf("expected port 9090, got %d", cfg.Server.Port)
	}

	if cfg.OpenAI.DefaultModel != "gemini-3.8-flash" {
		t.Errorf("expected default model gemini-3.8-flash, got %s", cfg.OpenAI.DefaultModel)
	}

	// Test reasoning effort mapping
	if effort := MapReasoningEffort("high", "medium"); effort != "high" {
		t.Errorf("expected high, got %s", effort)
	}
	if effort := MapReasoningEffort("none", "medium"); effort != "low" {
		t.Errorf("expected low for none (agy has no 'off' level), got %s", effort)
	}
}

func TestConfigEnvOverridesAndDefaults(t *testing.T) {
	// 1. Set various 12-factor environment variables
	_ = os.Setenv("PORT", "7777")
	_ = os.Setenv("HOST", "127.0.0.1")
	_ = os.Setenv("WORKER_COUNT", "4")
	_ = os.Setenv("LOG_LEVEL", "debug")
	_ = os.Setenv("DEFAULT_MODEL", "claude-sonnet-4.6")
	_ = os.Setenv("AGY_GW_MAX_RETRIES", "5")
	_ = os.Setenv("AGY_GW_RETRY_INITIAL_BACKOFF", "250ms")
	_ = os.Setenv("AGY_GW_RETRY_MAX_BACKOFF", "5s")
	_ = os.Setenv("AGY_GW_CLIENT_KEYS", "secret-key-1:60,secret-key-2:120")
	_ = os.Setenv("AGY_GW_ALLOWED_CORS_ORIGINS", "http://localhost:3000, http://localhost:5173")

	defer func() {
		_ = os.Unsetenv("PORT")
		_ = os.Unsetenv("HOST")
		_ = os.Unsetenv("WORKER_COUNT")
		_ = os.Unsetenv("LOG_LEVEL")
		_ = os.Unsetenv("DEFAULT_MODEL")
		_ = os.Unsetenv("AGY_GW_MAX_RETRIES")
		_ = os.Unsetenv("AGY_GW_RETRY_INITIAL_BACKOFF")
		_ = os.Unsetenv("AGY_GW_RETRY_MAX_BACKOFF")
		_ = os.Unsetenv("AGY_GW_CLIENT_KEYS")
		_ = os.Unsetenv("AGY_GW_ALLOWED_CORS_ORIGINS")
	}()

	// 2. Load without existing YAML file (tests DefaultConfig + applyEnvOverrides fallback)
	cfg, err := Load("non-existent-default/config.yaml")
	// If path doesn't exist and is config.yaml, it initializes cleanly
	if err != nil {
		cfg, err = Load("config.yaml")
	}
	if err != nil {
		t.Fatalf("failed to load config with defaults and env overrides: %v", err)
	}

	if cfg.Server.Port != 7777 {
		t.Errorf("expected port 7777 from PORT env, got %d", cfg.Server.Port)
	}
	if cfg.Server.Host != "127.0.0.1" {
		t.Errorf("expected host 127.0.0.1 from HOST env, got %s", cfg.Server.Host)
	}
	if cfg.Pool.WorkerCount != 4 {
		t.Errorf("expected worker_count 4 from WORKER_COUNT env, got %d", cfg.Pool.WorkerCount)
	}
	if cfg.Observability.LogLevel != "debug" {
		t.Errorf("expected log_level debug from LOG_LEVEL env, got %s", cfg.Observability.LogLevel)
	}
	if cfg.OpenAI.DefaultModel != "claude-sonnet-4.6" {
		t.Errorf("expected default model claude-sonnet-4.6 from DEFAULT_MODEL env, got %s", cfg.OpenAI.DefaultModel)
	}
	if len(cfg.Security.APIKeys) < 2 {
		t.Fatalf("expected at least 2 parsed client API keys, got %d", len(cfg.Security.APIKeys))
	}
	if cfg.Security.APIKeys[0].KeySecret != "secret-key-1" || cfg.Security.APIKeys[0].RateLimitRPM != 60 {
		t.Errorf("unexpected APIKey[0]: %+v", cfg.Security.APIKeys[0])
	}
	if cfg.Security.APIKeys[1].KeySecret != "secret-key-2" || cfg.Security.APIKeys[1].RateLimitRPM != 120 {
		t.Errorf("unexpected APIKey[1]: %+v", cfg.Security.APIKeys[1])
	}
	if len(cfg.Security.AllowedCORSOrigins) != 2 || cfg.Security.AllowedCORSOrigins[0] != "http://localhost:3000" {
		t.Errorf("unexpected AllowedCORSOrigins: %v", cfg.Security.AllowedCORSOrigins)
	}
	if cfg.Execution.MaxRetries != 5 || cfg.Accounts.MaxFailoverRetries != 5 {
		t.Errorf("expected MaxRetries 5 from AGY_GW_MAX_RETRIES env, got exec=%d, acc=%d", cfg.Execution.MaxRetries, cfg.Accounts.MaxFailoverRetries)
	}
	if cfg.Execution.RetryInitialBackoff != 250*time.Millisecond {
		t.Errorf("expected RetryInitialBackoff 250ms, got %v", cfg.Execution.RetryInitialBackoff)
	}
	if cfg.Execution.RetryMaxBackoff != 5*time.Second {
		t.Errorf("expected RetryMaxBackoff 5s, got %v", cfg.Execution.RetryMaxBackoff)
	}
}

func TestConfig_1MContextDefaults(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Server.MaxRequestBodyBytes != 104857600 {
		t.Errorf("expected 100 MiB (104857600) MaxRequestBodyBytes, got %d", cfg.Server.MaxRequestBodyBytes)
	}
	if cfg.Server.IdleTimeout != 300*time.Second {
		t.Errorf("expected 300s IdleTimeout, got %v", cfg.Server.IdleTimeout)
	}
	if cfg.Server.ShutdownDrainTimeout != 120*time.Second {
		t.Errorf("expected 120s ShutdownDrainTimeout, got %v", cfg.Server.ShutdownDrainTimeout)
	}
	if cfg.Pool.InferenceTimeout != 900*time.Second {
		t.Errorf("expected 900s InferenceTimeout, got %v", cfg.Pool.InferenceTimeout)
	}
	if cfg.Pool.FirstTokenTimeout != 120*time.Second {
		t.Errorf("expected 120s FirstTokenTimeout, got %v", cfg.Pool.FirstTokenTimeout)
	}
	if cfg.Pool.MaxScannerBufferBytes != 134217728 {
		t.Errorf("expected 128 MiB (134217728) MaxScannerBufferBytes, got %d", cfg.Pool.MaxScannerBufferBytes)
	}
	if cfg.MCP.ClientToolCalling.MaxToolArgumentBytes != 16777216 {
		t.Errorf("expected 16 MiB (16777216) MaxToolArgumentBytes, got %d", cfg.MCP.ClientToolCalling.MaxToolArgumentBytes)
	}
}

func TestConfig_ParseDurationEnv(t *testing.T) {
	defaultDur := 30 * time.Second

	tests := []struct {
		input    string
		expected time.Duration
	}{
		{"1800s", 1800 * time.Second},
		{"30m", 30 * time.Minute},
		{"1h", 1 * time.Hour},
		{"1800", 1800 * time.Second}, // raw number without unit treated as seconds
		{"45.5", 45500 * time.Millisecond},
		{"", defaultDur},
		{"invalid", defaultDur},
		{"0", defaultDur},
		{"-10s", defaultDur},
	}

	for _, tt := range tests {
		got := parseDurationEnv(tt.input, defaultDur)
		if got != tt.expected {
			t.Errorf("parseDurationEnv(%q) = %v; want %v", tt.input, got, tt.expected)
		}
	}
}

func TestConfig_NewEnvOverrides(t *testing.T) {
	t.Setenv("AGY_GW_QUOTA_POLL_INTERVAL", "1800s")
	t.Setenv("AGY_GW_UNSUPPORTED_PARAM_POLICY", "reject")
	t.Setenv("AGY_GW_VISION_MODE", "flatten")
	t.Setenv("AGY_GW_CLEAN_WORKSPACE", "false")
	t.Setenv("AGY_GW_DEFAULT_COOLDOWN_5H", "45m")
	t.Setenv("AGY_GW_DEFAULT_COOLDOWN_WEEKLY", "12h")

	cfg := DefaultConfig()
	applyEnvOverrides(cfg)

	if cfg.Accounts.QuotaPollInterval != 1800*time.Second {
		t.Errorf("expected QuotaPollInterval 1800s, got %v", cfg.Accounts.QuotaPollInterval)
	}
	if cfg.OpenAI.UnsupportedParamPolicy != "reject" {
		t.Errorf("expected UnsupportedParamPolicy 'reject', got %v", cfg.OpenAI.UnsupportedParamPolicy)
	}
	if cfg.Execution.VisionMode != "flatten" {
		t.Errorf("expected VisionMode 'flatten', got %v", cfg.Execution.VisionMode)
	}
	if cfg.Execution.CleanWorkspaceBetweenRequests {
		t.Errorf("expected CleanWorkspaceBetweenRequests false, got %v", cfg.Execution.CleanWorkspaceBetweenRequests)
	}
	if cfg.Accounts.DefaultCooldown5h != 45*time.Minute {
		t.Errorf("expected DefaultCooldown5h 45m, got %v", cfg.Accounts.DefaultCooldown5h)
	}
	if cfg.Accounts.DefaultCooldownWeekly != 12*time.Hour {
		t.Errorf("expected DefaultCooldownWeekly 12h, got %v", cfg.Accounts.DefaultCooldownWeekly)
	}
}
