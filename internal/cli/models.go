// Package cli provides interactive terminal and command-line interfaces for Anthropic model
// aliasing, dynamic capability ranking, and manual admin overrides.
package cli

import (
	"bufio"
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
	"agy-gateway/internal/models"
)

// ModelsCLI coordinates model alias management and interactive terminal sessions.
type ModelsCLI struct {
	cfg     *config.Config
	authMgr *auth.Manager
	mr      *models.ModelRegistry
	in      io.Reader
	out     io.Writer
}

// NewModelsCLI initializes a ModelsCLI controller with dependency injection.
//
//	@purpose Creates an initialized ModelsCLI instance for terminal or scripted invocation.
//	@rationale Encapsulates registry, auth manager, and I/O streams for testability.
//	@preconditions cfg must not be nil.
//	- @param: cfg Application configuration.
//	- @param: authMgr Account credentials manager.
//	- @param: mr Dynamic model registry (created lazily if nil).
//	- @param: in Terminal standard input stream.
//	- @param: out Terminal standard output stream.
//	@returns Ready-to-use ModelsCLI instance.
//	@throws Never panics.
//	@invariants Thread-safe for sequential CLI operations.
func NewModelsCLI(cfg *config.Config, authMgr *auth.Manager, mr *models.ModelRegistry, in io.Reader, out io.Writer) *ModelsCLI {
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	if mr == nil && cfg != nil && authMgr != nil {
		mr = models.NewModelRegistry(cfg, authMgr, nil)
	}
	return &ModelsCLI{
		cfg:     cfg,
		authMgr: authMgr,
		mr:      mr,
		in:      in,
		out:     out,
	}
}

// PrintUsage outputs formatted CLI command help.
func (c *ModelsCLI) PrintUsage() {
	fmt.Fprintln(c.out, `Google Antigravity Gateway — Anthropic Model Aliasing & Ranking CLI

Usage:
  agy-gateway models [command] [flags]
  agy-accounts models [command] [flags]

Commands:
  (no command)         Launch interactive TUI alias management menu
  interactive, -i      Launch interactive TUI alias management menu explicitly
  list, aliases        List all active model aliases, sources, and elected champions
  set <alias> <target> Create or update a manual alias override (e.g. claude-3-7-sonnet claude-sonnet-4-6)
  remove, del <alias>  Delete a manual alias override (reverting to dynamic champion routing)
  sync                 Discover models from accounts & fetch live rankings from models.dev
  champions            Display currently elected family champions
  help                 Display this help message

Flags:
  -config <path>       Path to config.yaml
  -json                Output command results in machine-parseable JSON format
  -desc <text>         Optional description when setting a manual alias override`)
}

// Run parses arguments and executes the designated models subcommand.
//
//	@purpose Dispatches CLI commands or initiates interactive terminal loop.
//	@rationale Standard command dispatching with flag parsing and context propagation.
//	@preconditions ctx must be valid.
//	- @param: ctx Execution context.
//	- @param: args Command-line arguments slice.
//	@returns Nil on success or a typed execution error.
func (c *ModelsCLI) Run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return c.RunInteractive(ctx)
	}

	cmd := strings.ToLower(args[0])
	subArgs := args[1:]

	switch cmd {
	case "interactive", "-i", "--interactive":
		return c.RunInteractive(ctx)

	case "list", "aliases", "ls":
		fs := flag.NewFlagSet("list", flag.ContinueOnError)
		fs.SetOutput(c.out)
		asJSON := fs.Bool("json", false, "Output as JSON")
		if err := fs.Parse(subArgs); err != nil {
			return err
		}
		return c.List(ctx, *asJSON)

	case "set":
		fs := flag.NewFlagSet("set", flag.ContinueOnError)
		fs.SetOutput(c.out)
		desc := fs.String("desc", "", "Description for manual alias override")
		asJSON := fs.Bool("json", false, "Output as JSON")
		if err := fs.Parse(subArgs); err != nil {
			return err
		}
		remaining := fs.Args()
		if len(remaining) < 2 {
			return errors.New("usage: models set <alias> <target> [-desc 'description']")
		}
		description := *desc
		if description == "" && len(remaining) > 2 {
			description = strings.Join(remaining[2:], " ")
		}
		return c.Set(ctx, remaining[0], remaining[1], description, *asJSON)

	case "remove", "delete", "del", "rm":
		fs := flag.NewFlagSet("remove", flag.ContinueOnError)
		fs.SetOutput(c.out)
		asJSON := fs.Bool("json", false, "Output as JSON")
		if err := fs.Parse(subArgs); err != nil {
			return err
		}
		remaining := fs.Args()
		if len(remaining) < 1 {
			return errors.New("usage: models remove <alias>")
		}
		return c.Remove(ctx, remaining[0], *asJSON)

	case "sync":
		fs := flag.NewFlagSet("sync", flag.ContinueOnError)
		fs.SetOutput(c.out)
		asJSON := fs.Bool("json", false, "Output as JSON")
		if err := fs.Parse(subArgs); err != nil {
			return err
		}
		return c.Sync(ctx, *asJSON)

	case "champions":
		fs := flag.NewFlagSet("champions", flag.ContinueOnError)
		fs.SetOutput(c.out)
		asJSON := fs.Bool("json", false, "Output as JSON")
		if err := fs.Parse(subArgs); err != nil {
			return err
		}
		return c.Champions(ctx, *asJSON)

	case "help", "-h", "--help":
		c.PrintUsage()
		return nil

	default:
		return fmt.Errorf("unknown command %q. Run 'models help' for usage", cmd)
	}
}

// List prints all active aliases and elected champions in table or JSON format.
func (c *ModelsCLI) List(ctx context.Context, jsonOutput bool) error {
	if c.mr == nil {
		return errors.New("model registry is not initialized")
	}

	// Trigger initial discovery if empty
	if len(c.mr.ListModels()) == 0 {
		_ = c.mr.SyncModels(ctx)
	}

	aliases := c.mr.AliasStore().GetAllAliases()
	champions := c.mr.Champions()

	if jsonOutput {
		payload := map[string]interface{}{
			"total":     len(aliases),
			"champions": champions,
			"aliases":   aliases,
		}
		enc := json.NewEncoder(c.out)
		enc.SetIndent("", "  ")
		return enc.Encode(payload)
	}

	fmt.Fprintln(c.out, "\n=== ELECTED FAMILY CHAMPIONS ===")
	if len(champions) == 0 {
		fmt.Fprintln(c.out, "No champions elected yet. Run 'models sync' to discover accounts & rank models.")
	} else {
		for fam, info := range champions {
			if info != nil {
				fmt.Fprintf(c.out, "  • %-16s -> %s (%s)\n", fam, info.ID, info.DisplayName)
			}
		}
	}

	fmt.Fprintln(c.out, "\n=== ACTIVE MODEL ALIASES ===")
	if len(aliases) == 0 {
		fmt.Fprintln(c.out, "No active model aliases configured.")
		return nil
	}

	w := tabwriter.NewWriter(c.out, 0, 4, 3, ' ', 0)
	fmt.Fprintln(w, "ALIAS\tTARGET MODEL\tSOURCE\tUPDATED\tDESCRIPTION")
	fmt.Fprintln(w, "-----\t------------\t------\t-------\t-----------")

	for _, a := range aliases {
		srcLabel := string(a.Source)
		if a.Source == models.SourceAdminOverride {
			srcLabel = "MANUAL OVERRIDE"
		} else if a.Source == models.SourceAutoSynthesized {
			srcLabel = "AUTO (CHAMPION)"
		}

		updatedStr := "-"
		if !a.UpdatedAt.IsZero() {
			updatedStr = a.UpdatedAt.Format(time.RFC3339)
		}

		desc := a.Description
		if desc == "" {
			desc = "-"
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", a.Alias, a.Target, srcLabel, updatedStr, desc)
	}
	return w.Flush()
}

// Set creates or overrides a manual model alias mapping.
func (c *ModelsCLI) Set(ctx context.Context, alias, target, description string, jsonOutput bool) error {
	if c.mr == nil {
		return errors.New("model registry is not initialized")
	}

	alias = strings.TrimSpace(alias)
	target = strings.TrimSpace(target)
	if alias == "" || target == "" {
		return errors.New("both alias and target model must not be empty")
	}

	if err := c.mr.AliasStore().SetManual(alias, target, description); err != nil {
		return fmt.Errorf("failed to save manual alias: %w", err)
	}

	if jsonOutput {
		return json.NewEncoder(c.out).Encode(map[string]interface{}{
			"status": "configured",
			"alias":  alias,
			"target": target,
		})
	}

	fmt.Fprintf(c.out, "✓ Successfully configured manual alias: %s -> %s\n", alias, target)
	return nil
}

// Remove deletes a manual alias override.
func (c *ModelsCLI) Remove(ctx context.Context, alias string, jsonOutput bool) error {
	if c.mr == nil {
		return errors.New("model registry is not initialized")
	}

	alias = strings.TrimSpace(alias)
	if alias == "" {
		return errors.New("alias cannot be empty")
	}

	deleted, err := c.mr.AliasStore().DeleteManual(alias)
	if err != nil {
		return fmt.Errorf("failed to delete manual alias: %w", err)
	}
	if !deleted {
		return fmt.Errorf("manual alias override for %q not found", alias)
	}

	if jsonOutput {
		return json.NewEncoder(c.out).Encode(map[string]interface{}{
			"status": "deleted",
			"alias":  alias,
		})
	}

	fmt.Fprintf(c.out, "✓ Successfully removed manual alias override: %s (reverted to dynamic champion)\n", alias)
	return nil
}

// Sync runs dynamic model discovery and refreshes rankings from models.dev.
func (c *ModelsCLI) Sync(ctx context.Context, jsonOutput bool) error {
	if c.mr == nil {
		return errors.New("model registry is not initialized")
	}

	fmt.Fprintln(c.out, "Synchronizing models from accounts and refreshing intelligence from models.dev...")
	if err := c.mr.SyncModels(ctx); err != nil {
		return fmt.Errorf("model sync failed: %w", err)
	}

	champions := c.mr.Champions()
	discoveredCount := len(c.mr.ListModels())
	aliasCount := len(c.mr.AliasStore().GetAllAliases())

	if jsonOutput {
		return json.NewEncoder(c.out).Encode(map[string]interface{}{
			"status":            "synchronized",
			"discovered_models": discoveredCount,
			"total_aliases":     aliasCount,
			"champions":         champions,
		})
	}

	fmt.Fprintf(c.out, "✓ Sync completed: %d models discovered, %d active aliases.\n", discoveredCount, aliasCount)
	return c.List(ctx, false)
}

// Champions outputs the elected family champions.
func (c *ModelsCLI) Champions(ctx context.Context, jsonOutput bool) error {
	if c.mr == nil {
		return errors.New("model registry is not initialized")
	}

	champions := c.mr.Champions()
	if jsonOutput {
		return json.NewEncoder(c.out).Encode(champions)
	}

	fmt.Fprintln(c.out, "\n=== ELECTED FAMILY CHAMPIONS ===")
	if len(champions) == 0 {
		fmt.Fprintln(c.out, "No champions elected yet. Run 'models sync' to discover accounts & rank models.")
		return nil
	}

	for fam, info := range champions {
		if info != nil {
			fmt.Fprintf(c.out, "  • %-16s -> %s (%s)\n", fam, info.ID, info.DisplayName)
		}
	}
	return nil
}

// RunInteractive presents an interactive terminal loop for Anthropic model aliasing.
func (c *ModelsCLI) RunInteractive(ctx context.Context) error {
	reader := bufio.NewReader(c.in)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		c.printInteractiveHeader()
		fmt.Fprint(c.out, "Select an option [0-4]: ")

		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				fmt.Fprintln(c.out, "\nExiting model alias manager.")
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
			c.interactiveSet(ctx, reader)

		case "3":
			c.interactiveRemove(ctx, reader)

		case "4":
			_ = c.Sync(ctx, false)

		case "0", "q", "quit", "exit", "back":
			fmt.Fprintln(c.out, "Returning to main menu.")
			return nil

		default:
			fmt.Fprintf(c.out, "Invalid choice %q. Please select 0 to 4.\n", choice)
		}

		fmt.Fprint(c.out, "\nPress Enter to return to menu...")
		_, _ = reader.ReadString('\n')
	}
}

func (c *ModelsCLI) printInteractiveHeader() {
	fmt.Fprintln(c.out, `================================================================================
       GOOGLE ANTIGRAVITY GATEWAY — ANTHROPIC MODEL ALIASES & RANKING
================================================================================
[1] List All Model Aliases & Champions  (Inspect active mappings & elected champions)
[2] Set / Override Manual Model Alias   (Pin alias to specific discovered model)
[3] Remove Manual Model Alias Override  (Revert alias to dynamic capability routing)
[4] Trigger Dynamic Sync & Ranking      (Refresh from accounts and models.dev)
[0] Back / Exit
--------------------------------------------------------------------------------`)
}

func (c *ModelsCLI) interactiveSet(ctx context.Context, reader *bufio.Reader) {
	fmt.Fprint(c.out, "Enter canonical alias (e.g. claude-3-7-sonnet): ")
	aliasLine, _ := reader.ReadString('\n')
	alias := strings.TrimSpace(aliasLine)
	if alias == "" {
		fmt.Fprintln(c.out, "Operation canceled: alias cannot be empty.")
		return
	}

	fmt.Fprint(c.out, "Enter target model ID (e.g. claude-sonnet-4-6): ")
	targetLine, _ := reader.ReadString('\n')
	target := strings.TrimSpace(targetLine)
	if target == "" {
		fmt.Fprintln(c.out, "Operation canceled: target model cannot be empty.")
		return
	}

	fmt.Fprint(c.out, "Enter optional description (or press Enter to skip): ")
	descLine, _ := reader.ReadString('\n')
	desc := strings.TrimSpace(descLine)

	if err := c.Set(ctx, alias, target, desc, false); err != nil {
		fmt.Fprintf(c.out, "Error: %v\n", err)
	}
}

func (c *ModelsCLI) interactiveRemove(ctx context.Context, reader *bufio.Reader) {
	fmt.Fprint(c.out, "Enter alias to remove manual override for: ")
	line, _ := reader.ReadString('\n')
	alias := strings.TrimSpace(line)
	if alias == "" {
		fmt.Fprintln(c.out, "Operation canceled: alias cannot be empty.")
		return
	}

	if err := c.Remove(ctx, alias, false); err != nil {
		fmt.Fprintf(c.out, "Error: %v\n", err)
	}
}
