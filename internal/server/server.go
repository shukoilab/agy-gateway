package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	httpSwagger "github.com/swaggo/http-swagger/v2"

	_ "agy-gateway/docs"
	"agy-gateway/internal/config"
	"agy-gateway/internal/mcp"
)

// Server coordinates the net/http server lifecycle and route registration.
type Server struct {
	httpServer *http.Server
	cfg        *config.Config
	logger     *slog.Logger
}

// NewServer configures routes, middleware, and timeout parameters.
func NewServer(
	cfg *config.Config,
	openaiHandlers *OpenAIHandlers,
	anthropicHandlers *AnthropicHandlers,
	adminHandlers *AdminHandlers,
	opsHandlers *OpsHandlers,
	loopbackServer *mcp.LoopbackServer,
	logger *slog.Logger,
) *Server {
	if logger == nil {
		logger = slog.Default()
	}

	mw := NewMiddleware(cfg)
	mux := http.NewServeMux()

	// 1. Operational Endpoints (Unauthenticated)
	mux.HandleFunc("GET /healthz", opsHandlers.HandleHealthz)
	mux.HandleFunc("GET /readyz", opsHandlers.HandleReadyz)
	mux.HandleFunc("GET /metrics", opsHandlers.HandleMetrics)

	// 1b. Internal Loopback MCP Bridge Endpoint (Localhost JSON-RPC 2.0 router for agy acp stdio bridge)
	if loopbackServer != nil {
		mux.HandleFunc("POST /internal/mcp/", loopbackServer.ServeHTTP)
	}

	// 2. OpenAI Endpoints (Protected by ClientAuth)
	mux.Handle("POST /v1/chat/completions", mw.ClientAuth(http.HandlerFunc(openaiHandlers.HandleChatCompletions)))
	mux.Handle("GET /v1/models", mw.ClientAuth(http.HandlerFunc(openaiHandlers.HandleListModels)))
	mux.Handle("GET /v1/models/", mw.ClientAuth(http.HandlerFunc(openaiHandlers.HandleGetModel)))

	// 3. Anthropic Messages Endpoint (Protected by ClientAuth with dual-header support)
	if anthropicHandlers != nil {
		mux.Handle("POST /v1/messages", mw.ClientAuth(http.HandlerFunc(anthropicHandlers.HandleMessages)))
	}

	// 3. Admin Control Plane Endpoints (Protected by AdminAuth)
	adminPrefix := "/admin/v1/"
	mux.Handle(adminPrefix+"accounts", mw.AdminAuth(http.HandlerFunc(adminHandlers.HandleListAccounts)))
	mux.Handle(adminPrefix+"accounts/quota", mw.AdminAuth(http.HandlerFunc(adminHandlers.HandleQuota)))
	mux.Handle(adminPrefix+"accounts/quota/", mw.AdminAuth(http.HandlerFunc(adminHandlers.HandleQuotaByID)))
	mux.Handle(adminPrefix+"accounts/usage", mw.AdminAuth(http.HandlerFunc(adminHandlers.HandleUsage)))
	mux.Handle(adminPrefix+"accounts/switch", mw.AdminAuth(http.HandlerFunc(adminHandlers.HandleSwitch)))
	mux.Handle(adminPrefix+"accounts/rotate", mw.AdminAuth(http.HandlerFunc(adminHandlers.HandleRotate)))
	mux.Handle(adminPrefix+"accounts/login/init", mw.AdminAuth(http.HandlerFunc(adminHandlers.HandleLoginInit)))
	mux.Handle(adminPrefix+"accounts/login/callback", mw.AdminAuth(http.HandlerFunc(adminHandlers.HandleLoginCallback)))
	mux.Handle(adminPrefix+"accounts/login/", mw.AdminAuth(http.HandlerFunc(adminHandlers.HandleLoginStatus)))
	mux.Handle(adminPrefix+"vault/status", mw.AdminAuth(http.HandlerFunc(adminHandlers.HandleVaultStatus)))
	mux.Handle(adminPrefix+"vault/clean", mw.AdminAuth(http.HandlerFunc(adminHandlers.HandleVaultClean)))

	// Admin - Anthropic Models & Aliases
	mux.Handle("GET "+adminPrefix+"models/anthropic/aliases", mw.AdminAuth(http.HandlerFunc(adminHandlers.HandleListAnthropicAliases)))
	mux.Handle("POST "+adminPrefix+"models/anthropic/aliases", mw.AdminAuth(http.HandlerFunc(adminHandlers.HandleSetAnthropicAlias)))
	mux.Handle("DELETE "+adminPrefix+"models/anthropic/aliases/{alias}", mw.AdminAuth(http.HandlerFunc(adminHandlers.HandleDeleteAnthropicAlias)))
	mux.Handle("DELETE "+adminPrefix+"models/anthropic/aliases/", mw.AdminAuth(http.HandlerFunc(adminHandlers.HandleDeleteAnthropicAlias)))
	mux.Handle("POST "+adminPrefix+"models/anthropic/sync", mw.AdminAuth(http.HandlerFunc(adminHandlers.HandleSyncAnthropicModels)))

	// Admin - Auto-Updater
	mux.Handle("GET "+adminPrefix+"updater/status", mw.AdminAuth(http.HandlerFunc(adminHandlers.HandleGetUpdaterStatus)))
	mux.Handle("POST "+adminPrefix+"updater/check", mw.AdminAuth(http.HandlerFunc(adminHandlers.HandleTriggerUpdateCheck)))

	// 4. OpenAPI / Swagger Documentation UI (Public / Interactive)
	mux.HandleFunc("GET /swagger/", httpSwagger.Handler(
		httpSwagger.URL("/swagger/doc.json"),
		httpSwagger.PersistAuthorization(true),
		httpSwagger.DocExpansion("list"),
		httpSwagger.DeepLinking(true),
	))
	mux.HandleFunc("GET /swagger", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/swagger/index.html", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/swagger/index.html", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /docs/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/swagger/index.html", http.StatusMovedPermanently)
	})

	// Wrap root with Recovery, CORS, MaxBytes, and RequestID middleware
	var rootHandler http.Handler = mux
	rootHandler = mw.MaxBytes(rootHandler)
	rootHandler = mw.CORS(rootHandler)
	rootHandler = mw.RequestID(rootHandler)
	rootHandler = mw.Recovery(rootHandler)

	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	if strings.HasPrefix(addr, ":") {
		addr = "0.0.0.0" + addr
	}

	readHeaderTimeout := cfg.Server.ReadHeaderTimeout
	if readHeaderTimeout <= 0 {
		readHeaderTimeout = 10 * time.Second
	}
	idleTimeout := cfg.Server.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = 120 * time.Second
	}

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           rootHandler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}

	return &Server{
		httpServer: httpSrv,
		cfg:        cfg,
		logger:     logger,
	}
}

// Handler returns the underlying configured root http.Handler (useful for testing and embedding).
func (s *Server) Handler() http.Handler {
	return s.httpServer.Handler
}

// Start launches the HTTP server listening on the configured address.
func (s *Server) Start() error {
	s.logger.Info("agy-gateway HTTP server listening", "addr", s.httpServer.Addr)
	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server error: %w", err)
	}
	return nil
}

// Shutdown gracefully drains active connections within the configured timeout.
func (s *Server) Shutdown(ctx context.Context) error {
	drainTimeout := s.cfg.Server.ShutdownDrainTimeout
	if drainTimeout <= 0 {
		drainTimeout = 30 * time.Second
	}

	drainCtx, cancel := context.WithTimeout(ctx, drainTimeout)
	defer cancel()

	s.logger.Info("shutting down HTTP server gracefully", "timeout", drainTimeout)
	return s.httpServer.Shutdown(drainCtx)
}
