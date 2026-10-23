package rag

import (
	"context"
	"errors"
	"fmt"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/source"
)

type ImportArtifactInput struct {
	CollectionID string `json:"collection_id"`
	ArtifactRef  string `json:"artifact_ref"`
}

type ImportResultInput struct {
	CollectionID string `json:"collection_id"`
	ResultJobID  string `json:"result_job_id"`
}

// ImportArtifact explicitly copies a readable original into a knowledge
// collection; it never starts indexing until this call enqueues the embed job.
// ErrInvalidInput marks caller-input validation failures (HTTP 400);
// everything else from import paths is an internal failure.
var ErrInvalidInput = errors.New("rag input")

func (s *Service) ImportArtifact(ctx context.Context, p auth.Principal, input ImportArtifactInput) (UploadDocumentOutput, error) {
	if input.ArtifactRef == "" || s.artifacts == nil {
		return UploadDocumentOutput{}, fmt.Errorf("%w: collection and artifact reference are required", ErrInvalidInput)
	}
	collection, err := s.GetCollection(ctx, p, input.CollectionID)
	if err != nil {
		return UploadDocumentOutput{}, err
	}
	if !collectionManageable(collection, p) {
		return UploadDocumentOutput{}, source.ErrDenied
	}
	object, version, err := s.repo.FindArtifactObjectVersion(ctx, p.TenantID, input.ArtifactRef)
	if err != nil || s.authorizer.CanReadVersion(ctx, p, object, version, source.ReadOptions{}) != nil {
		return UploadDocumentOutput{}, source.ErrDenied
	}
	restrictions, err := s.authorizer.EffectiveRestrictions(ctx, object, &version, input.ArtifactRef)
	if err != nil {
		return UploadDocumentOutput{}, err
	}
	reader, err := s.artifacts.Read(ctx, artifacts.Ref{TenantID: p.TenantID, ID: input.ArtifactRef, Kind: artifacts.KindOriginal})
	if err != nil {
		return UploadDocumentOutput{}, source.ErrDenied
	}
	defer func() { _ = reader.Close() }()
	data, err := readAll(reader)
	if err != nil {
		return UploadDocumentOutput{}, fmt.Errorf("read import artifact: %w", err)
	}
	return s.importBytes(ctx, p, input.CollectionID, input.ArtifactRef, "upload", UploadDocumentInput{
		FileName: version.OriginalFilename, MimeType: version.MimeType, Bytes: data,
		SourceType: "import", SourceID: input.ArtifactRef, ProvenanceKind: "upload",
		InputArtifact: input.ArtifactRef, Audience: []string{p.ActorID},
		Restrictions: []source.SourceRestriction{restrictions},
	})
}

// ImportStoredResult imports only a completed result belonging to the caller;
// its original input must independently remain readable at import time.
func (s *Service) ImportStoredResult(ctx context.Context, p auth.Principal, input ImportResultInput) (UploadDocumentOutput, error) {
	if s.jobs == nil || s.artifacts == nil || input.ResultJobID == "" {
		return UploadDocumentOutput{}, fmt.Errorf("%w: result job is required", ErrInvalidInput)
	}
	collection, err := s.GetCollection(ctx, p, input.CollectionID)
	if err != nil {
		return UploadDocumentOutput{}, err
	}
	if !collectionManageable(collection, p) {
		return UploadDocumentOutput{}, source.ErrDenied
	}
	job, err := s.jobs.Get(ctx, p, input.ResultJobID)
	if err != nil {
		return UploadDocumentOutput{}, source.ErrDenied
	}
	result, err := s.repo.GetStoredResult(ctx, p.TenantID, input.ResultJobID)
	if err != nil || job.Status != "succeeded" || result.Status != "succeeded" || result.ResultRef == "" || result.InputRef == "" || result.Operation != "document.preview" {
		return UploadDocumentOutput{}, source.ErrDenied
	}
	var restrictions []source.SourceRestriction
	object, version, sourceErr := s.repo.FindArtifactObjectVersion(ctx, p.TenantID, result.InputRef)
	if sourceErr == nil {
		if s.authorizer.CanReadVersion(ctx, p, object, version, source.ReadOptions{}) != nil {
			return UploadDocumentOutput{}, source.ErrDenied
		}
		effective, err := s.authorizer.EffectiveRestrictions(ctx, object, &version, result.InputRef)
		if err != nil {
			return UploadDocumentOutput{}, err
		}
		restrictions = append(restrictions, effective)
	} else if !errors.Is(sourceErr, source.ErrDenied) {
		return UploadDocumentOutput{}, sourceErr
	} else {
		rows, err := s.repo.RestrictionsForSubject(ctx, p.TenantID, source.RestrictionArtifact, result.InputRef)
		if err != nil {
			return UploadDocumentOutput{}, err
		}
		restrictions = rows
	}
	reader, err := s.artifacts.Read(ctx, artifacts.Ref{TenantID: p.TenantID, ID: result.ResultRef, Kind: artifacts.KindResult})
	if err != nil {
		return UploadDocumentOutput{}, source.ErrDenied
	}
	defer func() { _ = reader.Close() }()
	data, err := readAll(reader)
	if err != nil {
		return UploadDocumentOutput{}, fmt.Errorf("read result artifact: %w", err)
	}
	return s.importBytes(ctx, p, input.CollectionID, input.ResultJobID, "generated", UploadDocumentInput{
		FileName: "generated-result.txt", MimeType: "text/plain", Bytes: data,
		SourceType: "generated", SourceID: input.ResultJobID, ProvenanceKind: "generated",
		InputArtifact: result.InputRef, ResultJobID: input.ResultJobID,
		ResultArtifact: result.ResultRef, Audience: []string{p.ActorID}, Restrictions: restrictions,
	})
}

func (s *Service) importBytes(ctx context.Context, p auth.Principal, collectionID, importRef, kind string, document UploadDocumentInput) (UploadDocumentOutput, error) {
	collection, err := s.GetCollection(ctx, p, collectionID)
	if err != nil {
		return UploadDocumentOutput{}, err
	}
	if !collectionManageable(collection, p) {
		return UploadDocumentOutput{}, source.ErrDenied
	}
	objectID, found, err := s.repo.GetKnowledgeImport(ctx, p.TenantID, collectionID, importRef)
	if err != nil {
		return UploadDocumentOutput{}, err
	}
	if found {
		doc, err := s.repo.GetDocument(ctx, p.TenantID, objectID)
		return UploadDocumentOutput{Document: doc}, err
	}
	out, err := s.UploadDocument(ctx, p, collectionID, document)
	if err != nil {
		return UploadDocumentOutput{}, err
	}
	if err := s.repo.RecordKnowledgeImport(ctx, p.TenantID, collectionID, importRef, out.Document.ID, kind); err != nil {
		return UploadDocumentOutput{}, err
	}
	return out, nil
}
