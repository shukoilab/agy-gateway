package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"agy-gateway/internal/config"
)

func TestManager_GetQuota_Mock(t *testing.T) {
	cfg := &config.Config{}
	mgr := NewManager(cfg, nil)

	mgr.accountsMu.Lock()
	mgr.accounts = []Account{
		{
			ID:       "acct_test123",
			Email:    "user1@gmail.com",
			Alias:    "work-account",
			Tier:     "free",
			Status:   StatusHealthy,
			Enabled:  true,
			LastUsed: time.Now(),
		},
		{
			ID:       "acct_test456",
			Email:    "user2@gmail.com",
			Alias:    "personal-account",
			Tier:     "pro",
			Status:   StatusHealthy,
			Enabled:  true,
			LastUsed: time.Now(),
		},
	}
	mgr.accountsMu.Unlock()

	// Query all accounts
	res, err := mgr.GetQuota(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error fetching all quotas: %v", err)
	}

	if res.Total != 2 {
		t.Fatalf("expected 2 accounts, got %d", res.Total)
	}

	// Query specific account by ID
	singleRes, err := mgr.GetQuota(context.Background(), "acct_test123")
	if err != nil {
		t.Fatalf("unexpected error fetching single quota: %v", err)
	}
	if singleRes.Total != 1 || singleRes.Accounts[0].ID != "acct_test123" {
		t.Fatalf("expected account acct_test123, got %+v", singleRes.Accounts)
	}

	// Query specific account by email
	singleEmailRes, err := mgr.GetQuota(context.Background(), "user2@gmail.com")
	if err != nil {
		t.Fatalf("unexpected error fetching single quota by email: %v", err)
	}
	if singleEmailRes.Total != 1 || singleEmailRes.Accounts[0].Email != "user2@gmail.com" {
		t.Fatalf("expected account user2@gmail.com, got %+v", singleEmailRes.Accounts)
	}

	// Query non-existent account
	_, err = mgr.GetQuota(context.Background(), "non_existent")
	if !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("expected ErrAccountNotFound, got %v", err)
	}
}
