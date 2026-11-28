package models

import (
	"testing"

	"agy-gateway/internal/auth"
	"agy-gateway/internal/config"
	"agy-gateway/internal/openai"
)

func TestParseAgyModelsOutput(t *testing.T) {
	rawOutput := []byte(`Fetching available models...
gemini-3.8-flash-high	Gemini 3.8 Flash (High)
gemini-3.8-flash-medium	Gemini 3.8 Flash (Medium)
gemini-3.8-flash-low	Gemini 3.8 Flash (Low)
claude-sonnet-4-6	Claude Sonnet 4.6 (Thinking)
claude-opus-4-6-thinking	Claude Opus 4.6 (Thinking)
gpt-oss-120b-medium	GPT-OSS 120B (Medium)
`)

	parsed := parseAgyModelsOutput(rawOutput)
	if len(parsed) != 7 { // 6 discovered + 1 inferred base model (gemini-3.8-flash)
		t.Fatalf("expected 7 models (including inferred base), got %d", len(parsed))
	}

	modelMap := make(map[string]*ModelInfo)
	for _, m := range parsed {
		modelMap[m.ID] = m
	}

	// Verify gemini-3.8-flash-high
	high, exists := modelMap["gemini-3.8-flash-high"]
	if !exists {
		t.Fatal("missing gemini-3.8-flash-high")
	}
	if high.UpstreamModel != "gemini-3.8-flash" {
		t.Errorf("expected upstream gemini-3.8-flash, got %s", high.UpstreamModel)
	}
	if high.PresetEffort != "high" {
		t.Errorf("expected preset effort high, got %s", high.PresetEffort)
	}
	if high.OwnedBy != "google" {
		t.Errorf("expected owned_by google, got %s", high.OwnedBy)
	}
	if high.ContextWindow != 1048576 {
		t.Errorf("expected context window 1048576, got %d", high.ContextWindow)
	}
	if high.MaxOutputTokens != 65536 {
		t.Errorf("expected max output tokens 65536, got %d", high.MaxOutputTokens)
	}

	// Verify inferred base model
	base, exists := modelMap["gemini-3.8-flash"]
	if !exists {
		t.Fatal("missing inferred base model gemini-3.8-flash")
	}
	if base.UpstreamModel != "gemini-3.8-flash" || base.PresetEffort != "" {
		t.Errorf("invalid base model config: %+v", base)
	}

	// Verify claude opus is NOT gated on tier.
	//
	// @rationale A Free account was measured serving claude-opus-4-6-thinking successfully, so the old
	// "model id contains opus => paid" heuristic refused requests that work. Tier is reporting-only.
	opus, exists := modelMap["claude-opus-4-6-thinking"]
	if !exists {
		t.Fatal("missing claude-opus-4-6-thinking")
	}
	if opus.RequiresPaidTier {
		t.Error("opus must not be gated on tier: a Free account serves it")
	}
	if opus.OwnedBy != "anthropic" {
		t.Errorf("expected owned_by anthropic, got %s", opus.OwnedBy)
	}

	// Verify gpt-oss owned by openai
	gpt, exists := modelMap["gpt-oss-120b-medium"]
	if !exists {
		t.Fatal("missing gpt-oss-120b-medium")
	}
	if gpt.OwnedBy != "openai" {
		t.Errorf("expected owned_by openai, got %s", gpt.OwnedBy)
	}
}

func TestModelRegistryThreadSafety(t *testing.T) {
	cfg := &config.Config{
		OpenAI: config.OpenAIConfig{
			DefaultModel: "gemini-3.8-flash",
		},
	}
	authMgr := auth.NewManager(cfg, nil)
	mr := NewModelRegistry(cfg, authMgr, nil)

	// Manually inject parsed models
	rawOutput := []byte(`
gemini-3.8-flash-high	Gemini 3.8 Flash (High)
claude-sonnet-4-6	Claude Sonnet 4.6 (Thinking)
`)
	models := parseAgyModelsOutput(rawOutput)
	mr.mu.Lock()
	for _, m := range models {
		mr.models[m.ID] = m
	}
	mr.modelList = []openai.ModelObject{
		{ID: "gemini-3.8-flash", Object: "model", Created: 1740000000, OwnedBy: "google"},
		{ID: "gemini-3.8-flash-high", Object: "model", Created: 1740000000, OwnedBy: "google"},
		{ID: "claude-sonnet-4-6", Object: "model", Created: 1740000000, OwnedBy: "anthropic"},
	}
	mr.mu.Unlock()

	// Concurrent read test
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				_ = mr.ListModels()
				_, _ = mr.FindModel("gemini-3.8-flash")
				_ = mr.DefaultModel()
			}
			done <- true
		}()
	}

	for i := 0; i < 10; i++ {
		<-done
	}
}

func TestModelRegistryFindModelAliasing(t *testing.T) {
	cfg := &config.Config{
		Anthropic: config.AnthropicConfig{
			DefaultModel: "",
		},
	}
	authMgr := auth.NewManager(cfg, nil)
	mr := NewModelRegistry(cfg, authMgr, nil)

	mr.mu.Lock()
	mr.models = map[string]*ModelInfo{
		"gemini-3.8-flash":  {ID: "gemini-3.8-flash", OwnedBy: "google"},
		"claude-sonnet-4-6": {ID: "claude-sonnet-4-6", OwnedBy: "anthropic"},
	}
	mr.champions = map[string]*ModelInfo{
		"claude-sonnet": mr.models["claude-sonnet-4-6"],
		"gemini-flash":  mr.models["gemini-3.8-flash"],
	}
	mr.mu.Unlock()

	mr.AliasStore().SetDynamicAliases(map[string]string{
		"claude-3-7-sonnet": "claude-sonnet-4-6",
		"claude-3-5-sonnet": "claude-sonnet-4-6",
	})

	// Test dynamic alias resolution
	m, found := mr.FindModel("claude-3-7-sonnet")
	if !found || m.ID != "claude-sonnet-4-6" {
		t.Fatalf("expected claude-sonnet-4-6, got %+v (found=%v)", m, found)
	}

	// Test normalized dot resolution
	m, found = mr.FindModel("claude.3.7.sonnet")
	if !found || m.ID != "claude-sonnet-4-6" {
		t.Fatalf("expected claude-sonnet-4-6 for dot-normalized, got %+v (found=%v)", m, found)
	}

	// Test admin manual override taking precedence
	if err := mr.AliasStore().SetManual("claude-3-7-sonnet", "gemini-3.8-flash", "test override"); err != nil {
		t.Fatalf("SetManual failed: %v", err)
	}

	m, found = mr.FindModel("claude-3-7-sonnet")
	if !found || m.ID != "gemini-3.8-flash" {
		t.Fatalf("expected gemini-3.8-flash after manual override, got %+v", m)
	}

	// Test AnthropicDefaultModel
	def := mr.AnthropicDefaultModel()
	if def != "claude-sonnet-4-6" {
		t.Errorf("expected default to be claude-sonnet-4-6, got %s", def)
	}
}

func TestModelRegistryCrossFamilyFallback(t *testing.T) {
	cfg := &config.Config{}
	authMgr := auth.NewManager(cfg, nil)
	mr := NewModelRegistry(cfg, authMgr, nil)

	mr.mu.Lock()
	mr.models = map[string]*ModelInfo{
		"gemini-3.8-flash": {ID: "gemini-3.8-flash", OwnedBy: "google"},
	}
	mr.champions = map[string]*ModelInfo{
		"gemini-flash": mr.models["gemini-3.8-flash"],
	}
	mr.mu.Unlock()

	// Requesting claude-3-7-sonnet with NO Claude models available
	m, found := mr.FindModel("claude-3-7-sonnet")
	if !found {
		t.Fatal("expected cross-family fallback to succeed, got not found")
	}
	if m.ID != "gemini-3.8-flash" {
		t.Errorf("expected cross-family fallback to gemini-3.8-flash, got %s", m.ID)
	}
}
