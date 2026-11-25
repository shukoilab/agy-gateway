package server

import (
	"time"

	"agy-gateway/internal/models"
)

// SwitchAccountRequest defines the payload for switching the default active Antigravity account.
//
//	@purpose	Encapsulates the administrative target identifier for manual account switching.
//	@rationale	Strongly typed request model enabling strict payload deserialization and OpenAPI schema generation.
//	@invariants	Query field must not be empty.
type SwitchAccountRequest struct {
	// Query matches the account ID, email address, or assigned alias to activate
	Query string `json:"query" example:"work-account"`
}

// StatusResponse represents an administrative or operational confirmation payload.
//
//	@purpose	Standardized JSON response envelope for idempotent state change operations.
//	@rationale	Provides uniform machine-parseable confirmations across switch, rotate, and callback endpoints.
type StatusResponse struct {
	// Status outcome description (e.g. "switched", "rotated", "completed", "healthy")
	Status string `json:"status" example:"switched"`
	// Target account or entity impacted by the operation, if applicable
	Target string `json:"target,omitempty" example:"work-account"`
}

// LoginInitRequest defines input parameters to initialize a headless OAuth onboarding flow.
//
//	@purpose	Carries optional administrator-specified configuration for new Google accounts.
type LoginInitRequest struct {
	// Optional alias to assign to the newly authenticated account upon successful onboarding
	Alias string `json:"alias,omitempty" example:"secondary-account"`
}

// LoginInitResponse represents the result of initializing an OAuth onboarding session.
//
//	@purpose	Delivers the tracking login_id and the Google OAuth2 consent URL.
//	@rationale	Decouples credential initialization from browser interactions, supporting headless and remote setups.
type LoginInitResponse struct {
	// Unique tracking identifier for this in-flight login session
	LoginID string `json:"login_id" example:"login-b7c4a1"`
	// Google OAuth2 authorization URL that the administrator must open in a browser
	AuthURL string `json:"auth_url" example:"https://accounts.google.com/o/oauth2/auth?client_id=example.apps.googleusercontent.com&redirect_uri=http%3A%2F%2Flocalhost%3A43125&response_type=code&scope=email%20profile"`
}

// LoginCallbackRequest represents the payload to finalize an in-flight OAuth flow.
//
//	@purpose	Transfers the authorization code or loopback redirect URI to complete token exchange.
//	@rationale	Accommodates both direct authorization code pasting and full loopback URL replay.
//	@invariants	Either Code or RedirectURL must be supplied alongside LoginID.
type LoginCallbackRequest struct {
	// Unique login session ID returned by /admin/v1/accounts/login/init
	LoginID string `json:"login_id" example:"login-b7c4a1"`
	// Authorization code extracted from Google's redirect query string (e.g. "4/0AY0e-g...")
	Code string `json:"code,omitempty" example:"4/0AY0e-g7X1234567890"`
	// Full loopback redirect URL captured from the browser address bar
	RedirectURL string `json:"redirect_url,omitempty" example:"http://localhost:43125/?code=4/0AY0e-g7X1234567890&scope=email"`
}

// LoginStatusResponse reports the state of an in-flight OAuth onboarding session.
//
//	@purpose	Exposes session lifecycle flags to polling clients or administrators.
type LoginStatusResponse struct {
	// Unique login session ID
	ID string `json:"id" example:"login-b7c4a1"`
	// Optional account alias requested during initialization
	Alias string `json:"alias,omitempty" example:"secondary-account"`
	// Google OAuth2 authorization URL
	AuthURL string `json:"auth_url" example:"https://accounts.google.com/o/oauth2/auth?..."`
	// Ephemeral loopback port pinned for the callback redirect listener
	PinnedPort string `json:"pinned_port,omitempty" example:"43125"`
	// Timestamp when the onboarding session was initialized
	CreatedAt time.Time `json:"created_at" example:"2026-11-24T10:00:00Z"`
	// True if token exchange succeeded and account credentials were saved to the vault
	Completed bool `json:"completed" example:"true"`
	// True if the onboarding process failed or was terminated
	Failed bool `json:"failed" example:"false"`
}

// VaultCleanRequest configures parameters for credential vault cleaning.
//
//	@purpose	Controls whether orphaned or invalid token cleanup is simulated or committed.
type VaultCleanRequest struct {
	// When true, simulates vault cleaning without deleting files from disk
	DryRun bool `json:"dry_run" example:"false"`
}

// OpsHealthResponse represents the operational status of liveness and readiness probes.
//
//	@purpose	Standardized operational probe response schema for orchestrators.
type OpsHealthResponse struct {
	// Status health indicator ("healthy" or "ready")
	Status string `json:"status" example:"healthy"`
}

// AdminErrorResponse describes an administrative control plane error.
//
//	@purpose	Uniform error payload for unauthorized, forbidden, or failed administrative calls.
type AdminErrorResponse struct {
	// Human-readable error message explaining the failure condition
	Error string `json:"error" example:"Account not found"`
	// Optional query parameter or account identifier that caused the error
	Query string `json:"query,omitempty" example:"work-account"`
}

// SetModelAliasRequest defines the payload for setting or updating a manual Anthropic model alias.
//
//	@purpose	Encapsulates the target model mapping and optional human description.
type SetModelAliasRequest struct {
	// Canonical alias or requested model identifier (e.g. "claude-3-7-sonnet")
	Alias string `json:"alias" example:"claude-3-7-sonnet"`
	// Target model discovered from accounts to route requests to (e.g. "claude-sonnet-4-6")
	Target string `json:"target" example:"claude-sonnet-4-6"`
	// Optional description or rationale for this manual mapping override
	Description string `json:"description,omitempty" example:"Manual override pinned to Sonnet 4.6"`
}

// AnthropicAliasesResponse describes the response listing all active Anthropic model aliases and champions.
//
//	@purpose	Reports manual overrides, dynamic synthesized aliases, and family champions.
type AnthropicAliasesResponse struct {
	// Total count of active model aliases
	Total int `json:"total" example:"12"`
	// Elected family champions mapped to discovered models
	Champions map[string]string `json:"champions"`
	// Active model aliases sorted with manual overrides first
	Aliases []models.ModelAliasEntry `json:"aliases"`
}

// ModelSyncResponse reports the result of triggering a dynamic model discovery and alias sync.
//
//	@purpose	Summarizes discovered model counts, elected champions, and synthesized aliases.
type ModelSyncResponse struct {
	// Status outcome indicator (e.g. "synchronized")
	Status string `json:"status" example:"synchronized"`
	// Number of unique models discovered across accounts
	DiscoveredModels int `json:"discovered_models" example:"6"`
	// Total count of active aliases (manual + dynamic)
	TotalAliases int `json:"total_aliases" example:"12"`
	// Elected family champions
	Champions map[string]string `json:"champions"`
}

// OpsReadinessResponse reports worker pool availability.
//
// @purpose Gives orchestrators a truthful readiness signal.
// @rationale A static readiness string hides a pool that has no warm workers.
type OpsReadinessResponse struct {
	// Status readiness indicator ("ready" or "not_ready")
	Status string `json:"status" example:"ready"`
	// WorkersTotal configured worker count
	WorkersTotal int `json:"workers_total" example:"2"`
	// WorkersAvailable workers currently queued for lease
	WorkersAvailable int `json:"workers_available" example:"2"`
}
