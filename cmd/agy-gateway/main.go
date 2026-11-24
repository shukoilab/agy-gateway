package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"agy-gateway/internal/auth"
	"agy-gateway/internal/config"
	"agy-gateway/internal/models"
	"agy-gateway/internal/pool"
	"agy-gateway/internal/server"
)

// @title           agy-gateway API
// @version         3.4.0
// @description     Enterprise-grade OpenAI-compatible reverse proxy gateway bridging OpenAI client applications to Google Antigravity CLI (`agy acp`).
// @description     Features multi-account quota rotation, live quota tracking (`agy /usage`), streaming reasoning tokens (`reasoning_effort` / `reasoning_content`), client-side tool calling with JIT prompt envelope translation, and headless OAuth onboarding.
// @termsOfService  https://github.com/shukoilab/agy-gateway

// @contact.name   ShukoiLab
// @contact.url    https://github.com/shukoilab/agy-gateway
// @contact.email  support@shukoilab.com

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

var version = "3.4.0"

func main() {
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
	adminHandlers := server.NewAdminHandlers(cfg, authMgr, onboardingMgr, modelRegistry)
	opsHandlers := server.NewOpsHandlers()

	srv := server.NewServer(cfg, openaiHandlers, adminHandlers, opsHandlers, logger)

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

	// 10. Start HTTP server
	if err := srv.Start(); err != nil {
		logger.Error("HTTP server exited unexpectedly", "err", err)
		os.Exit(1)
	}

	logger.Info("agy-gateway terminated cleanly")
}

func runHealthcheck(configPath string) {
	cfg, err := config.Load(configPath)
	port := 8080
	if err == nil && cfg.Server.Port > 0 {
		port = cfg.Server.Port
	}

	client := &http.Client{Timeout: 3 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	resp, err := client.Get(url)
	if err != nil || resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
	os.Exit(0)
}

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

func stringsEqual(a, b string) bool {
	return stringsToLower(a) == stringsToLower(b)
}
