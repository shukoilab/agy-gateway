// Package models provides dynamic model discovery from Antigravity CLI accounts,
// external model metadata inspection via models.dev, and capability-based model aliasing.
package models

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"agy-gateway/internal/version"
)

const (
	// DefaultModelsDevURL is the public open catalogue endpoint.
	DefaultModelsDevURL = "https://models.dev/api.json?type=all"
	// DefaultCacheTTL is the duration after which disk cache expires.
	DefaultCacheTTL = 24 * time.Hour
)

// ModelsDevEntry represents parsed metadata for an individual model in models.dev.
type ModelsDevEntry struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Family      string  `json:"family"`
	ReleaseDate string  `json:"release_date"`
	Context     int     `json:"context"`
	Output      int     `json:"output"`
	CostInput   float64 `json:"cost_input"`
	CostOutput  float64 `json:"cost_output"`
	Reasoning   bool    `json:"reasoning"`
}

// ModelsDevCatalog contains indexed model metadata and timestamp.
type ModelsDevCatalog struct {
	LastFetched time.Time                  `json:"last_fetched"`
	Models      map[string]*ModelsDevEntry `json:"models"`
}

// ModelsDevClient manages cached and offline-fallback retrieval of models.dev catalogue.
type ModelsDevClient struct {
	cachePath  string
	httpClient *http.Client
	cacheTTL   time.Duration
	logger     *slog.Logger
	mu         sync.RWMutex
}

// NewModelsDevClient creates an initialized client with safe defaults.
func NewModelsDevClient(cacheDir string, logger *slog.Logger) *ModelsDevClient {
	if logger == nil {
		logger = slog.Default()
	}
	cachePath := ""
	if cacheDir != "" {
		cachePath = filepath.Join(cacheDir, "modelsdev-cache.json")
	}

	return &ModelsDevClient{
		cachePath: cachePath,
		httpClient: &http.Client{
			Transport: &http.Transport{
				Proxy:             http.ProxyFromEnvironment,
				IdleConnTimeout:   30 * time.Second,
				DisableKeepAlives: false,
			},
			Timeout: 10 * time.Second,
		},
		cacheTTL: DefaultCacheTTL,
		logger:   logger.With("component", "modelsdev-client"),
	}
}

// GetCatalog retrieves the catalogue: first checking memory/disk cache, then remote, and finally embedded fallback.
func (c *ModelsDevClient) GetCatalog(ctx context.Context) *ModelsDevCatalog {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 1. Try disk cache if fresh
	if c.cachePath != "" {
		if cat, err := c.loadDiskCache(); err == nil && cat != nil {
			if time.Since(cat.LastFetched) < c.cacheTTL && len(cat.Models) > 0 {
				c.logger.Debug("using fresh models.dev disk cache", "models_count", len(cat.Models))
				return cat
			}
		}
	}

	// 2. Attempt remote fetch
	cat, err := c.fetchRemote(ctx)
	if err == nil && cat != nil && len(cat.Models) > 0 {
		c.logger.Info("retrieved fresh model catalog from models.dev", "models_count", len(cat.Models))
		if c.cachePath != "" {
			_ = c.saveDiskCache(cat)
		}
		return cat
	}

	c.logger.Warn("remote models.dev fetch failed, falling back to cache or embedded baseline", "err", err)

	// 3. Fallback to expired disk cache if available
	if c.cachePath != "" {
		if cached, loadErr := c.loadDiskCache(); loadErr == nil && cached != nil && len(cached.Models) > 0 {
			c.logger.Info("using existing disk cache as fallback", "models_count", len(cached.Models))
			return cached
		}
	}

	// 4. Guaranteed offline embedded baseline
	c.logger.Info("using embedded baseline model catalogue")
	return c.getEmbeddedBaseline()
}

func (c *ModelsDevClient) fetchRemote(ctx context.Context) (*ModelsDevCatalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, DefaultModelsDevURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", version.UserAgent(""))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models.dev responded with HTTP %d", resp.StatusCode)
	}

	var rawData map[string]struct {
		Models map[string]struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Family      string `json:"family"`
			ReleaseDate string `json:"release_date"`
			Reasoning   bool   `json:"reasoning"`
			Limit       struct {
				Context int `json:"context"`
				Output  int `json:"output"`
			} `json:"limit"`
			Cost struct {
				Input  float64 `json:"input"`
				Output float64 `json:"output"`
			} `json:"cost"`
		} `json:"models"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&rawData); err != nil {
		return nil, fmt.Errorf("failed to decode models.dev response: %w", err)
	}

	cat := &ModelsDevCatalog{
		LastFetched: time.Now(),
		Models:      make(map[string]*ModelsDevEntry),
	}

	for _, providerData := range rawData {
		for mid, m := range providerData.Models {
			id := m.ID
			if id == "" {
				id = mid
			}
			cat.Models[id] = &ModelsDevEntry{
				ID:          id,
				Name:        m.Name,
				Family:      m.Family,
				ReleaseDate: m.ReleaseDate,
				Context:     m.Limit.Context,
				Output:      m.Limit.Output,
				CostInput:   m.Cost.Input,
				CostOutput:  m.Cost.Output,
				Reasoning:   m.Reasoning,
			}
		}
	}

	return cat, nil
}

func (c *ModelsDevClient) loadDiskCache() (*ModelsDevCatalog, error) {
	if c.cachePath == "" {
		return nil, os.ErrNotExist
	}
	data, err := os.ReadFile(c.cachePath)
	if err != nil {
		return nil, err
	}

	var cat ModelsDevCatalog
	if err := json.Unmarshal(data, &cat); err != nil {
		return nil, err
	}
	return &cat, nil
}

func (c *ModelsDevClient) saveDiskCache(cat *ModelsDevCatalog) error {
	if c.cachePath == "" || cat == nil {
		return nil
	}
	_ = os.MkdirAll(filepath.Dir(c.cachePath), 0755)

	data, err := json.MarshalIndent(cat, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.cachePath, data, 0644)
}

// getEmbeddedBaseline provides a comprehensive offline catalog of known models.
func (c *ModelsDevClient) getEmbeddedBaseline() *ModelsDevCatalog {
	models := map[string]*ModelsDevEntry{
		// Claude Family
		"claude-opus-4-6": {
			ID: "claude-opus-4-6", Name: "Claude Opus 4.6", Family: "claude-opus",
			ReleaseDate: "2026-02-04", Context: 1000000, Output: 128000, CostInput: 5.0, CostOutput: 25.0, Reasoning: true,
		},
		"claude-sonnet-4-6": {
			ID: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6", Family: "claude-sonnet",
			ReleaseDate: "2026-02-17", Context: 1000000, Output: 128000, CostInput: 3.0, CostOutput: 15.0, Reasoning: true,
		},
		"claude-sonnet-4-5": {
			ID: "claude-sonnet-4-5", Name: "Claude Sonnet 4.5", Family: "claude-sonnet",
			ReleaseDate: "2025-09-29", Context: 200000, Output: 64000, CostInput: 3.0, CostOutput: 15.0, Reasoning: true,
		},
		"claude-3-5-sonnet": {
			ID: "claude-3-5-sonnet", Name: "Claude 3.5 Sonnet", Family: "claude-sonnet",
			ReleaseDate: "2024-10-22", Context: 200000, Output: 8192, CostInput: 3.0, CostOutput: 15.0, Reasoning: false,
		},
		"claude-haiku-4-5": {
			ID: "claude-haiku-4-5", Name: "Claude Haiku 4.5", Family: "claude-haiku",
			ReleaseDate: "2025-10-15", Context: 200000, Output: 8192, CostInput: 0.8, CostOutput: 4.0, Reasoning: false,
		},
		"claude-3-5-haiku": {
			ID: "claude-3-5-haiku", Name: "Claude 3.5 Haiku", Family: "claude-haiku",
			ReleaseDate: "2024-10-22", Context: 200000, Output: 8192, CostInput: 0.8, CostOutput: 4.0, Reasoning: false,
		},
		"claude-3-opus": {
			ID: "claude-3-opus", Name: "Claude 3 Opus", Family: "claude-opus",
			ReleaseDate: "2024-02-29", Context: 200000, Output: 4096, CostInput: 15.0, CostOutput: 75.0, Reasoning: false,
		},

		// Gemini Family
		"gemini-3.8-flash": {
			ID: "gemini-3.8-flash", Name: "Gemini 3.8 Flash", Family: "gemini-flash",
			ReleaseDate: "2026-02-15", Context: 1048576, Output: 65536, CostInput: 0.075, CostOutput: 0.3, Reasoning: true,
		},
		"gemini-3.8-pro": {
			ID: "gemini-3.8-pro", Name: "Gemini 3.8 Pro", Family: "gemini-pro",
			ReleaseDate: "2026-02-20", Context: 2097152, Output: 65536, CostInput: 1.25, CostOutput: 5.0, Reasoning: true,
		},
		"gemini-2.5-flash": {
			ID: "gemini-2.5-flash", Name: "Gemini 2.5 Flash", Family: "gemini-flash",
			ReleaseDate: "2025-05-15", Context: 1048576, Output: 8192, CostInput: 0.075, CostOutput: 0.3, Reasoning: false,
		},

		// GPT Family
		"gpt-oss-120b-medium": {
			ID: "gpt-oss-120b-medium", Name: "GPT-OSS 120B", Family: "gpt-oss",
			ReleaseDate: "2025-11-10", Context: 128000, Output: 16384, CostInput: 0.5, CostOutput: 2.0, Reasoning: true,
		},
	}

	return &ModelsDevCatalog{
		LastFetched: time.Now(),
		Models:      models,
	}
}
