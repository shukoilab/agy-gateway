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
func (h *AdminHandlers) HandleListAccounts(w http.ResponseWriter, r *http.Request) {
	_ = h.authMgr.Sync(r.Context())
	accounts := h.authMgr.GetAccounts()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(accounts)
}

// HandleQuota serves GET /admin/v1/accounts/quota and GET /admin/v1/accounts/quota/{id}.
func (h *AdminHandlers) HandleQuota(w http.ResponseWriter, r *http.Request) {
	// Extract target from path /admin/v1/accounts/quota/{id} or query parameters
	target := ""
	path := strings.TrimPrefix(r.URL.Path, "/admin/v1/accounts/quota")
	path = strings.Trim(path, "/")
	if path != "" {
		target = path
	}
	if target == "" {
		target = r.URL.Query().Get("account")
	}
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
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "Account not found", "query": target})
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if target != "" && len(res.Accounts) == 1 {
		_ = json.NewEncoder(w).Encode(res.Accounts[0])
		return
	}
	_ = json.NewEncoder(w).Encode(res)
}

// HandleUsage serves GET /admin/v1/accounts/usage.
func (h *AdminHandlers) HandleUsage(w http.ResponseWriter, r *http.Request) {
	data, err := h.authMgr.Usage(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// HandleSwitch serves POST /admin/v1/accounts/switch.
func (h *AdminHandlers) HandleSwitch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query string `json:"query"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Query == "" {
		http.Error(w, `{"error":"Missing query parameter"}`, http.StatusBadRequest)
		return
	}

	if err := h.authMgr.SwitchAccount(r.Context(), body.Query); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if h.modelRegistry != nil {
		_ = h.modelRegistry.SyncModels(r.Context())
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "switched", "target": body.Query})
}

// HandleRotate serves POST /admin/v1/accounts/rotate.
func (h *AdminHandlers) HandleRotate(w http.ResponseWriter, r *http.Request) {
	if err := h.authMgr.RotateAccount(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if h.modelRegistry != nil {
		_ = h.modelRegistry.SyncModels(r.Context())
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "rotated"})
}

// HandleLoginInit serves POST /admin/v1/accounts/login/init.
func (h *AdminHandlers) HandleLoginInit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Alias string `json:"alias"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	loginID, authURL, err := h.onboard.InitLogin(r.Context(), body.Alias)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"login_id": loginID,
		"auth_url": authURL,
	})
}

// HandleLoginCallback serves POST /admin/v1/accounts/login/callback.
func (h *AdminHandlers) HandleLoginCallback(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LoginID     string `json:"login_id"`
		Code        string `json:"code"`
		RedirectURL string `json:"redirect_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.LoginID == "" || (body.Code == "" && body.RedirectURL == "") {
		http.Error(w, `{"error":"Missing login_id or code/redirect_url"}`, http.StatusBadRequest)
		return
	}

	if err := h.onboard.CompleteCallback(r.Context(), body.LoginID, body.Code, body.RedirectURL); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	_ = h.authMgr.Sync(r.Context())
	if h.modelRegistry != nil {
		_ = h.modelRegistry.SyncModels(r.Context())
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "completed"})
}

// HandleLoginStatus serves GET /admin/v1/accounts/login/{login_id}.
func (h *AdminHandlers) HandleLoginStatus(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 5 {
		http.Error(w, `{"error":"Missing login_id in path"}`, http.StatusBadRequest)
		return
	}
	loginID := parts[4]

	session, exists := h.onboard.GetSession(loginID)
	if !exists {
		http.Error(w, `{"error":"Session not found"}`, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(session)
}

// HandleVaultStatus serves GET /admin/v1/vault/status.
func (h *AdminHandlers) HandleVaultStatus(w http.ResponseWriter, r *http.Request) {
	data, err := h.authMgr.Status(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// HandleVaultClean serves POST /admin/v1/vault/clean.
func (h *AdminHandlers) HandleVaultClean(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DryRun bool `json:"dry_run"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	data, err := h.authMgr.Clean(r.Context(), body.DryRun)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}
