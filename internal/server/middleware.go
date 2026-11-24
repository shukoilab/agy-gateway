// Package server provides HTTP routing, OpenAI-compatible handlers,
// admin control plane endpoints, and production middleware.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"agy-gateway/internal/config"
)

// Middleware holds dependencies for HTTP security and rate limiting.
type Middleware struct {
	cfg          *config.Config
	limiterMu    sync.Mutex
	clientLimits map[string]*clientSlotLimiter
}

type clientSlotLimiter struct {
	tokens    int
	lastCheck time.Time
	maxRPM    int
}

// NewMiddleware creates an initialized Middleware suite.
func NewMiddleware(cfg *config.Config) *Middleware {
	return &Middleware{
		cfg:          cfg,
		clientLimits: make(map[string]*clientSlotLimiter),
	}
}

// RequestID injects a unique X-Request-ID header into every incoming request.
func (m *Middleware) RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" {
			reqID = uuid.New().String()
		}
		w.Header().Set("X-Request-ID", reqID)
		next.ServeHTTP(w, r)
	})
}

// MaxBytes wraps the request body with a size limiter to prevent unbounded memory consumption.
func (m *Middleware) MaxBytes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := m.cfg.Server.MaxRequestBodyBytes
		if limit <= 0 {
			limit = 100 * 1024 * 1024 // 100 MiB default (1M+ context support)
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

// CORS enforces allowed origins and intercepts HTTP OPTIONS preflight queries.
//
// @purpose Provides enterprise-grade Cross-Origin Resource Sharing (CORS) header injection and preflight negotiation.
// @rationale Enables web-based dashboards, Swagger UI, and third-party frontend apps to securely communicate with the gateway API.
// @preconditions m.cfg must be non-nil; r must be a valid HTTP request pointer.
// - @param: next Downstream HTTP handler to execute if request is not an OPTIONS preflight.
// @returns An http.Handler wrapping next with CORS headers and OPTIONS short-circuiting.
// @throws None.
// @invariants O(K) lookup where K is count of allowed CORS origins. Safe for concurrent request streams.
func (m *Middleware) CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowedOrigin := ""

		if len(m.cfg.Security.AllowedCORSOrigins) == 0 {
			allowedOrigin = "*"
		} else {
			for _, o := range m.cfg.Security.AllowedCORSOrigins {
				if o == "*" || o == origin {
					allowedOrigin = origin
					if o == "*" && origin == "" {
						allowedOrigin = "*"
					}
					break
				}
			}
		}

		if allowedOrigin != "" {
			w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PATCH, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, X-Request-ID, User-Agent, x-api-key, anthropic-version, anthropic-beta")
			w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID, Content-Length")
			w.Header().Set("Access-Control-Max-Age", "86400")
			if allowedOrigin != "*" {
				w.Header().Set("Vary", "Origin")
			}
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// Recovery catches downstream panics, logs stack traces, and returns a safe 500 error.
//
// @purpose Prevents unexpected handler panics from crashing the HTTP server process or orphaning connections.
// @rationale Enforces fault tolerance and guarantees that the client always receives a structured, compliant JSON response on internal errors.
// @preconditions next must be a non-nil http.Handler.
// - @param: next Downstream HTTP handler whose execution will be guarded by defer/recover.
// @returns An http.Handler wrapped with structured recovery handling.
// @throws None (intercepts all panics gracefully).
// @invariants Panic stack traces are emitted to structured logs; returns HTTP 500 with OpenAI-compatible JSON schema.
func (m *Middleware) Recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				stack := debug.Stack()
				slog.Error("panic intercepted by recovery middleware", "panic", rec, "stack", string(stack))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]string{
						"message": fmt.Sprintf("Internal gateway panic: %v", rec),
						"type":    "internal_server_error",
					},
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// extractBearerOrRawToken extracts the API token from an Authorization header.
// It gracefully supports "Bearer <token>", "bearer <token>", and direct raw tokens "<token>".
//
// @purpose Normalizes Authorization headers across HTTP clients, OpenAPI/Swagger UI, and CLI tools.
// @rationale Interactive tools such as Swagger UI frequently populate the Authorization header
// with the raw token value directly when users paste their API key into the authorization modal.
// Accepting both standard Bearer scheme and raw tokens ensures seamless developer UX while maintaining
// strict cryptographic constant-time comparison against configured secrets.
// @preconditions None.
// - @param: authHeader Raw Authorization header value.
// @returns Normalized token string with surrounding whitespace and any leading Bearer prefix stripped.
// @invariants Memory safe, O(N) where N is header length, thread safe.
func extractBearerOrRawToken(authHeader string) string {
	authHeader = strings.TrimSpace(authHeader)
	if authHeader == "" {
		return ""
	}
	if len(authHeader) >= 7 && strings.EqualFold(authHeader[:7], "bearer ") {
		return strings.TrimSpace(authHeader[7:])
	}
	return authHeader
}

// ClientAuth verifies the client API key against configured security credentials.
// Supports both Anthropic (x-api-key header) and OpenAI (Authorization: Bearer header or raw token) conventions.
func (m *Middleware) ClientAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// If no client keys are configured, allow access
		if len(m.cfg.Security.APIKeys) == 0 {
			next.ServeHTTP(w, r)
			return
		}

		isAnthropic := strings.HasPrefix(r.URL.Path, "/v1/messages")

		token := strings.TrimSpace(r.Header.Get("x-api-key"))
		if token == "" {
			token = extractBearerOrRawToken(r.Header.Get("Authorization"))
		}

		if token == "" {
			if isAnthropic {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("anthropic-version", "2023-06-01")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"Missing or invalid x-api-key or Bearer token"}}`))
				return
			}
			http.Error(w, `{"error":{"message":"Missing or invalid Bearer token","type":"invalid_request_error"}}`, http.StatusUnauthorized)
			return
		}

		var matchedKey *config.APIKeyConfig
		for _, key := range m.cfg.Security.APIKeys {
			secret := key.KeySecret
			if secret != "" && subtle.ConstantTimeCompare([]byte(token), []byte(secret)) == 1 {
				matchedKey = &key
				break
			}
		}

		if matchedKey == nil {
			if isAnthropic {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("anthropic-version", "2023-06-01")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"Incorrect API key provided"}}`))
				return
			}
			http.Error(w, `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error"}}`, http.StatusUnauthorized)
			return
		}

		// Enforce fixed-slot rate limiting if configured
		if matchedKey.RateLimitRPM > 0 && !m.allowRateLimit(matchedKey.ID, matchedKey.RateLimitRPM) {
			if isAnthropic {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("anthropic-version", "2023-06-01")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Rate limit exceeded"}}`))
				return
			}
			http.Error(w, `{"error":{"message":"Rate limit exceeded","type":"rate_limit_error"}}`, http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// AdminAuth verifies the Authorization Bearer token or raw key strictly against the Admin API key.
func (m *Middleware) AdminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		adminKey := m.cfg.Security.AdminAPIKeySecret
		if adminKey == "" {
			http.Error(w, `{"error":"Admin API key is not configured"}`, http.StatusForbidden)
			return
		}

		token := extractBearerOrRawToken(r.Header.Get("Authorization"))
		if token == "" {
			token = strings.TrimSpace(r.Header.Get("x-api-key"))
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(adminKey)) != 1 {
			http.Error(w, `{"error":"Unauthorized admin access"}`, http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (m *Middleware) allowRateLimit(keyID string, rpm int) bool {
	m.limiterMu.Lock()
	defer m.limiterMu.Unlock()

	limiter, exists := m.clientLimits[keyID]
	now := time.Now()
	if !exists {
		limiter = &clientSlotLimiter{
			tokens:    rpm,
			lastCheck: now,
			maxRPM:    rpm,
		}
		m.clientLimits[keyID] = limiter
	}

	elapsed := now.Sub(limiter.lastCheck)
	limiter.lastCheck = now
	limiter.tokens += int(elapsed.Minutes() * float64(limiter.maxRPM))
	if limiter.tokens > limiter.maxRPM {
		limiter.tokens = limiter.maxRPM
	}

	if limiter.tokens > 0 {
		limiter.tokens--
		return true
	}
	return false
}
