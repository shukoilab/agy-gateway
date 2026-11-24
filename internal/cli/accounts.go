// Package cli provides interactive terminal and non-interactive command-line interfaces
// for managing Google Antigravity credential accounts, OAuth onboarding, and live quotas.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"agy-gateway/internal/auth"
	"agy-gateway/internal/config"
)

// AccountsCLI coordinates account CRUD operations and terminal user interactions.
type AccountsCLI struct {
	cfg *config.Config
	mgr *auth.Manager
	oom *auth.OAuthOnboardingManager
	out io.Writer
	in  io.Reader
}

// NewAccountsCLI initializes an AccountsCLI instance.
//
// @purpose Creates a fully wired account CLI controller.
// @rationale Supports dependency injection for terminal I/O streams during testing.
// @preconditions cfg and mgr must not be nil.
// - @param: cfg Gateway configuration.
// - @param: mgr Account manager.
// - @param: oom Optional OAuth onboarding manager (can be nil, initialized lazily).
// - @param: in Standard input stream.
// - @param: out Standard output stream.
// @returns A ready-to-use AccountsCLI instance.
// @throws Never panics.
// @invariants Safe for sequential CLI executions.
func NewAccountsCLI(cfg *config.Config, mgr *auth.Manager, oom *auth.OAuthOnboardingManager, in io.Reader, out io.Writer) *AccountsCLI {
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	if oom == nil && cfg != nil && mgr != nil {
		oom = auth.NewOAuthOnboardingManager(cfg, mgr, nil)
	}
	return &AccountsCLI{
		cfg: cfg,
		mgr: mgr,
		oom: oom,
		in:  in,
		out: out,
	}
}

// PrintUsage outputs formatted command help instructions.
func (c *AccountsCLI) PrintUsage() {
	fmt.Fprintln(c.out, `Google Antigravity Gateway — Account Management CLI

Usage:
  agy-accounts [command] [flags]
  agy-gateway accounts [command] [flags]

Commands:
  (no command)      Launch interactive TUI menu
  interactive, -i   Launch interactive TUI menu explicitly
  list              List all accounts with health status and live quota
  add, login        Add a new account via Google OAuth
  switch <query>    Switch the active primary account in the vault
  rotate            Force credential rotation to the next healthy account
  alias set <q> <a> Assign a friendly nickname alias to an account
  alias clear <q>   Remove an assigned alias from an account
  remove <query>    Permanently delete an account from the vault
  clean             Purge expired OAuth cache and temporary files
  status            Display raw agy-auth vault diagnostics
  help              Display this help message

Flags:
  -config <path>    Path to config.yaml (default: /etc/agy-gateway/config.yaml or config.yaml)
  -json             Output command results in JSON format
  -yes, -y          Skip interactive confirmation for destructive operations
  -alias <name>     Specify alias when adding a new account via 'add'
  -dry-run          Preview vault cleaning without modifying files`)
}

// Run parses arguments and executes the designated subcommand or interactive menu.
//
// @purpose Dispatches CLI commands or initiates the interactive menu loop.
// @rationale Provides a unified CLI entry point for both interactive and scripted workflows.
// @preconditions ctx must be valid.
// - @param: ctx Execution context.
// - @param: args Command-line arguments slice (excluding binary name).
// @returns Nil on success or a typed execution error.
// @throws Propagates operational errors from subcommands.
// @invariants Deterministic routing based on subcommand tokens.
func (c *AccountsCLI) Run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return c.RunInteractive(ctx)
	}

	cmd := strings.ToLower(args[0])
	subArgs := args[1:]

	switch cmd {
	case "interactive", "-i", "--interactive":
		return c.RunInteractive(ctx)

	case "list", "ls":
		fs := flag.NewFlagSet("list", flag.ContinueOnError)
		fs.SetOutput(c.out)
		asJSON := fs.Bool("json", false, "Output as JSON")
		if err := fs.Parse(subArgs); err != nil {
			return err
		}
		return c.List(ctx, *asJSON)

	case "add", "login":
		fs := flag.NewFlagSet("add", flag.ContinueOnError)
		fs.SetOutput(c.out)
		alias := fs.String("alias", "", "Friendly alias name")
		if err := fs.Parse(subArgs); err != nil {
			return err
		}
		return c.Add(ctx, *alias)

	case "switch":
		if len(subArgs) == 0 {
			return errors.New("switch requires an account query (ID, email, or alias)")
		}
		return c.Switch(ctx, subArgs[0])

	case "rotate":
		return c.Rotate(ctx)

	case "alias":
		if len(subArgs) == 0 {
			return errors.New("alias requires an action: 'set <query> <alias>' or 'clear <query>'")
		}
		action := strings.ToLower(subArgs[0])
		if action == "set" {
			if len(subArgs) < 3 {
				return errors.New("usage: alias set <query> <alias>")
			}
			return c.SetAlias(ctx, subArgs[1], subArgs[2])
		} else if action == "clear" {
			if len(subArgs) < 2 {
				return errors.New("usage: alias clear <query>")
			}
			return c.ClearAlias(ctx, subArgs[1])
		}
		return fmt.Errorf("unknown alias action %q; use 'set' or 'clear'", action)

	case "remove", "rm", "delete":
		if len(subArgs) == 0 {
			return errors.New("remove requires an account query (ID, email, or alias)")
		}
		return c.Remove(ctx, subArgs[0])

	case "clean":
		fs := flag.NewFlagSet("clean", flag.ContinueOnError)
		fs.SetOutput(c.out)
		dryRun := fs.Bool("dry-run", false, "Preview cleanup without deleting files")
		if err := fs.Parse(subArgs); err != nil {
			return err
		}
		return c.Clean(ctx, *dryRun)

	case "status":
		fs := flag.NewFlagSet("status", flag.ContinueOnError)
		fs.SetOutput(c.out)
		asJSON := fs.Bool("json", false, "Output as JSON")
		if err := fs.Parse(subArgs); err != nil {
			return err
		}
		return c.Status(ctx, *asJSON)

	case "help", "--help", "-h":
		c.PrintUsage()
		return nil

	default:
		c.PrintUsage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// List queries and displays all stored accounts and their live quotas.
func (c *AccountsCLI) List(ctx context.Context, asJSON bool) error {
	quotaResp, err := c.mgr.GetQuota(ctx, "")
	if err != nil {
		return fmt.Errorf("failed to fetch accounts and quotas: %w", err)
	}

	if asJSON {
		encoder := json.NewEncoder(c.out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(quotaResp)
	}

	if len(quotaResp.Accounts) == 0 {
		fmt.Fprintln(c.out, "No accounts currently stored in vault. Use 'add' to onboard an account.")
		return nil
	}

	w := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NO\tID\tALIAS\tEMAIL\tTIER\tSTATUS\t5H QUOTA\tWEEKLY\tCOOLDOWN")
	fmt.Fprintln(w, "--\t--\t-----\t-----\t----\t------\t--------\t------\t--------")

	for i, acc := range quotaResp.Accounts {
		alias := acc.Alias
		if alias == "" {
			alias = "-"
		}

		fiveHour := "N/A"
		weekly := "N/A"
		for _, grp := range acc.Groups {
			for _, b := range grp.Buckets {
				if b.Window == "5h" {
					fiveHour = fmt.Sprintf("%.1f%%", b.RemainingPct)
				} else if b.Window == "weekly" {
					weekly = fmt.Sprintf("%.1f%%", b.RemainingPct)
				}
			}
		}

		cooldown := "-"
		if acc.CooldownUntil != nil && !acc.CooldownUntil.IsZero() {
			rem := time.Until(*acc.CooldownUntil).Round(time.Second)
			if rem > 0 {
				cooldown = rem.String()
			}
		}

		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			i+1,
			acc.ID,
			alias,
			acc.Email,
			acc.Plan,
			acc.Status,
			fiveHour,
			weekly,
			cooldown,
		)
	}

	return w.Flush()
}

// Switch changes the active credential account.
func (c *AccountsCLI) Switch(ctx context.Context, query string) error {
	if err := c.mgr.SwitchAccount(ctx, query); err != nil {
		return fmt.Errorf("switch account failed: %w", err)
	}
	fmt.Fprintf(c.out, "Successfully switched active account to %q\n", query)
	return nil
}

// Rotate triggers account rotation to the next eligible healthy account.
func (c *AccountsCLI) Rotate(ctx context.Context) error {
	if err := c.mgr.RotateAccount(ctx); err != nil {
		return fmt.Errorf("rotate account failed: %w", err)
	}
	fmt.Fprintln(c.out, "Successfully rotated to next healthy account.")
	return nil
}

// SetAlias assigns a friendly nickname to an account.
func (c *AccountsCLI) SetAlias(ctx context.Context, query, alias string) error {
	if err := c.mgr.SetAlias(ctx, query, alias); err != nil {
		return fmt.Errorf("set alias failed: %w", err)
	}
	fmt.Fprintf(c.out, "Successfully set alias %q for account %q\n", alias, query)
	return nil
}

// ClearAlias removes a friendly nickname from an account.
func (c *AccountsCLI) ClearAlias(ctx context.Context, query string) error {
	if err := c.mgr.ClearAlias(ctx, query); err != nil {
		return fmt.Errorf("clear alias failed: %w", err)
	}
	fmt.Fprintf(c.out, "Successfully cleared alias for account %q\n", query)
	return nil
}

// Remove deletes an account from the vault.
func (c *AccountsCLI) Remove(ctx context.Context, query string) error {
	if err := c.mgr.RemoveAccount(ctx, query); err != nil {
		return fmt.Errorf("remove account failed: %w", err)
	}
	fmt.Fprintf(c.out, "Successfully removed account %q from vault.\n", query)
	return nil
}

// Status outputs the raw agy-auth status diagnostics.
func (c *AccountsCLI) Status(ctx context.Context, asJSON bool) error {
	out, err := c.mgr.Status(ctx)
	if err != nil {
		return fmt.Errorf("status query failed: %w", err)
	}

	if asJSON {
		var pretty bytes.Buffer
		if jsonErr := json.Indent(&pretty, out, "", "  "); jsonErr == nil {
			fmt.Fprintln(c.out, pretty.String())
			return nil
		}
	}
	fmt.Fprintln(c.out, string(out))
	return nil
}

// Clean purges stale sessions and temporary artifacts.
func (c *AccountsCLI) Clean(ctx context.Context, dryRun bool) error {
	out, err := c.mgr.Clean(ctx, dryRun)
	if err != nil {
		return fmt.Errorf("clean failed: %w", err)
	}
	fmt.Fprintln(c.out, string(out))
	return nil
}

// Add initiates an interactive or headless OAuth login onboarding flow.
func (c *AccountsCLI) Add(ctx context.Context, alias string) error {
	return c.runAddWizard(ctx, nil, alias)
}
