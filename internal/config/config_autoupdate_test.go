package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"agy-gateway/internal/config"
)

func TestConfig_AutoUpdate_Defaults(t *testing.T) {
	cfg := config.DefaultConfig()

	if !cfg.AutoUpdate.Enabled {
		t.Errorf("expected AutoUpdate.Enabled to be true by default")
	}
	if cfg.AutoUpdate.Interval != 6*time.Hour {
		t.Errorf("expected AutoUpdate.Interval to be 6h, got %v", cfg.AutoUpdate.Interval)
	}
	if !cfg.AutoUpdate.CheckOnStartup {
		t.Errorf("expected AutoUpdate.CheckOnStartup to be true by default")
	}
	if cfg.AutoUpdate.BinDir != "/var/lib/agy-gateway/bin" {
		t.Errorf("expected AutoUpdate.BinDir to be /var/lib/agy-gateway/bin, got %s", cfg.AutoUpdate.BinDir)
	}
	if !cfg.AutoUpdate.AgyCLI.Enabled {
		t.Errorf("expected AutoUpdate.AgyCLI.Enabled to be true by default")
	}
	if !cfg.AutoUpdate.AgyAuth.Enabled {
		t.Errorf("expected AutoUpdate.AgyAuth.Enabled to be true by default")
	}
}

func TestConfig_AutoUpdate_ThreadSafeGettersSetters(t *testing.T) {
	cfg := config.DefaultConfig()

	cfg.SetAgyPath("/custom/bin/agy")
	if p := cfg.GetAgyPath(); p != "/custom/bin/agy" {
		t.Errorf("expected /custom/bin/agy, got %s", p)
	}

	cfg.SetAgyAuthPath("/custom/bin/agy-auth")
	if p := cfg.GetAgyAuthPath(); p != "/custom/bin/agy-auth" {
		t.Errorf("expected /custom/bin/agy-auth, got %s", p)
	}
}

func TestConfig_AutoUpdate_BinDirPrioritization(t *testing.T) {
	tmpDir := t.TempDir()
	agyBin := filepath.Join(tmpDir, "agy")
	authBin := filepath.Join(tmpDir, "agy-auth")

	// Create dummy executable files
	if err := os.WriteFile(agyBin, []byte("#!/bin/sh\necho 1.5.17\n"), 0755); err != nil {
		t.Fatalf("failed to write dummy agy: %v", err)
	}
	if err := os.WriteFile(authBin, []byte("#!/bin/sh\necho 0.5.4\n"), 0755); err != nil {
		t.Fatalf("failed to write dummy agy-auth: %v", err)
	}

	t.Setenv("AGY_GW_AUTO_UPDATE_BIN_DIR", tmpDir)
	t.Setenv("AGY_GW_AUTO_UPDATE_ENABLED", "true")
	t.Setenv("AGY_GW_AUTO_UPDATE_INTERVAL", "12h")

	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	if cfg.AutoUpdate.BinDir != tmpDir {
		t.Errorf("expected BinDir %s, got %s", tmpDir, cfg.AutoUpdate.BinDir)
	}
	if cfg.AutoUpdate.Interval != 12*time.Hour {
		t.Errorf("expected Interval 12h, got %v", cfg.AutoUpdate.Interval)
	}
	if cfg.GetAgyPath() != agyBin {
		t.Errorf("expected AgyPath to resolve to %s, got %s", agyBin, cfg.GetAgyPath())
	}
	if cfg.GetAgyAuthPath() != authBin {
		t.Errorf("expected AgyAuthPath to resolve to %s, got %s", authBin, cfg.GetAgyAuthPath())
	}
}
