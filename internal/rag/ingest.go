package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"enclava-go/internal/auth"
	"enclava-go/internal/jobs"
	"enclava-go/internal/source"
)

// IngestJobHandler binds each observation's immutable artifact and source
// revision to the collection, then queues the normal fenced embedding job.
func (s *Service) IngestJobHandler() jobs.HandlerFunc {
	return func(ctx context.Context, claim *jobs.Claim, _ jobs.InputFunc) (string, error) {
		job := claim.Job()
		var principal auth.Principal
		if err := json.Unmarshal(job.PrincipalSnapshot, &principal); err != nil {
			return "", fmt.Errorf("decode ingest principal: %w", err)
		}
		var ref source.ObjectRef
		rawRef, err := json.Marshal(job.Policy["source_ref"])
		if err != nil || json.Unmarshal(rawRef, &ref) != nil {
			return "", errors.New("rag ingest: invalid source_ref")
		}
		if principal.TenantID != job.TenantID || ref.TenantID != job.TenantID || ref.ObjectID == "" || ref.ObservedSeq <= 0 || ref.ObjectID != job.Subject.ObjectID || ref.ObservedSeq != job.Subject.ObservedRevision {
			return "", source.ErrDenied
		}
		collectionID, _ := job.Policy["collection_id"].(string)
		artifactRef, _ := job.Policy["artifact_ref"].(map[string]any)
		artifactID, _ := artifactRef["ID"].(string)
		if artifactID == "" {
			artifactID, _ = artifactRef["id"].(string)
		}
		if collectionID == "" || artifactID == "" || artifactID != job.InputRef {
			return "", errors.New("rag ingest: missing collection or artifact reference")
		}
		if s.artifacts == nil || s.jobs == nil {
			return "", errors.New("rag: durable ingest pipeline is not configured")
		}
		collection, err := s.repo.GetCollection(ctx, job.TenantID, collectionID)
		if err != nil {
			return "", err
		}
		if !collection.IsActive {
			return "", errors.New("rag ingest: collection is disabled")
		}
		object, err := s.repo.GetObject(ctx, job.TenantID, ref.ObjectID)
		if err != nil {
			return "", err
		}
		if object.TenantID != ref.TenantID || object.ObservedSeq > ref.ObservedSeq || object.DeletedAt != nil || object.TombstonedAt != nil {
			return "", source.ErrDenied
		}
		var observed struct {
			Filename string `json:"filename"`
			MIMEType string `json:"mime_type"`
			NativeID string `json:"native_id"`
			URL      string `json:"url"`
		}
		if payload, ok := job.Policy["observation_payload"]; ok {
			b, _ := json.Marshal(payload)
			_ = json.Unmarshal(b, &observed)
		}
		if strings.TrimSpace(observed.Filename) == "" {
			observed.Filename = ref.ObjectID
		}
		if strings.TrimSpace(observed.MIMEType) == "" {
			observed.MIMEType = "application/octet-stream"
		}
		if err := claim.MarkDispatched(ctx); err != nil {
			return "", err
		}
		provenance, err := json.Marshal(map[string]string{
			"connection_id": job.Subject.ConnectionID,
			"source_kind":   fmt.Sprint(job.Policy["source_kind"]),
			"native_id":     observed.NativeID,
			"source_uri":    observed.URL,
		})
		if err != nil {
			return "", fmt.Errorf("rag ingest: encode connector provenance: %w", err)
		}
		version := source.ObjectVersion{
			ObjectID: ref.ObjectID, ObservedSeq: ref.ObservedSeq,
			ArtifactRef: artifactID, SHA256: job.InputHash,
			MimeType: observed.MIMEType, OriginalFilename: observed.Filename,
			Provenance: string(provenance),
		}
		if prior, getErr := s.repo.GetObjectVersion(ctx, job.TenantID, ref.ObjectID, ref.ObservedSeq); getErr == nil {
			if prior.ArtifactRef != version.ArtifactRef || prior.SHA256 != version.SHA256 {
				return "", errors.New("rag ingest: source revision conflicts with a stored version")
			}
		} else if err := s.repo.InsertObjectVersion(ctx, job.TenantID, version); err != nil {
			return "", err
		}
		if err := s.repo.AddCollectionItem(ctx, job.TenantID, collectionID, ref.ObjectID, principal.ActorID); err != nil {
			return "", err
		}
		versionID := fmt.Sprintf("%s#%d", ref.ObjectID, ref.ObservedSeq)
		generation, err := s.repo.NextGeneration(ctx, job.TenantID, collectionID, versionID)
		if err != nil {
			return "", err
		}
		if err := s.enqueueEmbed(ctx, principal, collection, ref.ObjectID, versionID, ref.ObservedSeq, generation); err != nil {
			return "", err
		}
		return versionID, nil
	}
}
