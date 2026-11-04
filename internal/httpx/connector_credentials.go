package httpx

import (
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"

	"enclava-go/internal/auth"
	"enclava-go/internal/credentials"
)

// ConfigureCredentials wires the dashboard-managed connector credential
// store (the ConfigureActivity pattern); its routes answer 501 while unset.
func (a *App) ConfigureCredentials(svc *credentials.Service) { a.credentials = svc }

func init() { registerDashboardPage((*App).registerConnectorOAuthRoutes) }

func (a *App) registerConnectorOAuthRoutes() {
	a.mux.HandleFunc("/dashboard/connectors/oauth/start/{provider}", a.sessionAuth.requireCap(capAccessManage, a.handleConnectorOAuthStart))
	a.mux.HandleFunc("/dashboard/connectors/oauth/callback/{provider}", a.sessionAuth.requireCap(capAccessManage, a.handleConnectorOAuthCallback))
}

func writeCredentialError(w http.ResponseWriter, err error, usedBy []string) {
	switch {
	case errors.Is(err, credentials.ErrDenied):
		WriteOpenAIError(w, http.StatusForbidden, err.Error(), "permission_error", "forbidden")
	case errors.Is(err, credentials.ErrDisabled):
		WriteOpenAIError(w, http.StatusConflict, err.Error(), "invalid_request_error", "credentials_disabled")
	case errors.Is(err, credentials.ErrInvalid):
		WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "credential_invalid")
	case errors.Is(err, credentials.ErrConflict):
		WriteOpenAIError(w, http.StatusConflict, err.Error(), "invalid_request_error", "credential_conflict")
	case errors.Is(err, credentials.ErrNotFound):
		WriteOpenAIError(w, http.StatusNotFound, err.Error(), "invalid_request_error", "not_found")
	case errors.Is(err, credentials.ErrInUse):
		WriteJSON(w, http.StatusConflict, map[string]any{
			"error":   OpenAIError{Message: "sources still use this credential — point them at another credential first", Type: "invalid_request_error", Code: "credential_in_use"},
			"used_by": usedBy,
		})
	default:
		log.Printf("connector credentials: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "connector_credentials_failed")
	}
}

func (a *App) credentialsConfigured(w http.ResponseWriter) bool {
	setNoStoreHeaders(w)
	if a.credentials == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "connector credentials are not configured", "server_error", "not_configured")
		return false
	}
	return true
}

// handleConnectorCredentials lists (GET) or adds a token credential (POST
// {provider, name, secret, origin}); secrets are write-only.
func (a *App) handleConnectorCredentials(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !a.credentialsConfigured(w) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		listing, err := a.credentials.List(r.Context(), p)
		if err != nil {
			writeCredentialError(w, err, nil)
			return
		}
		WriteJSON(w, http.StatusOK, listing)
	case http.MethodPost:
		var in struct {
			Provider string `json:"provider"`
			Name     string `json:"name"`
			Secret   string `json:"secret"`
			Origin   string `json:"origin"`
		}
		if err := DecodeJSON(r, &in); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error", "invalid_json")
			return
		}
		view, err := a.credentials.CreateToken(r.Context(), p, in.Provider, in.Name, in.Secret, in.Origin)
		if err != nil {
			writeCredentialError(w, err, nil)
			return
		}
		a.recordSecurityAudit(r, p, "connector_credential.created", "connector_credential", view.ID, "success")
		WriteJSON(w, http.StatusCreated, view)
	default:
		WriteMethodNotAllowed(w)
	}
}

// handleConnectorCredentialByID rotates (PUT {secret}) or deletes a
// credential; deleting one that sources use answers 409 with used_by.
func (a *App) handleConnectorCredentialByID(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !a.credentialsConfigured(w) {
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/connectors/credentials/"), "/")
	if id == "" || strings.Contains(id, "/") {
		WriteOpenAIError(w, http.StatusNotFound, "connector credential not found", "invalid_request_error", "not_found")
		return
	}
	switch r.Method {
	case http.MethodPut:
		var in struct {
			Secret string `json:"secret"`
		}
		if err := DecodeJSON(r, &in); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error", "invalid_json")
			return
		}
		view, err := a.credentials.Rotate(r.Context(), p, id, in.Secret)
		if err != nil {
			writeCredentialError(w, err, nil)
			return
		}
		a.recordSecurityAudit(r, p, "connector_credential.rotated", "connector_credential", id, "success")
		WriteJSON(w, http.StatusOK, view)
	case http.MethodDelete:
		usedBy, err := a.credentials.Delete(r.Context(), p, id)
		if err != nil {
			writeCredentialError(w, err, usedBy)
			return
		}
		a.recordSecurityAudit(r, p, "connector_credential.deleted", "connector_credential", id, "success")
		WriteJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
	default:
		WriteMethodNotAllowed(w)
	}
}

// handleConnectorOAuthApp stores (PUT {client_id, client_secret}) or removes
// the tenant's OAuth app for one provider.
func (a *App) handleConnectorOAuthApp(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !a.credentialsConfigured(w) {
		return
	}
	provider := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/connectors/oauth-apps/"), "/")
	switch r.Method {
	case http.MethodPut:
		var in struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
		}
		if err := DecodeJSON(r, &in); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error", "invalid_json")
			return
		}
		app, err := a.credentials.SetOAuthApp(r.Context(), p, provider, in.ClientID, in.ClientSecret)
		if err != nil {
			writeCredentialError(w, err, nil)
			return
		}
		a.recordSecurityAudit(r, p, "connector_oauth_app.saved", "connector_oauth_app", provider, "success")
		WriteJSON(w, http.StatusOK, app)
	case http.MethodDelete:
		if err := a.credentials.DeleteOAuthApp(r.Context(), p, provider); err != nil {
			writeCredentialError(w, err, nil)
			return
		}
		a.recordSecurityAudit(r, p, "connector_oauth_app.deleted", "connector_oauth_app", provider, "success")
		WriteJSON(w, http.StatusOK, map[string]any{"deleted": true, "provider": provider})
	default:
		WriteMethodNotAllowed(w)
	}
}

// redirectToCredentials lands the browser back on the Credentials tab with a
// one-shot notice.
func redirectToCredentials(w http.ResponseWriter, r *http.Request, connected, message string) {
	q := url.Values{}
	if connected != "" {
		q.Set("connected", connected)
	}
	if message != "" {
		q.Set("error", message)
	}
	target := "/dashboard/sources/credentials"
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	setNoStoreHeaders(w)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func credentialFailureMessage(err error) string {
	for _, known := range []error{credentials.ErrInvalid, credentials.ErrConflict, credentials.ErrDisabled, credentials.ErrDenied, credentials.ErrNotFound} {
		if errors.Is(err, known) {
			return err.Error()
		}
	}
	log.Printf("connector oauth: %v", err)
	return "connecting failed — see the server log"
}

// handleConnectorOAuthStart sends the administrator to the provider's
// consent screen: GET /dashboard/connectors/oauth/start/{provider}?name=.
func (a *App) handleConnectorOAuthStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	if a.credentials == nil {
		redirectToCredentials(w, r, "", "connector credentials are not configured")
		return
	}
	p, _ := auth.PrincipalFromContext(r.Context())
	target, err := a.credentials.StartOAuth(r.Context(), p, r.PathValue("provider"), r.URL.Query().Get("name"))
	if err != nil {
		redirectToCredentials(w, r, "", credentialFailureMessage(err))
		return
	}
	setNoStoreHeaders(w)
	http.Redirect(w, r, target, http.StatusSeeOther) //nolint:gosec // Target is the provider's fixed authorization endpoint built server-side.
}

// handleConnectorOAuthCallback is the redirect URI registered at the
// provider: it exchanges the code and stores the grant.
func (a *App) handleConnectorOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	if a.credentials == nil {
		redirectToCredentials(w, r, "", "connector credentials are not configured")
		return
	}
	q := r.URL.Query()
	if denied := q.Get("error"); denied != "" {
		message := q.Get("error_description")
		if message == "" {
			message = denied
		}
		redirectToCredentials(w, r, "", "the provider did not authorize the connection: "+message)
		return
	}
	p, _ := auth.PrincipalFromContext(r.Context())
	view, err := a.credentials.CompleteOAuth(r.Context(), p, r.PathValue("provider"), q.Get("state"), q.Get("code"))
	if err != nil {
		redirectToCredentials(w, r, "", credentialFailureMessage(err))
		return
	}
	a.recordSecurityAudit(r, p, "connector_credential.connected", "connector_credential", view.ID, "success")
	redirectToCredentials(w, r, view.ID, "")
}
