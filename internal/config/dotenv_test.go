package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"agy-gateway/internal/config"
)

func TestLoadDotEnv(t *testing.T) {
	tmpDir := t.TempDir()
	envPath := filepath.Join(tmpDir, ".env")

	content := `
# Example environment file
export AGY_GW_TEST_PORT=9090
AGY_GW_TEST_HOST="127.0.0.1"
AGY_GW_TEST_STRING='hello world'
AGY_GW_TEST_COMMENT=active # inline comment
`
	if err := os.WriteFile(envPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test env: %v", err)
	}

	// Pre-set variable to ensure it is not overwritten
	_ = os.Setenv("AGY_GW_TEST_HOST", "0.0.0.0")

	loaded, err := config.LoadDotEnv(envPath)
	if err != nil {
		t.Fatalf("LoadDotEnv returned error: %v", err)
	}

	if loaded < 3 {
		t.Errorf("expected at least 3 newly loaded env vars, got %d", loaded)
	}

	if val := os.Getenv("AGY_GW_TEST_PORT"); val != "9090" {
		t.Errorf("expected AGY_GW_TEST_PORT='9090', got %q", val)
	}

	if val := os.Getenv("AGY_GW_TEST_HOST"); val != "0.0.0.0" {
		t.Errorf("expected pre-existing AGY_GW_TEST_HOST to remain '0.0.0.0', got %q", val)
	}

	if val := os.Getenv("AGY_GW_TEST_STRING"); val != "hello world" {
		t.Errorf("expected AGY_GW_TEST_STRING='hello world', got %q", val)
	}

	if val := os.Getenv("AGY_GW_TEST_COMMENT"); val != "active" {
		t.Errorf("expected AGY_GW_TEST_COMMENT='active', got %q", val)
	}
}
