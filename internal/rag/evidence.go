package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		latest, err := s.repo.LatestObjectVersion(ctx, p.TenantID, record.ObjectID)
		if err != nil {
			if errors.Is(err, source.ErrDenied) {
				return nil, source.ErrDenied
			}
			return nil, err
		}
		currentGeneration, err := s.repo.CurrentPublishedGeneration(ctx, p.TenantID, record.CollectionID, record.ObjectID, versionRowID(latest))
		if err != nil {
			return nil, err
		}
		resolution := EvidenceResolution{ID: record.ID, Status: "superseded", CitedGeneration: record.Generation, Spans: record.SpanRefs}
		resolution.CurrentGeneration = currentGeneration
		if currentGeneration != 0 && versionRowID(latest) == record.ObjectVersionID && currentGeneration == record.Generation {
			resolution.Status = "current"
		}
		var provenance map[string]any
		_ = json.Unmarshal([]byte(version.Provenance), &provenance)
		resolution.SourceKind = stringFromPayload(provenance, "source_kind")
		resolution.Title = stringFromPayload(provenance, "title")
		if resolution.Title == "" {
			resolution.Title = version.OriginalFilename
		}
		resolution.NativeLink = safeNativeLink(stringFromPayload(provenance, "url"))
		if resolution.NativeLink == "" {
			resolution.NativeLink = safeNativeLink(stringFromPayload(provenance, "source_uri"))
		}
		if deep, err := s.githubDeepLink(ctx, p.TenantID, record, resolution.SourceKind, resolution.NativeLink); err != nil {
			return nil, err
		} else if deep != "" {
			resolution.NativeLink = deep
		}
		locations, err := s.evidenceLocations(ctx, p.TenantID, record.SpanRefs)
		if err != nil {
			return nil, err
		}
		resolution.Locations = locations
		if s.artifacts != nil && version.ArtifactRef != "" {
			resolution.RetainedCopyLabel = "retained copy"
			resolution.RetainedCopyURL = "/api/v1/rag/evidence/" + url.PathEscape(record.ID) + "/copy"
		}
		results = append(results, resolution)
	}
	return results, nil
}

type EvidenceText struct {
	EvidenceID   string        `json:"evidence_id"`
	DocumentID   string        `json:"document_id"`
	CollectionID string        `json:"collection_id"`
	Content      string        `json:"content"`
	Spans        []source.Span `json:"spans,omitempty"`
}

// ReadEvidence returns one chunk only while its exact published version and
// source authorization remain current.
func (s *Service) ReadEvidence(ctx context.Context, p auth.Principal, id string) (EvidenceText, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(p.TenantID) == "" {
		return EvidenceText{}, source.ErrDenied
	}
	record, err := s.repo.GetEvidence(ctx, p.TenantID, strings.TrimSpace(id))
	if denied, failure := deniedOrFailed(err); denied {
		return EvidenceText{}, source.ErrDenied
	} else if failure != nil {
		return EvidenceText{}, failure
	}
	object, _, err := s.authorizedEvidenceSource(ctx, p, record)
	if err != nil {
		if errors.Is(err, source.ErrDenied) {
			return EvidenceText{}, source.ErrDenied
		}
		return EvidenceText{}, err
	}
	if object.ObservedSeq != record.ObservedSeq {
		return EvidenceText{}, source.ErrDenied
	}
	current, err := s.repo.PublishedGenerationForVersion(ctx, p.TenantID, record.CollectionID, record.ObjectVersionID, record.Generation)
	if err != nil {
		return EvidenceText{}, err
	}
	if !current {
		return EvidenceText{}, source.ErrDenied
	}
	chunks, err := s.repo.ListGenerationChunks(ctx, p.TenantID, record.CollectionID, record.ObjectVersionID, record.Generation)
	if err != nil {
		return EvidenceText{}, err
	}
	for _, chunk := range chunks {
		if chunk.ID != record.ChunkID || chunk.DocumentID != record.ObjectID {
			continue
		}
		return EvidenceText{
			EvidenceID: record.ID, DocumentID: record.ObjectID,
			CollectionID: record.CollectionID, Content: chunk.Content, Spans: record.SpanRefs,
		}, nil
	}
	return EvidenceText{}, source.ErrDenied
}

// evidenceLocations resolves span block IDs against the retained parsed
// blocks — the single locator-resolution path for evidence views. Only
// persisted locator fields (page/box/table/clause/section) surface; blocks
// that were never parsed resolve to nothing and are never invented.
func (s *Service) evidenceLocations(ctx context.Context, tenantID string, spans []source.Span) ([]source.Locator, error) {
	var blockIDs []string
	seen := make(map[string]struct{}, len(spans))
	for _, span := range spans {
		if span.BlockID == "" {
			continue
		}
		if _, dup := seen[span.BlockID]; dup {
			continue
		}
		seen[span.BlockID] = struct{}{}
		blockIDs = append(blockIDs, span.BlockID)
	}
	if len(blockIDs) == 0 {
		return nil, nil
	}
	locators, err := s.repo.ListBlockLocators(ctx, tenantID, blockIDs)
	if err != nil {
		return nil, err
	}
	out := make([]source.Locator, 0, len(blockIDs))
	for _, id := range blockIDs {
		if locator, ok := locators[id]; ok {
			out = append(out, locator)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
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
	member, err := s.repo.CollectionHasObject(ctx, p.TenantID, record.CollectionID, record.ObjectID)
	if err != nil {
		return source.Object{}, source.ObjectVersion{}, err
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

// githubDeepLink turns a GitHub blob URL into a line anchor when the cited
// chunk's text is byte-exact inside the retained original — anything less
// exact (decoded transcodes, multi-block joins, missing artifact) keeps the
// plain blob URL rather than guessing a line range.
func (s *Service) githubDeepLink(ctx context.Context, tenantID string, record EvidenceRecord, kind, link string) (string, error) {
	if s.artifacts == nil || kind != "github" || !strings.Contains(link, "/blob/") || strings.Contains(link, "#") {
		return "", nil
	}
	chunks, err := s.repo.ListGenerationChunks(ctx, tenantID, record.CollectionID, record.ObjectVersionID, record.Generation)
	if err != nil {
		return "", err
	}
	var content string
	for _, chunk := range chunks {
		if chunk.ID == record.ChunkID && chunk.DocumentID == record.ObjectID {
			content = chunk.Content
			break
		}
	}
	if content == "" {
		return "", nil
	}
	version, err := s.repo.GetObjectVersion(ctx, tenantID, record.ObjectID, record.ObservedSeq)
	if err != nil {
		return "", nil // version not resolvable for anchoring; keep blob link
	}
	reader, err := s.artifacts.Read(ctx, artifacts.Ref{TenantID: tenantID, ID: version.ArtifactRef, Kind: artifacts.KindOriginal})
	if err != nil {
		return "", nil // no retained bytes, nothing to anchor against
	}
	defer func() { _ = reader.Close() }()
	raw, err := readAll(reader)
	if err != nil {
		return "", nil
	}
	idx := bytes.Index(raw, []byte(content))
	if idx < 0 || bytes.LastIndex(raw, []byte(content)) != idx {
		return "", nil // must occur exactly once to anchor a unique range
	}
	startLine := bytes.Count(raw[:idx], []byte("\n")) + 1
	endLine := bytes.Count(raw[:idx+len(content)-1], []byte("\n")) + 1
	if startLine == endLine {
		return fmt.Sprintf("%s#L%d", link, startLine), nil
	}
	return fmt.Sprintf("%s#L%d-L%d", link, startLine, endLine), nil
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
	defer func() { _ = reader.Close() }()
	return readAll(reader)
}
