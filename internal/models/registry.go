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
	cfg       *config.Config
	authMgr   *auth.Manager
	logger    *slog.Logger
	mu        sync.RWMutex
	models    map[string]*ModelInfo
	modelList []openai.ModelObject
	lastSync  time.Time
}

// NewModelRegistry initializes a new dynamic ModelRegistry.
func NewModelRegistry(cfg *config.Config, authMgr *auth.Manager, logger *slog.Logger) *ModelRegistry {
	if logger == nil {
		logger = slog.Default()
	}
	return &ModelRegistry{
		cfg:       cfg,
		authMgr:   authMgr,
		logger:    logger.With("component", "model-registry"),
		models:    make(map[string]*ModelInfo),
		modelList: make([]openai.ModelObject, 0),
	}
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
		mr.logger.Info("dynamic model discovery completed", "model_count", len(mr.modelList))
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
func (mr *ModelRegistry) FindModel(id string) (*ModelInfo, bool) {
	mr.mu.RLock()
	defer mr.mu.RUnlock()

	// Exact match
	if m, exists := mr.models[id]; exists {
		return m, true
	}

	// Case-insensitive match
	for _, m := range mr.models {
		if strings.EqualFold(m.ID, id) {
			return m, true
		}
	}

	return nil, false
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
			supportedEfforts = []string{"low", "medium", "high", "extra"}
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
				SupportedEfforts: []string{"low", "medium", "high", "extra"},
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
