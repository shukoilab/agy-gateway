package config

import (
	"os"
	"testing"
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
	if effort := MapReasoningEffort("none", "medium"); effort != "off" {
		t.Errorf("expected off for none, got %s", effort)
	}
}
