// linking.go owns the identity-linking rules (AUTH-02/D-06): a verified
// issuer+subject resolves to a user ONLY through a stored auth_identities
// row. An unlinked identity never gains silent access — when a user with
// the verified email exists, access is gated behind an interactive link
// confirmation (ConfirmLink re-verifies that account's local password, then
// records linked_by). When no matching user exists there is no automatic
// provisioning on this path; the same link challenge decides which account
// binds. Identical emails never merge accounts.
package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// resolveIdentity maps a verified issuer+subject to a tenant user through
// stored auth_identities. Linked → the user (disabled status denies). Not
// linked → LinkRequired with a one-shot challenge row carrying the verified
// subject/email and the candidate user — the callback renders the link
// step; no session is ever issued here.
func (o *OIDCService) resolveIdentity(ctx context.Context, tenantID string, provider OIDCProvider, subject, email string, emailVerified bool, returnTo string) (FlowResult, error) {
	if subject == "" {
		return FlowResult{}, errors.New("id_token missing subject")
	}
	identity, err := o.repo.GetAuthIdentity(ctx, tenantID, provider.Issuer, subject)
	switch {
	case err == nil:
		user, err := o.repo.GetUser(ctx, tenantID, identity.UserID)
		if err != nil {
			return FlowResult{}, fmt.Errorf("resolve linked user: %w", err)
		}
		if user.Status != "active" {
			return FlowResult{}, errors.New("user is disabled")
		}
		return FlowResult{UserID: user.ID, ReturnTo: returnTo}, nil
	case !errors.Is(err, ErrNotFound):
		return FlowResult{}, fmt.Errorf("lookup auth identity: %w", err)
	}

	// Unlinked identity: find the candidate account by VERIFIED email only —
	// an unverified email claim never identifies an existing user.
	candidate := ""
	if emailVerified && email != "" {
		if user, err := o.repo.GetUserByEmail(ctx, tenantID, email); err == nil {
			if user.Status != "active" {
				return FlowResult{}, errors.New("user is disabled")
			}
			candidate = user.ID
		} else if !errors.Is(err, ErrNotFound) {
			return FlowResult{}, fmt.Errorf("lookup user by email: %w", err)
		}
	}
	return o.newLinkChallenge(ctx, tenantID, provider, subject, email, candidate, returnTo)
}

// newLinkChallenge stores a one-shot pending-link oidc_states row. The
// plaintext token returned to the callback is sha256-keyed into state_hash;
// consumed_at stays NULL so ConfirmLink reuses the same single-use consume.
func (o *OIDCService) newLinkChallenge(ctx context.Context, tenantID string, provider OIDCProvider, subject, email, candidateUserID, returnTo string) (FlowResult, error) {
	token, err := randomToken()
	if err != nil {
		return FlowResult{}, err
	}
	row := OIDCState{
		ID:                "olink_" + randomHex(12),
		StateHash:         oidcStateHash(token),
		ProviderID:        provider.ID,
		Nonce:             "link",
		PKCEVerifier:      "-",
		ReturnTo:          returnTo,
		Subject:           subject,
		Email:             email,
		PendingLinkUserID: candidateUserID,
		ExpiresAt:         time.Now().UTC().Add(oidcStateTTL),
	}
	if err := o.repo.CreateOIDCState(ctx, tenantID, row); err != nil {
		return FlowResult{}, err
	}
	return FlowResult{
		LinkRequired: true,
		LinkToken:    token,
		ProviderName: provider.Name,
		Email:        email,
		ReturnTo:     returnTo,
	}, nil
}

// ConfirmLink completes an interactive account link: the pending challenge
// must be unconsumed and unexpired, and the supplied local credentials must
// verify — when the challenge carries a candidate user the credentials must
// match THAT account; otherwise the caller picks the account by logging in.
// The identity row records linked_by_user_id and is consumed exactly once.
// Disabled users deny; no session is minted — the caller issues it.
func (o *OIDCService) ConfirmLink(ctx context.Context, tenantID, linkToken, email, password string) (FlowResult, error) {
	tokenHash := oidcStateHash(linkToken)
	challenge, err := o.repo.GetOIDCState(ctx, tenantID, tokenHash)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return FlowResult{}, errors.New("invalid or expired link challenge")
		}
		return FlowResult{}, err
	}
	if challenge.Subject == "" || challenge.ConsumedAt != nil {
		return FlowResult{}, errors.New("invalid or expired link challenge")
	}
	if time.Now().UTC().After(challenge.ExpiresAt) {
		return FlowResult{}, errors.New("link challenge expired")
	}
	provider, err := o.repo.GetOIDCProvider(ctx, tenantID, challenge.ProviderID)
	if err != nil {
		return FlowResult{}, fmt.Errorf("resolve OIDC provider: %w", err)
	}

	svc := &Service{repo: o.repo}
	principal, err := svc.AuthenticatePassword(ctx, tenantID, email, password)
	if err != nil {
		return FlowResult{}, err
	}
	if challenge.PendingLinkUserID != "" && principal.UserID != challenge.PendingLinkUserID {
		// The candidate account is fixed by the verified email — credentials
		// for a different account never redirect the link.
		return FlowResult{}, errInvalidCredential
	}
	if _, err := o.repo.ConsumeOIDCState(ctx, tenantID, tokenHash); err != nil {
		if errors.Is(err, ErrNotFound) {
			return FlowResult{}, errors.New("invalid or expired link challenge")
		}
		return FlowResult{}, err
	}
	err = o.repo.RecordAuthIdentity(ctx, tenantID, AuthIdentity{
		ID:             "aid_" + randomHex(12),
		UserID:         principal.UserID,
		Issuer:         provider.Issuer,
		Subject:        challenge.Subject,
		LinkedAt:       time.Now().UTC(),
		LinkedByUserID: principal.UserID,
	})
	if err != nil {
		// Unique (tenant, issuer, subject) collision means a concurrent link
		// won — the identity is already bound; deny rather than rebind.
		return FlowResult{}, fmt.Errorf("record auth identity: %w", err)
	}
	return FlowResult{UserID: principal.UserID, ReturnTo: challenge.ReturnTo}, nil
}

// ResolveTenantID is the login-path tenant locator used by the HTTP layer:
// the browser supplies a slug, the stored row supplies the UUID.
func (s *Service) ResolveTenantID(ctx context.Context, slug string) (string, error) {
	return s.repo.GetTenantBySlug(ctx, strings.ToLower(strings.TrimSpace(slug)))
}
