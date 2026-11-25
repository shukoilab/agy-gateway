// Package models manages dynamic model discovery from Antigravity CLI accounts,
// caching, and 100% compliant OpenAI model schemas.
package models

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"agy-gateway/internal/auth"
	"agy-gateway/internal/config"
	"agy-gateway/internal/openai"
)

// ModelInfo encapsulates runtime metadata for an upstream model discovered from an account.
type ModelInfo struct {
	ID               string   `json:"id"`
	DisplayName      string   `json:"display_name"`
	UpstreamModel    string   `json:"upstream_model"`
	PresetEffort     string   `json:"preset_effort,omitempty"`
	OwnedBy          string   `json:"owned_by"`
	Created          int64    `json:"created"`
	RequiresPaidTier bool     `json:"requires_paid_tier"`
	SupportedEfforts []string `json:"supported_efforts"`
	AccountIDs       []string `json:"account_ids,omitempty"`
	ContextWindow    int      `json:"context_window,omitempty"`
	MaxOutputTokens  int      `json:"max_output_tokens,omitempty"`
}

// ModelRegistry provides thread-safe dynamic model discovery and catalog queries.
type ModelRegistry struct {
	cfg             *config.Config
	authMgr         *auth.Manager
	modelsDevClient *ModelsDevClient
	ranker          *ModelRanker
	aliasStore      *AliasStore
	logger          *slog.Logger
	mu              sync.RWMutex
	models          map[string]*ModelInfo
	modelList       []openai.ModelObject
	champions       map[string]*ModelInfo
	lastSync        time.Time
}

// NewModelRegistry initializes a new dynamic ModelRegistry.
func NewModelRegistry(cfg *config.Config, authMgr *auth.Manager, logger *slog.Logger) *ModelRegistry {
	if logger == nil {
		logger = slog.Default()
	}

	vaultDir := ""
	if cfg != nil && cfg.Storage.VaultXDGConfigDir != "" {
		vaultDir = cfg.Storage.VaultXDGConfigDir
	}

	return &ModelRegistry{
		cfg:             cfg,
		authMgr:         authMgr,
		modelsDevClient: NewModelsDevClient(vaultDir, logger),
		ranker:          NewModelRanker(),
		aliasStore:      NewAliasStore(vaultDir, logger),
		logger:          logger.With("component", "model-registry"),
		models:          make(map[string]*ModelInfo),
		modelList:       make([]openai.ModelObject, 0),
		champions:       make(map[string]*ModelInfo),
	}
}

// AliasStore returns the underlying thread-safe alias store.
func (mr *ModelRegistry) AliasStore() *AliasStore {
	return mr.aliasStore
}

// ModelsDevClient returns the models.dev client.
func (mr *ModelRegistry) ModelsDevClient() *ModelsDevClient {
	return mr.modelsDevClient
}

// Champions returns the elected model champions.
func (mr *ModelRegistry) Champions() map[string]*ModelInfo {
	mr.mu.RLock()
	defer mr.mu.RUnlock()
	out := make(map[string]*ModelInfo, len(mr.champions))
	for k, v := range mr.champions {
		out[k] = v
	}
	return out
}

// RegisterModel adds or updates a model in the registry.
func (mr *ModelRegistry) RegisterModel(info *ModelInfo) {
	mr.mu.Lock()
	defer mr.mu.Unlock()

	mr.models[info.ID] = info

	found := false
	for i, m := range mr.modelList {
		if m.ID == info.ID {
			mr.modelList[i] = openai.ModelObject{
				ID:              info.ID,
				Object:          "model",
				Created:         info.Created,
				OwnedBy:         info.OwnedBy,
				ContextWindow:   info.ContextWindow,
				MaxOutputTokens: info.MaxOutputTokens,
			}
			found = true
			break
		}
	}
	if !found {
		mr.modelList = append(mr.modelList, openai.ModelObject{
			ID:              info.ID,
			Object:          "model",
			Created:         info.Created,
			OwnedBy:         info.OwnedBy,
			ContextWindow:   info.ContextWindow,
			MaxOutputTokens: info.MaxOutputTokens,
		})
	}

	mr.refreshChampionsLocked()
}

func (mr *ModelRegistry) refreshChampionsLocked() {
	if mr.modelsDevClient == nil || mr.ranker == nil || mr.aliasStore == nil {
		return
	}
	catalog := mr.modelsDevClient.GetCatalog(context.Background())
	mr.champions = mr.ranker.ElectChampions(mr.models, catalog)
	dynamicAliases := mr.ranker.SynthesizeAliases(mr.champions)
	mr.aliasStore.SetDynamicAliases(dynamicAliases)
}

// SyncModels dynamically queries available models from all registered accounts.
func (mr *ModelRegistry) SyncModels(ctx context.Context) error {
	mr.mu.Lock()
	defer mr.mu.Unlock()

	accounts := mr.authMgr.GetAccounts()
	mr.logger.Debug("starting dynamic model discovery", "account_count", len(accounts))

	discovered := make(map[string]*ModelInfo)

	// Identify candidate home directories to query models from
	candidateHomes := make([]string, 0)

	// 1. Worker directories
	if mr.cfg.Storage.WorkersRootDir != "" {
		for i := 0; i < mr.cfg.Pool.WorkerCount; i++ {
			wHome := filepath.Join(mr.cfg.Storage.WorkersRootDir, fmt.Sprintf("w%d", i))
			tokenPath := filepath.Join(wHome, ".gemini", "antigravity-cli", "antigravity-oauth-token")
			if _, err := os.Stat(tokenPath); err == nil {
				candidateHomes = append(candidateHomes, wHome)
			}
		}
	}

	// 2. Default user home
	if home := os.Getenv("HOME"); home != "" {
		tokenPath := filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token")
		if _, err := os.Stat(tokenPath); err == nil {
			candidateHomes = append(candidateHomes, home)
		}
	}

	// If no candidate homes found with tokens, use default home as fallback
	if len(candidateHomes) == 0 {
		candidateHomes = append(candidateHomes, os.Getenv("HOME"))
	}

	// Query models from candidate homes
	for _, homeDir := range candidateHomes {
		out, err := mr.authMgr.ExecAgy(ctx, homeDir, "models")
		if err != nil {
			mr.logger.Debug("failed to query agy models from home", "homeDir", homeDir, "err", err)
			continue
		}

		models := parseAgyModelsOutput(out)
		for _, m := range models {
			if existing, exists := discovered[m.ID]; exists {
				// Merge accounts if present
				for _, acc := range m.AccountIDs {
					if !contains(existing.AccountIDs, acc) {
						existing.AccountIDs = append(existing.AccountIDs, acc)
					}
				}
			} else {
				discovered[m.ID] = m
			}
		}
	}

	// If discovery succeeded, update internal cache
	if len(discovered) > 0 {
		mr.models = discovered

		// Build sorted OpenAI model list
		list := make([]openai.ModelObject, 0, len(discovered))
		for _, m := range discovered {
			list = append(list, openai.ModelObject{
				ID:              m.ID,
				Object:          "model",
				Created:         m.Created,
				OwnedBy:         m.OwnedBy,
				ContextWindow:   m.ContextWindow,
				MaxOutputTokens: m.MaxOutputTokens,
			})
		}

		sort.Slice(list, func(i, j int) bool {
			return list[i].ID < list[j].ID
		})

		mr.modelList = list
		mr.lastSync = time.Now()

		// Dynamic Ranking and Alias Synthesis
		catalog := mr.modelsDevClient.GetCatalog(ctx)
		champions := mr.ranker.ElectChampions(discovered, catalog)
		mr.champions = champions

		// Synthesize dynamic aliases from elected champions
		dynamicAliases := mr.ranker.SynthesizeAliases(champions)
		mr.aliasStore.SetDynamicAliases(dynamicAliases)

		mr.logger.Info("dynamic model discovery completed",
			"model_count", len(mr.modelList),
			"champions", len(mr.champions),
			"dynamic_aliases", len(dynamicAliases),
		)
		return nil
	}

	mr.logger.Warn("no models discovered from accounts; registry unchanged")
	return nil
}

// ListModels returns a slice of ModelObject schemas 100% compliant with OpenAI.
func (mr *ModelRegistry) ListModels() []openai.ModelObject {
	mr.mu.RLock()
	defer mr.mu.RUnlock()

	out := make([]openai.ModelObject, len(mr.modelList))
	copy(out, mr.modelList)
	return out
}

// FindModel searches the dynamic registry for a matching model.
// Resolution order:
// 1. Manual Admin Overrides & Dynamic Aliases via AliasStore.Resolve(id)
// 2. Exact match in discovered models
// 3. Case-insensitive match in discovered models
// 4. Normalized match (punctuation / dash normalized)
// 5. Prefix match
// 6. Graceful cross-family fallback (Claude -> Gemini Flash champion)
func (mr *ModelRegistry) FindModel(id string) (*ModelInfo, bool) {
	mr.mu.RLock()
	defer mr.mu.RUnlock()

	// 1. Check AliasStore (Manual Admin Overrides + Dynamic Synthesized Aliases)
	if target, ok := mr.aliasStore.Resolve(id); ok {
		if m, exists := mr.models[target]; exists {
			return m, true
		}
		// If target has prefix match in models
		for _, m := range mr.models {
			if strings.HasPrefix(m.ID, target) {
				return m, true
			}
		}
	}

	// 2. Exact match
	if m, exists := mr.models[id]; exists {
		return m, true
	}

	// 3. Case-insensitive match
	for _, m := range mr.models {
		if strings.EqualFold(m.ID, id) {
			return m, true
		}
	}

	// 4. Normalized match
	normID := NormalizeAlias(id)
	if target, ok := mr.aliasStore.Resolve(normID); ok {
		if m, exists := mr.models[target]; exists {
			return m, true
		}
	}
	for _, m := range mr.models {
		if NormalizeAlias(m.ID) == normID {
			return m, true
		}
	}

	// 5. Prefix match
	for _, m := range mr.models {
		if strings.HasPrefix(m.ID, normID) || strings.HasPrefix(normID, m.ID) {
			return m, true
		}
	}

	// 6. Family & Cross-Family Fallback for Claude models:
	if strings.Contains(normID, "claude") || strings.Contains(normID, "sonnet") || strings.Contains(normID, "haiku") || strings.Contains(normID, "opus") {
		// 6a. Try finding a model within the same Claude sub-family
		if strings.Contains(normID, "sonnet") {
			if champ, ok := mr.champions["claude-sonnet"]; ok && champ != nil {
				return champ, true
			}
			for _, m := range mr.models {
				if strings.Contains(m.ID, "sonnet") {
					return m, true
				}
			}
		}
		if strings.Contains(normID, "haiku") {
			if champ, ok := mr.champions["claude-haiku"]; ok && champ != nil {
				return champ, true
			}
			for _, m := range mr.models {
				if strings.Contains(m.ID, "haiku") {
					return m, true
				}
			}
		}
		if strings.Contains(normID, "opus") {
			if champ, ok := mr.champions["claude-opus"]; ok && champ != nil {
				return champ, true
			}
			for _, m := range mr.models {
				if strings.Contains(m.ID, "opus") {
					return m, true
				}
			}
		}

		// 6b. Try any available Claude model
		for _, m := range mr.models {
			if strings.Contains(m.ID, "claude") {
				return m, true
			}
		}

		// 6c. Graceful cross-family fallback to Gemini Flash champion when accounts have zero Claude models
		if champ, ok := mr.champions["gemini-flash"]; ok && champ != nil {
			mr.logger.Warn("cross-family fallback: routing Claude request to Gemini Flash champion", "requested", id, "target", champ.ID)
			return champ, true
		}
		for _, m := range mr.models {
			if strings.HasPrefix(m.ID, "gemini") {
				mr.logger.Warn("cross-family fallback: routing Claude request to discovered Gemini model", "requested", id, "target", m.ID)
				return m, true
			}
		}
	}

	return nil, false
}

// AnthropicDefaultModel returns the default model ID for Anthropic requests.
func (mr *ModelRegistry) AnthropicDefaultModel() string {
	mr.mu.RLock()
	defer mr.mu.RUnlock()

	// 1. Config override
	if mr.cfg != nil && mr.cfg.Anthropic.DefaultModel != "" {
		cfgTarget := mr.cfg.Anthropic.DefaultModel
		if _, exists := mr.models[cfgTarget]; exists {
			return cfgTarget
		}
		if target, ok := mr.aliasStore.Resolve(cfgTarget); ok {
			if _, exists := mr.models[target]; exists {
				return target
			}
		}
	}

	// 2. Alias store "default" alias
	if target, ok := mr.aliasStore.Resolve("default"); ok {
		if _, exists := mr.models[target]; exists {
			return target
		}
	}

	// 3. Elected Champions in preference order: Sonnet -> Haiku -> Opus -> Flash
	for _, fam := range []string{"claude-sonnet", "claude-haiku", "claude-opus", "gemini-flash"} {
		if champ, ok := mr.champions[fam]; ok && champ != nil {
			return champ.ID
		}
	}

	// 4. Fallback search among discovered models
	for _, id := range []string{"claude-3-5-sonnet", "claude-3-sonnet", "claude-3-5-haiku", "gemini-3.8-flash"} {
		if _, exists := mr.models[id]; exists {
			return id
		}
	}

	// 5. First discovered model
	if len(mr.modelList) > 0 {
		return mr.modelList[0].ID
	}

	return "claude-3-5-sonnet"
}

// DefaultModel returns the default model ID from configuration or the first discovered model.
func (mr *ModelRegistry) DefaultModel() string {
	mr.mu.RLock()
	defer mr.mu.RUnlock()

	if mr.cfg.OpenAI.DefaultModel != "" {
		if _, exists := mr.models[mr.cfg.OpenAI.DefaultModel]; exists {
			return mr.cfg.OpenAI.DefaultModel
		}
	}

	// Fallback to gemini-3.8-flash if present
	if _, exists := mr.models["gemini-3.8-flash"]; exists {
		return "gemini-3.8-flash"
	}

	// First available
	if len(mr.modelList) > 0 {
		return mr.modelList[0].ID
	}

	return "gemini-3.8-flash"
}

// parseAgyModelsOutput parses raw stdout from `agy models`.
func parseAgyModelsOutput(output []byte) []*ModelInfo {
	var result []*ModelInfo
	baseModels := make(map[string]bool)

	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "Fetching") {
			continue
		}

		parts := strings.SplitN(line, "\t", 2)
		modelID := strings.TrimSpace(parts[0])
		displayName := modelID
		if len(parts) > 1 {
			displayName = strings.TrimSpace(parts[1])
		}

		var upstreamModel string
		var presetEffort string
		var supportedEfforts []string

		switch {
		case strings.HasPrefix(modelID, "gemini") && strings.HasSuffix(modelID, "-high"):
			upstreamModel = strings.TrimSuffix(modelID, "-high")
			presetEffort = "high"
			supportedEfforts = []string{"high"}
			baseModels[upstreamModel] = true
		case strings.HasPrefix(modelID, "gemini") && strings.HasSuffix(modelID, "-medium"):
			upstreamModel = strings.TrimSuffix(modelID, "-medium")
			presetEffort = "medium"
			supportedEfforts = []string{"medium"}
			baseModels[upstreamModel] = true
		case strings.HasPrefix(modelID, "gemini") && strings.HasSuffix(modelID, "-low"):
			upstreamModel = strings.TrimSuffix(modelID, "-low")
			presetEffort = "low"
			supportedEfforts = []string{"low"}
			baseModels[upstreamModel] = true
		default:
			upstreamModel = modelID
			presetEffort = ""
			supportedEfforts = []string{"low", "medium", "high", "xhigh"}
		}

		ownedBy := "google"
		if strings.Contains(modelID, "claude") {
			ownedBy = "anthropic"
		} else if strings.Contains(modelID, "gpt") {
			ownedBy = "openai"
		}

		requiresPaidTier := strings.Contains(modelID, "opus")

		contextWindow := 1048576 // 1M tokens default for Gemini family
		maxOutputTokens := 65536
		if strings.Contains(modelID, "claude") {
			contextWindow = 200000
			maxOutputTokens = 8192
		} else if strings.Contains(modelID, "gpt") {
			contextWindow = 128000
			maxOutputTokens = 16384
		}

		result = append(result, &ModelInfo{
			ID:               modelID,
			DisplayName:      displayName,
			UpstreamModel:    upstreamModel,
			PresetEffort:     presetEffort,
			OwnedBy:          ownedBy,
			Created:          1740000000,
			RequiresPaidTier: requiresPaidTier,
			SupportedEfforts: supportedEfforts,
			AccountIDs:       []string{},
			ContextWindow:    contextWindow,
			MaxOutputTokens:  maxOutputTokens,
		})
	}

	// Register base models if not already explicitly present
	for baseID := range baseModels {
		found := false
		for _, m := range result {
			if m.ID == baseID {
				found = true
				break
			}
		}
		if !found {
			ownedBy := "google"
			if strings.Contains(baseID, "claude") {
				ownedBy = "anthropic"
			} else if strings.Contains(baseID, "gpt") {
				ownedBy = "openai"
			}

			baseContextWindow := 1048576 // 1M tokens default for Gemini family
			baseMaxOutputTokens := 65536
			if strings.Contains(baseID, "claude") {
				baseContextWindow = 200000
				baseMaxOutputTokens = 8192
			} else if strings.Contains(baseID, "gpt") {
				baseContextWindow = 128000
				baseMaxOutputTokens = 16384
			}

			result = append(result, &ModelInfo{
				ID:               baseID,
				DisplayName:      strings.Title(strings.ReplaceAll(baseID, "-", " ")),
				UpstreamModel:    baseID,
				PresetEffort:     "",
				OwnedBy:          ownedBy,
				Created:          1740000000,
				RequiresPaidTier: strings.Contains(baseID, "opus"),
				SupportedEfforts: []string{"low", "medium", "high", "xhigh"},
				AccountIDs:       []string{},
				ContextWindow:    baseContextWindow,
				MaxOutputTokens:  baseMaxOutputTokens,
			})
		}
	}

	return result
}

func contains(slice []string, val string) bool {
	for _, item := range slice {
		if item == val {
			return true
		}
	}
	return false
}
