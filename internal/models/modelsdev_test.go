package models_test

import (
	"context"
	"testing"
	"time"

	"agy-gateway/internal/models"
)

func TestModelsDevClient_EmbeddedBaselineFallback(t *testing.T) {
	tmp := t.TempDir()
	client := models.NewModelsDevClient(tmp, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cat := client.GetCatalog(ctx)
	if cat == nil {
		t.Fatal("expected non-nil catalog")
	}

	if len(cat.Models) == 0 {
		t.Fatal("expected non-empty models catalog")
	}

	// Verify baseline contains standard models
	for _, expected := range []string{"claude-sonnet-4-6", "claude-opus-4-6", "gemini-3.8-flash"} {
		if _, exists := cat.Models[expected]; !exists {
			t.Errorf("expected baseline to contain %q", expected)
		}
	}
}
