package models_test

import (
	"testing"

	"agy-gateway/internal/models"
)

func TestModelRanker_DeterministicScoring(t *testing.T) {
	ranker := models.NewModelRanker()

	scoreOpus := ranker.ScoreModel("claude-opus-4-6-thinking", &models.ModelsDevEntry{
		ID:          "claude-opus-4-6",
		ReleaseDate: "2026-02-04",
		Context:     1000000,
		Reasoning:   true,
	})

	scoreSonnet := ranker.ScoreModel("claude-sonnet-4-6", &models.ModelsDevEntry{
		ID:          "claude-sonnet-4-6",
		ReleaseDate: "2026-02-17",
		Context:     1000000,
		Reasoning:   true,
	})

	scoreHaiku := ranker.ScoreModel("claude-3-5-haiku", &models.ModelsDevEntry{
		ID:          "claude-3-5-haiku",
		ReleaseDate: "2024-10-22",
		Context:     200000,
		Reasoning:   false,
	})

	scoreFlash := ranker.ScoreModel("gemini-3.8-flash", &models.ModelsDevEntry{
		ID:          "gemini-3.8-flash",
		ReleaseDate: "2026-02-15",
		Context:     1048576,
		Reasoning:   true,
	})

	if scoreOpus <= scoreSonnet {
		t.Errorf("expected Opus score (%d) > Sonnet score (%d)", scoreOpus, scoreSonnet)
	}
	if scoreSonnet <= scoreHaiku {
		t.Errorf("expected Sonnet score (%d) > Haiku score (%d)", scoreSonnet, scoreHaiku)
	}
	if scoreSonnet <= scoreFlash {
		t.Errorf("expected Sonnet score (%d) > Flash score (%d)", scoreSonnet, scoreFlash)
	}
}

func TestModelRanker_ElectChampionsAndSynthesizeAliases(t *testing.T) {
	ranker := models.NewModelRanker()

	discovered := map[string]*models.ModelInfo{
		"gemini-3.8-flash": {
			ID:            "gemini-3.8-flash",
			UpstreamModel: "gemini-3.8-flash",
		},
		"claude-3-5-sonnet": {
			ID:            "claude-3-5-sonnet",
			UpstreamModel: "claude-3-5-sonnet",
		},
		"claude-sonnet-4-6": {
			ID:            "claude-sonnet-4-6",
			UpstreamModel: "claude-sonnet-4-6",
		},
	}

	catalog := &models.ModelsDevCatalog{
		Models: map[string]*models.ModelsDevEntry{
			"claude-sonnet-4-6": {
				ID:          "claude-sonnet-4-6",
				ReleaseDate: "2026-02-17",
				Context:     1000000,
			},
			"claude-3-5-sonnet": {
				ID:          "claude-3-5-sonnet",
				ReleaseDate: "2024-10-22",
				Context:     200000,
			},
			"gemini-3.8-flash": {
				ID:          "gemini-3.8-flash",
				ReleaseDate: "2026-02-15",
				Context:     1048576,
			},
		},
	}

	champions := ranker.ElectChampions(discovered, catalog)

	sonnetChamp, ok := champions["claude-sonnet"]
	if !ok || sonnetChamp.ID != "claude-sonnet-4-6" {
		t.Fatalf("expected Sonnet champion to be claude-sonnet-4-6, got %+v", sonnetChamp)
	}

	flashChamp, ok := champions["gemini-flash"]
	if !ok || flashChamp.ID != "gemini-3.8-flash" {
		t.Fatalf("expected Flash champion to be gemini-3.8-flash, got %+v", flashChamp)
	}

	aliases := ranker.SynthesizeAliases(champions)

	// Verify that claude-3-7-sonnet and claude-3-5-sonnet alias to the champion claude-sonnet-4-6
	if target := aliases["claude-3-7-sonnet"]; target != "claude-sonnet-4-6" {
		t.Errorf("expected claude-3-7-sonnet to map to claude-sonnet-4-6, got %q", target)
	}
	if target := aliases["claude-sonnet-4-5"]; target != "claude-sonnet-4-6" {
		t.Errorf("expected claude-sonnet-4-5 to map to claude-sonnet-4-6, got %q", target)
	}
	if target := aliases["claude-3.5-sonnet"]; target != "claude-sonnet-4-6" {
		t.Errorf("expected claude-3.5-sonnet (dot) to map to claude-sonnet-4-6, got %q", target)
	}
}

func TestModelRanker_CrossFamilyFallbackWhenNoClaude(t *testing.T) {
	ranker := models.NewModelRanker()

	// Account ONLY has Gemini models
	discovered := map[string]*models.ModelInfo{
		"gemini-3.8-flash": {
			ID:            "gemini-3.8-flash",
			UpstreamModel: "gemini-3.8-flash",
		},
	}

	catalog := &models.ModelsDevCatalog{
		Models: map[string]*models.ModelsDevEntry{
			"gemini-3.8-flash": {
				ID:          "gemini-3.8-flash",
				ReleaseDate: "2026-02-15",
			},
		},
	}

	champions := ranker.ElectChampions(discovered, catalog)
	aliases := ranker.SynthesizeAliases(champions)

	// In accounts with zero Claude models, Claude requests must fall back to Gemini Flash
	if target := aliases["claude-3-5-sonnet"]; target != "gemini-3.8-flash" {
		t.Errorf("expected cross-family fallback to gemini-3.8-flash, got %q", target)
	}
}
