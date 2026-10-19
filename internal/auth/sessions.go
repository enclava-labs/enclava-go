package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"
)

// sessionIdleWindow bounds how long an untouched session stays usable.
const sessionIdleWindow = 30 * time.Minute

// IssueSession mints an opaque bearer token for a stored user, persists only
// its sha256 hash plus a per-session CSRF token, and returns the plaintext
// token + CSRF token exactly once.
func (s *Service) IssueSession(ctx context.Context, tenantID, userID string, ttl time.Duration) (token string, csrfToken string, sessionID string, err error) {
	if ttl <= 0 {
		return "", "", "", errors.New("session ttl must be positive")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", "", err
	}
	token = "ses_" + base64.RawURLEncoding.EncodeToString(raw)
	csrfRaw := make([]byte, 32)
	if _, err := rand.Read(csrfRaw); err != nil {
		return "", "", "", err
	}
	csrfToken = base64.RawURLEncoding.EncodeToString(csrfRaw)
	now := time.Now().UTC()
	idle := now.Add(sessionIdleWindow)
	expires := now.Add(ttl)
	if idle.After(expires) {
		idle = expires
	}
	session := Session{
		ID:            "sess_" + randomHex(12),
		TenantID:      tenantID,
		UserID:        userID,
		TokenHash:     sessionTokenHash(token),
		CSRFToken:     csrfToken,
		CreatedAt:     now,
		ExpiresAt:     expires,
		IdleExpiresAt: idle,
	}
	if err := s.repo.CreateSession(ctx, tenantID, session); err != nil {
		return "", "", "", err
	}
	return token, csrfToken, session.ID, nil
}

// AuthenticateSession validates an opaque session token: sha256 token_hash
// pre-auth lookup, then expiry, idle (with TouchSession sliding renewal) and
// revocation checks. Tenant resolves from the verified session row only.
func (s *Service) AuthenticateSession(ctx context.Context, token string) (Principal, error) {
	if token == "" {
		return Principal{}, errors.New("missing session token")
	}
	session, err := s.repo.GetSessionByTokenHash(ctx, sessionTokenHash(token))
	if err != nil {
		return Principal{}, errInvalidCredential
	}
	now := time.Now().UTC()
	if session.RevokedAt != nil {
		return Principal{}, errors.New("session is revoked")
	}
	if now.After(session.ExpiresAt) {
		return Principal{}, errors.New("session is expired")
	}
	if now.After(session.IdleExpiresAt) {
		return Principal{}, errors.New("session idle expired")
	}
	user, err := s.repo.GetUser(ctx, session.TenantID, session.UserID)
	if err != nil {
		return Principal{}, errInvalidCredential
	}
	if user.Status != "active" {
		return Principal{}, errors.New("user is disabled")
	}
	principal, err := s.principalForUser(ctx, user)
	if err != nil {
		return Principal{}, err
	}
	principal.CredentialID = session.ID
	principal.CredentialKind = CredentialKindSession
	principal.SessionID = session.ID
	// Sliding idle renewal capped by the absolute expiry.
	idle := now.Add(sessionIdleWindow)
	if idle.After(session.ExpiresAt) {
		idle = session.ExpiresAt
	}
	_ = s.repo.TouchSession(ctx, session.TenantID, session.ID, idle)
	return principal, nil
}

// RevokeSession denies the very next AuthenticateSession for that token.
func (s *Service) RevokeSession(ctx context.Context, tenantID, sessionID, reason string) error {
	return s.repo.RevokeSession(ctx, tenantID, sessionID, time.Now().UTC(), reason)
}

// RevokeUserSessions revokes every session for a user (disable/logout path).
func (s *Service) RevokeUserSessions(ctx context.Context, tenantID, userID, reason string) error {
	return s.repo.RevokeUserSessions(ctx, tenantID, userID, time.Now().UTC(), reason)
}

// ValidateCSRF constant-time compares a supplied token against the stored
// per-session CSRF token; cookie-authenticated mutations require it.
func (s *Service) ValidateCSRF(ctx context.Context, tenantID, sessionID, supplied string) error {
	if supplied == "" {
		return errors.New("missing CSRF token")
	}
	session, err := s.repo.GetSession(ctx, tenantID, sessionID)
	if err != nil {
		return err
	}
	if session.RevokedAt != nil {
		return errors.New("session is revoked")
	}
	if subtle.ConstantTimeCompare([]byte(session.CSRFToken), []byte(supplied)) != 1 {
		return errors.New("invalid CSRF token")
	}
	return nil
}

func sessionTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
