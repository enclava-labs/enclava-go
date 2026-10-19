package httpx

import (
	"net/http"
	"strings"

	"enclava-go/internal/auth"
)

// enforceAllowlists applies the stored endpoint and IP grants at dispatch.
// Grants carry the decoded {"mode":"all"}→All or {"mode":"list"}→item map
// shape: All==true is unrestricted; an empty/absent/malformed list denies
// everything (fail closed — it never "skips the check"). Endpoint grants
// hold inference endpoint IDs (route families like "chat/completions"),
// never URL path patterns. Only API-key credentials carry endpoint/IP
// grants — session-authenticated dashboard requests are scope-gated only.
func (a *App) enforceAllowlists(w http.ResponseWriter, r *http.Request, principal auth.Principal, next func(http.ResponseWriter, *http.Request, auth.Principal)) {
	if principal.CredentialKind == auth.CredentialKindAPIKey {
		if !principal.AllowedEndpointsAll {
			if len(principal.AllowedEndpoints) == 0 {
				WriteForbidden(w, "endpoint is not allowed for this API key")
				return
			}
			if id := inferenceEndpointID(r.URL.Path); id != "" && !auth.EndpointAllowed(principal, id) {
				WriteForbidden(w, "endpoint is not allowed for this API key")
				return
			}
		}
		if !principal.AllowedIPsAll {
			ip := clientIP(r, a.trustProxy)
			if ip == "" {
				WriteForbidden(w, "source IP could not be determined")
				return
			}
			if !sourceIPAllowed(principal.AllowedIPs, ip) {
				WriteForbidden(w, "source IP is not allowed for this API key")
				return
			}
		}
	}
	next(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)), principal)
}

// inferenceEndpointID maps an inference route to its endpoint ID — the
// route family: "/v1/chat/completions" resolves to "chat/completions" and
// "/v1/models/{id}" to "models". Non-inference paths return "" and are
// governed by scopes only.
func inferenceEndpointID(path string) string {
	var rest string
	switch {
	case strings.HasPrefix(path, "/api/v1/"):
		rest = strings.TrimPrefix(path, "/api/v1/")
	case strings.HasPrefix(path, "/v1/"):
		rest = strings.TrimPrefix(path, "/v1/")
	default:
		return ""
	}
	rest = strings.Trim(rest, "/")
	if rest == "" {
		return ""
	}
	if strings.HasPrefix(rest, "chat/completions") {
		return "chat/completions"
	}
	if i := strings.Index(rest, "/"); i > 0 {
		return rest[:i]
	}
	return rest
}
