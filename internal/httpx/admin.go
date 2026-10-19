package httpx

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/store/sqlite"
)

// adminAPIKeyCreateRequest is the admin key payload. The allowed_* fields
// carry grant item lists — for allowed_endpoints the items are INFERENCE
// ENDPOINT IDs (enforced at dispatch by auth.EndpointAllowed), never URL
// path patterns; HTTP route permission comes from Scopes only. A nil
// allowlist field encodes the unrestricted {"mode":"all"} grant; an
// explicit empty list denies everything.
type adminAPIKeyCreateRequest struct {
	Name                    string   `json:"name"`
	Description             string   `json:"description,omitempty"` // unused on the postgres row; accepted for compatibility
	OwnerUserID             string   `json:"owner_user_id,omitempty"`
	ServiceAccountID        string   `json:"service_account_id,omitempty"`
	Scopes                  []string `json:"scopes"`
	AllowedModels           []string `json:"allowed_models,omitempty"`
	AllowedEndpoints        []string `json:"allowed_endpoints,omitempty"`
	AllowedExtractTemplates []string `json:"allowed_extract_templates,omitempty"`
	AllowedChatbots         []string `json:"allowed_chatbots,omitempty"`
	AllowedAgents           []string `json:"allowed_agents,omitempty"`
	AllowedIPs              []string `json:"allowed_ips,omitempty"`
	RateLimitPerMinute      *int     `json:"rate_limit_per_minute,omitempty"`
	RateLimitPerHour        *int     `json:"rate_limit_per_hour,omitempty"`
	RateLimitPerDay         *int     `json:"rate_limit_per_day,omitempty"`
	IsUnlimited             *bool    `json:"is_unlimited,omitempty"`        // legacy no-op; billing budgets replaced it
	BudgetLimitTokens       *int64   `json:"budget_limit_tokens,omitempty"` // legacy no-op; billing budgets replaced it
	BudgetLimitCents        *int64   `json:"budget_limit_cents,omitempty"`  // legacy no-op; billing budgets replaced it
	BudgetPeriod            string   `json:"budget_period,omitempty"`       // legacy no-op
	Tags                    []string `json:"tags,omitempty"`                // unused on the postgres row; accepted for compatibility
	ExpiresAt               *string  `json:"expires_at,omitempty"`          // RFC3339; null or "" clears expiry
	IsActive                *bool    `json:"is_active,omitempty"`
}

type adminAPIKeyCreateResponse struct {
	APIKey    any    `json:"api_key"`
	SecretKey string `json:"secret_key"`
}

func (a *App) handleAdminAPIKeys(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	setNoStoreHeaders(w)
	if a.identity == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "admin api key management not configured", "server_error", "not_configured")
		return
	}
	switch r.Method {
	case http.MethodGet:
		// The postgres identity store has no soft-delete column; revoked keys
		// simply list with is_active=false.
		keys, err := a.identity.ListAPIKeys(r.Context(), principal.TenantID)
		if err != nil {
			log.Printf("failed to list api keys: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "api_keys_list_failed")
			return
		}
		for i := range keys {
			keys[i] = redactAdminAPIKey(keys[i])
		}
		WriteJSON(w, http.StatusOK, map[string]any{"api_keys": keys})
	case http.MethodPost:
		var req adminAPIKeyCreateRequest
		if err := DecodeJSON(r, &req); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
			return
		}
		if err := sanitizeAdminAPIKeyRequest(&req, true); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_request")
			return
		}
		if req.Name == "" {
			WriteOpenAIError(w, http.StatusBadRequest, "name is required", "invalid_request_error", "missing_name")
			return
		}
		record, err := a.apiKeyRecordFromAdminRequest(r.Context(), principal, req)
		if err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_request")
			return
		}
		plaintext, prefix, hash, usedAlgo, err := a.generateKey(a.apiKeyPrefix, a.apiKeyAlgo, a.apiKeyPepper)
		if err != nil {
			log.Printf("failed to generate api key: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "key_generate_failed")
			return
		}
		record.KeyPrefix = prefix
		record.KeyHash = hash
		record.KeyHashAlgo = usedAlgo
		if err := a.identity.CreateAPIKey(r.Context(), principal.TenantID, record); err != nil {
			log.Printf("failed to create api key: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "api_key_create_failed")
			return
		}

		created, err := a.identity.GetAPIKey(r.Context(), principal.TenantID, record.ID)
		if err != nil {
			log.Printf("failed to fetch created api key: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "api_key_fetch_failed")
			return
		}
		a.recordAPIKeyBillingAudit(r.Context(), "api_key.created", auth.LegacyAPIKeyID(principal.TenantID, record.ID), principal, adminBudgetMetadata(created))
		created = redactAdminAPIKey(created)
		WriteJSON(w, http.StatusCreated, adminAPIKeyCreateResponse{
			APIKey:    created,
			SecretKey: plaintext,
		})
	default:
		WriteMethodNotAllowed(w)
	}
}

func (a *App) handleAdminAPIKeyByID(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	setNoStoreHeaders(w)
	if a.identity == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "admin api key management not configured", "server_error", "not_configured")
		return
	}
	raw := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/api-keys/")
	if raw == "" {
		WriteOpenAIError(w, http.StatusBadRequest, "api key id is required", "invalid_request_error", "missing_id")
		return
	}

	// Sub-actions
	if strings.HasSuffix(raw, "/regenerate") {
		id := strings.TrimSuffix(strings.TrimSuffix(raw, "/regenerate"), "/")
		if id == "" {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid api key id", "invalid_request_error", "invalid_id")
			return
		}
		if r.Method != http.MethodPost {
			WriteMethodNotAllowed(w)
			return
		}
		key, err := a.identity.GetAPIKey(r.Context(), principal.TenantID, id)
		if err != nil {
			a.writeKeyFetchError(w, err)
			return
		}
		plaintext, prefix, hash, usedAlgo, err := a.generateKey(a.apiKeyPrefix, a.apiKeyAlgo, a.apiKeyPepper)
		if err != nil {
			log.Printf("failed to generate key for regeneration: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "key_generate_failed")
			return
		}
		if err := a.identity.SetAPIKeySecret(r.Context(), principal.TenantID, id, prefix, hash, usedAlgo); err != nil {
			log.Printf("failed to regenerate api key secret: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "api_key_regenerate_failed")
			return
		}
		// A regenerated secret is usable immediately: reactivating clears any
		// revocation, matching the legacy SetSecret+ActivateKey pair.
		if !key.IsActive {
			key.IsActive = true
			if err := a.identity.UpdateAPIKey(r.Context(), principal.TenantID, key); err != nil {
				log.Printf("failed to reactivate api key after regeneration: %v", err)
				WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "api_key_activate_failed")
				return
			}
		}
		key, err = a.identity.GetAPIKey(r.Context(), principal.TenantID, id)
		if err != nil {
			a.writeKeyFetchError(w, err)
			return
		}
		a.recordAPIKeyBillingAudit(r.Context(), "api_key.secret_regenerated", auth.LegacyAPIKeyID(principal.TenantID, id), principal, nil)
		key = redactAdminAPIKey(key)
		WriteJSON(w, http.StatusOK, adminAPIKeyCreateResponse{APIKey: key, SecretKey: plaintext})
		return
	}

	if strings.HasSuffix(raw, "/delete") {
		id := strings.TrimSuffix(strings.TrimSuffix(raw, "/delete"), "/")
		if id == "" {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid api key id", "invalid_request_error", "invalid_id")
			return
		}
		if r.Method != http.MethodPost && r.Method != http.MethodDelete {
			WriteMethodNotAllowed(w)
			return
		}
		if err := a.identity.RevokeAPIKey(r.Context(), principal.TenantID, id, time.Now().UTC()); err != nil {
			log.Printf("failed to delete api key: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "api_key_delete_failed")
			return
		}
		a.recordAPIKeyBillingAudit(r.Context(), "api_key.deleted", auth.LegacyAPIKeyID(principal.TenantID, id), principal, nil)
		WriteJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
		return
	}

	if strings.HasSuffix(raw, "/restore") {
		id := strings.TrimSuffix(strings.TrimSuffix(raw, "/restore"), "/")
		if id == "" {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid api key id", "invalid_request_error", "invalid_id")
			return
		}
		if r.Method != http.MethodPost {
			WriteMethodNotAllowed(w)
			return
		}
		key, err := a.identity.GetAPIKey(r.Context(), principal.TenantID, id)
		if err != nil {
			a.writeKeyFetchError(w, err)
			return
		}
		key.IsActive = true
		key.RevokedAt = nil
		if err := a.identity.UpdateAPIKey(r.Context(), principal.TenantID, key); err != nil {
			log.Printf("failed to restore api key: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "api_key_restore_failed")
			return
		}
		a.recordAPIKeyBillingAudit(r.Context(), "api_key.restored", auth.LegacyAPIKeyID(principal.TenantID, id), principal, nil)
		WriteJSON(w, http.StatusOK, map[string]any{"restored": true, "id": id})
		return
	}

	id := strings.TrimSuffix(raw, "/")
	if id == "" {
		WriteOpenAIError(w, http.StatusBadRequest, "invalid api key id", "invalid_request_error", "invalid_id")
		return
	}

	switch r.Method {
	case http.MethodGet:
		key, err := a.identity.GetAPIKey(r.Context(), principal.TenantID, id)
		if err != nil {
			a.writeKeyFetchError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, redactAdminAPIKey(key))
	case http.MethodPut:
		key, err := a.identity.GetAPIKey(r.Context(), principal.TenantID, id)
		if err != nil {
			a.writeKeyFetchError(w, err)
			return
		}
		var req adminAPIKeyCreateRequest
		if err := DecodeJSON(r, &req); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
			return
		}
		if err := sanitizeAdminAPIKeyRequest(&req, false); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_request")
			return
		}
		applyAdminPatch(&key, req)
		if err := a.identity.UpdateAPIKey(r.Context(), principal.TenantID, key); err != nil {
			log.Printf("failed to update api key: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "api_key_update_failed")
			return
		}
		updated, err := a.identity.GetAPIKey(r.Context(), principal.TenantID, id)
		if err != nil {
			a.writeKeyFetchError(w, err)
			return
		}
		a.recordAPIKeyBillingAudit(r.Context(), "api_key.updated", auth.LegacyAPIKeyID(principal.TenantID, id), principal, adminBudgetMetadata(updated))
		WriteJSON(w, http.StatusOK, redactAdminAPIKey(updated))
	case http.MethodDelete:
		if err := a.identity.RevokeAPIKey(r.Context(), principal.TenantID, id, time.Now().UTC()); err != nil {
			log.Printf("failed to delete api key: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "api_key_delete_failed")
			return
		}
		a.recordAPIKeyBillingAudit(r.Context(), "api_key.deleted", auth.LegacyAPIKeyID(principal.TenantID, id), principal, nil)
		WriteJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
	default:
		WriteMethodNotAllowed(w)
	}
}

// writeKeyFetchError maps a GetAPIKey failure: auth.ErrNotFound is a 404,
// anything else is a store error.
func (a *App) writeKeyFetchError(w http.ResponseWriter, err error) {
	if errors.Is(err, auth.ErrNotFound) {
		WriteOpenAIError(w, http.StatusNotFound, "api key not found", "invalid_request_error", "not_found")
		return
	}
	log.Printf("failed to fetch api key: %v", err)
	WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "api_key_fetch_failed")
}

func (a *App) handleAdminUsageSummary(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	setNoStoreHeaders(w)
	if a.identity == nil || a.usageRepo == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "usage tracking not configured", "server_error", "not_configured")
		return
	}
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	since := time.Now().UTC().AddDate(0, 0, -30)
	usage, err := a.usageRepo.SumAll(r.Context(), since)
	if err != nil {
		log.Printf("failed to fetch usage summary: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "usage_summary_failed")
		return
	}
	var totalTokens int64
	for _, u := range usage {
		totalTokens += u.TotalTokens
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"usage":        usage,
		"total_tokens": totalTokens,
		"period_days":  30,
	})
}

func setNoStoreHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, no-cache, max-age=0, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}

func (a *App) generateKey(prefix, algo, pepper string) (plaintext string, storagePrefix string, hash string, usedAlgo string, err error) {
	// 32 bytes -> 43 chars base64url without padding
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", "", "", err
	}
	secret := base64.RawURLEncoding.EncodeToString(buf)
	plaintext = prefix + secret
	storagePrefix, hash, usedAlgo, err = auth.HashForStorage(plaintext, algo, pepper)
	return plaintext, storagePrefix, hash, usedAlgo, err
}

func sanitizeAdminAPIKeyRequest(req *adminAPIKeyCreateRequest, requireName bool) error {
	req.Name = strings.TrimSpace(req.Name)
	req.Description = strings.TrimSpace(req.Description)
	req.BudgetPeriod = strings.TrimSpace(req.BudgetPeriod)
	if req.ExpiresAt != nil {
		v := strings.TrimSpace(*req.ExpiresAt)
		req.ExpiresAt = &v
	}

	if requireName && req.Name == "" {
		return errors.New("name is required")
	}
	if req.OwnerUserID != "" && req.ServiceAccountID != "" {
		return errors.New("only one of owner_user_id and service_account_id may be set")
	}
	if req.RateLimitPerMinute != nil && *req.RateLimitPerMinute <= 0 {
		return errors.New("rate_limit_per_minute must be a positive integer")
	}
	if req.RateLimitPerHour != nil && *req.RateLimitPerHour <= 0 {
		return errors.New("rate_limit_per_hour must be a positive integer")
	}
	if req.RateLimitPerDay != nil && *req.RateLimitPerDay <= 0 {
		return errors.New("rate_limit_per_day must be a positive integer")
	}
	if req.BudgetLimitTokens != nil && *req.BudgetLimitTokens < 0 {
		return errors.New("budget_limit_tokens must be zero or greater")
	}
	if req.BudgetLimitCents != nil && *req.BudgetLimitCents < 0 {
		return errors.New("budget_limit_cents must be zero or greater")
	}
	if req.BudgetPeriod != "" && req.BudgetPeriod != "total" && req.BudgetPeriod != "monthly" {
		return errors.New("budget_period must be empty, 'total', or 'monthly'")
	}
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		if _, parseErr := time.Parse(time.RFC3339, *req.ExpiresAt); parseErr != nil {
			return errors.New("expires_at must be RFC3339")
		}
	}
	return nil
}

func redactAdminAPIKey(k auth.APIKeyRecord) auth.APIKeyRecord {
	k.KeyHash = ""
	k.KeyHashAlgo = ""
	return k
}

// adminGrantJSON marshals an admin-supplied item list into the stored grant
// document shape. A nil list (absent field) and the legacy "*" item both
// encode {"mode":"all"}; anything else encodes {"mode":"list","items":[...]}.
func adminGrantJSON(items []string) json.RawMessage {
	if items == nil {
		return json.RawMessage(`{"mode":"all"}`)
	}
	for _, item := range items {
		if strings.TrimSpace(item) == "*" {
			return json.RawMessage(`{"mode":"all"}`)
		}
	}
	raw, err := json.Marshal(struct {
		Mode  string   `json:"mode"`
		Items []string `json:"items"`
	}{Mode: "list", Items: items})
	if err != nil {
		return json.RawMessage(`{"mode":"list","items":[]}`)
	}
	return raw
}

// apiKeyRecordFromAdminRequest builds the stored row for a create call. The
// owner defaults to the calling user when neither owner field is supplied —
// the schema requires exactly one owner, never a global key.
func (a *App) apiKeyRecordFromAdminRequest(ctx context.Context, principal auth.Principal, req adminAPIKeyCreateRequest) (auth.APIKeyRecord, error) {
	ownerUserID := strings.TrimSpace(req.OwnerUserID)
	serviceAccountID := strings.TrimSpace(req.ServiceAccountID)
	if ownerUserID == "" && serviceAccountID == "" {
		ownerUserID = principal.UserID
	}
	if ownerUserID == "" && serviceAccountID == "" {
		return auth.APIKeyRecord{}, errors.New("owner_user_id or service_account_id is required")
	}
	if ownerUserID != "" {
		if _, err := a.identity.GetUser(ctx, principal.TenantID, ownerUserID); err != nil {
			return auth.APIKeyRecord{}, errors.New("owner_user_id does not reference an existing user")
		}
	}
	if serviceAccountID != "" {
		if _, err := a.identity.GetServiceAccount(ctx, principal.TenantID, serviceAccountID); err != nil {
			return auth.APIKeyRecord{}, errors.New("service_account_id does not reference an existing service account")
		}
	}
	record := auth.APIKeyRecord{
		ID:                 NewID("key"),
		TenantID:           principal.TenantID,
		OwnerUserID:        ownerUserID,
		ServiceAccountID:   serviceAccountID,
		Name:               strings.TrimSpace(req.Name),
		Scopes:             adminGrantJSON(req.Scopes),
		AllowedModels:      adminGrantJSON(req.AllowedModels),
		AllowedTemplates:   adminGrantJSON(req.AllowedExtractTemplates),
		AllowedChatbots:    adminGrantJSON(req.AllowedChatbots),
		AllowedAgents:      adminGrantJSON(req.AllowedAgents),
		AllowedEndpoints:   adminGrantJSON(req.AllowedEndpoints),
		AllowedIPs:         adminGrantJSON(req.AllowedIPs),
		RateLimitPerMinute: req.RateLimitPerMinute,
		RateLimitPerHour:   req.RateLimitPerHour,
		RateLimitPerDay:    req.RateLimitPerDay,
		IsActive:           true,
	}
	if req.IsActive != nil {
		record.IsActive = *req.IsActive
	}
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		if t, parseErr := time.Parse(time.RFC3339, *req.ExpiresAt); parseErr == nil {
			tt := t.UTC()
			record.ExpiresAt = &tt
		}
	}
	return record, nil
}

// applyAdminPatch maps the mutable admin payload onto the stored record.
// Allowlist fields rewrite the stored grant document; a supplied "*" or null
// re-opens the grant (mode:all), a supplied list narrows it.
func applyAdminPatch(k *auth.APIKeyRecord, req adminAPIKeyCreateRequest) {
	if strings.TrimSpace(req.Name) != "" {
		k.Name = strings.TrimSpace(req.Name)
	}
	if req.Scopes != nil {
		k.Scopes = adminGrantJSON(req.Scopes)
	}
	if req.AllowedModels != nil {
		k.AllowedModels = adminGrantJSON(req.AllowedModels)
	}
	if req.AllowedEndpoints != nil {
		k.AllowedEndpoints = adminGrantJSON(req.AllowedEndpoints)
	}
	if req.AllowedExtractTemplates != nil {
		k.AllowedTemplates = adminGrantJSON(req.AllowedExtractTemplates)
	}
	if req.AllowedChatbots != nil {
		k.AllowedChatbots = adminGrantJSON(req.AllowedChatbots)
	}
	if req.AllowedAgents != nil {
		k.AllowedAgents = adminGrantJSON(req.AllowedAgents)
	}
	if req.AllowedIPs != nil {
		k.AllowedIPs = adminGrantJSON(req.AllowedIPs)
	}
	if req.RateLimitPerMinute != nil {
		k.RateLimitPerMinute = req.RateLimitPerMinute
	}
	if req.RateLimitPerHour != nil {
		k.RateLimitPerHour = req.RateLimitPerHour
	}
	if req.RateLimitPerDay != nil {
		k.RateLimitPerDay = req.RateLimitPerDay
	}
	if req.IsActive != nil {
		k.IsActive = *req.IsActive
	}
	if req.ExpiresAt != nil {
		if *req.ExpiresAt == "" {
			k.ExpiresAt = nil
		} else if t, parseErr := time.Parse(time.RFC3339, *req.ExpiresAt); parseErr == nil {
			tt := t.UTC()
			k.ExpiresAt = &tt
		}
	}
}

func (a *App) recordAPIKeyBillingAudit(ctx context.Context, action string, apiKeyID int64, actor auth.Principal, metadata map[string]any) {
	if a.billingAuditRepo == nil || apiKeyID <= 0 {
		return
	}
	subjectID := strconv.FormatInt(apiKeyID, 10)
	if metadata == nil {
		metadata = map[string]any{}
	}
	if err := a.billingAuditRepo.Create(ctx, sqlite.BillingAuditLog{
		ID:          NewID("billaudit"),
		APIKeyID:    &apiKeyID,
		SubjectType: "api_key",
		SubjectID:   subjectID,
		Action:      action,
		ActorType:   "api_key",
		ActorID:     strconv.FormatInt(actor.APIKeyID, 10),
		Metadata:    metadata,
	}); err != nil {
		log.Printf("failed to write billing audit event %s for api key %d: %v", action, apiKeyID, err)
	}
}

func adminBudgetMetadata(k auth.APIKeyRecord) map[string]any {
	return map[string]any{
		"rate_limit_per_minute": k.RateLimitPerMinute,
		"rate_limit_per_hour":   k.RateLimitPerHour,
		"rate_limit_per_day":    k.RateLimitPerDay,
	}
}
