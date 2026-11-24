package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"agy-gateway/docs"
	"agy-gateway/internal/auth"
	"agy-gateway/internal/cli"
	"agy-gateway/internal/config"
	"agy-gateway/internal/models"
	"agy-gateway/internal/pool"
	"agy-gateway/internal/server"
)

// @title           agy-gateway API
// @version         3.5.0
// @description     Enterprise-grade OpenAI-compatible reverse proxy gateway bridging OpenAI client applications to Google Antigravity CLI (`agy acp`).
// @description     Features multi-account quota rotation, live quota tracking (`agy /usage`), streaming reasoning tokens (`reasoning_effort` / `reasoning_content`), client-side tool calling with JIT prompt envelope translation, and headless OAuth onboarding.
// @termsOfService  https://github.com/shukoilab/agy-gateway

// @contact.name   ShukoiLab
// @contact.url    https://github.com/shukoilab/agy-gateway
// @contact.email  shukoilab@proton.me

// @license.name  MIT
// @license.url   https://github.com/shukoilab/agy-gateway/blob/main/LICENSE

// @host      localhost:8080
// @BasePath  /

// @securityDefinitions.apikey  ClientBearerAuth
// @in                          header
// @name                        Authorization
// @description                 Client API authorization. Format: `Bearer <your-client-api-key>`. Used to access OpenAI-compatible endpoints (`/v1/*`).

// @securityDefinitions.apikey  AdminBearerAuth
// @in                          header
// @name                        Authorization
// @description                 Admin Control Plane authorization. Format: `Bearer <your-admin-api-key>`. Used to access administrative endpoints (`/admin/v1/*`).

var version = "3.5.0"

func main() {
	// 0. Load .env file if present in working directory (non-overwriting)
	_, _ = config.LoadDotEnv()

	// Direct delegation if invoked as 'agy-gateway accounts [subcommand]'
	if len(os.Args) > 1 && (os.Args[1] == "accounts" || os.Args[1] == "account") {
		runAccountsSubcommand(os.Args[2:])
		return
	}

	// Direct delegation if invoked as 'agy-gateway models [subcommand]'
	if len(os.Args) > 1 && (os.Args[1] == "models" || os.Args[1] == "model") {
		runModelsSubcommand(os.Args[2:])
		return
	}

	configPath := flag.String("config", "config.yaml", "Path to YAML configuration file")
	healthcheck := flag.Bool("healthcheck", false, "Run local HTTP health check probe and exit")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("agy-gateway version %s\n", version)
		os.Exit(0)
	}

	// 1. Handle Docker healthcheck mode
	if *healthcheck {
		runHealthcheck(*configPath)
		return
	}

	// 2. Load and validate configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: Config load failed: %v\n", err)
		os.Exit(1)
	}

	// Dynamically configure Swagger UI host and version
	docs.SwaggerInfo.Version = version
	if cfg.Server.Host != "" && cfg.Server.Host != "0.0.0.0" && cfg.Server.Host != "127.0.0.1" && cfg.Server.Host != "localhost" {
		docs.SwaggerInfo.Host = fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	} else {
		// Empty Host instructs Swagger UI in the browser to bind dynamically to window.location.host
		docs.SwaggerInfo.Host = ""
	}

	// 3. Initialize structured logger
	var logHandler slog.Handler
	logOpts := &slog.HandlerOptions{
		Level: parseLogLevel(cfg.Observability.LogLevel),
	}
	if stringsEqual(cfg.Observability.LogFormat, "json") {
		logHandler = slog.NewJSONHandler(os.Stdout, logOpts)
	} else {
		logHandler = slog.NewTextHandler(os.Stdout, logOpts)
	}
	logger := slog.New(logHandler).With("version", version)
	slog.SetDefault(logger)

	logger.Info("starting agy-gateway", "version", version, "config", *configPath)

	// 4. Create base storage directories
	for _, dir := range []string{
		cfg.Storage.BaseDataDir,
		cfg.Storage.VaultXDGConfigDir,
		cfg.Storage.WorkersRootDir,
		cfg.Storage.WorkersTmpDir,
		cfg.Storage.WorkspacesRootDir,
	} {
		if dir != "" {
			_ = os.MkdirAll(dir, 0755)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 5. Initialize auth manager and sync accounts
	authMgr := auth.NewManager(cfg, logger)
	if err := authMgr.Sync(ctx); err != nil {
		logger.Warn("initial agy-auth sync reported error (proceeding with vault state)", "err", err)
	}

	// 6. Start quota poller and OAuth onboarding manager
	quotaPoller := auth.NewQuotaPoller(authMgr, cfg, logger)
	quotaPoller.Start(ctx)
	defer quotaPoller.Stop()

	onboardingMgr := auth.NewOAuthOnboardingManager(cfg, authMgr, logger)
	defer onboardingMgr.Close()

	// 7. Initialize worker pool & failover coordinator
	workerPool, err := pool.NewPool(ctx, cfg, authMgr, logger)
	if err != nil {
		logger.Error("failed to initialize worker pool", "err", err)
		os.Exit(1)
	}
	defer workerPool.Close()

	failoverCoord := pool.NewFailoverCoordinator(workerPool, authMgr, logger)

	// 8. Initialize dynamic model discovery registry
	modelRegistry := models.NewModelRegistry(cfg, authMgr, logger)
	if err := modelRegistry.SyncModels(ctx); err != nil {
		logger.Warn("initial dynamic model discovery encountered issue", "err", err)
	}

	// 9. Wire HTTP handlers and server
	openaiHandlers := server.NewOpenAIHandlers(cfg, workerPool, failoverCoord, modelRegistry, logger)
	anthropicHandlers := server.NewAnthropicHandlers(cfg, workerPool, failoverCoord, modelRegistry, logger)
	adminHandlers := server.NewAdminHandlers(cfg, authMgr, onboardingMgr, modelRegistry)
	opsHandlers := server.NewOpsHandlers()

	srv := server.NewServer(cfg, openaiHandlers, anthropicHandlers, adminHandlers, opsHandlers, logger)

	// 10. Intercept OS shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		logger.Info("received termination signal; initiating graceful drain", "signal", sig.String())
		cancel()

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownDrainTimeout)
		defer shutdownCancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("server shutdown encountered error", "err", err)
		}
	}()

	// 11. Start HTTP server
	if err := srv.Start(); err != nil {
		logger.Error("HTTP server exited unexpectedly", "err", err)
		os.Exit(1)
	}

	logger.Info("agy-gateway terminated cleanly")
}

// @purpose: Performs an HTTP GET against /healthz on the dynamically discovered port and verifies 200 OK.
// @rationale: Decouples health probing logic from process exit semantics, enabling isolated unit testing.
// @preconditions: None.
// - @param: configPath (string) - Path to configuration file.
// - @returns: nil if healthy, or an error detailing probe failure.
// - @throws: None.
// - @invariants: Guaranteed 3s timeout; closes response body on every execution path.
func checkHealth(configPath string) error {
	_, _ = config.LoadDotEnv()

	port := 8080
	cfg, err := config.Load(configPath)
	if err == nil && cfg.Server.Port > 0 {
		port = cfg.Server.Port
	} else {
		if envPort := os.Getenv("AGY_GW_PORT"); envPort != "" {
			if p, err := strconv.Atoi(envPort); err == nil && p > 0 {
				port = p
			}
		} else if envPort := os.Getenv("PORT"); envPort != "" {
			if p, err := strconv.Atoi(envPort); err == nil && p > 0 {
				port = p
			}
		}
	}

	client := &http.Client{Timeout: 3 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("probe request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy HTTP status: %d", resp.StatusCode)
	}
	return nil
}

// @purpose: Executes an ephemeral HTTP health probe against /healthz and terminates the process with 0 (healthy) or 1 (unhealthy).
// @rationale: Decouples health checks from hardcoded curl commands and hardcoded ports, dynamically discovering the active port from config or environment variables.
// @preconditions: Target server process should be running or starting up.
// - @param: configPath (string) - Path to the configuration file (optional; falls back to env vars if absent).
// - @returns: None (calls os.Exit).
// - @throws: os.Exit(1) on failure to connect or non-200 HTTP status.
// - @invariants: Fast-fail execution within 3 seconds timeout. Prevents socket/descriptor leaks.
func runHealthcheck(configPath string) {
	if err := checkHealth(configPath); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// @purpose: Dispatches the accounts sub-command to the internal CLI account manager.
// @rationale: Provides a unified single-binary entrypoint for both gateway daemon and account CLI tasks.
// @preconditions: None.
// - @param: args ([]string) - Subcommand arguments passed after "accounts".
// - @returns: None (terminates or returns upon command completion).
// - @throws: os.Exit(1) on fatal config or execution failures.
// - @invariants: Cleans up signal notifications and I/O descriptors. Thread-safe.
func runAccountsSubcommand(args []string) {
	configPath := "config.yaml"
	if _, err := os.Stat("/etc/agy-gateway/config.yaml"); err == nil {
		configPath = "/etc/agy-gateway/config.yaml"
	}
	for i, arg := range args {
		if arg == "-config" && i+1 < len(args) {
			configPath = args[i+1]
		}
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: Config load failed: %v\n", err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	}))
	mgr := auth.NewManager(cfg, logger)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	_ = mgr.Sync(ctx)

	accountsCLI := cli.NewAccountsCLI(cfg, mgr, nil, os.Stdin, os.Stdout)
	if err := accountsCLI.Run(ctx, args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// @purpose: Runs the Anthropic models management CLI subcommand.
// @rationale: Provides a CLI interface for inspecting models, rankings, and managing aliases.
// @preconditions: None.
// - @param: args ([]string) - Subcommand arguments passed after "models".
// - @returns: None (terminates or returns upon command completion).
// - @throws: os.Exit(1) on fatal config or execution failures.
// - @invariants: Cleans up signal notifications and I/O descriptors. Thread-safe.
func runModelsSubcommand(args []string) {
	configPath := "config.yaml"
	if _, err := os.Stat("/etc/agy-gateway/config.yaml"); err == nil {
		configPath = "/etc/agy-gateway/config.yaml"
	}
	for i, arg := range args {
		if arg == "-config" && i+1 < len(args) {
			configPath = args[i+1]
		}
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: Config load failed: %v\n", err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	}))
	mgr := auth.NewManager(cfg, logger)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	_ = mgr.Sync(ctx)

	mr := models.NewModelRegistry(cfg, mgr, logger)
	modelsCLI := cli.NewModelsCLI(cfg, mgr, mr, os.Stdin, os.Stdout)
	if err := modelsCLI.Run(ctx, args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// @purpose: Maps a string representation of log level into slog.Level.
// @rationale: Allows flexible case-insensitive log level configuration via YAML or env vars.
// @preconditions: None.
// - @param: level (string) - Case-insensitive string name of log level (debug, info, warn, error).
// - @returns: Corresponding slog.Level (defaults to slog.LevelInfo).
// - @throws: None.
// - @invariants: O(n) scan where n is string length. Thread-safe.
func parseLogLevel(level string) slog.Level {
	switch stringsToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// @purpose: Converts an ASCII string to lowercase without external dependencies or heavy allocations.
// @rationale: Provides zero-dependency string case normalization for config options.
// @preconditions: None.
// - @param: s (string) - Input ASCII string.
// - @returns: Lowercase string copy.
// - @throws: None.
// - @invariants: O(n) time, O(n) space. Thread-safe.
func stringsToLower(s string) string {
	b := make([]byte, len(s))
	for i := range s {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

// @purpose: Compares two strings case-insensitively.
// @rationale: Provides equality checking for configuration format and protocol values.
// @preconditions: None.
// - @param: a (string) - First string.
// - @param: b (string) - Second string.
// - @returns: True if lowercase representations match.
// - @throws: None.
// - @invariants: O(len(a) + len(b)). Thread-safe.
func stringsEqual(a, b string) bool {
	return stringsToLower(a) == stringsToLower(b)
}
