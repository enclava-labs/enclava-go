package httpx

import (
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"

	"enclava-go/internal/auth"
	"enclava-go/internal/inference"
)

// endpointRequest is the admin payload for tenant inference endpoints.
// authorized_private and the assurance fields stay distinct: authorized
// private is an explicit operator grant; assurance_* records observed
// verification and is never inferred from it.
type endpointRequest struct {
	ID                string   `json:"id,omitempty"`
	Protocol          string   `json:"protocol,omitempty"`
	SDKProvider       string   `json:"sdk_provider,omitempty"`
	Capabilities      []string `json:"capabilities,omitempty"`
	BaseOrigin        string   `json:"base_origin,omitempty"`
	CredentialRef     *string  `json:"credential_ref,omitempty"`
	AssuranceMode     *string  `json:"assurance_mode,omitempty"`
	AssuranceVerified *bool    `json:"assurance_verified,omitempty"`
	AuthorizedPrivate *bool    `json:"authorized_private,omitempty"`
	IsEnabled         *bool    `json:"is_enabled,omitempty"`
}

// providerEndpointView keeps the /api/v1/providers list shape while carrying
// canonical endpoint fields; authorized_private and assurance_* are separate.
type providerEndpointView struct {
	inference.Endpoint
	Type    string         `json:"type"`
	BaseURL string         `json:"base_url"`
	Config  map[string]any `json:"config"`
}

func endpointView(e inference.Endpoint) providerEndpointView {
	config := map[string]any{}
	if e.AssuranceMode != "" {
		config["assurance_mode"] = e.AssuranceMode
	}
	return providerEndpointView{
		Endpoint: e,
		Type:     e.Protocol,
		BaseURL:  e.BaseOrigin,
		Config:   config,
	}
}

// providerTenantID scopes all endpoint admin to the request principal's
// tenant; an absent principal tenant is a truthful 401.
func (a *App) providerTenantID(w http.ResponseWriter, p auth.Principal) (string, bool) {
	if strings.TrimSpace(p.TenantID) != "" {
		return p.TenantID, true
	}
	WriteUnauthorized(w, "authenticated principal required")
	return "", false
}

func (a *App) handleProviders(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	setNoStoreHeaders(w)
	if a.endpoints == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "providers are not configured", "server_error", "not_configured")
		return
	}
	tenantID, ok := a.providerTenantID(w, p)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		endpoints, err := a.endpoints.List(r.Context(), tenantID)
		if err != nil {
			log.Printf("failed to list providers: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "providers_list_failed")
			return
		}
		includeDisabled := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_disabled")), "true")
		typeFilter := strings.TrimSpace(r.URL.Query().Get("type"))
		providers := make([]providerEndpointView, 0, len(endpoints))
		for _, e := range endpoints {
			if !includeDisabled && !e.Enabled {
				continue
			}
			if typeFilter != "" && !strings.EqualFold(e.Protocol, typeFilter) {
				continue
			}
			providers = append(providers, endpointView(e))
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"providers": providers,
			"runtime": map[string]any{
				"provider_id": a.providerID,
				"ready":       a.ProviderReady(),
				"snapshot":    a.ProviderSnapshot(),
			},
		})
	case http.MethodPost:
		var req endpointRequest
		if err := DecodeJSON(r, &req); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
			return
		}
		endpoint, err := endpointFromCreateRequest(tenantID, req)
		if err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "provider_invalid")
			return
		}
		if err := a.endpoints.Create(r.Context(), endpoint); err != nil {
			writeProviderNotFoundOrError(w, err, "provider_create_failed")
			return
		}
		created, err := a.endpoints.Get(r.Context(), tenantID, endpoint.ID)
		if err != nil {
			log.Printf("failed to fetch created provider config: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "provider_fetch_failed")
			return
		}
		WriteJSON(w, http.StatusCreated, endpointView(created))
	default:
		WriteMethodNotAllowed(w)
	}
}

func (a *App) handleProviderByID(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	setNoStoreHeaders(w)
	if a.endpoints == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "providers are not configured", "server_error", "not_configured")
		return
	}
	tenantID, ok := a.providerTenantID(w, p)
	if !ok {
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/providers/"), "/")
	if id == "" {
		WriteOpenAIError(w, http.StatusBadRequest, "provider id is required", "invalid_request_error", "missing_provider")
		return
	}
	switch r.Method {
	case http.MethodGet:
		endpoint, err := a.endpoints.Get(r.Context(), tenantID, id)
		if err != nil {
			writeProviderNotFoundOrError(w, err, "provider_fetch_failed")
			return
		}
		WriteJSON(w, http.StatusOK, endpointView(endpoint))
	case http.MethodPut:
		existing, err := a.endpoints.Get(r.Context(), tenantID, id)
		if err != nil {
			writeProviderNotFoundOrError(w, err, "provider_fetch_failed")
			return
		}
		var req endpointRequest
		if err := DecodeJSON(r, &req); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
			return
		}
		updated, err := endpointFromUpdateRequest(existing, req)
		if err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "provider_invalid")
			return
		}
		if req.IsEnabled != nil && *req.IsEnabled != existing.Enabled {
			if err := a.endpoints.SetEnabled(r.Context(), tenantID, id, *req.IsEnabled); err != nil {
				writeProviderNotFoundOrError(w, err, "provider_update_failed")
				return
			}
			updated.Enabled = *req.IsEnabled
		}
		if err := a.endpoints.Update(r.Context(), updated); err != nil {
			writeProviderNotFoundOrError(w, err, "provider_update_failed")
			return
		}
		endpoint, err := a.endpoints.Get(r.Context(), tenantID, id)
		if err != nil {
			writeProviderNotFoundOrError(w, err, "provider_fetch_failed")
			return
		}
		WriteJSON(w, http.StatusOK, endpointView(endpoint))
	case http.MethodDelete:
		// The endpoint seam has no destructive delete; DELETE disables the
		// endpoint so routing stops selecting it.
		if err := a.endpoints.SetEnabled(r.Context(), tenantID, id, false); err != nil {
			writeProviderNotFoundOrError(w, err, "provider_delete_failed")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
	default:
		WriteMethodNotAllowed(w)
	}
}

func endpointFromCreateRequest(tenantID string, req endpointRequest) (inference.Endpoint, error) {
	endpoint := inference.Endpoint{
		ID: strings.TrimSpace(req.ID), TenantID: tenantID, Protocol: strings.TrimSpace(req.Protocol), SDKProvider: strings.TrimSpace(req.SDKProvider),
		Capabilities: req.Capabilities, BaseOrigin: strings.TrimSpace(req.BaseOrigin), Enabled: true,
	}
	if req.CredentialRef != nil {
		endpoint.CredentialRef = strings.TrimSpace(*req.CredentialRef)
	}
	if endpoint.Capabilities == nil {
		endpoint.Capabilities = []string{}
	}
	if endpoint.ID == "" {
		endpoint.ID = NewID("provider")
	}
	if req.AssuranceMode != nil {
		endpoint.AssuranceMode = strings.TrimSpace(*req.AssuranceMode)
	}
	if req.AuthorizedPrivate != nil {
		endpoint.AuthorizedPrivate = *req.AuthorizedPrivate
	}
	if req.IsEnabled != nil {
		endpoint.Enabled = *req.IsEnabled
	}
	if req.AssuranceVerified != nil && *req.AssuranceVerified {
		return inference.Endpoint{}, errInvalidProvider("assurance_verified_at is set by observed verification only")
	}
	return validateEndpoint(endpoint)
}

func endpointFromUpdateRequest(existing inference.Endpoint, req endpointRequest) (inference.Endpoint, error) {
	endpoint := existing
	if strings.TrimSpace(req.Protocol) != "" {
		endpoint.Protocol = strings.TrimSpace(req.Protocol)
	}
	if strings.TrimSpace(req.SDKProvider) != "" {
		endpoint.SDKProvider = strings.TrimSpace(req.SDKProvider)
	}
	if req.Capabilities != nil {
		endpoint.Capabilities = req.Capabilities
	}
	if strings.TrimSpace(req.BaseOrigin) != "" {
		endpoint.BaseOrigin = strings.TrimSpace(req.BaseOrigin)
	}
	if req.CredentialRef != nil {
		endpoint.CredentialRef = strings.TrimSpace(*req.CredentialRef)
	}
	if req.AssuranceMode != nil {
		endpoint.AssuranceMode = strings.TrimSpace(*req.AssuranceMode)
	}
	if req.AuthorizedPrivate != nil {
		endpoint.AuthorizedPrivate = *req.AuthorizedPrivate
	}
	if req.AssuranceVerified != nil && *req.AssuranceVerified {
		return inference.Endpoint{}, errInvalidProvider("assurance_verified_at is set by observed verification only")
	}
	if endpoint.Protocol != existing.Protocol ||
		endpoint.SDKProvider != existing.SDKProvider ||
		endpoint.BaseOrigin != existing.BaseOrigin ||
		endpoint.CredentialRef != existing.CredentialRef ||
		endpoint.AssuranceMode != existing.AssuranceMode ||
		endpoint.AuthorizedPrivate != existing.AuthorizedPrivate ||
		!equalCapabilities(endpoint.Capabilities, existing.Capabilities) {
		endpoint.AssuranceVerifiedAt = nil
	}
	endpoint.Enabled = existing.Enabled
	return validateEndpoint(endpoint)
}

func equalCapabilities(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func validateEndpoint(e inference.Endpoint) (inference.Endpoint, error) {
	if strings.TrimSpace(e.ID) == "" {
		return inference.Endpoint{}, errInvalidProvider("id is required")
	}
	if !validProviderID(e.ID) {
		return inference.Endpoint{}, errInvalidProvider("id may contain only letters, numbers, hyphen, underscore, and dot")
	}
	if strings.TrimSpace(e.Protocol) == "" {
		return inference.Endpoint{}, errInvalidProvider("protocol is required")
	}
	if !knownProviderType(e.Protocol) {
		return inference.Endpoint{}, errInvalidProvider("protocol must be one of: tinfoil, privatemode, openai_compatible, redpill, confidential_sdk")
	}
	if e.Protocol == "confidential_sdk" && strings.TrimSpace(e.SDKProvider) == "" {
		return inference.Endpoint{}, errInvalidProvider("sdk_provider is required for confidential_sdk")
	}
	u, err := url.Parse(strings.TrimSpace(e.BaseOrigin))
	if err != nil || u.Host == "" {
		return inference.Endpoint{}, errInvalidProvider("base_origin must be an absolute origin")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !e.AuthorizedPrivate {
			return inference.Endpoint{}, errInvalidProvider("http base_origin requires authorized_private")
		}
	default:
		return inference.Endpoint{}, errInvalidProvider("base_origin scheme rejected")
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return inference.Endpoint{}, errInvalidProvider("base_origin must be a bare origin")
	}
	return e, nil
}

func knownProviderType(providerType string) bool {
	switch strings.ToLower(strings.TrimSpace(providerType)) {
	case "tinfoil", "privatemode", "openai_compatible", "redpill", "confidential_sdk":
		return true
	default:
		return false
	}
}

func validProviderID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

type invalidProviderError string

func (e invalidProviderError) Error() string { return string(e) }

func errInvalidProvider(msg string) error {
	return invalidProviderError(msg)
}

func writeProviderNotFoundOrError(w http.ResponseWriter, err error, code string) {
	if errors.Is(err, inference.ErrEndpointNotFound) {
		WriteOpenAIError(w, http.StatusNotFound, "provider not found", "invalid_request_error", "not_found")
		return
	}
	if errors.As(err, new(invalidProviderError)) {
		WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "provider_invalid")
		return
	}
	log.Printf("provider error: %v", err)
	WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", code)
}
