package rag

import (
	"context"
	"encoding/json"
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

// ImportArtifact explicitly copies a readable original into a knowledge
// collection; it never starts indexing until this call enqueues the embed job.
// ErrInvalidInput marks caller-input validation failures (HTTP 400);
// everything else from import paths is an internal failure.
var ErrInvalidInput = errors.New("rag input")

// ErrValidationAckRequired rejects importing an extraction result that
// failed output validation until the caller acknowledges the errors.
var ErrValidationAckRequired = errors.New("rag: extraction result needs review")

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
		InputArtifact: input.ArtifactRef,
		Restrictions:  []source.SourceRestriction{restrictions},
	})
}

// validImportOperation covers the stored job operations whose completed
// results may be imported into knowledge: document previews and the
// extract.process / extract.vision extraction results ("Add to knowledge").
func validImportOperation(operation string) bool {
	switch operation {
	case "document.preview", "extract.process", "extract.vision":
		return true
	}
	return false
}

// ImportStoredResult imports only a completed result belonging to the caller;
// its original input must independently remain readable at import time.
// ResultJobID is a JOB ID — the wire field `result_ref` keeps its name for
// compatibility but always names a durable job row.
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
	if err != nil || job.Status != "succeeded" || result.Status != "succeeded" || result.ResultRef == "" || result.InputRef == "" || !validImportOperation(result.Operation) {
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
	fileName, mimeType := "generated-result.txt", "text/plain"
	if result.Operation == "extract.process" || result.Operation == "extract.vision" {
		// Extraction result artifacts are the durable JSON result view.
		fileName, mimeType = "extraction-result.json", "application/json"
		var doc struct {
			ValidationErrors []string `json:"validation_errors"`
		}
		// An undecodable result is denied by provenance, not by validation —
		// only a real stored error list demands acknowledgement.
		if json.Unmarshal(data, &doc) == nil && len(doc.ValidationErrors) > 0 &&
			!input.AcknowledgeValidation {
			return UploadDocumentOutput{}, fmt.Errorf("%w: %d validation error(s) require acknowledgement",
				ErrValidationAckRequired, len(doc.ValidationErrors))
		}
	}
	return s.importBytes(ctx, p, input.CollectionID, input.ResultJobID, "generated", UploadDocumentInput{
		FileName: fileName, MimeType: mimeType, Bytes: data,
		SourceType: "generated", SourceID: input.ResultJobID, ProvenanceKind: "generated",
		InputArtifact: result.InputRef, ResultJobID: input.ResultJobID,
		ResultArtifact: result.ResultRef, Restrictions: restrictions,
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
	objectID, complete, err := s.repo.ReserveKnowledgeImport(ctx, p.TenantID, collectionID, importRef, newID("ragdoc"), kind)
	if err != nil {
		return UploadDocumentOutput{}, err
	}
	if complete {
		doc, err := s.repo.GetDocument(ctx, p.TenantID, objectID)
		return UploadDocumentOutput{Document: doc}, err
	}
	if doc, err := s.repo.GetDocument(ctx, p.TenantID, objectID); err == nil {
		if err := s.repo.RecordKnowledgeImport(ctx, p.TenantID, collectionID, importRef, objectID, kind); err != nil {
			return UploadDocumentOutput{}, err
		}
		return UploadDocumentOutput{Document: doc}, nil
	} else if !errors.Is(err, source.ErrDenied) {
		return UploadDocumentOutput{}, err
	}
	document.ObjectID = objectID
	out, err := s.UploadDocument(ctx, p, collectionID, document)
	if err != nil {
		return UploadDocumentOutput{}, err
	}
	if err := s.repo.RecordKnowledgeImport(ctx, p.TenantID, collectionID, importRef, objectID, kind); err != nil {
		return UploadDocumentOutput{}, err
	}
	return out, nil
}
