package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
)

// RunInteractive launches the interactive TUI menu loop for account management.
//
// @purpose Provides a user-friendly, terminal-driven dashboard for account lifecycle tasks.
// @rationale Operators executing docker exec -it require clear prompts without memorizing CLI syntax.
// @preconditions c.in and c.out must be connected to readable and writable terminal streams.
// - @param: ctx Execution context to observe cancellation.
// @returns Nil on clean exit or a terminating I/O error.
// @throws Never crashes; gracefully surfaces sub-action errors and returns to main menu.
// @invariants Safe against EOF (Ctrl+D) and interrupt cancellations.
func (c *AccountsCLI) RunInteractive(ctx context.Context) error {
	reader := bufio.NewReader(c.in)

	for {
		// Observe context termination
		if ctx.Err() != nil {
			return ctx.Err()
		}

		c.printHeader()
		fmt.Fprint(c.out, "Select an option [0-10]: ")

		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				fmt.Fprintln(c.out, "\nExiting account manager.")
				return nil
			}
			return fmt.Errorf("failed reading input: %w", err)
		}

		choice := strings.TrimSpace(line)
		fmt.Fprintln(c.out)

		switch choice {
		case "1":
			_ = c.List(ctx, false)

		case "2":
			if err := c.runAddWizard(ctx, reader, ""); err != nil {
				fmt.Fprintf(c.out, "Error adding account: %v\n", err)
			}

		case "3":
			c.interactiveSwitch(ctx, reader)

		case "4":
			if err := c.Rotate(ctx); err != nil {
				fmt.Fprintf(c.out, "Error rotating account: %v\n", err)
			}

		case "5":
			c.interactiveSetAlias(ctx, reader)

		case "6":
			c.interactiveClearAlias(ctx, reader)

		case "7":
			c.interactiveRemove(ctx, reader)

		case "8":
			if err := c.Clean(ctx, false, false); err != nil {
				fmt.Fprintf(c.out, "Error cleaning vault: %v\n", err)
			}

		case "9":
			if err := c.Status(ctx, true); err != nil {
				fmt.Fprintf(c.out, "Error fetching status: %v\n", err)
			}

		case "10":
			modelsCLI := NewModelsCLI(c.cfg, c.mgr, nil, c.in, c.out)
			_ = modelsCLI.RunInteractive(ctx)

		case "11":
			updaterCLI := NewUpdaterCLI(c.cfg, nil, c.in, c.out)
			_ = updaterCLI.RunInteractive(ctx)

		case "0", "q", "quit", "exit":
			fmt.Fprintln(c.out, "Exiting account manager. Goodbye!")
			return nil

		default:
			fmt.Fprintf(c.out, "Invalid selection %q. Please enter a number between 0 and 11.\n", choice)
		}

		fmt.Fprintln(c.out, "\nPress Enter to return to menu...")
		_, enterErr := reader.ReadString('\n')
		if enterErr != nil {
			return nil
		}
	}
}

func (c *AccountsCLI) printHeader() {
	fmt.Fprintln(c.out, `================================================================================
          GOOGLE ANTIGRAVITY GATEWAY — ACCOUNT MANAGEMENT CLI
================================================================================
Config: `+c.cfg.Storage.VaultXDGConfigDir+` | Storage: `+c.cfg.Storage.BaseDataDir+`

[1] List Accounts & Live Quotas     (View all IDs, aliases, health & quotas)
[2] Add New Account (OAuth Login)   (Initiate headless Google OAuth login flow)
[3] Switch Active Account           (Set active account in central vault)
[4] Force Account Rotation          (Cycle active credentials to next healthy account)
[5] Set Account Alias               (Assign friendly nickname to account ID)
[6] Clear Account Alias             (Remove friendly nickname from account)
[7] Remove Account                  (Delete account credentials permanently)
[8] Clean Vault Cache               (Purge orphaned sessions & stale files)
[9] View Raw Status / Diagnostics   (Show raw JSON telemetry from agy-auth)
[10] Anthropic Model Aliases & Rank  (Inspect mappings, pin manual overrides)
[11] Auto-Updater & Binary Versions (Inspect & update agy CLI / agy-auth)
[0] Exit
--------------------------------------------------------------------------------`)
}

func (c *AccountsCLI) interactiveSwitch(ctx context.Context, reader *bufio.Reader) {
	fmt.Fprintln(c.out, "Current Accounts:")
	_ = c.List(ctx, false)
	fmt.Fprintln(c.out)

	fmt.Fprint(c.out, "Enter Account Number, ID, Email, or Alias to switch to: ")
	input, _ := reader.ReadString('\n')
	query := strings.TrimSpace(input)
	if query == "" {
		fmt.Fprintln(c.out, "Cancelled.")
		return
	}

	// Support choosing by numeric row index
	if idx, err := strconv.Atoi(query); err == nil && idx > 0 {
		accs := c.mgr.GetAccounts()
		if idx <= len(accs) {
			query = accs[idx-1].ID
		}
	}

	if err := c.Switch(ctx, query); err != nil {
		fmt.Fprintf(c.out, "Error switching account: %v\n", err)
	}
}

func (c *AccountsCLI) interactiveSetAlias(ctx context.Context, reader *bufio.Reader) {
	fmt.Fprint(c.out, "Enter Account ID or Email: ")
	inputQuery, _ := reader.ReadString('\n')
	query := strings.TrimSpace(inputQuery)
	if query == "" {
		fmt.Fprintln(c.out, "Cancelled.")
		return
	}

	fmt.Fprint(c.out, "Enter friendly alias name (e.g. dev-primary): ")
	inputAlias, _ := reader.ReadString('\n')
	alias := strings.TrimSpace(inputAlias)
	if alias == "" {
		fmt.Fprintln(c.out, "Alias cannot be empty.")
		return
	}

	if err := c.SetAlias(ctx, query, alias); err != nil {
		fmt.Fprintf(c.out, "Error setting alias: %v\n", err)
	}
}

func (c *AccountsCLI) interactiveClearAlias(ctx context.Context, reader *bufio.Reader) {
	fmt.Fprint(c.out, "Enter Account ID, Email, or Alias to clear: ")
	inputQuery, _ := reader.ReadString('\n')
	query := strings.TrimSpace(inputQuery)
	if query == "" {
		fmt.Fprintln(c.out, "Cancelled.")
		return
	}

	if err := c.ClearAlias(ctx, query); err != nil {
		fmt.Fprintf(c.out, "Error clearing alias: %v\n", err)
	}
}

func (c *AccountsCLI) interactiveRemove(ctx context.Context, reader *bufio.Reader) {
	fmt.Fprintln(c.out, "Current Accounts:")
	_ = c.List(ctx, false)
	fmt.Fprintln(c.out)

	fmt.Fprint(c.out, "Enter Account ID, Email, or Alias to DELETE permanently: ")
	inputQuery, _ := reader.ReadString('\n')
	query := strings.TrimSpace(inputQuery)
	if query == "" {
		fmt.Fprintln(c.out, "Cancelled.")
		return
	}

	fmt.Fprintf(c.out, "Are you sure you want to PERMANENTLY remove account %q? [y/N]: ", query)
	confirm, _ := reader.ReadString('\n')
	if strings.ToLower(strings.TrimSpace(confirm)) != "y" {
		fmt.Fprintln(c.out, "Operation cancelled.")
		return
	}

	if err := c.Remove(ctx, query); err != nil {
		fmt.Fprintf(c.out, "Error removing account: %v\n", err)
	}
}

func (c *AccountsCLI) runAddWizard(ctx context.Context, reader *bufio.Reader, alias string) error {
	if reader == nil {
		reader = bufio.NewReader(c.in)
	}

	if alias == "" {
		fmt.Fprint(c.out, "Enter optional alias for the new account (press Enter to skip): ")
		input, _ := reader.ReadString('\n')
		alias = strings.TrimSpace(input)
	}

	fmt.Fprintln(c.out, "Initializing headless OAuth onboarding session...")
	loginID, authURL, err := c.oom.InitLogin(ctx, alias)
	if err != nil {
		return fmt.Errorf("failed to initialize OAuth session: %w", err)
	}

	fmt.Fprintln(c.out, `
================================================================================
                    GOOGLE ANTIGRAVITY OAUTH LOGIN
================================================================================
1. Open this URL in your web browser:

   `+authURL+`

2. Sign in with your Google Account and grant permissions.
3. After completing login, Google will redirect to localhost or display an auth code.
4. Paste the authorization code OR the full redirect URL (http://localhost:...) below:
--------------------------------------------------------------------------------`)

	fmt.Fprint(c.out, "Enter code or redirect URL: ")
	inputCode, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed reading input: %w", err)
	}

	rawInput := strings.TrimSpace(inputCode)
	if rawInput == "" {
		return errors.New("input was empty; login cancelled")
	}

	var code, redirectURL string
	if strings.HasPrefix(rawInput, "http://") || strings.HasPrefix(rawInput, "https://") {
		redirectURL = rawInput
		if parsed, parseErr := url.Parse(rawInput); parseErr == nil {
			if qCode := parsed.Query().Get("code"); qCode != "" {
				code = qCode
			}
		}
	} else {
		code = rawInput
	}

	fmt.Fprintln(c.out, "Exchanging token with Google Antigravity vault...")
	if err := c.oom.CompleteCallback(ctx, loginID, code, redirectURL); err != nil {
		return fmt.Errorf("token exchange failed: %w", err)
	}

	fmt.Fprintln(c.out, "Authentication successful! Account credentials saved to vault.")
	_ = c.mgr.Sync(ctx)
	// The plan label is reporting-only but the operator expects `list` to show the real tier right
	// after adding: a missing plan renders "Unknown", which reads as a probe failure rather than a
	// fresh account. Failure here must not fail the add — the account is already stored.
	if err := c.mgr.RefreshPlan(ctx); err != nil {
		fmt.Fprintf(c.out, "Note: plan label unavailable (%v); the account is stored and usable.\n", err)
	}
	return nil
}
