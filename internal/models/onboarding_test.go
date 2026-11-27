package models

import (
	"testing"

	"agy-gateway/internal/auth"
	"agy-gateway/internal/config"
)

// TestNeedsOnboarding pins the fresh-install answer.
//
// @purpose Proves a gateway with no accounts and no catalog reports "needs onboarding".
// @rationale A fresh install booted fine but answered model_not_found for every request, which reads
// as a typo in the model name rather than "nobody has logged in yet".
// @preconditions None.
// @param t Standard Go test handle.
// @returns None.
// @throws None; failures are reported through t.Fatalf.
// @invariants Registers no subprocess and no network client.
func TestNeedsOnboarding(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := auth.NewManager(cfg, nil)
	mgr.SetAccountsForTest(nil)

	mr := NewModelRegistry(cfg, mgr, nil)
	if !mr.NeedsOnboarding() {
		t.Fatal("an empty vault with an empty catalog must report needs-onboarding")
	}

	// A catalog that already knows models must never take the onboarding path, even if the vault
	// read is momentarily empty: answering 429 there would reject a perfectly valid request.
	mr.RegisterModel(&ModelInfo{ID: "gemini-3.8-flash-low"})
	if mr.NeedsOnboarding() {
		t.Fatal("a populated catalog must not report needs-onboarding")
	}
}
