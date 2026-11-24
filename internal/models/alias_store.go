package models

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// AliasSource identifies whether a model mapping was established by manual admin override or dynamic heuristics.
type AliasSource string

const (
	// SourceAdminOverride indicates a permanent manual mapping created by an administrator.
	SourceAdminOverride AliasSource = "admin_override"
	// SourceAutoSynthesized indicates an automated mapping elected by capability rank.
	SourceAutoSynthesized AliasSource = "auto_synthesized"
	// SourceFallback indicates an emergency cross-family fallback mapping.
	SourceFallback AliasSource = "fallback"
)

// ModelAliasEntry represents an active model alias definition.
type ModelAliasEntry struct {
	Alias       string      `json:"alias" example:"claude-3-7-sonnet"`
	Target      string      `json:"target" example:"claude-sonnet-4-6"`
	Source      AliasSource `json:"source" example:"auto_synthesized"`
	Description string      `json:"description,omitempty" example:"Auto-routed to highest ranking Sonnet model in active accounts"`
	UpdatedAt   time.Time   `json:"updated_at" example:"2026-11-24T12:00:00Z"`
}

// AliasStore manages thread-safe resolution and persistent storage of manual and dynamic model aliases.
type AliasStore struct {
	filePath       string
	manualAliases  map[string]ModelAliasEntry
	dynamicAliases map[string]string
	logger         *slog.Logger
	mu             sync.RWMutex
}

// NewAliasStore creates an initialized AliasStore with optional file persistence.
func NewAliasStore(vaultDir string, logger *slog.Logger) *AliasStore {
	if logger == nil {
		logger = slog.Default()
	}
	filePath := ""
	if vaultDir != "" {
		filePath = filepath.Join(vaultDir, "anthropic-model-aliases.json")
	}

	store := &AliasStore{
		filePath:       filePath,
		manualAliases:  make(map[string]ModelAliasEntry),
		dynamicAliases: make(map[string]string),
		logger:         logger.With("component", "alias-store"),
	}

	_ = store.Load()
	return store
}

// NormalizeAlias cleans punctuation and normalizes dots/underscores to hyphens for robust matching.
func NormalizeAlias(alias string) string {
	s := strings.ToLower(strings.TrimSpace(alias))
	s = strings.ReplaceAll(s, ".", "-")
	s = strings.ReplaceAll(s, "_", "-")
	return s
}

// Resolve looks up a target model for the given alias.
// Priority: Manual Admin Overrides > Dynamic Heuristics.
func (s *AliasStore) Resolve(alias string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	norm := NormalizeAlias(alias)

	// 1. Check manual admin overrides
	if entry, ok := s.manualAliases[norm]; ok {
		return entry.Target, true
	}

	// 2. Check dynamic synthesized aliases
	if target, ok := s.dynamicAliases[norm]; ok {
		return target, true
	}

	// 3. Prefix matching for versioned variants
	for k, target := range s.manualAliases {
		if strings.HasPrefix(norm, k) {
			return target.Target, true
		}
	}
	for k, target := range s.dynamicAliases {
		if strings.HasPrefix(norm, k) {
			return target, true
		}
	}

	return "", false
}

// SetManual adds or updates a manual admin alias override and persists it.
func (s *AliasStore) SetManual(alias, target, description string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	norm := NormalizeAlias(alias)
	if norm == "" {
		return fmt.Errorf("alias cannot be empty")
	}
	if strings.TrimSpace(target) == "" {
		return fmt.Errorf("target model cannot be empty")
	}

	s.manualAliases[norm] = ModelAliasEntry{
		Alias:       norm,
		Target:      strings.TrimSpace(target),
		Source:      SourceAdminOverride,
		Description: description,
		UpdatedAt:   time.Now().UTC(),
	}

	s.logger.Info("set manual model alias", "alias", norm, "target", target)
	return s.saveLocked()
}

// DeleteManual removes a manual admin alias override.
func (s *AliasStore) DeleteManual(alias string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	norm := NormalizeAlias(alias)
	if _, exists := s.manualAliases[norm]; !exists {
		return false, nil
	}

	delete(s.manualAliases, norm)
	s.logger.Info("deleted manual model alias", "alias", norm)
	return true, s.saveLocked()
}

// SetDynamicAliases updates the automated synthesized alias mapping.
func (s *AliasStore) SetDynamicAliases(synthesized map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.dynamicAliases = make(map[string]string, len(synthesized))
	for k, v := range synthesized {
		s.dynamicAliases[NormalizeAlias(k)] = v
	}
	s.logger.Debug("updated dynamic model aliases", "count", len(s.dynamicAliases))
}

// GetAllAliases returns all active aliases (manual overrides first, then dynamic heuristics), sorted alphabetically.
func (s *AliasStore) GetAllAliases() []ModelAliasEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []ModelAliasEntry
	seen := make(map[string]bool)

	// 1. Manual Overrides
	for _, m := range s.manualAliases {
		result = append(result, m)
		seen[m.Alias] = true
	}

	// 2. Dynamic Synthesized Aliases
	for alias, target := range s.dynamicAliases {
		if !seen[alias] {
			result = append(result, ModelAliasEntry{
				Alias:       alias,
				Target:      target,
				Source:      SourceAutoSynthesized,
				Description: "Auto-synthesized family champion mapping",
				UpdatedAt:   time.Now().UTC(),
			})
			seen[alias] = true
		}
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].Source == SourceAdminOverride && result[j].Source != SourceAdminOverride {
			return true
		}
		if result[i].Source != SourceAdminOverride && result[j].Source == SourceAdminOverride {
			return false
		}
		return result[i].Alias < result[j].Alias
	})

	return result
}

// Load loads manual overrides from disk if the file exists.
func (s *AliasStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.filePath == "" {
		return nil
	}

	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var items []ModelAliasEntry
	if err := json.Unmarshal(data, &items); err != nil {
		return err
	}

	s.manualAliases = make(map[string]ModelAliasEntry)
	for _, item := range items {
		s.manualAliases[NormalizeAlias(item.Alias)] = item
	}
	s.logger.Info("loaded manual model aliases from vault", "count", len(s.manualAliases))
	return nil
}

func (s *AliasStore) saveLocked() error {
	if s.filePath == "" {
		return nil
	}
	_ = os.MkdirAll(filepath.Dir(s.filePath), 0755)

	var list []ModelAliasEntry
	for _, v := range s.manualAliases {
		list = append(list, v)
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].Alias < list[j].Alias
	})

	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(s.filePath, data, 0644)
}
