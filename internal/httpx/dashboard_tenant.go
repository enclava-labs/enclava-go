// dashboard_tenant.go — the shell's userrow + org-name assembly (OA-10).
// Best-effort reads: the shell renders even when a lookup fails, falling back
// to the identifiers the principal already carries.
package httpx

import (
	"context"
	"strings"
	"unicode"

	"enclava-go/internal/auth"
)

// dashboardUser is the sidebar userrow model.
type dashboardUser struct {
	Initials, Name, Email, RoleLabel string
}

// dashboardUserFor resolves the signed-in user's display identity and role
// names; repo failures degrade to principal-backed fallbacks, never errors.
func (a *App) dashboardUserFor(ctx context.Context, p auth.Principal) dashboardUser {
	var u dashboardUser
	if a.identity != nil && p.TenantID != "" && p.UserID != "" {
		if row, err := a.identity.GetUser(ctx, p.TenantID, p.UserID); err == nil {
			u.Name, u.Email = row.DisplayName, row.Email
		}
		if roles, err := a.identity.RolesForUser(ctx, p.TenantID, p.UserID); err == nil {
			names := make([]string, 0, len(roles))
			for _, r := range roles {
				if r.Name != "" {
					names = append(names, r.Name)
				}
			}
			u.RoleLabel = strings.Join(names, ", ")
		}
	}
	if u.RoleLabel == "" {
		u.RoleLabel = "—"
	}
	if u.Name == "" {
		u.Name = u.Email
	}
	if u.Name == "" {
		u.Name = p.UserID
	}
	u.Initials = dashboardInitials(u.Name, u.Email)
	return u
}

// dashboardInitials renders up to two uppercase initials: first letters of
// display-name words, falling back to the email local-part.
func dashboardInitials(name, email string) string {
	src := strings.TrimSpace(name)
	if src == "" {
		src = strings.TrimSpace(email)
	}
	if src == "" {
		return "?"
	}
	if i := strings.IndexByte(src, '@'); i > 0 {
		src = src[:i]
	}
	words := strings.FieldsFunc(src, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	out := make([]rune, 0, 2)
	for _, w := range words {
		for _, r := range w {
			out = append(out, unicode.ToUpper(r))
			break
		}
		if len(out) == 2 {
			break
		}
	}
	// A single word still gets its second letter so the avatar reads "MA".
	if len(out) == 1 && len(words) > 0 {
		w := []rune(words[0])
		if len(w) > 1 {
			out = append(out, unicode.ToUpper(w[1]))
		}
	}
	if len(out) == 0 {
		return "?"
	}
	return string(out)
}

// dashboardOrgName resolves the tenant display name, falling back to the
// humanized session tenant slug, then to the product name.
func (a *App) dashboardOrgName(ctx context.Context, tenantID string) string {
	if a.identity != nil && tenantID != "" {
		if name, err := a.identity.GetTenantDisplayName(ctx, tenantID); err == nil && strings.TrimSpace(name) != "" {
			return name
		}
	}
	if a.sessionAuth != nil {
		if org := dashboardTitleWords(a.sessionAuth.tenantSlug); org != "" {
			return org
		}
	}
	return "Enclava"
}

// dashboardTitleWords humanizes a slug into Title words ("acme-corp" →
// "Acme Corp"); "" stays "".
func dashboardTitleWords(s string) string {
	words := strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	out := make([]string, 0, len(words))
	for _, w := range words {
		r := []rune(w)
		out = append(out, string(unicode.ToUpper(r[0]))+string(r[1:]))
	}
	return strings.Join(out, " ")
}
