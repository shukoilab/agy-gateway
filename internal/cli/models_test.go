package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agy-gateway/internal/auth"
	"agy-gateway/internal/config"
	"agy-gateway/internal/models"
)

func setupTestModelsCLI(t *testing.T) (*ModelsCLI, *models.ModelRegistry, *bytes.Buffer, *bytes.Buffer, string) {
	tempDir, err := os.MkdirTemp("", "models-cli-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	cfg := &config.Config{
		Storage: config.StorageConfig{
			VaultXDGConfigDir: filepath.Join(tempDir, "vault"),
		},
	}
	authMgr := auth.NewManager(cfg, nil)
	mr := models.NewModelRegistry(cfg, authMgr, nil)

	mr.RegisterModel(&models.ModelInfo{
		ID:          "claude-sonnet-4-6",
		DisplayName: "Claude Sonnet 4.6",
		OwnedBy:     "anthropic",
	})
	mr.RegisterModel(&models.ModelInfo{
		ID:          "gemini-3.8-flash",
		DisplayName: "Gemini 3.8 Flash",
		OwnedBy:     "google",
	})

	mr.AliasStore().SetDynamicAliases(map[string]string{
		"claude-3-7-sonnet": "claude-sonnet-4-6",
	})

	inBuf := new(bytes.Buffer)
	outBuf := new(bytes.Buffer)

	cli := NewModelsCLI(cfg, authMgr, mr, inBuf, outBuf)
	return cli, mr, inBuf, outBuf, tempDir
}

func TestModelsCLI_HelpAndList(t *testing.T) {
	cli, _, _, outBuf, tempDir := setupTestModelsCLI(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()

	// 1. Test help
	err := cli.Run(ctx, []string{"help"})
	if err != nil {
		t.Fatalf("help failed: %v", err)
	}
	if !strings.Contains(outBuf.String(), "Anthropic Model Aliasing & Ranking CLI") {
		t.Fatalf("expected help output, got: %s", outBuf.String())
	}
	outBuf.Reset()

	// 2. Test list (table format)
	err = cli.Run(ctx, []string{"list"})
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	output := outBuf.String()
	if !strings.Contains(output, "claude-3-7-sonnet") || !strings.Contains(output, "claude-sonnet-4-6") {
		t.Fatalf("expected list output to contain alias and target, got: %s", output)
	}
	outBuf.Reset()

	// 3. Test list with -json
	err = cli.Run(ctx, []string{"list", "-json"})
	if err != nil {
		t.Fatalf("list -json failed: %v", err)
	}
	var jsonResp map[string]interface{}
	if err := json.Unmarshal(outBuf.Bytes(), &jsonResp); err != nil {
		t.Fatalf("expected valid JSON, got error: %v (output: %s)", err, outBuf.String())
	}
	if jsonResp["total"].(float64) < 1 {
		t.Errorf("expected total >= 1, got %v", jsonResp["total"])
	}
}

func TestModelsCLI_SetAndRemoveManualAlias(t *testing.T) {
	cli, mr, _, outBuf, tempDir := setupTestModelsCLI(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()

	// 1. Set manual alias override
	err := cli.Run(ctx, []string{"set", "claude-3-7-sonnet", "gemini-3.8-flash", "Test manual pin"})
	if err != nil {
		t.Fatalf("set failed: %v", err)
	}
	if !strings.Contains(outBuf.String(), "Successfully configured manual alias") {
		t.Fatalf("unexpected set output: %s", outBuf.String())
	}
	outBuf.Reset()

	// Verify resolution reflects manual override
	target, ok := mr.AliasStore().Resolve("claude-3-7-sonnet")
	if !ok || target != "gemini-3.8-flash" {
		t.Fatalf("expected resolved target gemini-3.8-flash, got %s (ok=%v)", target, ok)
	}

	// 2. Remove manual alias override
	err = cli.Run(ctx, []string{"remove", "claude-3-7-sonnet"})
	if err != nil {
		t.Fatalf("remove failed: %v", err)
	}
	if !strings.Contains(outBuf.String(), "Successfully removed manual alias override") {
		t.Fatalf("unexpected remove output: %s", outBuf.String())
	}
	outBuf.Reset()

	// Verify resolution reverted to dynamic synthesized alias
	target, ok = mr.AliasStore().Resolve("claude-3-7-sonnet")
	if !ok || target != "claude-sonnet-4-6" {
		t.Fatalf("expected resolved target to revert to claude-sonnet-4-6, got %s (ok=%v)", target, ok)
	}

	// 3. Removing non-existent alias returns error
	err = cli.Run(ctx, []string{"remove", "non-existent-alias"})
	if err == nil {
		t.Fatal("expected error removing non-existent alias, got nil")
	}
}

func TestModelsCLI_InteractiveExit(t *testing.T) {
	cli, _, inBuf, outBuf, tempDir := setupTestModelsCLI(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()

	// Simulate user typing '0' then Enter
	inBuf.WriteString("0\n")

	err := cli.Run(ctx, []string{"interactive"})
	if err != nil {
		t.Fatalf("interactive exit failed: %v", err)
	}

	if !strings.Contains(outBuf.String(), "Returning to main menu") {
		t.Fatalf("expected exit message, got: %s", outBuf.String())
	}
}
