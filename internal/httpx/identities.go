// identities.go — tenant-scoped identity administration API (AUTH-01/02/04,
// plan 01-24): users, roles, service accounts, per-account sessions/linked
// identities, self-service account operations and the explicit OIDC link
// confirmation. Tenant and actor always come from the verified principal —
// request JSON carries payload only, never authority. Privilege escalation is
// denied twice: the route requires platform.admin and every grant written
// (role scopes AND role resource grants, service-account scopes, key scopes)
// must be a subset of the calling principal's own authority.
package httpx

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/store/postgres"
)

// --- request/response documents -------------------------------------------

type identityUserRequest struct {
	Email       string   `json:"email,omitempty"`
	DisplayName string   `json:"display_name,omitempty"`
	Password    string   `json:"password,omitempty"`
	Status      string   `json:"status,omitempty"`
	RoleIDs     []string `json:"role_ids,omitempty"`
	// Team is pointer-typed so a PATCH can distinguish absent (unchanged)
	// from explicit empty (clear — "No team" is a first-class state).
	Team *string `json:"team,omitempty"`
}

type identityPasswordRequest struct {
	CurrentPassword string `json:"current_password,omitempty"`
	Password        string `json:"password"`
}

type identityRoleRequest struct {
	Name   string         `json:"name,omitempty"`
	Scopes []string       `json:"scopes"`
	Grants *roleGrantsDTO `json:"grants,omitempty"`
}

// roleGrantsDTO is the canonical grant envelope for the roles HTTP surface:
// each field carries the stored {"mode":"all"}/{"mode":"list","items":[...]}
// document verbatim, in and out — there is no alternate request or response
// shape. Absent/null/malformed fields decode deny (the storage contract).
type roleGrantsDTO struct {
	Models           json.RawMessage `json:"models,omitempty"`
	Endpoints        json.RawMessage `json:"endpoints,omitempty"`
	Assistants       json.RawMessage `json:"assistants,omitempty"`
	ExtractTemplates json.RawMessage `json:"extract_templates,omitempty"`
}

type identityServiceAccountRequest struct {
	Name   string   `json:"name,omitempty"`
	Scopes []string `json:"scopes"`
	Status string   `json:"status,omitempty"`
}

type identityLinkRequest struct {
	LinkToken string `json:"link_token"`
	Password  string `json:"password"`
}

// redactUser strips the password hash before rendering. Roles render through
// the canonical roleDTO so every user-bearing output carries the same grant
// document shape as the roles API.
func redactUser(u auth.User, roles []auth.Role) map[string]any {
	out := map[string]any{
		"id":              u.ID,
		"email":           u.Email,
		"display_name":    u.DisplayName,
		"status":          u.Status,
		"team":            u.Team,
		"password_set":    u.PasswordSetAt != nil,
		"password_set_at": u.PasswordSetAt,
		"last_login_at":   u.LastLoginAt,
		"created_at":      u.CreatedAt,
		"roles":           roleDTOList(roles),
	}
	return out
}

func redactSession(s auth.Session, currentID string) map[string]any {
	return map[string]any{
		"id":              s.ID,
		"user_id":         s.UserID,
		"created_at":      s.CreatedAt,
		"expires_at":      s.ExpiresAt,
		"idle_expires_at": s.IdleExpiresAt,
		"revoked_at":      s.RevokedAt,
		"revoke_reason":   s.RevokeReason,
		"current":         s.ID == currentID,
	}
}

func redactAuthIdentity(a auth.AuthIdentity) map[string]any {
	return map[string]any{
		"id":                a.ID,
		"user_id":           a.UserID,
		"issuer":            a.Issuer,
		"subject":           a.Subject,
		"linked_at":         a.LinkedAt,
		"linked_by_user_id": a.LinkedByUserID,
	}
}

// --- shared guards ---------------------------------------------------------

// scopesSubsetOfCaller reports whether every granted scope is held by the
// calling principal — a request can never mint authority the caller lacks.
func scopesSubsetOfCaller(caller auth.Principal, granted []string) bool {
	for _, s := range granted {
		if !auth.HasScope(caller, s) {
			return false
		}
	}
	return true
}

// allowlistDocJSON encodes an Allowlist as the canonical stored grant
// document — the HTTP write-side of auth.DecodeAllowlist.
func allowlistDocJSON(a auth.Allowlist) json.RawMessage {
	items := make(map[string]struct{}, len(a.Items))
	for _, id := range a.Items {
		items[id] = struct{}{}
	}
	return auth.EncodeAllowlist(items, a.All)
}

// sortedItems renders a decoded item set as the sorted slice the Allowlist
// storage type carries.
func sortedItems(items map[string]struct{}) []string {
	out := make([]string, 0, len(items))
	for id := range items {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// resourceGrantsFromRequest decodes a request's grant documents into the
// stored ResourceGrants shape; a nil envelope or absent/null/malformed field
// decodes deny — never unrestricted.
func resourceGrantsFromRequest(dto *roleGrantsDTO) auth.ResourceGrants {
	decode := func(raw json.RawMessage) auth.Allowlist {
		items, all := auth.DecodeAllowlist(raw)
		return auth.Allowlist{Items: sortedItems(items), All: all}
	}
	if dto == nil {
		return auth.ResourceGrants{}
	}
	return auth.ResourceGrants{
		Models:           decode(dto.Models),
		Endpoints:        decode(dto.Endpoints),
		Assistants:       decode(dto.Assistants),
		ExtractTemplates: decode(dto.ExtractTemplates),
	}
}

// grantDelta decodes one grant envelope field for a role PATCH: a nil raw
// field is absent from the envelope (stored grant unchanged); a present
// field — explicit null or an empty item list included — replaces with its
// decoded allowlist (deny).
func grantDelta(raw json.RawMessage) *auth.Allowlist {
	if raw == nil {
		return nil
	}
	items, all := auth.DecodeAllowlist(raw)
	return new(auth.Allowlist{Items: sortedItems(items), All: all})
}

// grantCoveredByCaller reports whether one stored grant document stays
// inside the caller's authority: a mode:"all" document only where the
// caller's matching *All flag is set, every item individually covered
// otherwise. Shared by the role and key-grant bounds — one semantic.
func grantCoveredByCaller(callerAll bool, covers func(string) bool, a auth.Allowlist) bool {
	if a.All {
		return callerAll
	}
	for _, id := range a.Items {
		if !covers(id) {
			return false
		}
	}
	return true
}

// roleGrantsSubsetOfCaller denies creating or assigning role grants beyond
// the managing caller's own resource authority (D-25). Assistants are one
// stored kind feeding both chatbots and agents, so items pass on either.
func roleGrantsSubsetOfCaller(caller auth.Principal, g auth.ResourceGrants) bool {
	return grantCoveredByCaller(caller.AllowedModelsAll, func(id string) bool { return auth.ModelAllowed(caller, id) }, g.Models) &&
		grantCoveredByCaller(caller.AllowedEndpointsAll, func(id string) bool { return auth.EndpointAllowed(caller, id) }, g.Endpoints) &&
		grantCoveredByCaller(caller.AllowedChatbotsAll && caller.AllowedAgentsAll,
			func(id string) bool { return auth.ChatbotAllowed(caller, id) || auth.AgentAllowed(caller, id) }, g.Assistants) &&
		grantCoveredByCaller(caller.AllowedExtractTemplatesAll, func(id string) bool { return auth.ExtractTemplateAllowed(caller, id) }, g.ExtractTemplates)
}

// roleDTO renders one role for HTTP — the single canonical shape for every
// role-bearing output (list/get/create, nested user roles and /me). Grants
// are re-encoded from role.Grants, so each emitted document round-trips
// through DecodeAllowlist unchanged.
func roleDTO(role auth.Role, userIDs []string) map[string]any {
	out := map[string]any{
		"id":     role.ID,
		"name":   role.Name,
		"scopes": role.Scopes,
		"grants": map[string]any{
			"models":            allowlistDocJSON(role.Grants.Models),
			"endpoints":         allowlistDocJSON(role.Grants.Endpoints),
			"assistants":        allowlistDocJSON(role.Grants.Assistants),
			"extract_templates": allowlistDocJSON(role.Grants.ExtractTemplates),
		},
		"created_at": role.CreatedAt,
	}
	if userIDs != nil {
		out["user_ids"] = userIDs
	}
	return out
}

// roleDTOList renders roles without membership expansion.
func roleDTOList(roles []auth.Role) []map[string]any {
	out := make([]map[string]any, 0, len(roles))
	for _, ro := range roles {
		out = append(out, roleDTO(ro, nil))
	}
	return out
}

// lastAdminExcluded denies mutations that would remove the tenant's last
// usable platform.admin holder. excludeUserID/excludeKeyIDs name the holders
// the pending mutation removes; if no user or key carrying the scope would
// remain, the write is refused so the tenant can never lock itself out.
func (a *App) lastAdminExcluded(w http.ResponseWriter, r *http.Request, tenantID, excludeUserID string, excludeKeyIDs map[string]bool) bool {
	users, keys, err := a.identity.AdminHolderCounts(r.Context(), tenantID, auth.ScopePlatformAdmin)
	if err != nil {
		log.Printf("failed to count admin holders: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "admin_count_failed")
		return false
	}
	remaining := 0
	for _, id := range users {
		if id != excludeUserID {
			remaining++
		}
	}
	for _, id := range keys {
		if !excludeKeyIDs[id] {
			remaining++
		}
	}
	if remaining == 0 {
		WriteOpenAIError(w, http.StatusConflict, "operation would remove the last usable administrator", "invalid_request_error", "last_admin")
		return false
	}
	return true
}

// userIsAdmin reports whether the user currently holds platform.admin via a
// role — the caller knows the user exists.
func (a *App) userIsAdmin(r *http.Request, tenantID, userID string) bool {
	roles, err := a.identity.RolesForUser(r.Context(), tenantID, userID)
	if err != nil {
		return false
	}
	for _, ro := range roles {
		for _, s := range ro.Scopes {
			if s == auth.ScopePlatformAdmin {
				return true
			}
		}
	}
	return false
}

func (a *App) identityConfigured(w http.ResponseWriter) bool {
	if a.identity == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "identity management not configured", "server_error", "not_configured")
		return false
	}
	return true
}

// --- users ------------------------------------------------------------------

func (a *App) handleIdentityUsers(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	setNoStoreHeaders(w)
	if !a.identityConfigured(w) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		users, err := a.identity.ListUsers(r.Context(), principal.TenantID)
		if err != nil {
			log.Printf("list users: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "users_list_failed")
			return
		}
		out := make([]map[string]any, 0, len(users))
		for _, u := range users {
			roles, err := a.identity.RolesForUser(r.Context(), principal.TenantID, u.ID)
			if err != nil {
				log.Printf("roles for user %s: %v", u.ID, err)
				WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "users_list_failed")
				return
			}
			out = append(out, redactUser(u, roles))
		}
		WriteJSON(w, http.StatusOK, map[string]any{"users": out})
	case http.MethodPost:
		var req identityUserRequest
		if err := DecodeJSON(r, &req); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
			return
		}
		email := strings.ToLower(strings.TrimSpace(req.Email))
		if email == "" || !strings.Contains(email, "@") {
			WriteOpenAIError(w, http.StatusBadRequest, "a valid email is required", "invalid_request_error", "invalid_email")
			return
		}
		if err := auth.ValidatePassword(req.Password); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_password")
			return
		}
		team := ""
		if req.Team != nil {
			team = strings.TrimSpace(*req.Team)
			if len(team) > 80 {
				WriteOpenAIError(w, http.StatusBadRequest, "team must be at most 80 characters", "invalid_request_error", "invalid_team")
				return
			}
		}
		// Role ids validate before any write: the memberships then commit
		// inside the same transaction as the user row (ADM-I01).
		for _, roleID := range req.RoleIDs {
			if !a.checkRoleAssignable(w, r, principal, roleID) {
				return
			}
		}
		// Administrator-controlled enrollment only — there is no public signup.
		user, err := a.authSvc.CreateUser(r.Context(), principal, principal.TenantID, email, strings.TrimSpace(req.DisplayName), req.Password, team, req.RoleIDs)
		if err != nil {
			a.writeIdentityError(w, err, "user_create_failed")
			return
		}
		a.recordSecurityAudit(r, principal, "identity.user.created", "user", user.ID, "success")
		for _, roleID := range req.RoleIDs {
			a.recordSecurityAudit(r, principal, "identity.role.assigned", "user", user.ID, "success", map[string]any{"role_id": roleID})
		}
		roles, _ := a.identity.RolesForUser(r.Context(), principal.TenantID, user.ID)
		WriteJSON(w, http.StatusCreated, redactUser(user, roles))
	default:
		WriteMethodNotAllowed(w)
	}
}

func (a *App) handleIdentityUserByID(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	setNoStoreHeaders(w)
	if !a.identityConfigured(w) {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/identities/users/")
	segs := strings.Split(strings.Trim(rest, "/"), "/")
	userID := segs[0]
	if userID == "" {
		WriteOpenAIError(w, http.StatusBadRequest, "user id is required", "invalid_request_error", "missing_id")
		return
	}
	// Every sub-resource first resolves the user inside the caller's tenant —
	// a foreign-tenant id is a 404, never a leak or cross-tenant write.
	user, err := a.identity.GetUser(r.Context(), principal.TenantID, userID)
	if err != nil {
		a.writeIdentityError(w, err, "user_fetch_failed")
		return
	}
	if len(segs) == 1 {
		a.handleIdentityUser(w, r, principal, user)
		return
	}
	switch segs[1] {
	case "password":
		if r.Method != http.MethodPost {
			WriteMethodNotAllowed(w)
			return
		}
		var req identityPasswordRequest
		if err := DecodeJSON(r, &req); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
			return
		}
		a.setUserPassword(w, r, principal, user, req.Password)
	case "roles":
		if len(segs) == 2 {
			if r.Method != http.MethodGet {
				WriteMethodNotAllowed(w)
				return
			}
			roles, err := a.identity.RolesForUser(r.Context(), principal.TenantID, user.ID)
			if err != nil {
				log.Printf("roles for user %s: %v", user.ID, err)
				WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "roles_list_failed")
				return
			}
			WriteJSON(w, http.StatusOK, map[string]any{"roles": roleDTOList(roles)})
			return
		}
		if len(segs) != 3 {
			WriteOpenAIError(w, http.StatusNotFound, "not found", "invalid_request_error", "not_found")
			return
		}
		roleID := segs[2]
		switch r.Method {
		case http.MethodPut, http.MethodPost:
			a.assignRoleChecked(w, r, principal, user.ID, roleID)
		case http.MethodDelete:
			a.unassignRoleChecked(w, r, principal, user, roleID)
		default:
			WriteMethodNotAllowed(w)
		}
	case "sessions":
		if len(segs) == 2 {
			if r.Method != http.MethodGet {
				WriteMethodNotAllowed(w)
				return
			}
			a.listUserSessions(w, r, principal.TenantID, user.ID)
			return
		}
		if len(segs) != 3 {
			WriteOpenAIError(w, http.StatusNotFound, "not found", "invalid_request_error", "not_found")
			return
		}
		if r.Method != http.MethodDelete && r.Method != http.MethodPost {
			WriteMethodNotAllowed(w)
			return
		}
		a.revokeUserSession(w, r, principal, user.ID, segs[2])
	case "identities":
		if len(segs) != 2 || r.Method != http.MethodGet {
			if r.Method != http.MethodGet {
				WriteMethodNotAllowed(w)
				return
			}
			WriteOpenAIError(w, http.StatusNotFound, "not found", "invalid_request_error", "not_found")
			return
		}
		a.listAuthIdentities(w, r, principal.TenantID, user.ID)
	default:
		WriteOpenAIError(w, http.StatusNotFound, "not found", "invalid_request_error", "not_found")
	}
}

func (a *App) handleIdentityUser(w http.ResponseWriter, r *http.Request, principal auth.Principal, user auth.User) {
	switch r.Method {
	case http.MethodGet:
		roles, err := a.identity.RolesForUser(r.Context(), principal.TenantID, user.ID)
		if err != nil {
			log.Printf("roles for user %s: %v", user.ID, err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "roles_list_failed")
			return
		}
		WriteJSON(w, http.StatusOK, redactUser(user, roles))
	case http.MethodPatch, http.MethodPut:
		var req identityUserRequest
		if err := DecodeJSON(r, &req); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
			return
		}
		status := strings.TrimSpace(req.Status)
		if status != "" && status != "active" && status != "disabled" {
			WriteOpenAIError(w, http.StatusBadRequest, "status must be 'active' or 'disabled'", "invalid_request_error", "invalid_status")
			return
		}
		var team string
		if req.Team != nil {
			team = strings.TrimSpace(*req.Team)
			if len(team) > 80 {
				WriteOpenAIError(w, http.StatusBadRequest, "team must be at most 80 characters", "invalid_request_error", "invalid_team")
				return
			}
		}
		// Disabling an administrator cannot remove the tenant's last one —
		// the remaining-holder check and the status write commit together
		// under the tenant authority lock.
		if status != "" && status != user.Status {
			if status != "active" && user.Status == "active" && a.userIsAdmin(r, principal.TenantID, user.ID) {
				if err := a.identity.SetUserStatusAtomically(r.Context(), principal.TenantID, user.ID, status); err != nil {
					a.writeRoleWriteError(w, err, "user_update_failed")
					return
				}
			} else if err := a.identity.UpdateUserStatus(r.Context(), principal.TenantID, user.ID, status); err != nil {
				log.Printf("update user status %s: %v", user.ID, err)
				WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "user_update_failed")
				return
			}
			action := "identity.user.disabled"
			if status == "active" {
				action = "identity.user.enabled"
			}
			a.recordSecurityAudit(r, principal, action, "user", user.ID, "success")
			if status != "active" {
				// Revocation takes effect on the next request for every live
				// session of the disabled account.
				if err := a.authSvc.RevokeUserSessions(r.Context(), principal.TenantID, user.ID, "user disabled"); err != nil {
					log.Printf("revoke sessions for disabled user %s: %v", user.ID, err)
				} else {
					a.recordSecurityAudit(r, principal, "auth.sessions.revoked", "user", user.ID, "success")
				}
			}
		}
		if req.Team != nil {
			if err := a.identity.UpdateUserTeam(r.Context(), principal.TenantID, user.ID, team); err != nil {
				log.Printf("update user team %s: %v", user.ID, err)
				WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "user_update_failed")
				return
			}
			a.recordSecurityAudit(r, principal, "identity.user.updated", "user", user.ID, "success")
		}
		updated, err := a.identity.GetUser(r.Context(), principal.TenantID, user.ID)
		if err != nil {
			a.writeIdentityError(w, err, "user_fetch_failed")
			return
		}
		roles, _ := a.identity.RolesForUser(r.Context(), principal.TenantID, user.ID)
		WriteJSON(w, http.StatusOK, redactUser(updated, roles))
	default:
		WriteMethodNotAllowed(w)
	}
}

// checkRoleAssignable verifies the role exists in the tenant and never widens
// the caller's authority — the shared gate for both the assign subroute and
// the create-user role_ids list.
func (a *App) checkRoleAssignable(w http.ResponseWriter, r *http.Request, principal auth.Principal, roleID string) bool {
	role, err := a.identity.GetRole(r.Context(), principal.TenantID, roleID)
	if err != nil {
		a.writeIdentityError(w, err, "role_fetch_failed")
		return false
	}
	if !scopesSubsetOfCaller(principal, role.Scopes) {
		WriteForbidden(w, "role grants scopes the caller does not hold")
		return false
	}
	// Assigning an existing role is itself a grant of that role's resource
	// authority — it can never widen the caller's own (D-25).
	if !roleGrantsSubsetOfCaller(principal, role.Grants) {
		WriteForbidden(w, "role grants exceed the caller's resource authority")
		return false
	}
	return true
}

// assignRoleChecked writes one user_roles membership and the assigned:true
// response: the role must exist in the tenant and pass the authority checks.
func (a *App) assignRoleChecked(w http.ResponseWriter, r *http.Request, principal auth.Principal, userID, roleID string) {
	if !a.checkRoleAssignable(w, r, principal, roleID) {
		return
	}
	if err := a.identity.AssignRole(r.Context(), principal.TenantID, userID, roleID); err != nil {
		log.Printf("assign role %s to %s: %v", roleID, userID, err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "role_assign_failed")
		return
	}
	a.recordSecurityAudit(r, principal, "identity.role.assigned", "user", userID, "success", map[string]any{"role_id": roleID})
	WriteJSON(w, http.StatusOK, map[string]any{"assigned": true, "user_id": userID, "role_id": roleID})
}

func (a *App) unassignRoleChecked(w http.ResponseWriter, r *http.Request, principal auth.Principal, user auth.User, roleID string) {
	// The remaining-holder check and the delete commit together under the
	// tenant authority lock — removing an admin role membership can never
	// strand the tenant between check and write (409 last_admin).
	if err := a.identity.UnassignRoleAtomically(r.Context(), principal.TenantID, user.ID, roleID); err != nil {
		a.writeRoleWriteError(w, err, "role_unassign_failed")
		return
	}
	a.recordSecurityAudit(r, principal, "identity.role.unassigned", "user", user.ID, "success", map[string]any{"role_id": roleID})
	WriteJSON(w, http.StatusOK, map[string]any{"assigned": false, "user_id": user.ID, "role_id": roleID})
}

// setUserPassword rewrites the stored bcrypt hash; plaintext never persists.
func (a *App) setUserPassword(w http.ResponseWriter, r *http.Request, principal auth.Principal, user auth.User, password string) {
	if err := auth.ValidatePassword(password); err != nil {
		WriteOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_password")
		return
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		log.Printf("hash password: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "password_hash_failed")
		return
	}
	keepSessionID := ""
	if principal.UserID == user.ID {
		keepSessionID = principal.SessionID
	}
	if err := a.identity.SetPasswordAndRevokeSessions(r.Context(), user.TenantID, user.ID, hash, time.Now().UTC(), keepSessionID); err != nil {
		log.Printf("set password for %s: %v", user.ID, err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "password_set_failed")
		return
	}
	a.recordSecurityAudit(r, principal, "auth.password.changed", "user", user.ID, "success")
	a.recordSecurityAudit(r, principal, "auth.sessions.revoked", "user", user.ID, "success")
	WriteJSON(w, http.StatusOK, map[string]any{"updated": true, "user_id": user.ID})
}

func (a *App) listUserSessions(w http.ResponseWriter, r *http.Request, tenantID, userID string) {
	sessions, err := a.identity.ListSessionsForUser(r.Context(), tenantID, userID)
	if err != nil {
		log.Printf("list sessions for %s: %v", userID, err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "sessions_list_failed")
		return
	}
	out := make([]map[string]any, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, redactSession(s, ""))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

func serviceAccountDTO(sa auth.ServiceAccount) map[string]any {
	return map[string]any{
		"id": sa.ID, "name": sa.Name, "scopes": sa.Scopes,
		"status": sa.Status, "created_at": sa.CreatedAt,
	}
}

func (a *App) revokeUserSession(w http.ResponseWriter, r *http.Request, principal auth.Principal, userID, sessionID string) {
	// The session must belong to the asserted user inside the tenant —
	// otherwise any session id becomes revocable across accounts.
	sess, err := a.identity.GetSession(r.Context(), principal.TenantID, sessionID)
	if err != nil || sess.UserID != userID {
		if err != nil {
			a.writeIdentityError(w, err, "session_fetch_failed")
			return
		}
		WriteOpenAIError(w, http.StatusNotFound, "session not found", "invalid_request_error", "not_found")
		return
	}
	if err := a.authSvc.RevokeSession(r.Context(), principal.TenantID, sessionID, "administrator revocation"); err != nil {
		log.Printf("revoke session %s: %v", sessionID, err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "session_revoke_failed")
		return
	}
	a.recordSecurityAudit(r, principal, "auth.sessions.revoked", "session", sessionID, "success")
	WriteJSON(w, http.StatusOK, map[string]any{"revoked": true, "id": sessionID})
}

func (a *App) listAuthIdentities(w http.ResponseWriter, r *http.Request, tenantID, userID string) {
	links, err := a.identity.ListAuthIdentitiesForUser(r.Context(), tenantID, userID)
	if err != nil {
		log.Printf("list auth identities for %s: %v", userID, err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "identities_list_failed")
		return
	}
	out := make([]map[string]any, 0, len(links))
	for _, l := range links {
		out = append(out, redactAuthIdentity(l))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"identities": out})
}

// --- roles -----------------------------------------------------------------

func (a *App) handleIdentityRoles(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	setNoStoreHeaders(w)
	if !a.identityConfigured(w) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		roles, err := a.identity.ListRoles(r.Context(), principal.TenantID)
		if err != nil {
			log.Printf("list roles: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "roles_list_failed")
			return
		}
		out := make([]map[string]any, 0, len(roles))
		for _, ro := range roles {
			userIDs, err := a.identity.RoleUsers(r.Context(), principal.TenantID, ro.ID)
			if err != nil {
				log.Printf("role users %s: %v", ro.ID, err)
				WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "roles_list_failed")
				return
			}
			out = append(out, roleDTO(ro, userIDs))
		}
		WriteJSON(w, http.StatusOK, map[string]any{"roles": out})
	case http.MethodPost:
		var req identityRoleRequest
		if err := DecodeJSON(r, &req); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" {
			WriteOpenAIError(w, http.StatusBadRequest, "name is required", "invalid_request_error", "missing_name")
			return
		}
		if !scopesSubsetOfCaller(principal, req.Scopes) {
			WriteForbidden(w, "role scopes must be a subset of the caller's scopes")
			return
		}
		// Deny by default: an absent grants envelope stores explicit empty
		// deny lists — never nil→all (that legacy nil→all is keys-only).
		grants := resourceGrantsFromRequest(req.Grants)
		if !roleGrantsSubsetOfCaller(principal, grants) {
			WriteForbidden(w, "role grants exceed the caller's resource authority")
			return
		}
		role := auth.Role{ID: NewID("role"), Name: name, Scopes: req.Scopes, Grants: grants}
		if err := a.identity.CreateRole(r.Context(), principal.TenantID, role); err != nil {
			log.Printf("create role: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "role_create_failed")
			return
		}
		// Re-read so the response carries the stored created_at (ADM-I02).
		stored, err := a.identity.GetRole(r.Context(), principal.TenantID, role.ID)
		if err != nil {
			a.writeIdentityError(w, err, "role_fetch_failed")
			return
		}
		a.recordSecurityAudit(r, principal, "identity.role.created", "role", stored.ID, "success")
		WriteJSON(w, http.StatusCreated, roleDTO(stored, nil))
	default:
		WriteMethodNotAllowed(w)
	}
}

func (a *App) handleIdentityRoleByID(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	setNoStoreHeaders(w)
	if !a.identityConfigured(w) {
		return
	}
	roleID := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/identities/roles/"), "/")
	if roleID == "" || strings.Contains(roleID, "/") {
		WriteOpenAIError(w, http.StatusBadRequest, "role id is required", "invalid_request_error", "missing_id")
		return
	}
	role, err := a.identity.GetRole(r.Context(), principal.TenantID, roleID)
	if err != nil {
		a.writeIdentityError(w, err, "role_fetch_failed")
		return
	}
	switch r.Method {
	case http.MethodGet:
		userIDs, err := a.identity.RoleUsers(r.Context(), principal.TenantID, role.ID)
		if err != nil {
			log.Printf("role users %s: %v", role.ID, err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "roles_list_failed")
			return
		}
		WriteJSON(w, http.StatusOK, roleDTO(role, userIDs))
	case http.MethodPatch:
		a.handleIdentityRolePatch(w, r, principal, role)
	case http.MethodDelete:
		a.handleIdentityRoleDelete(w, r, principal, role.ID)
	default:
		WriteMethodNotAllowed(w)
	}
}

// handleIdentityRolePatch applies a role update (OA-20): absent fields stay
// unchanged, present-including-empty replace. The handler validates the
// deltas and bounds the merged result by the caller's own authority — the
// caller is never handed a merged role to write; UpdateRoleAtomically
// re-derives the merge and the authority-removal decision from the STORED row
// under the tenant authority lock and refuses an overtaken read (409
// stale_role), so a delayed PATCH can never overwrite a concurrent grant.
func (a *App) handleIdentityRolePatch(w http.ResponseWriter, r *http.Request, principal auth.Principal, role auth.Role) {
	var req identityRoleRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
		return
	}
	var deltas postgres.RoleUpdateDeltas
	if req.Name != "" {
		name := strings.TrimSpace(req.Name)
		if name == "" {
			WriteOpenAIError(w, http.StatusBadRequest, "name cannot be empty", "invalid_request_error", "invalid_name")
			return
		}
		deltas.Name = new(name)
	}
	if req.Scopes != nil {
		deltas.Scopes = new(req.Scopes)
	}
	if req.Grants != nil {
		deltas.Models = grantDelta(req.Grants.Models)
		deltas.Endpoints = grantDelta(req.Grants.Endpoints)
		deltas.Assistants = grantDelta(req.Grants.Assistants)
		deltas.ExtractTemplates = grantDelta(req.Grants.ExtractTemplates)
	}
	// Provisional subset bound on the merged result the caller can see; the
	// locked transaction refuses the write outright when the stored row has
	// moved (409 stale_role), so this read can never mint unheld authority.
	merged := role
	if deltas.Name != nil {
		merged.Name = *deltas.Name
	}
	if deltas.Scopes != nil {
		merged.Scopes = *deltas.Scopes
	}
	if deltas.Models != nil {
		merged.Grants.Models = *deltas.Models
	}
	if deltas.Endpoints != nil {
		merged.Grants.Endpoints = *deltas.Endpoints
	}
	if deltas.Assistants != nil {
		merged.Grants.Assistants = *deltas.Assistants
	}
	if deltas.ExtractTemplates != nil {
		merged.Grants.ExtractTemplates = *deltas.ExtractTemplates
	}
	if !scopesSubsetOfCaller(principal, merged.Scopes) {
		WriteForbidden(w, "role scopes must be a subset of the caller's scopes")
		return
	}
	if !roleGrantsSubsetOfCaller(principal, merged.Grants) {
		WriteForbidden(w, "role grants exceed the caller's resource authority")
		return
	}
	updated, err := a.identity.UpdateRoleAtomically(r.Context(), principal.TenantID, role.ID, deltas, role.UpdatedAt)
	if err != nil {
		a.writeRoleWriteError(w, err, "role_update_failed")
		return
	}
	a.recordSecurityAudit(r, principal, "identity.role.updated", "role", role.ID, "success")
	userIDs, err := a.identity.RoleUsers(r.Context(), principal.TenantID, role.ID)
	if err != nil {
		log.Printf("role users %s: %v", role.ID, err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "roles_list_failed")
		return
	}
	WriteJSON(w, http.StatusOK, roleDTO(updated, userIDs))
}

// handleIdentityRoleDelete refuses a membered role (409 role_has_members),
// the tenant's last platform.admin grantor (409 last_admin) and absent or
// foreign-tenant roles (404) — the member check, the holder check and the
// delete commit atomically inside DeleteRoleIfUnused.
func (a *App) handleIdentityRoleDelete(w http.ResponseWriter, r *http.Request, principal auth.Principal, roleID string) {
	if err := a.identity.DeleteRoleIfUnused(r.Context(), principal.TenantID, roleID); err != nil {
		a.writeRoleWriteError(w, err, "role_delete_failed")
		return
	}
	a.recordSecurityAudit(r, principal, "identity.role.deleted", "role", roleID, "success")
	WriteJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": roleID})
}

// writeRoleWriteError maps the role-administration sentinels onto their HTTP
// outcomes; everything else falls through to the identity error mapping
// (auth.ErrNotFound → 404, scope denials → 403, store failures → 500).
func (a *App) writeRoleWriteError(w http.ResponseWriter, err error, code string) {
	switch {
	case errors.Is(err, postgres.ErrStaleRole):
		WriteOpenAIError(w, http.StatusConflict, "role changed since it was read — reload and retry", "invalid_request_error", "stale_role")
	case errors.Is(err, postgres.ErrLastAdmin):
		WriteOpenAIError(w, http.StatusConflict, "operation would remove the last usable administrator", "invalid_request_error", "last_admin")
	case errors.Is(err, postgres.ErrRoleHasMembers):
		WriteOpenAIError(w, http.StatusConflict, "role still has members", "invalid_request_error", "role_has_members")
	default:
		a.writeIdentityError(w, err, code)
	}
}

// --- service accounts ------------------------------------------------------

func (a *App) handleIdentityServiceAccounts(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	setNoStoreHeaders(w)
	if !a.identityConfigured(w) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		accounts, err := a.identity.ListServiceAccounts(r.Context(), principal.TenantID)
		if err != nil {
			log.Printf("list service accounts: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "service_accounts_list_failed")
			return
		}
		out := make([]map[string]any, 0, len(accounts))
		for _, sa := range accounts {
			out = append(out, serviceAccountDTO(sa))
		}
		WriteJSON(w, http.StatusOK, map[string]any{"service_accounts": out})
	case http.MethodPost:
		var req identityServiceAccountRequest
		if err := DecodeJSON(r, &req); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" {
			WriteOpenAIError(w, http.StatusBadRequest, "name is required", "invalid_request_error", "missing_name")
			return
		}
		if !scopesSubsetOfCaller(principal, req.Scopes) {
			WriteForbidden(w, "service account scopes must be a subset of the caller's scopes")
			return
		}
		sa := auth.ServiceAccount{
			ID: NewID("svcacct"), TenantID: principal.TenantID,
			Name: name, Scopes: req.Scopes, Status: "active",
		}
		if err := a.identity.CreateServiceAccount(r.Context(), principal.TenantID, sa); err != nil {
			log.Printf("create service account: %v", err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "service_account_create_failed")
			return
		}
		WriteJSON(w, http.StatusCreated, serviceAccountDTO(sa))
	default:
		WriteMethodNotAllowed(w)
	}
}

func (a *App) handleIdentityServiceAccountByID(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	setNoStoreHeaders(w)
	if !a.identityConfigured(w) {
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/identities/service-accounts/"), "/")
	if id == "" || strings.Contains(id, "/") {
		WriteOpenAIError(w, http.StatusBadRequest, "service account id is required", "invalid_request_error", "missing_id")
		return
	}
	sa, err := a.identity.GetServiceAccount(r.Context(), principal.TenantID, id)
	if err != nil {
		a.writeIdentityError(w, err, "service_account_fetch_failed")
		return
	}
	switch r.Method {
	case http.MethodGet:
		WriteJSON(w, http.StatusOK, serviceAccountDTO(sa))
	case http.MethodPatch, http.MethodPut:
		var req identityServiceAccountRequest
		if err := DecodeJSON(r, &req); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
			return
		}
		status := strings.TrimSpace(req.Status)
		if status != "" && status != "active" && status != "disabled" {
			WriteOpenAIError(w, http.StatusBadRequest, "status must be 'active' or 'disabled'", "invalid_request_error", "invalid_status")
			return
		}
		if status != "" && status != "active" && sa.Status == "active" {
			// A disabled service account takes its keys down with it; if
			// those keys held the tenant's last admin grant the write is
			// refused — the holder count and the status write commit
			// together under the tenant authority lock.
			keys, err := a.identity.ListAPIKeys(r.Context(), principal.TenantID)
			if err != nil {
				log.Printf("list keys for service account guard: %v", err)
				WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "api_keys_list_failed")
				return
			}
			for _, k := range keys {
				if k.ServiceAccountID == sa.ID && k.RevokedAt == nil && keyRecordHasScope(k, auth.ScopePlatformAdmin) {
					if err := a.identity.SetServiceAccountStatusAtomically(r.Context(), principal.TenantID, sa.ID, status); err != nil {
						a.writeRoleWriteError(w, err, "service_account_update_failed")
						return
					}
					sa.Status = status
					break
				}
			}
		}
		if status != "" && status != sa.Status {
			if err := a.identity.UpdateServiceAccountStatus(r.Context(), principal.TenantID, sa.ID, status); err != nil {
				log.Printf("update service account %s: %v", sa.ID, err)
				WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "service_account_update_failed")
				return
			}
			sa.Status = status
		}
		WriteJSON(w, http.StatusOK, serviceAccountDTO(sa))
	default:
		WriteMethodNotAllowed(w)
	}
}

// keyRecordHasScope decodes a key's stored scope grant — only an explicit
// items list carries scopes (mode:all decodes to the empty set by contract).
func keyRecordHasScope(k auth.APIKeyRecord, scope string) bool {
	items, _ := auth.DecodeAllowlist(k.Scopes)
	_, ok := items[scope]
	return ok
}

// --- current account (self-service) ----------------------------------------

// handleIdentityMe serves the authenticated user's own account. The caller is
// already authenticated by any scope; only a session/user principal maps to a
// stored user record — service-account principals have none.
func (a *App) handleIdentityMe(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	setNoStoreHeaders(w)
	if !a.identityConfigured(w) {
		return
	}
	if principal.UserID == "" {
		WriteForbidden(w, "a user identity is required")
		return
	}
	user, err := a.identity.GetUser(r.Context(), principal.TenantID, principal.UserID)
	if err != nil {
		a.writeIdentityError(w, err, "user_fetch_failed")
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/identities/me"), "/")
	segs := strings.Split(rest, "/")
	if segs[0] == "" {
		if r.Method != http.MethodGet {
			WriteMethodNotAllowed(w)
			return
		}
		roles, err := a.identity.RolesForUser(r.Context(), principal.TenantID, user.ID)
		if err != nil {
			log.Printf("roles for user %s: %v", user.ID, err)
			WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "roles_list_failed")
			return
		}
		WriteJSON(w, http.StatusOK, redactUser(user, roles))
		return
	}
	switch segs[0] {
	case "password":
		if len(segs) != 1 || r.Method != http.MethodPost {
			WriteMethodNotAllowed(w)
			return
		}
		var req identityPasswordRequest
		if err := DecodeJSON(r, &req); err != nil {
			WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
			return
		}
		// Self-service changes re-prove the current credential — a stolen
		// session alone cannot take over the account.
		if !auth.VerifyPassword(user.PasswordHash, req.CurrentPassword) {
			WriteForbidden(w, "current password is incorrect")
			return
		}
		a.setUserPassword(w, r, principal, user, req.Password)
	case "sessions":
		if len(segs) == 1 {
			if r.Method != http.MethodGet {
				WriteMethodNotAllowed(w)
				return
			}
			sessions, err := a.identity.ListSessionsForUser(r.Context(), principal.TenantID, user.ID)
			if err != nil {
				log.Printf("list sessions for %s: %v", user.ID, err)
				WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "sessions_list_failed")
				return
			}
			out := make([]map[string]any, 0, len(sessions))
			for _, s := range sessions {
				out = append(out, redactSession(s, principal.SessionID))
			}
			WriteJSON(w, http.StatusOK, map[string]any{"sessions": out})
			return
		}
		if len(segs) != 2 || (r.Method != http.MethodDelete && r.Method != http.MethodPost) {
			WriteMethodNotAllowed(w)
			return
		}
		a.revokeUserSession(w, r, principal, user.ID, segs[1])
	case "identities":
		if len(segs) != 1 || r.Method != http.MethodGet {
			WriteMethodNotAllowed(w)
			return
		}
		a.listAuthIdentities(w, r, principal.TenantID, user.ID)
	default:
		WriteOpenAIError(w, http.StatusNotFound, "not found", "invalid_request_error", "not_found")
	}
}

// --- explicit OIDC linking --------------------------------------------------

// handleIdentityLink redeems a pending link challenge for the authenticated
// caller: the verified issuer+subject binds to THIS account only after the
// account's own password verifies again. Email equality never binds — the
// email is resolved server-side from the caller's stored user record.
func (a *App) handleIdentityLink(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	setNoStoreHeaders(w)
	if !a.identityConfigured(w) {
		return
	}
	if r.Method != http.MethodPost {
		WriteMethodNotAllowed(w)
		return
	}
	if a.sessionAuth == nil || a.sessionAuth.oidc == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "OIDC linking is not configured", "server_error", "not_configured")
		return
	}
	if principal.UserID == "" {
		WriteForbidden(w, "a user identity is required")
		return
	}
	var req identityLinkRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
		return
	}
	if strings.TrimSpace(req.LinkToken) == "" {
		WriteOpenAIError(w, http.StatusBadRequest, "link_token is required", "invalid_request_error", "missing_link_token")
		return
	}
	user, err := a.identity.GetUser(r.Context(), principal.TenantID, principal.UserID)
	if err != nil {
		a.writeIdentityError(w, err, "user_fetch_failed")
		return
	}
	res, err := a.sessionAuth.oidc.ConfirmLink(r.Context(), principal.TenantID, req.LinkToken, user.Email, req.Password)
	if err != nil || res.UserID != principal.UserID {
		// A challenge issued for another account, a wrong password, or a
		// replayed token all surface identically: the link is refused.
		WriteForbidden(w, "account linking failed — the challenge or credentials are invalid")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"linked": true, "user_id": res.UserID})
}

// writeIdentityError maps auth.ErrNotFound to 404 and known privilege denials
// to 403; everything else is a store failure.
func (a *App) writeIdentityError(w http.ResponseWriter, err error, code string) {
	if errors.Is(err, auth.ErrNotFound) {
		WriteOpenAIError(w, http.StatusNotFound, "not found", "invalid_request_error", "not_found")
		return
	}
	if strings.Contains(err.Error(), "scope required") {
		WriteForbidden(w, err.Error())
		return
	}
	log.Printf("identity operation failed (%s): %v", code, err)
	WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", code)
}
