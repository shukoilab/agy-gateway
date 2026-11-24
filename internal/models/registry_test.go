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

	// Verify claude opus requires paid tier
	opus, exists := modelMap["claude-opus-4-6-thinking"]
	if !exists {
		t.Fatal("missing claude-opus-4-6-thinking")
	}
	if !opus.RequiresPaidTier {
		t.Error("expected claude-opus to require paid tier")
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
