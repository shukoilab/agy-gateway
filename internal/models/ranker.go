package models

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

var versionRegex = regexp.MustCompile(`(\d+)[.-](\d+)`)

// ModelRanker evaluates capabilities and elects family champions for dynamic model routing.
type ModelRanker struct{}

// NewModelRanker creates an initialized ranker.
func NewModelRanker() *ModelRanker {
	return &ModelRanker{}
}

// ScoreModel calculates an objective capability score for a given model.
func (r *ModelRanker) ScoreModel(modelID string, entry *ModelsDevEntry) int {
	score := 0
	lowerID := strings.ToLower(modelID)

	// 1. Family Base Score
	family := ""
	if entry != nil && entry.Family != "" {
		family = strings.ToLower(entry.Family)
	}

	switch {
	case strings.Contains(lowerID, "opus") || strings.Contains(family, "opus"):
		score += 30000
	case strings.Contains(lowerID, "sonnet") || strings.Contains(family, "sonnet"):
		score += 20000
	case strings.Contains(lowerID, "pro") || strings.Contains(family, "pro"):
		score += 18000
	case strings.Contains(lowerID, "haiku") || strings.Contains(family, "haiku"):
		score += 12000
	case strings.Contains(lowerID, "flash") || strings.Contains(family, "flash"):
		score += 10000
	case strings.Contains(lowerID, "gpt-oss") || strings.Contains(family, "gpt"):
		score += 8000
	default:
		score += 5000
	}

	// 2. Release Date Weight (from models.dev if available)
	if entry != nil && entry.ReleaseDate != "" {
		if t, err := time.Parse("2006-01-02", entry.ReleaseDate); err == nil {
			// Days since year 2024
			days := int(t.Sub(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)).Hours() / 24)
			if days > 0 {
				score += days * 2 // Every day newer adds +2 points
			}
		}
	}

	// 3. Numeric Version Extraction
	if matches := versionRegex.FindStringSubmatch(lowerID); len(matches) == 3 {
		major, _ := strconv.Atoi(matches[1])
		minor, _ := strconv.Atoi(matches[2])
		score += (major * 1000) + (minor * 100)
	}

	// 4. Reasoning Effort / Thinking Boost
	switch {
	case strings.HasSuffix(lowerID, "-high") || strings.Contains(lowerID, "thinking"):
		score += 500
	case strings.HasSuffix(lowerID, "-medium"):
		score += 300
	case strings.HasSuffix(lowerID, "-low"):
		score += 100
	}
	if entry != nil && entry.Reasoning {
		score += 200
	}

	// 5. Context Window Bonus
	if entry != nil && entry.Context >= 1000000 {
		score += 300
	}

	return score
}

// ElectChampions identifies the top-scoring model in each architectural family.
func (r *ModelRanker) ElectChampions(discovered map[string]*ModelInfo, catalog *ModelsDevCatalog) map[string]*ModelInfo {
	champions := make(map[string]*ModelInfo)
	scores := make(map[string]int)

	for id, info := range discovered {
		var mdev *ModelsDevEntry
		if catalog != nil && catalog.Models != nil {
			mdev = catalog.Models[id]
			if mdev == nil {
				mdev = catalog.Models[info.UpstreamModel]
			}
		}

		score := r.ScoreModel(id, mdev)

		// Determine family category
		familyKey := ""
		lower := strings.ToLower(id)
		switch {
		case strings.Contains(lower, "opus"):
			familyKey = "claude-opus"
		case strings.Contains(lower, "sonnet"):
			familyKey = "claude-sonnet"
		case strings.Contains(lower, "haiku"):
			familyKey = "claude-haiku"
		case strings.Contains(lower, "gemini") && strings.Contains(lower, "pro"):
			familyKey = "gemini-pro"
		case strings.Contains(lower, "gemini") && strings.Contains(lower, "flash"):
			familyKey = "gemini-flash"
		case strings.Contains(lower, "gpt"):
			familyKey = "gpt-oss"
		}

		if familyKey != "" {
			if existingScore, exists := scores[familyKey]; !exists || score > existingScore {
				scores[familyKey] = score
				champions[familyKey] = info
			}
		}
	}

	return champions
}

// SynthesizeAliases generates canonical alias mappings to elected champions.
func (r *ModelRanker) SynthesizeAliases(champions map[string]*ModelInfo) map[string]string {
	aliases := make(map[string]string)

	sonnet := champions["claude-sonnet"]
	haiku := champions["claude-haiku"]
	opus := champions["claude-opus"]
	flash := champions["gemini-flash"]
	pro := champions["gemini-pro"]

	// Fallback targets if a family has zero models in provisioned accounts
	defaultClaude := sonnet
	if defaultClaude == nil {
		defaultClaude = haiku
	}
	if defaultClaude == nil {
		defaultClaude = opus
	}
	if defaultClaude == nil {
		defaultClaude = flash // Graceful cross-family fallback to Gemini Flash
	}
	if defaultClaude == nil {
		defaultClaude = pro
	}

	// 1. Sonnet Aliases
	targetSonnet := defaultClaude
	if sonnet != nil {
		targetSonnet = sonnet
	}
	if targetSonnet != nil {
		for _, a := range []string{
			"claude-3-7-sonnet-20250219",
			"claude-3-7-sonnet",
			"claude-sonnet-4-5",
			"claude-3-5-sonnet-20241022",
			"claude-3-5-sonnet-latest",
			"claude-3-5-sonnet",
			"claude-sonnet",
			"claude-3.5-sonnet",
			"claude-3.7-sonnet",
		} {
			aliases[a] = targetSonnet.ID
		}
	}

	// 2. Haiku Aliases
	targetHaiku := haiku
	if targetHaiku == nil {
		targetHaiku = targetSonnet
	}
	if targetHaiku != nil {
		for _, a := range []string{
			"claude-3-5-haiku-20241022",
			"claude-3-5-haiku-latest",
			"claude-3-5-haiku",
			"claude-haiku",
			"claude-3.5-haiku",
		} {
			aliases[a] = targetHaiku.ID
		}
	}

	// 3. Opus Aliases
	targetOpus := opus
	if targetOpus == nil {
		targetOpus = targetSonnet
	}
	if targetOpus != nil {
		for _, a := range []string{
			"claude-3-opus-20240229",
			"claude-3-opus-latest",
			"claude-3-opus",
			"claude-opus",
		} {
			aliases[a] = targetOpus.ID
		}
	}

	// 4. Gemini Aliases
	if flash != nil {
		for _, a := range []string{
			"gemini",
			"gemini-flash",
			"gemini-flash-latest",
		} {
			aliases[a] = flash.ID
		}
	}
	if pro != nil {
		for _, a := range []string{
			"gemini-pro",
			"gemini-pro-latest",
		} {
			aliases[a] = pro.ID
		}
	}

	// 5. Cross-Vendor Flagship Aliases
	flagship := sonnet
	if flagship == nil {
		flagship = pro
	}
	if flagship == nil {
		flagship = flash
	}
	if flagship != nil {
		aliases["gpt-4o"] = flagship.ID
		aliases["gpt-4"] = flagship.ID
		aliases["default"] = flagship.ID
	}

	return aliases
}
