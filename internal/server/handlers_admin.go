package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"agy-gateway/internal/auth"
	"agy-gateway/internal/config"
	"agy-gateway/internal/models"
)

// AdminHandlers provides endpoints for the headless Admin Control Plane.
type AdminHandlers struct {
	cfg           *config.Config
	authMgr       *auth.Manager
	onboard       *auth.OAuthOnboardingManager
	modelRegistry *models.ModelRegistry
}

// NewAdminHandlers creates an AdminHandlers instance.
func NewAdminHandlers(
	cfg *config.Config,
	authMgr *auth.Manager,
	onboard *auth.OAuthOnboardingManager,
	modelRegistry *models.ModelRegistry,
) *AdminHandlers {
	return &AdminHandlers{
		cfg:           cfg,
		authMgr:       authMgr,
		onboard:       onboard,
		modelRegistry: modelRegistry,
	}
}

// HandleListAccounts serves GET /admin/v1/accounts.
//
// @Summary      List Antigravity accounts
// @Description  Lists all Google Antigravity credential accounts managed by the gateway, including IDs, aliases, emails, subscription tiers, health statuses, and cooldown timestamps.
// @Tags         Admin - Accounts
// @Accept       json
// @Produce      json
// @Security     AdminBearerAuth
// @Success      200  {array}   auth.Account        "List of registered accounts"
// @Failure      401  {object}  AdminErrorResponse  "Unauthorized: missing or invalid Admin Bearer token"
// @Failure      403  {object}  AdminErrorResponse  "Forbidden: Admin API key is not configured"
// @Router       /admin/v1/accounts [get]
func (h *AdminHandlers) HandleListAccounts(w http.ResponseWriter, r *http.Request) {
	_ = h.authMgr.Sync(r.Context())
	accounts := h.authMgr.GetAccounts()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(accounts)
}

// HandleQuota serves GET /admin/v1/accounts/quota.
//
// @Summary      Get live accounts quota
// @Description  Queries live quota statistics directly from Google Antigravity (`agy /usage`) across all accounts or filtered by query. Reports weekly and 5-hour limit percentages, reset countdowns, and exhaustion cooldown flags.
// @Tags         Admin - Accounts
// @Accept       json
// @Produce      json
// @Security     AdminBearerAuth
// @Param        account  query     string                     false  "Filter by specific account ID or alias"
// @Success      200      {object}  auth.AccountsQuotaResponse "Live quota status report for all accounts"
// @Failure      401      {object}  AdminErrorResponse         "Unauthorized: missing or invalid Admin Bearer token"
// @Failure      403      {object}  AdminErrorResponse         "Forbidden: Admin API key is not configured"
// @Failure      404      {object}  AdminErrorResponse         "Specified account was not found"
// @Failure      500      {object}  AdminErrorResponse         "Internal failure polling live quota from agy CLI"
// @Router       /admin/v1/accounts/quota [get]
func (h *AdminHandlers) HandleQuota(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("account")
	if target == "" {
		target = r.URL.Query().Get("query")
	}
	if target == "" {
		target = r.URL.Query().Get("id")
	}

	res, err := h.authMgr.GetQuota(r.Context(), target)
	if err != nil {
		if errors.Is(err, auth.ErrAccountNotFound) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(AdminErrorResponse{Error: "Account not found", Query: target})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(AdminErrorResponse{Error: err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if target != "" && len(res.Accounts) == 1 {
		_ = json.NewEncoder(w).Encode(res.Accounts[0])
		return
	}
	_ = json.NewEncoder(w).Encode(res)
}

// HandleQuotaByID serves GET /admin/v1/accounts/quota/{id}.
//
// @Summary      Get live quota for specific account
// @Description  Queries live quota metrics for a single designated account by its ID, alias, or email address.
// @Tags         Admin - Accounts
// @Accept       json
// @Produce      json
// @Security     AdminBearerAuth
// @Param        id   path      string                 true  "Account ID, alias, or email address"
// @Success      200  {object}  auth.AccountQuotaInfo  "Detailed live quota for the targeted account"
// @Failure      401  {object}  AdminErrorResponse     "Unauthorized: missing or invalid Admin Bearer token"
// @Failure      403  {object}  AdminErrorResponse     "Forbidden: Admin API key is not configured"
// @Failure      404  {object}  AdminErrorResponse     "Account not found"
// @Failure      500  {object}  AdminErrorResponse     "Internal failure querying quota from agy CLI"
// @Router       /admin/v1/accounts/quota/{id} [get]
func (h *AdminHandlers) HandleQuotaByID(w http.ResponseWriter, r *http.Request) {
	target := strings.TrimPrefix(r.URL.Path, "/admin/v1/accounts/quota/")
	target = strings.Trim(target, "/")

	res, err := h.authMgr.GetQuota(r.Context(), target)
	if err != nil {
		if errors.Is(err, auth.ErrAccountNotFound) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(AdminErrorResponse{Error: "Account not found", Query: target})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(AdminErrorResponse{Error: err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if len(res.Accounts) > 0 {
		_ = json.NewEncoder(w).Encode(res.Accounts[0])
		return
	}
	_ = json.NewEncoder(w).Encode(res)
}

// HandleUsage serves GET /admin/v1/accounts/usage.
//
// @Summary      Get raw agy usage JSON
// @Description  Returns the raw, unadulterated JSON envelope directly produced by `agy -p "/usage" --output-format json`.
// @Tags         Admin - Accounts
// @Accept       json
// @Produce      json
// @Security     AdminBearerAuth
// @Success      200  {object}  auth.AgyUsageEnvelope  "Raw agy usage JSON response envelope"
// @Failure      401  {object}  AdminErrorResponse     "Unauthorized: missing or invalid Admin Bearer token"
// @Failure      403  {object}  AdminErrorResponse     "Forbidden: Admin API key is not configured"
// @Failure      500  {object}  AdminErrorResponse     "Failed to execute agy usage command"
// @Router       /admin/v1/accounts/usage [get]
func (h *AdminHandlers) HandleUsage(w http.ResponseWriter, r *http.Request) {
	data, err := h.authMgr.Usage(r.Context())
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(AdminErrorResponse{Error: err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// HandleSwitch serves POST /admin/v1/accounts/switch.
//
// @Summary      Switch active account
// @Description  Manually sets the active default Google Antigravity account used by workers and synchronizes available models.
// @Tags         Admin - Accounts
// @Accept       json
// @Produce      json
// @Security     AdminBearerAuth
// @Param        request  body      SwitchAccountRequest  true  "Target account identifier, email, or alias"
// @Success      200      {object}  StatusResponse        "Account successfully switched"
// @Failure      400      {object}  AdminErrorResponse    "Missing or empty query parameter"
// @Failure      401      {object}  AdminErrorResponse    "Unauthorized: missing or invalid Admin Bearer token"
// @Failure      403      {object}  AdminErrorResponse    "Forbidden: Admin API key is not configured"
// @Failure      500      {object}  AdminErrorResponse    "Failed to switch account in vault"
// @Router       /admin/v1/accounts/switch [post]
func (h *AdminHandlers) HandleSwitch(w http.ResponseWriter, r *http.Request) {
	var body SwitchAccountRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Query == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(AdminErrorResponse{Error: "Missing query parameter"})
		return
	}

	if err := h.authMgr.SwitchAccount(r.Context(), body.Query); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(AdminErrorResponse{Error: err.Error()})
		return
	}
	if h.modelRegistry != nil {
		_ = h.modelRegistry.SyncModels(r.Context())
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(StatusResponse{Status: "switched", Target: body.Query})
}

// HandleRotate serves POST /admin/v1/accounts/rotate.
//
// @Summary      Rotate to next eligible account
// @Description  Manually forces account rotation to the next available healthy account with sufficient quota.
// @Tags         Admin - Accounts
// @Accept       json
// @Produce      json
// @Security     AdminBearerAuth
// @Success      200  {object}  StatusResponse      "Account rotated successfully"
// @Failure      401  {object}  AdminErrorResponse  "Unauthorized: missing or invalid Admin Bearer token"
// @Failure      403  {object}  AdminErrorResponse  "Forbidden: Admin API key is not configured"
// @Failure      500  {object}  AdminErrorResponse  "Failed to rotate account or no eligible accounts"
// @Router       /admin/v1/accounts/rotate [post]
func (h *AdminHandlers) HandleRotate(w http.ResponseWriter, r *http.Request) {
	if err := h.authMgr.RotateAccount(r.Context()); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(AdminErrorResponse{Error: err.Error()})
		return
	}
	if h.modelRegistry != nil {
		_ = h.modelRegistry.SyncModels(r.Context())
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(StatusResponse{Status: "rotated"})
}

// HandleLoginInit serves POST /admin/v1/accounts/login/init.
//
// @Summary      Initialize headless OAuth onboarding
// @Description  Spawns a background `agy-auth login` process on an isolated loopback port and generates a Google OAuth2 consent URL for administrative onboarding.
// @Tags         Admin - OAuth Onboarding
// @Accept       json
// @Produce      json
// @Security     AdminBearerAuth
// @Param        request  body      LoginInitRequest   false  "Optional account alias to configure"
// @Success      201      {object}  LoginInitResponse  "OAuth onboarding flow initialized with Google consent URL"
// @Failure      401      {object}  AdminErrorResponse "Unauthorized: missing or invalid Admin Bearer token"
// @Failure      403      {object}  AdminErrorResponse "Forbidden: Admin API key is not configured"
// @Failure      500      {object}  AdminErrorResponse "Failed to initialize OAuth process"
// @Router       /admin/v1/accounts/login/init [post]
func (h *AdminHandlers) HandleLoginInit(w http.ResponseWriter, r *http.Request) {
	var body LoginInitRequest
	_ = json.NewDecoder(r.Body).Decode(&body)

	loginID, authURL, err := h.onboard.InitLogin(r.Context(), body.Alias)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(AdminErrorResponse{Error: err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(LoginInitResponse{
		LoginID: loginID,
		AuthURL: authURL,
	})
}

// HandleLoginCallback serves POST /admin/v1/accounts/login/callback.
//
// @Summary      Complete OAuth onboarding callback
// @Description  Submits the Google authorization code or full loopback redirect URL captured from the administrator's browser to finalize token exchange and store credentials in the central vault.
// @Tags         Admin - OAuth Onboarding
// @Accept       json
// @Produce      json
// @Security     AdminBearerAuth
// @Param        request  body      LoginCallbackRequest  true  "Login ID and either authorization code or full loopback redirect URL"
// @Success      200      {object}  StatusResponse        "OAuth token exchange completed and account synced"
// @Failure      400      {object}  AdminErrorResponse    "Invalid payload, invalid redirect URL, or token exchange failure"
// @Failure      401      {object}  AdminErrorResponse    "Unauthorized: missing or invalid Admin Bearer token"
// @Failure      403      {object}  AdminErrorResponse    "Forbidden: Admin API key is not configured"
// @Router       /admin/v1/accounts/login/callback [post]
func (h *AdminHandlers) HandleLoginCallback(w http.ResponseWriter, r *http.Request) {
	var body LoginCallbackRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.LoginID == "" || (body.Code == "" && body.RedirectURL == "") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(AdminErrorResponse{Error: "Missing login_id or code/redirect_url"})
		return
	}

	if err := h.onboard.CompleteCallback(r.Context(), body.LoginID, body.Code, body.RedirectURL); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(AdminErrorResponse{Error: err.Error()})
		return
	}

	_ = h.authMgr.Sync(r.Context())
	if h.modelRegistry != nil {
		_ = h.modelRegistry.SyncModels(r.Context())
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(StatusResponse{Status: "completed"})
}

// HandleLoginStatus serves GET /admin/v1/accounts/login/{login_id}.
//
// @Summary      Inspect in-flight OAuth onboarding status
// @Description  Checks whether an ongoing OAuth onboarding session is still waiting for user consent, has completed token exchange, or has timed out/failed.
// @Tags         Admin - OAuth Onboarding
// @Accept       json
// @Produce      json
// @Security     AdminBearerAuth
// @Param        login_id  path      string               true  "Login session identifier"
// @Success      200       {object}  LoginStatusResponse  "Current status of the OAuth onboarding session"
// @Failure      400       {object}  AdminErrorResponse   "Missing login_id in path"
// @Failure      401       {object}  AdminErrorResponse   "Unauthorized: missing or invalid Admin Bearer token"
// @Failure      403       {object}  AdminErrorResponse   "Forbidden: Admin API key is not configured"
// @Failure      404       {object}  AdminErrorResponse   "Session not found or expired"
// @Router       /admin/v1/accounts/login/{login_id} [get]
func (h *AdminHandlers) HandleLoginStatus(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 5 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(AdminErrorResponse{Error: "Missing login_id in path"})
		return
	}
	loginID := parts[4]

	session, exists := h.onboard.GetSession(loginID)
	if !exists {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(AdminErrorResponse{Error: "Session not found"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(session)
}

// HandleVaultStatus serves GET /admin/v1/vault/status.
//
// @Summary      Inspect credential vault status
// @Description  Returns diagnostic health information and status of the central credential vault and stored accounts.
// @Tags         Admin - Vault
// @Accept       json
// @Produce      json
// @Security     AdminBearerAuth
// @Success      200  {object}  map[string]interface{}  "Raw credential vault status details"
// @Failure      401  {object}  AdminErrorResponse      "Unauthorized: missing or invalid Admin Bearer token"
// @Failure      403  {object}  AdminErrorResponse      "Forbidden: Admin API key is not configured"
// @Failure      500  {object}  AdminErrorResponse      "Failed to query vault status"
// @Router       /admin/v1/vault/status [get]
func (h *AdminHandlers) HandleVaultStatus(w http.ResponseWriter, r *http.Request) {
	data, err := h.authMgr.Status(r.Context())
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(AdminErrorResponse{Error: err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// HandleVaultClean serves POST /admin/v1/vault/clean.
//
// @Summary      Clean credential vault
// @Description  Cleans expired, orphaned, or corrupted token caches from the central credential vault. Supports dry-run simulation mode.
// @Tags         Admin - Vault
// @Accept       json
// @Produce      json
// @Security     AdminBearerAuth
// @Param        request  body      VaultCleanRequest       false  "Vault cleaning options (e.g. dry_run)"
// @Success      200      {object}  map[string]interface{}  "Summary of cleaned token files and status"
// @Failure      401      {object}  AdminErrorResponse      "Unauthorized: missing or invalid Admin Bearer token"
// @Failure      403      {object}  AdminErrorResponse      "Forbidden: Admin API key is not configured"
// @Failure      500      {object}  AdminErrorResponse      "Failed to clean vault"
// @Router       /admin/v1/vault/clean [post]
func (h *AdminHandlers) HandleVaultClean(w http.ResponseWriter, r *http.Request) {
	var body VaultCleanRequest
	_ = json.NewDecoder(r.Body).Decode(&body)

	data, err := h.authMgr.Clean(r.Context(), body.DryRun)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(AdminErrorResponse{Error: err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}
