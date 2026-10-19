package auth_test

import (
	"context"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/testdb"
)

func TestSessionLifecycleAndRevocation(t *testing.T) {
	db, repo, svc, tenant := seedEnv(t)
	user := testdb.SeedUser(t, db, tenant, "s@example.com", []string{"models.list"})
	ctx := context.Background()

	token, csrf, sessionID, err := svc.IssueSession(ctx, tenant, user.ID, time.Hour)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	if token == "" || csrf == "" || sessionID == "" {
		t.Fatalf("empty token/csrf/id")
	}
	p, err := svc.AuthenticateSession(ctx, token)
	if err != nil {
		t.Fatalf("authenticate session: %v", err)
	}
	if p.TenantID != tenant || p.UserID != user.ID || p.SessionID != sessionID ||
		p.CredentialKind != auth.CredentialKindSession {
		t.Fatalf("principal = %+v", p)
	}
	if !auth.HasScope(p, "models.list") {
		t.Fatalf("role scopes missing: %v", p.Scopes)
	}
	if err := svc.ValidateCSRF(ctx, tenant, sessionID, csrf); err != nil {
		t.Fatalf("issued csrf rejected: %v", err)
	}
	if err := svc.ValidateCSRF(ctx, tenant, sessionID, "forged"); err == nil {
		t.Fatalf("forged csrf accepted")
	}

	if err := svc.RevokeSession(ctx, tenant, sessionID, "test"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := svc.AuthenticateSession(ctx, token); err == nil {
		t.Fatalf("revoked session must deny the next request")
	}
	// RevokeUserSessions denies all remaining sessions.
	token2, _, _, err := svc.IssueSession(ctx, tenant, user.ID, time.Hour)
	if err != nil {
		t.Fatalf("issue second session: %v", err)
	}
	if err := svc.RevokeUserSessions(ctx, tenant, user.ID, "logout-all"); err != nil {
		t.Fatalf("revoke all: %v", err)
	}
	if _, err := svc.AuthenticateSession(ctx, token2); err == nil {
		t.Fatalf("user-revoked session must deny")
	}
	_ = repo
}

func TestSessionAbsoluteAndIdleExpiry(t *testing.T) {
	db, repo, svc, tenant := seedEnv(t)
	user := testdb.SeedUser(t, db, tenant, "s2@example.com", nil)
	ctx := context.Background()

	token, _, sessionID, err := svc.IssueSession(ctx, tenant, user.ID, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	// Force absolute expiry.
	if _, err := db.SQL.ExecContext(ctx,
		`UPDATE sessions SET expires_at = now() - interval '1 minute' WHERE tenant_id = $1 AND id = $2`,
		tenant, sessionID); err != nil {
		t.Fatalf("expire session: %v", err)
	}
	if _, err := svc.AuthenticateSession(ctx, token); err == nil {
		t.Fatalf("expired session must deny")
	}

	// Idle expiry: issued fresh, idle window pushed into the past.
	token2, _, sessionID2, err := svc.IssueSession(ctx, tenant, user.ID, time.Hour)
	if err != nil {
		t.Fatalf("issue second: %v", err)
	}
	if _, err := db.SQL.ExecContext(ctx,
		`UPDATE sessions SET idle_expires_at = now() - interval '1 minute' WHERE tenant_id = $1 AND id = $2`,
		tenant, sessionID2); err != nil {
		t.Fatalf("idle-expire session: %v", err)
	}
	if _, err := svc.AuthenticateSession(ctx, token2); err == nil {
		t.Fatalf("idle-expired session must deny")
	}
	_ = repo
}
