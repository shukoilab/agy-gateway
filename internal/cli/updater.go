package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"agy-gateway/internal/config"
	"agy-gateway/internal/updater"
)

// UpdaterCLI provides terminal interfaces for managing and monitoring agy and agy-auth auto-updates.
type UpdaterCLI struct {
	cfg *config.Config
	svc *updater.Service
	in  io.Reader
	out io.Writer
}

// NewUpdaterCLI initializes an UpdaterCLI instance.
func NewUpdaterCLI(cfg *config.Config, svc *updater.Service, in io.Reader, out io.Writer) *UpdaterCLI {
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	if svc == nil && cfg != nil {
		svc = updater.NewService(cfg, nil, nil)
	}
	return &UpdaterCLI{
		cfg: cfg,
		svc: svc,
		in:  in,
		out: out,
	}
}

// Run parses subcommands and executes the requested action.
func (c *UpdaterCLI) Run(args []string) error {
	if len(args) == 0 {
		return c.ShowStatus(context.Background(), false)
	}

	fs := flag.NewFlagSet("updater", flag.ContinueOnError)
	fs.SetOutput(c.out)
	asJSON := fs.Bool("json", false, "Output results in JSON format")

	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	subcmd := strings.ToLower(args[0])
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	switch subcmd {
	case "status", "list":
		return c.ShowStatus(ctx, *asJSON)
	case "check", "update", "sync", "now":
		return c.CheckAndInstall(ctx, *asJSON)
	case "help", "-h", "--help":
		c.printHelp()
		return nil
	default:
		return fmt.Errorf("unknown updater subcommand %q; run 'update help' for usage", subcmd)
	}
}

// ShowStatus displays a formatted table or JSON summary of component versions and auto-update state.
func (c *UpdaterCLI) ShowStatus(ctx context.Context, asJSON bool) error {
	if c.svc == nil {
		return fmt.Errorf("updater service not initialized")
	}

	c.svc.RefreshCurrentVersions(ctx)
	status := c.svc.GetStatus()

	if asJSON {
		enc := json.NewEncoder(c.out)
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}

	fmt.Fprintln(c.out, "================================================================================")
	fmt.Fprintln(c.out, "        GOOGLE ANTIGRAVITY GATEWAY — AUTO-UPDATER & BINARY STATUS")
	fmt.Fprintln(c.out, "================================================================================")
	fmt.Fprintf(c.out, "Auto-Update Enabled : %v\n", status.Enabled)
	fmt.Fprintf(c.out, "Cron Check Interval : %s\n", status.Interval)
	if !status.NextCheckAt.IsZero() {
		fmt.Fprintf(c.out, "Next Scheduled Check: %s\n", status.NextCheckAt.Format(time.RFC3339))
	}
	fmt.Fprintf(c.out, "Engine Lifecycle    : %s\n", status.Status)
	fmt.Fprintf(c.out, "Binaries Directory  : %s\n", c.cfg.AutoUpdate.BinDir)
	fmt.Fprintln(c.out, "--------------------------------------------------------------------------------")

	w := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "COMPONENT\tCURRENT VER\tLATEST UPSTREAM\tUPDATE READY\tACTIVE BINARY PATH")
	fmt.Fprintln(w, "---------\t-----------\t---------------\t------------\t------------------")

	for comp, info := range status.Components {
		cur := info.CurrentVersion
		if cur == "" {
			cur = "(unknown)"
		}
		lat := info.LatestVersion
		if lat == "" {
			lat = "(not checked)"
		}
		ready := "No"
		if info.UpdateAvailable {
			ready = "YES (Update Available)"
		}
		p := info.ActiveBinaryPath
		if p == "" {
			p = "(none)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", comp, cur, lat, ready, p)
	}
	_ = w.Flush()
	fmt.Fprintln(c.out, "================================================================================")
	return nil
}

// CheckAndInstall triggers an immediate check against upstream manifests and upgrades any outdated binaries.
func (c *UpdaterCLI) CheckAndInstall(ctx context.Context, asJSON bool) error {
	if c.svc == nil {
		return fmt.Errorf("updater service not initialized")
	}

	if !asJSON {
		fmt.Fprintln(c.out, "Checking upstream repositories for agy CLI and agy-auth updates...")
	}

	result, err := c.svc.CheckAndUpdateAll(ctx)
	if asJSON {
		enc := json.NewEncoder(c.out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(result)
		return err
	}

	if result.AgyChecked {
		if result.AgyUpdated {
			fmt.Fprintf(c.out, "✓ Upgraded Google Antigravity CLI (agy): %s -> %s\n", result.AgyOldVersion, result.AgyNewVersion)
		} else {
			fmt.Fprintf(c.out, "• Google Antigravity CLI (agy) is up to date: %s\n", result.AgyOldVersion)
		}
	}

	if result.AgyAuthChecked {
		if result.AgyAuthUpdated {
			fmt.Fprintf(c.out, "✓ Upgraded @fikrilal/agy-auth: %s -> %s\n", result.AgyAuthOldVersion, result.AgyAuthNewVersion)
		} else {
			fmt.Fprintf(c.out, "• @fikrilal/agy-auth is up to date: %s\n", result.AgyAuthOldVersion)
		}
	}

	if len(result.Errors) > 0 {
		fmt.Fprintln(c.out, "\nErrors encountered during update:")
		for _, e := range result.Errors {
			fmt.Fprintf(c.out, "  - %s\n", e)
		}
	} else {
		fmt.Fprintln(c.out, "\nAll managed binaries are synchronized to their latest versions.")
	}

	return err
}

// RunInteractive launches the interactive TUI menu for auto-updater inspection and manual triggers.
func (c *UpdaterCLI) RunInteractive(ctx context.Context) error {
	reader := bufio.NewReader(c.in)
	for {
		fmt.Fprintln(c.out, `
--------------------------------------------------------------------------------
           AUTO-UPDATER & BINARY VERSION MANAGEMENT
--------------------------------------------------------------------------------
[1] View Version Status & Scheduled Interval
[2] Check & Update Now (Force Immediate Upgrade)
[0] Back to Main Menu
--------------------------------------------------------------------------------`)
		fmt.Fprint(c.out, "Select an option [0-2]: ")
		input, err := reader.ReadString('\n')
		if err != nil {
			return nil
		}

		choice := strings.TrimSpace(input)
		switch choice {
		case "1":
			_ = c.ShowStatus(ctx, false)
		case "2":
			_ = c.CheckAndInstall(ctx, false)
		case "0", "q", "exit":
			return nil
		default:
			fmt.Fprintln(c.out, "Invalid option. Please choose between 0 and 2.")
		}
	}
}

func (c *UpdaterCLI) printHelp() {
	fmt.Fprintln(c.out, `Google Antigravity Gateway — Auto-Updater CLI

Usage:
  agy-gateway update [command] [flags]
  agy-accounts update [command] [flags]

Commands:
  status, list   Display current version status, schedules, and active paths
  check, now     Trigger immediate check and download/install updates
  help           Display this help message

Flags:
  -json          Output results in JSON format`)
}
