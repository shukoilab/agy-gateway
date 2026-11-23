// Package server provides HTTP routing, OpenAI-compatible handlers,
// admin control plane endpoints, and production middleware.
package server

import (
	"crypto/subtle"
	"net/http"
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
			limit = 10 * 1024 * 1024 // 10 MiB default
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

// ClientAuth verifies the Authorization Bearer token against configured client API keys.
func (m *Middleware) ClientAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// If no client keys are configured, allow access
		if len(m.cfg.Security.APIKeys) == 0 {
			next.ServeHTTP(w, r)
			return
		}

		authHeader := r.Header.Get("Authorization")
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if token == "" || token == authHeader {
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
			http.Error(w, `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error"}}`, http.StatusUnauthorized)
			return
		}

		// Enforce fixed-slot rate limiting if configured
		if matchedKey.RateLimitRPM > 0 && !m.allowRateLimit(matchedKey.ID, matchedKey.RateLimitRPM) {
			http.Error(w, `{"error":{"message":"Rate limit exceeded","type":"rate_limit_error"}}`, http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// AdminAuth verifies the Authorization Bearer token strictly against the Admin API key.
func (m *Middleware) AdminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		adminKey := m.cfg.Security.AdminAPIKeySecret
		if adminKey == "" {
			http.Error(w, `{"error":"Admin API key is not configured"}`, http.StatusForbidden)
			return
		}

		authHeader := r.Header.Get("Authorization")
		token := strings.TrimPrefix(authHeader, "Bearer ")
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
