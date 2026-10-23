package connectors

import (
	"context"
	"errors"
	"strings"

	"enclava-go/internal/auth"
	"enclava-go/internal/source"
)

type BindSourcePrincipalInput struct {
	ConnectionID         string `json:"connection_id"`
	NativeUserID         string `json:"source_user_native_id"`
	MappedUserID         string `json:"mapped_user_id"`
	VerificationEvidence string `json:"verification_evidence"`
}

func (s *Service) BindSourcePrincipal(ctx context.Context, p auth.Principal, input BindSourcePrincipalInput) (source.SourceIdentity, error) {
	if !auth.HasScope(p, "connectors.manage") || !auth.HasScope(p, auth.ScopePlatformAdmin) {
		return source.SourceIdentity{}, source.ErrDenied
	}
	if s.canonical == nil || strings.TrimSpace(input.ConnectionID) == "" || strings.TrimSpace(input.NativeUserID) == "" || strings.TrimSpace(input.MappedUserID) == "" || strings.TrimSpace(input.VerificationEvidence) == "" {
		return source.SourceIdentity{}, errors.New("connection, observed source user, mapped user, and administrator verification evidence are required")
	}
	conn, err := s.repo.GetConnection(ctx, p.TenantID, input.ConnectionID)
	if err != nil {
		return source.SourceIdentity{}, source.ErrDenied
	}
	identity, err := s.canonical.ObservedSourceIdentity(ctx, p.TenantID, conn.ID, strings.TrimSpace(input.NativeUserID))
	if err != nil || identity.NativeID == "" {
		return source.SourceIdentity{}, source.ErrDenied
	}
	exists, err := s.canonical.UserExists(ctx, p.TenantID, strings.TrimSpace(input.MappedUserID))
	if err != nil {
		return source.SourceIdentity{}, err
	}
	if !exists {
		return source.SourceIdentity{}, source.ErrDenied
	}
	identity.MappedUserID = strings.TrimSpace(input.MappedUserID)
	if err := s.canonical.InsertSourceIdentity(ctx, identity); err != nil {
		return source.SourceIdentity{}, err
	}
	return identity, nil
}

func (s *Service) RevokeSourceBinding(ctx context.Context, p auth.Principal, bindingID string) error {
	if !auth.HasScope(p, "connectors.manage") || !auth.HasScope(p, auth.ScopePlatformAdmin) {
		return source.ErrDenied
	}
	if s.canonical == nil || strings.TrimSpace(bindingID) == "" {
		return source.ErrDenied
	}
	identity, err := s.canonical.SourceIdentityByID(ctx, p.TenantID, bindingID)
	if err != nil {
		return source.ErrDenied
	}
	identity.MappedUserID = ""
	return s.canonical.InsertSourceIdentity(ctx, identity)
}

func (s *Service) RevokeConnectionSourceBinding(ctx context.Context, p auth.Principal, connectionID, bindingID string) error {
	if !auth.HasScope(p, "connectors.manage") || !auth.HasScope(p, auth.ScopePlatformAdmin) {
		return source.ErrDenied
	}
	identity, err := s.canonical.SourceIdentityByID(ctx, p.TenantID, bindingID)
	if err != nil || identity.ConnectionID != connectionID {
		return source.ErrDenied
	}
	return s.RevokeSourceBinding(ctx, p, bindingID)
}
func (s *Service) ListSourceBindings(ctx context.Context, p auth.Principal, connectionID string) ([]source.SourceIdentity, error) {
	if !auth.HasScope(p, "connectors.manage") || !auth.HasScope(p, auth.ScopePlatformAdmin) {
		return nil, source.ErrDenied
	}
	if s.canonical == nil {
		return nil, errors.New("source identity repository is not configured")
	}
	if _, err := s.repo.GetConnection(ctx, p.TenantID, connectionID); err != nil {
		return nil, source.ErrDenied
	}
	return s.canonical.SourceIdentitiesForConnection(ctx, p.TenantID, connectionID)
}
