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

func TestManager_MarkAndFindEligibleAccount(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := NewManager(cfg, nil)

	mgr.accountsMu.Lock()
	mgr.accounts = []Account{
		{
			ID:      "acc_1",
			Email:   "acc1@example.com",
			Tier:    "free",
			Status:  StatusHealthy,
			Enabled: true,
		},
		{
			ID:      "acc_2",
			Email:   "acc2@example.com",
			Tier:    "pro",
			Status:  StatusHealthy,
			Enabled: true,
		},
	}
	mgr.accountsMu.Unlock()

	// 1. Find next eligible when excluding acc_1
	next, err := mgr.FindNextEligibleAccount("acc_1", false)
	if err != nil {
		t.Fatalf("expected next eligible account, got error: %v", err)
	}
	if next.ID != "acc_2" {
		t.Errorf("expected acc_2, got %s", next.ID)
	}

	// 2. Mark acc_2 as in cooldown
	mgr.MarkAccountStatus("acc_2", StatusCooldown5h, 1*time.Hour)
	accs := mgr.GetAccounts()
	if accs[1].Status != StatusCooldown5h || accs[1].CooldownUntil.IsZero() {
		t.Errorf("expected acc_2 to be in cooldown, got %+v", accs[1])
	}

	// 3. Finding next eligible excluding acc_1 now returns ErrNoEligibleAccounts
	_, err = mgr.FindNextEligibleAccount("acc_1", false)
	if !errors.Is(err, ErrNoEligibleAccounts) {
		t.Errorf("expected ErrNoEligibleAccounts, got %v", err)
	}

	// 4. Test cooldown auto-recovery
	mgr.accountsMu.Lock()
	mgr.accounts[1].CooldownUntil = time.Now().Add(-10 * time.Minute) // expired in the past
	mgr.accountsMu.Unlock()

	recovered, err := mgr.FindNextEligibleAccount("acc_1", false)
	if err != nil {
		t.Fatalf("expected auto-recovered account, got err: %v", err)
	}
	if recovered.ID != "acc_2" || recovered.Status != StatusHealthy {
		t.Errorf("expected recovered acc_2 with StatusHealthy, got %+v", recovered)
	}
}
