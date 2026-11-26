package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"agy-gateway/internal/config"
)

// TestSetupLogging_EmitsRemovedKeysAfterHandlerInstall closes the wiring gap: `config.Load` records
// the removed keys but never logs them, so the disclosure only reaches an operator if the entrypoint
// emits it after installing the configured handler. Deleting that call from setupLogging now fails.
func TestSetupLogging_EmitsRemovedKeysAfterHandlerInstall(t *testing.T) {
	cfg := &config.Config{}
	cfg.Observability.LogFormat = "json"
	cfg.Observability.LogLevel = "info"
	cfg.RemovedKeys = []string{"execution.auto_approve_permissions"}

	var buf bytes.Buffer
	setupLogging(cfg, &buf)

	// Valid JSON proves the record went through the configured handler rather than a bare
	// slog.Default() text sink that would bypass observability.log_format.
	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line); err != nil {
		t.Fatalf("removed-key warning is not JSON through the configured handler: %q (%v)", buf.String(), err)
	}
	if line["level"] != "WARN" {
		t.Errorf("expected WARN level, got %v", line["level"])
	}
	if line["key"] != "execution.auto_approve_permissions" {
		t.Errorf("expected the removed key, got %v", line["key"])
	}
	if line["reason"] == nil || line["reason"] == "" {
		t.Errorf("expected an actionable reason, got %v", line["reason"])
	}
}
