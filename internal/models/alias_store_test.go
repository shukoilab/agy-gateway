package models_test

import (
	"path/filepath"
	"testing"

	"agy-gateway/internal/models"
)

func TestAliasStore_NormalizeAlias(t *testing.T) {
	cases := map[string]string{
		"Claude.3.5.Sonnet": "claude-3-5-sonnet",
		"claude_3_5_sonnet": "claude-3-5-sonnet",
		"  claude-sonnet  ": "claude-sonnet",
	}

	for input, expected := range cases {
		if got := models.NormalizeAlias(input); got != expected {
			t.Errorf("NormalizeAlias(%q) = %q, expected %q", input, got, expected)
		}
	}
}

func TestAliasStore_CRUDAndPersistence(t *testing.T) {
	tmp := t.TempDir()
	store1 := models.NewAliasStore(tmp, nil)

	// Set dynamic aliases
	store1.SetDynamicAliases(map[string]string{
		"claude-3-5-sonnet": "claude-sonnet-4-6",
		"claude-3-5-haiku":  "claude-haiku-4-5",
	})

	// Dynamic resolution
	target, found := store1.Resolve("claude.3.5.sonnet")
	if !found || target != "claude-sonnet-4-6" {
		t.Fatalf("expected resolution to claude-sonnet-4-6, got %q (found: %v)", target, found)
	}

	// Set manual override
	err := store1.SetManual("claude-3-5-sonnet", "custom-pinned-model", "Test manual pin")
	if err != nil {
		t.Fatalf("failed to set manual alias: %v", err)
	}

	// Verify manual override takes precedence
	target, found = store1.Resolve("claude-3-5-sonnet")
	if !found || target != "custom-pinned-model" {
		t.Fatalf("expected manual override to custom-pinned-model, got %q", target)
	}

	// Reload from new store instance pointing to same vault dir
	store2 := models.NewAliasStore(tmp, nil)
	target, found = store2.Resolve("claude-3-5-sonnet")
	if !found || target != "custom-pinned-model" {
		t.Fatalf("expected reloaded store to preserve manual override, got %q", target)
	}

	// Delete manual override
	deleted, err := store2.DeleteManual("claude-3-5-sonnet")
	if err != nil || !deleted {
		t.Fatalf("expected successful deletion, got deleted=%v, err=%v", deleted, err)
	}

	// After deletion, manual alias is gone
	target, found = store2.Resolve("claude-3-5-sonnet")
	if found {
		t.Fatalf("expected alias to be unresolvable after deletion without dynamic setup, got %q", target)
	}

	_ = filepath.Join(tmp, "anthropic-model-aliases.json")
}
