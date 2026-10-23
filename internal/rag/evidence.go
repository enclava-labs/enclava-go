package rag

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/source"
)

// ResolveEvidence returns no per-ID distinction for unknown, cross-tenant, tombstoned, or forbidden IDs.
func (s *Service) ResolveEvidence(ctx context.Context, p auth.Principal, ids []string) ([]EvidenceResolution, error) {
	results := make([]EvidenceResolution, 0, len(ids))
	for _, id := range ids {
		record, err := s.repo.GetEvidence(ctx, p.TenantID, strings.TrimSpace(id))
		if err != nil {
			if errors.Is(err, source.ErrDenied) {
				return nil, source.ErrDenied
			}
			return nil, err
		}
		_, version, err := s.authorizedEvidenceSource(ctx, p, record)
		if err != nil {
			return nil, err
		}
		generationList, err := s.repo.ListPublishedGenerations(ctx, p.TenantID, record.CollectionID)
		if err != nil {
			return nil, err
		}
		latest, err := s.repo.LatestObjectVersion(ctx, p.TenantID, record.ObjectID)
		if err != nil {
			if errors.Is(err, source.ErrDenied) {
				return nil, source.ErrDenied
			}
			return nil, err
		}
		resolution := EvidenceResolution{ID: record.ID, Status: "superseded", CitedGeneration: record.Generation, Spans: record.SpanRefs}
		for _, generation := range generationList {
			if generation.ObjectID == record.ObjectID && generation.ObjectVersionID == versionRowID(latest) {
				resolution.CurrentGeneration = generation.Generation
				if versionRowID(latest) == record.ObjectVersionID && generation.Generation == record.Generation {
					resolution.Status = "current"
				}
			}
		}
		var provenance map[string]any
		_ = json.Unmarshal([]byte(version.Provenance), &provenance)
		resolution.NativeLink = safeNativeLink(stringFromPayload(provenance, "url"))
		if resolution.NativeLink == "" {
			resolution.NativeLink = safeNativeLink(stringFromPayload(provenance, "source_uri"))
		}
		if s.artifacts != nil && version.ArtifactRef != "" {
			resolution.RetainedCopyLabel = "retained copy"
			resolution.RetainedCopyURL = "/api/v1/rag/evidence/" + url.PathEscape(record.ID) + "/copy"
		}
		results = append(results, resolution)
	}
	return results, nil
}

func (s *Service) authorizedEvidenceSource(ctx context.Context, p auth.Principal, record EvidenceRecord) (source.Object, source.ObjectVersion, error) {
	if _, err := s.GetCollection(ctx, p, record.CollectionID); err != nil {
		if errors.Is(err, source.ErrDenied) {
			return source.Object{}, source.ObjectVersion{}, source.ErrDenied
		}
		return source.Object{}, source.ObjectVersion{}, err
	}
	object, err := s.repo.GetObject(ctx, p.TenantID, record.ObjectID)
	if denied, failure := deniedOrFailed(err); denied {
		return source.Object{}, source.ObjectVersion{}, source.ErrDenied
	} else if failure != nil {
		return source.Object{}, source.ObjectVersion{}, failure
	}
	if err := s.authorizer.CanRead(ctx, p, object, source.ReadOptions{}); err != nil {
		if errors.Is(err, source.ErrDenied) {
			return source.Object{}, source.ObjectVersion{}, source.ErrDenied
		}
		return source.Object{}, source.ObjectVersion{}, err
	}
	items, err := s.repo.ListCollectionItems(ctx, p.TenantID, record.CollectionID)
	if err != nil {
		return source.Object{}, source.ObjectVersion{}, err
	}
	member := false
	for _, id := range items {
		if id == record.ObjectID {
			member = true
			break
		}
	}
	if !member {
		return source.Object{}, source.ObjectVersion{}, source.ErrDenied
	}
	version, err := s.repo.GetObjectVersion(ctx, p.TenantID, record.ObjectID, record.ObservedSeq)
	if denied, failure := deniedOrFailed(err); denied {
		return source.Object{}, source.ObjectVersion{}, source.ErrDenied
	} else if failure != nil {
		return source.Object{}, source.ObjectVersion{}, failure
	}
	if versionRowID(version) != record.ObjectVersionID {
		return source.Object{}, source.ObjectVersion{}, source.ErrDenied
	}
	if err := s.authorizer.CanReadVersion(ctx, p, object, version, source.ReadOptions{}); err != nil {
		if errors.Is(err, source.ErrDenied) {
			return source.Object{}, source.ObjectVersion{}, source.ErrDenied
		}
		return source.Object{}, source.ObjectVersion{}, err
	}
	return object, version, nil
}

func safeNativeLink(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return ""
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https", "http", "slack", "notion":
		return parsed.String()
	}
	return ""
}

// DownloadEvidenceCopy reauthorizes the immutable source version before opening retained bytes.
func (s *Service) DownloadEvidenceCopy(ctx context.Context, p auth.Principal, id string) ([]byte, error) {
	record, err := s.repo.GetEvidence(ctx, p.TenantID, strings.TrimSpace(id))
	if err != nil {
		if errors.Is(err, source.ErrDenied) {
			return nil, source.ErrDenied
		}
		return nil, err
	}
	_, version, err := s.authorizedEvidenceSource(ctx, p, record)
	if err != nil {
		return nil, err
	}
	if s.artifacts == nil {
		return nil, errors.New("artifact store is not configured")
	}
	reader, err := s.artifacts.Read(ctx, artifacts.Ref{TenantID: p.TenantID, ID: version.ArtifactRef, Kind: artifacts.KindOriginal})
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return readAll(reader)
}
