package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agy-gateway/internal/config"
	"agy-gateway/internal/updater"
)

func TestUpdaterCLI_ShowStatus(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutoUpdate.BinDir = t.TempDir()
	cfg.AutoUpdate.Interval = 12 * time.Hour

	var out bytes.Buffer
	cli := NewUpdaterCLI(cfg, nil, nil, &out)

	ctx := context.Background()
	if err := cli.ShowStatus(ctx, false); err != nil {
		t.Fatalf("ShowStatus failed: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "AUTO-UPDATER & BINARY STATUS") {
		t.Errorf("expected header in output, got: %s", output)
	}
	if !strings.Contains(output, "agy") {
		t.Errorf("expected agy component in table, got: %s", output)
	}
	if !strings.Contains(output, "agy-auth") {
		t.Errorf("expected agy-auth component in table, got: %s", output)
	}
}

func TestUpdaterCLI_ShowStatus_JSON(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutoUpdate.BinDir = t.TempDir()

	var out bytes.Buffer
	cli := NewUpdaterCLI(cfg, nil, nil, &out)

	ctx := context.Background()
	if err := cli.ShowStatus(ctx, true); err != nil {
		t.Fatalf("ShowStatus JSON failed: %v", err)
	}

	var status updater.AutoUpdateStatusResponse
	if err := json.Unmarshal(out.Bytes(), &status); err != nil {
		t.Fatalf("failed to decode status JSON: %v", err)
	}

	if !status.Enabled {
		t.Errorf("expected status.Enabled to be true")
	}
}

func TestUpdaterCLI_Run_Subcommands(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutoUpdate.BinDir = t.TempDir()

	var out bytes.Buffer
	cli := NewUpdaterCLI(cfg, nil, nil, &out)

	// Test help
	if err := cli.Run([]string{"help"}); err != nil {
		t.Errorf("expected help to succeed, got %v", err)
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("expected help output, got %s", out.String())
	}

	out.Reset()
	// Test status
	if err := cli.Run([]string{"status"}); err != nil {
		t.Errorf("expected status to succeed, got %v", err)
	}
	if !strings.Contains(out.String(), "AUTO-UPDATER & BINARY STATUS") {
		t.Errorf("expected status output, got %s", out.String())
	}
}
