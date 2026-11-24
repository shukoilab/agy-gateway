package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"agy-gateway/internal/auth"
	"agy-gateway/internal/config"
)

func setupTestCLI(t *testing.T, inStr string) (*AccountsCLI, *bytes.Buffer, *auth.Manager) {
	cfg := config.DefaultConfig()
	mgr := auth.NewManager(cfg, nil)

	acc1 := auth.Account{
		ID:       "acc_test1",
		Email:    "alpha@gmail.com",
		Alias:    "primary-alpha",
		Tier:     "payg",
		Status:   auth.StatusHealthy,
		Enabled:  true,
		LastUsed: time.Now(),
	}
	acc2 := auth.Account{
		ID:       "acc_test2",
		Email:    "beta@gmail.com",
		Alias:    "backup-beta",
		Tier:     "free",
		Status:   auth.StatusCooldown5h,
		Enabled:  true,
		LastUsed: time.Now(),
	}

	mgr.SetAccountsForTest([]auth.Account{acc1, acc2})

	in := strings.NewReader(inStr)
	out := &bytes.Buffer{}
	cli := NewAccountsCLI(cfg, mgr, nil, in, out)
	return cli, out, mgr
}

func TestAccountsCLI_ListTable(t *testing.T) {
	cli, out, _ := setupTestCLI(t, "")

	err := cli.Run(context.Background(), []string{"list"})
	if err != nil {
		t.Fatalf("unexpected error running list: %v", err)
	}

	result := out.String()
	if !strings.Contains(result, "acc_test1") || !strings.Contains(result, "primary-alpha") {
		t.Errorf("expected acc_test1 in list output, got:\n%s", result)
	}
	if !strings.Contains(result, "acc_test2") || !strings.Contains(result, "COOLDOWN_5H") {
		t.Errorf("expected acc_test2 in list output, got:\n%s", result)
	}
}

func TestAccountsCLI_ListJSON(t *testing.T) {
	cli, out, _ := setupTestCLI(t, "")

	err := cli.Run(context.Background(), []string{"list", "-json"})
	if err != nil {
		t.Fatalf("unexpected error running list -json: %v", err)
	}

	result := out.String()
	if !strings.Contains(result, `"id": "acc_test1"`) {
		t.Errorf("expected JSON output containing acc_test1, got:\n%s", result)
	}
}

func TestAccountsCLI_Help(t *testing.T) {
	cli, out, _ := setupTestCLI(t, "")

	err := cli.Run(context.Background(), []string{"help"})
	if err != nil {
		t.Fatalf("unexpected error on help: %v", err)
	}

	if !strings.Contains(out.String(), "Account Management CLI") {
		t.Errorf("expected help output, got:\n%s", out.String())
	}
}

func TestAccountsCLI_SubcommandDispatch(t *testing.T) {
	cli, _, _ := setupTestCLI(t, "")

	// Invalid command returns error
	err := cli.Run(context.Background(), []string{"invalid_command"})
	if err == nil {
		t.Errorf("expected error for invalid command, got nil")
	}

	// Missing args for switch returns error
	err = cli.Run(context.Background(), []string{"switch"})
	if err == nil {
		t.Errorf("expected error for switch without args, got nil")
	}

	// Missing args for alias returns error
	err = cli.Run(context.Background(), []string{"alias"})
	if err == nil {
		t.Errorf("expected error for alias without args, got nil")
	}
}

func TestAccountsCLI_InteractiveExit(t *testing.T) {
	// Provide input "0\n" to select Exit
	cli, out, _ := setupTestCLI(t, "0\n")

	err := cli.RunInteractive(context.Background())
	if err != nil {
		t.Fatalf("unexpected error on interactive exit: %v", err)
	}

	if !strings.Contains(out.String(), "Exiting account manager") {
		t.Errorf("expected clean exit message, got:\n%s", out.String())
	}
}
