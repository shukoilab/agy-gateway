// Package main provides the standalone agy-accounts CLI tool for interactive and
// non-interactive CRUD lifecycle management of Google Antigravity credential accounts.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"agy-gateway/internal/auth"
	"agy-gateway/internal/cli"
	"agy-gateway/internal/config"
)

var version = "3.5.0"

func main() {
	// 0. Load .env file if present in working directory (non-overwriting)
	_, _ = config.LoadDotEnv()

	// 1. Define flags
	defaultConfig := "config.yaml"
	if _, err := os.Stat("/etc/agy-gateway/config.yaml"); err == nil {
		defaultConfig = "/etc/agy-gateway/config.yaml"
	}
	configPath := flag.String("config", defaultConfig, "Path to YAML configuration file")
	showVersion := flag.Bool("version", false, "Print version and exit")

	// Custom flag parsing to allow subcommands after flags or before flags
	flag.Usage = func() {
		dummyCLI := cli.NewAccountsCLI(nil, nil, nil, os.Stdin, os.Stdout)
		dummyCLI.PrintUsage()
	}
	flag.Parse()

	if *showVersion {
		fmt.Printf("agy-accounts version %s\n", version)
		os.Exit(0)
	}

	// 2. Load and validate configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: Config load failed: %v\n", err)
		os.Exit(1)
	}

	// 3. Initialize quiet logger for CLI tool
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	}))

	// 4. Initialize account manager & sync accounts from vault
	mgr := auth.NewManager(cfg, logger)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Initial vault sync
	_ = mgr.Sync(ctx)

	// 5. Run Models CLI if 'models' subcommand requested, Updater CLI if 'update' requested, otherwise Accounts CLI
	args := flag.Args()
	if len(args) > 0 && (args[0] == "models" || args[0] == "model") {
		modelsCLI := cli.NewModelsCLI(cfg, mgr, nil, os.Stdin, os.Stdout)
		if err := modelsCLI.Run(ctx, args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if len(args) > 0 && (args[0] == "update" || args[0] == "updater") {
		updaterCLI := cli.NewUpdaterCLI(cfg, nil, os.Stdin, os.Stdout)
		if err := updaterCLI.Run(args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	accountsCLI := cli.NewAccountsCLI(cfg, mgr, nil, os.Stdin, os.Stdout)
	if err := accountsCLI.Run(ctx, args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
