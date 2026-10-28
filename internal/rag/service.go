package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/extract"
	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
	"enclava-go/internal/source"
)

const (
	OperationEmbed  = "rag.embed"
	OperationIngest = "rag.ingest"
	OperationAccess = "rag.access"
)

// ErrProfileNotFound marks a chunk_profile_version_id that does not exist in
// the calling tenant — a caller-input failure (HTTP 400), distinct from
// infrastructure errors.
var ErrProfileNotFound = errors.New("chunk profile version not found")

// Repository is the tenant-scoped persistence boundary for rag.Service over
// the canonical source/object/version/domain schema. Implemented by
// postgres.RAGRepo. The embedded SourceRepository is the shared authorizer
// seam; rag additions cover knowledge collections, items, chunk profiles,
// chunks and index generations.
type Repository interface {
	source.SourceRepository

	// Collections.
	InsertCollection(ctx context.Context, c Collection) error
	ListCollections(ctx context.Context, tenantID string) ([]Collection, error)
	GetCollection(ctx context.Context, tenantID, id string) (Collection, error)
	UpdateCollection(ctx context.Context, c Collection) error
	DeleteCollection(ctx context.Context, tenantID, id string) error
	// EnsureChunkProfile returns the immutable chunk_profiles row for
	// (tenant, profileID, size, overlap), inserting version N+1 when the
	// (size, overlap) pair changed.
	EnsureChunkProfile(ctx context.Context, tenantID, profileID string, size, overlap int) (chunkProfileVersionID string, err error)
	SaveParsedDocument(ctx context.Context, parserVersion string, document source.Document) error
	// Collection items and document projection (latest version + latest
	// index generation state). ListDocuments filters by validated status
	// (all|ready|processing|failed|removed) in the same query; the failed /
	// tenant-wide reads are raw internal projections — the service
	// authorizes per principal BEFORE any caller sees rows.
	AddCollectionItem(ctx context.Context, tenantID, collectionID, objectID, addedBy string) error
	RemoveCollectionItem(ctx context.Context, tenantID, collectionID, objectID string) error
	MarkCollectionItemRemoved(ctx context.Context, tenantID, collectionID, objectID string) error
	ListCollectionItems(ctx context.Context, tenantID, collectionID string) ([]string, error)
	ListDocuments(ctx context.Context, tenantID, collectionID, status string, limit, offset int) ([]Document, error)
	ListFailedDocuments(ctx context.Context, tenantID string, limit, offset int) ([]Document, error)
	ListTenantDocuments(ctx context.Context, tenantID string, limit, offset int) ([]Document, error)
	CountDocuments(ctx context.Context, tenantID string, excludeRemoved bool) (int64, error)
	LiveCollectionSourceIDs(ctx context.Context, tenantID, collectionID string) (map[string]struct{}, error)
	GetDocument(ctx context.Context, tenantID, objectID string) (Document, error)
	ListBlockLocators(ctx context.Context, tenantID string, blockIDs []string) (map[string]source.Locator, error)
	ListPublishedGenerations(ctx context.Context, tenantID, collectionID string) ([]IndexGeneration, error)
	// ListPublishedGenerationsBounded is the deterministic, LIMIT-bounded
	// read the search paths use to cap enumeration work; it joins collection
	// items so only live members are examined. limit must be > 0.
	ListPublishedGenerationsBounded(ctx context.Context, tenantID, collectionID string, limit int) ([]IndexGeneration, error)
	// GetChunkProfile returns one immutable chunk profile version row for
	// the tenant, or ErrProfileNotFound.
	GetChunkProfile(ctx context.Context, tenantID, id string) (source.ChunkProfile, error)
	ListGenerationChunks(ctx context.Context, tenantID, collectionID, versionID string, generation int64) ([]DocumentChunk, error)
	SaveEvidence(ctx context.Context, record EvidenceRecord) error
	GetEvidence(ctx context.Context, tenantID, id string) (EvidenceRecord, error)

	// Index generations + chunks (single transactional publish boundary).
	// ReplaceVersionChunksAndStage deletes any prior chunk rows for the
	// version, inserts the new chunk set and upserts the (collection,
	// object_version, generation) row in 'staging' state — atomically.
	ReplaceVersionChunksAndStage(ctx context.Context, tenantID, collectionID, objectID, versionID string, seq, generation int64, profileVersionID string, chunks []DocumentChunk) error
	MarkGenerationState(ctx context.Context, tenantID, collectionID, versionID string, generation int64, state string) error
	NextGeneration(ctx context.Context, tenantID, collectionID, versionID string) (int64, error)
	FindArtifactObjectVersion(ctx context.Context, tenantID, artifactID string) (source.Object, source.ObjectVersion, error)
	ListPendingIndexReconciliations(ctx context.Context, tenantID string, limit int) ([]IndexReconciliation, error)
	CompleteIndexReconciliation(ctx context.Context, reconciliation IndexReconciliation) error
	ReserveKnowledgeImport(ctx context.Context, tenantID, collectionID, importRef, objectID, kind string) (reservedObjectID string, complete bool, err error)
	ListGenerationsForConnection(ctx context.Context, tenantID, connectionID string) ([]IndexGeneration, error)
	RecordKnowledgeImport(ctx context.Context, tenantID, collectionID, importRef, objectID, kind string) error
	GetStoredResult(ctx context.Context, tenantID, jobID string) (StoredResult, error)
	PublishGenerationFenced(ctx context.Context, tenantID, collectionID, objectID, versionID string, generation int64, jobID, leaseOwner, attemptID string, fenceToken int64) error
}

type VectorStore interface {
	EnsureCollection(ctx context.Context, collection string, vectorSize int) error
	UpsertPoints(ctx context.Context, collection string, points []QdrantPoint) error
	DeleteDocumentPoints(ctx context.Context, collection, documentID string) error
	DeleteGenerationPoints(ctx context.Context, collection, versionID string, generation int64) error
	SetPayload(ctx context.Context, collection string, filter map[string]any, payload map[string]any) error
	DeleteCollection(ctx context.Context, collection string) error
	SearchHybrid(ctx context.Context, collection string, vector []float64, sparse SparseVector, limit int, filter map[string]any) ([]QdrantSearchPoint, error)
}

// Executor is the shared authorized inference boundary every embedding
// attempt dispatches through; *inference.Service satisfies it.
type Executor interface {
	Execute(ctx context.Context, p auth.Principal, req inference.Request) (*inference.Result, error)
}

type Service struct {
	repo                  Repository
	vectorStore           VectorStore
	executor              Executor
	defaultEmbeddingModel string
	artifacts             *artifacts.Store
	parser                *extract.Parser
	jobs                  *jobs.Service
	authorizer            *source.Authorizer
	maxUploadMB           int64
}

// NewService builds the ported rag.Service. artifactStore and jobSvc may be
// nil only in narrow unit tests — durable ingest and priced embeddings
// require them; the executor is the single priced dispatch boundary and is
// required for any embedding operation.
func NewService(repo Repository, vectorStore VectorStore, executor Executor, defaultEmbeddingModel string, artifactStore *artifacts.Store, jobSvc *jobs.Service) *Service {
	if strings.TrimSpace(defaultEmbeddingModel) == "" {
		defaultEmbeddingModel = "nomic-embed-text"
	}
	return &Service{
		repo:                  repo,
		vectorStore:           vectorStore,
		executor:              executor,
		defaultEmbeddingModel: defaultEmbeddingModel,
		artifacts:             artifactStore,
		parser:                extract.NewParser(artifactStore),
		jobs:                  jobSvc,
		authorizer:            source.NewAuthorizer(repo, 0),
		maxUploadMB:           512,
	}
}

func requireTenant(p auth.Principal) error {
	if strings.TrimSpace(p.TenantID) == "" {
		return fmt.Errorf("%w: principal tenant is required", source.ErrDenied)
	}
	return nil
}

// ---------------------------------------------------------------------
// Collections

func (s *Service) CreateCollection(ctx context.Context, p auth.Principal, input CreateCollectionInput) (Collection, error) {
	if err := requireTenant(p); err != nil {
		return Collection{}, err
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return Collection{}, errors.New("name is required")
	}
	chunkSize, chunkOverlap, err := normalizeChunkSettings(input.ChunkSize, input.ChunkOverlap)
	if err != nil {
		return Collection{}, err
	}
	embeddingModel := strings.TrimSpace(input.EmbeddingModel)
	if embeddingModel == "" {
		embeddingModel = s.defaultEmbeddingModel
	}
	id := newID("ragcol")
	profileVersionID := ""
	if input.ChunkProfileVersionID != nil {
		profileVersionID = strings.TrimSpace(*input.ChunkProfileVersionID)
	}
	c := Collection{
		TenantID:              p.TenantID,
		ID:                    id,
		Name:                  name,
		Description:           strings.TrimSpace(input.Description),
		OwnerActorID:          p.ActorID,
		QdrantCollection:      "rag_" + safeCollectionName(id),
		EmbeddingModel:        embeddingModel,
		ChunkSize:             chunkSize,
		ChunkOverlap:          chunkOverlap,
		ChunkProfileVersionID: profileVersionID,
		ACL:                   normalizeACL(input.ACL),
		Audience:              collectionAudience(input.ACL),
		Metadata:              nonNilMap(input.Metadata),
		IsActive:              true,
	}
	if c.ChunkProfileVersionID != "" {
		// An explicit immutable profile version is validated and frozen onto
		// the collection; InsertCollection honors it instead of minting one.
		if err := s.validateChunkProfileVersion(ctx, p.TenantID, c.ChunkProfileVersionID); err != nil {
			return Collection{}, err
		}
	}
	if err := s.repo.InsertCollection(ctx, c); err != nil {
		return Collection{}, err
	}
	return s.repo.GetCollection(ctx, p.TenantID, id)
}

// validateChunkProfileVersion accepts only a real immutable version row in
// the tenant — a profile family ID or a foreign-tenant ID is invalid input.
func (s *Service) validateChunkProfileVersion(ctx context.Context, tenantID, id string) error {
	if _, err := s.repo.GetChunkProfile(ctx, tenantID, id); err != nil {
		if errors.Is(err, ErrProfileNotFound) {
			return fmt.Errorf("%w: unknown chunk_profile_version_id %q", ErrInvalidInput, id)
		}
		return err
	}
	return nil
}

func (s *Service) ListCollections(ctx context.Context, p auth.Principal) ([]Collection, error) {
	if err := requireTenant(p); err != nil {
		return nil, err
	}
	collections, err := s.repo.ListCollections(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	out := make([]Collection, 0, len(collections))
	for _, c := range collections {
		if collectionReadable(c, p) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *Service) GetCollection(ctx context.Context, p auth.Principal, id string) (Collection, error) {
	if err := requireTenant(p); err != nil {
		return Collection{}, err
	}
	c, err := s.repo.GetCollection(ctx, p.TenantID, strings.TrimSpace(id))
	if err != nil {
		return Collection{}, err
	}
	if !collectionReadable(c, p) {
		return Collection{}, source.ErrDenied
	}
	return c, nil
}

func (s *Service) UpdateCollection(ctx context.Context, p auth.Principal, id string, input UpdateCollectionInput) (Collection, error) {
	c, err := s.GetCollection(ctx, p, id)
	if err != nil {
		return Collection{}, err
	}
	if !collectionManageable(c, p) {
		return Collection{}, source.ErrDenied
	}
	if input.Name != nil {
		c.Name = strings.TrimSpace(*input.Name)
	}
	if c.Name == "" {
		return Collection{}, errors.New("name is required")
	}
	if input.Description != nil {
		c.Description = strings.TrimSpace(*input.Description)
	}
	if input.EmbeddingModel != nil {
		c.EmbeddingModel = strings.TrimSpace(*input.EmbeddingModel)
	}
	if c.EmbeddingModel == "" {
		return Collection{}, errors.New("embedding_model is required")
	}
	if input.ChunkSize != nil || input.ChunkOverlap != nil {
		chunkSize := c.ChunkSize
		chunkOverlap := c.ChunkOverlap
		if input.ChunkSize != nil {
			chunkSize = *input.ChunkSize
		}
		if input.ChunkOverlap != nil {
			chunkOverlap = *input.ChunkOverlap
		}
		chunkSize, chunkOverlap, err = normalizeChunkSettings(chunkSize, chunkOverlap)
		if err != nil {
			return Collection{}, err
		}
		// A profile change creates a new immutable version row; published
		// evidence is never silently re-chunked under a mutated profile.
		if _, err := s.repo.EnsureChunkProfile(ctx, p.TenantID, "rag-"+c.ID, chunkSize, chunkOverlap); err != nil {
			return Collection{}, err
		}
		c.ChunkSize = chunkSize
		c.ChunkOverlap = chunkOverlap
	}
	if input.ACL != nil {
		c.ACL = normalizeACL(input.ACL)
		c.Audience = collectionAudience(input.ACL)
	}
	if input.ChunkProfileVersionID != nil {
		// §6.7 Settings selector: the validated immutable version is stored
		// on the collection; already-published evidence is only re-chunked
		// when a document is explicitly reprocessed.
		versionID := strings.TrimSpace(*input.ChunkProfileVersionID)
		if versionID == "" {
			return Collection{}, fmt.Errorf("%w: chunk_profile_version_id must name an immutable version", ErrInvalidInput)
		}
		if err := s.validateChunkProfileVersion(ctx, p.TenantID, versionID); err != nil {
			return Collection{}, err
		}
		c.ChunkProfileVersionID = versionID
	}
	if input.Metadata != nil {
		c.Metadata = input.Metadata
	}
	if input.IsActive != nil {
		c.IsActive = *input.IsActive
	}
	if err := s.repo.UpdateCollection(ctx, c); err != nil {
		return Collection{}, err
	}
	return s.repo.GetCollection(ctx, p.TenantID, id)
}

func (s *Service) DeleteCollection(ctx context.Context, p auth.Principal, id string) error {
	c, err := s.GetCollection(ctx, p, id)
	if err != nil {
		return err
	}
	if !collectionManageable(c, p) {
		return source.ErrDenied
	}
	if s.vectorStore != nil {
		if err := s.vectorStore.DeleteCollection(ctx, c.QdrantCollection); err != nil {
			return err
		}
	}
	return s.repo.DeleteCollection(ctx, p.TenantID, c.ID)
}

// ---------------------------------------------------------------------
// Documents

func (s *Service) UploadDocument(ctx context.Context, p auth.Principal, collectionID string, input UploadDocumentInput) (UploadDocumentOutput, error) {
	c, err := s.GetCollection(ctx, p, collectionID)
	if err != nil {
		return UploadDocumentOutput{}, err
	}
	if !collectionManageable(c, p) {
		return UploadDocumentOutput{}, source.ErrDenied
	}
	return s.processNewDocument(ctx, p, c, input)
}

// validDocumentStatus covers the §6.7 status chips; unknown values are
// caller errors, never silently widened.
func validDocumentStatus(status string) bool {
	switch status {
	case DocumentStatusAll, DocumentStatusReady, DocumentStatusProcessing, DocumentStatusFailed, DocumentStatusRemoved:
		return true
	}
	return false
}

// ListDocuments returns one page of the caller's AUTHORIZED document set:
// per-document CanRead runs before pagination, so denied rows never consume
// the caller's page and no title or count leaks (OA-21). rag.manage is not
// an authorization bypass — a manage-scoped caller still sees only readable
// documents; status=removed tombstone metadata is the one manage-gated view.
func (s *Service) ListDocuments(ctx context.Context, p auth.Principal, collectionID, status string, limit, offset int) ([]Document, error) {
	c, err := s.GetCollection(ctx, p, collectionID)
	if err != nil {
		return nil, err
	}
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "" {
		status = DocumentStatusAll
	}
	if !validDocumentStatus(status) {
		return nil, fmt.Errorf("%w: unknown document status %q", ErrInvalidInput, status)
	}
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	if status == DocumentStatusRemoved && !auth.HasScope(p, "rag.manage") && !auth.HasScope(p, "platform.admin") {
		// Removed rows are unknown to non-managers, not an error.
		return []Document{}, nil
	}
	batch := 4 * limit
	if batch < 50 {
		batch = 50
	}
	if batch > 400 {
		batch = 400
	}
	var authorized []Document
	wanted := offset + limit
	for rawOffset := 0; len(authorized) < wanted; {
		rows, err := s.repo.ListDocuments(ctx, p.TenantID, c.ID, status, batch, rawOffset)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		for _, d := range rows {
			// Removed metadata is manage-gated above; live rows pass the
			// same source authorization as every other content read.
			if d.Status != DocumentStatusRemoved {
				if err := s.authorizeDocumentRead(ctx, p, d); err != nil {
					if errors.Is(err, source.ErrDenied) {
						continue
					}
					return nil, err
				}
			}
			authorized = append(authorized, d)
		}
		rawOffset += len(rows)
		if len(rows) < batch {
			break
		}
	}
	if offset >= len(authorized) {
		return []Document{}, nil
	}
	end := offset + limit
	if end > len(authorized) {
		end = len(authorized)
	}
	return authorized[offset:end], nil
}

// authorizeDocumentRead is the shared per-document read gate for list-style
// projections: the same object-level CanRead every other content read uses.
func (s *Service) authorizeDocumentRead(ctx context.Context, p auth.Principal, d Document) error {
	obj, err := s.repo.GetObject(ctx, p.TenantID, d.ID)
	if err != nil {
		return err
	}
	return s.authorizer.CanRead(ctx, p, obj, source.ReadOptions{})
}

// ListFailedDocuments returns up to limit failed documents the caller may
// read. The raw tenant-wide failure set is authorized BEFORE the limit, so
// restricted failures filling the head cannot hide an authorized failure
// deeper in the set — and rag.manage never bypasses source ACLs.
func (s *Service) ListFailedDocuments(ctx context.Context, p auth.Principal, limit int) ([]Document, error) {
	if err := requireTenant(p); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	batch := 4 * limit
	if batch < 50 {
		batch = 50
	}
	if batch > 400 {
		batch = 400
	}
	var authorized []Document
	for rawOffset := 0; len(authorized) < limit; {
		rows, err := s.repo.ListFailedDocuments(ctx, p.TenantID, batch, rawOffset)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		for _, d := range rows {
			if err := s.authorizeDocumentRead(ctx, p, d); err != nil {
				if errors.Is(err, source.ErrDenied) {
					continue
				}
				return nil, err
			}
			authorized = append(authorized, d)
			if len(authorized) == limit {
				break
			}
		}
		rawOffset += len(rows)
		if len(rows) < batch {
			break
		}
	}
	return authorized, nil
}

// CountDocuments counts the caller's authorized non-removed documents — the
// KPI read for every role including managers; no tenant-wide count is
// exposed through any capability.
func (s *Service) CountDocuments(ctx context.Context, p auth.Principal) (int64, error) {
	if err := requireTenant(p); err != nil {
		return 0, err
	}
	var count int64
	const batch = 200
	for rawOffset := 0; ; rawOffset += batch {
		rows, err := s.repo.ListTenantDocuments(ctx, p.TenantID, batch, rawOffset)
		if err != nil {
			return 0, err
		}
		if len(rows) == 0 {
			break
		}
		for _, d := range rows {
			if err := s.authorizeDocumentRead(ctx, p, d); err != nil {
				if errors.Is(err, source.ErrDenied) {
					continue
				}
				return 0, err
			}
			count++
		}
		if len(rows) < batch {
			break
		}
	}
	return count, nil
}

// LiveSourceIDs returns the source ids still present in the collection, so a
// caller driving incremental ingest can distinguish "unchanged" from
// "deleted since last sync".
func (s *Service) LiveSourceIDs(ctx context.Context, p auth.Principal, collectionID string) (map[string]struct{}, error) {
	c, err := s.GetCollection(ctx, p, collectionID)
	if err != nil {
		return nil, err
	}
	return s.repo.LiveCollectionSourceIDs(ctx, p.TenantID, c.ID)
}

func (s *Service) DownloadDocument(ctx context.Context, p auth.Principal, documentID string) (Document, []byte, error) {
	doc, err := s.repo.GetDocument(ctx, p.TenantID, documentID)
	if err != nil {
		return Document{}, nil, err
	}
	obj, err := s.repo.GetObject(ctx, p.TenantID, doc.ID)
	if err != nil {
		return Document{}, nil, err
	}
	if err := s.authorizer.CanRead(ctx, p, obj, source.ReadOptions{}); err != nil {
		return Document{}, nil, err
	}
	if s.artifacts == nil {
		return Document{}, nil, errors.New("artifact store is not configured")
	}
	rc, err := s.artifacts.Read(ctx, artifacts.Ref{TenantID: p.TenantID, ID: doc.ArtifactRef, Kind: artifacts.KindOriginal})
	if err != nil {
		return Document{}, nil, err
	}
	defer func() { _ = rc.Close() }()
	blob, err := readAll(rc)
	if err != nil {
		return Document{}, nil, err
	}
	return doc, blob, nil
}

func (s *Service) DeleteDocument(ctx context.Context, p auth.Principal, documentID string) error {
	doc, err := s.repo.GetDocument(ctx, p.TenantID, documentID)
	if err != nil {
		return err
	}
	c, err := s.repo.GetCollection(ctx, p.TenantID, doc.CollectionID)
	if err != nil {
		return err
	}
	if !collectionManageable(c, p) {
		return source.ErrDenied
	}
	if s.vectorStore != nil {
		if err := s.vectorStore.DeleteDocumentPoints(ctx, c.QdrantCollection, doc.ID); err != nil {
			return err
		}
	}
	// Soft delete: the object tombstone blocks reads (and evidence) while
	// the collection item keeps removed management metadata. Object/version
	// rows are retained — the management Removed view never re-exposes
	// content, so nothing is lost by keeping the records.
	if err := s.repo.TombstoneObject(ctx, p.TenantID, doc.ID); err != nil {
		return err
	}
	return s.repo.MarkCollectionItemRemoved(ctx, p.TenantID, doc.CollectionID, doc.ID)
}

func (s *Service) ReprocessDocument(ctx context.Context, p auth.Principal, documentID string) (UploadDocumentOutput, error) {
	doc, err := s.repo.GetDocument(ctx, p.TenantID, documentID)
	if err != nil {
		return UploadDocumentOutput{}, err
	}
	c, err := s.repo.GetCollection(ctx, p.TenantID, doc.CollectionID)
	if err != nil {
		return UploadDocumentOutput{}, err
	}
	if !collectionManageable(c, p) {
		return UploadDocumentOutput{}, source.ErrDenied
	}
	obj, err := s.repo.GetObject(ctx, p.TenantID, doc.ID)
	if err != nil {
		return UploadDocumentOutput{}, err
	}
	if err := s.authorizer.CanRead(ctx, p, obj, source.ReadOptions{}); err != nil {
		return UploadDocumentOutput{}, err
	}
	// Reprocess re-runs the embed pipeline against the latest immutable
	// version: the job idempotency key binds (object, seq, generation) so a
	// replay returns the same job, and a NEW generation row fences a stale
	// prior attempt from clobbering the fresh index.
	gen, err := s.repo.NextGeneration(ctx, p.TenantID, c.ID, doc.ObjectVersionID)
	if err != nil {
		return UploadDocumentOutput{}, err
	}
	if err := s.enqueueEmbed(ctx, p, c, obj.ID, doc.ObjectVersionID, doc.ObservedSeq, gen); err != nil {
		return UploadDocumentOutput{}, err
	}
	return UploadDocumentOutput{Document: doc}, nil
}

// ---------------------------------------------------------------------
// Ingest: durable artifact + object/version row + fenced embed job.

// processNewDocument persists bytes BEFORE the object version row and the
// embed job exist: Reader and Bytes callers share one artifact-writer path,
// and the persisted version metadata comes from the finalized artifact ref
// (its SHA256/ByteSize are measured over the streamed bytes) — never a
// second digest over an in-memory slice.
func (s *Service) processNewDocument(ctx context.Context, p auth.Principal, c Collection, input UploadDocumentInput) (UploadDocumentOutput, error) {
	input.FileName = strings.TrimSpace(filepath.Base(input.FileName))
	if input.FileName == "." || input.FileName == "" {
		return UploadDocumentOutput{}, errors.New("file_name is required")
	}
	if s.artifacts == nil || s.jobs == nil {
		return UploadDocumentOutput{}, errors.New("rag: durable ingest pipeline is not configured")
	}
	if strings.TrimSpace(input.MimeType) == "" {
		input.MimeType = "application/octet-stream"
	}
	sourceType := strings.TrimSpace(input.SourceType)
	if sourceType == "" {
		sourceType = "upload"
	}
	sourceID := strings.TrimSpace(input.SourceID)
	if sourceID == "" {
		sourceID = newID("upload")
	}

	w, ref, err := s.artifacts.Begin(ctx, artifacts.Meta{
		Ref:      artifacts.Ref{TenantID: p.TenantID, Kind: artifacts.KindOriginal},
		MimeType: input.MimeType,
	}, 10*time.Minute, s.maxUploadMB*1024*1024)
	if err != nil {
		return UploadDocumentOutput{}, fmt.Errorf("begin document artifact: %w", err)
	}
	if input.Reader != nil {
		_, err = io.Copy(w, input.Reader)
	} else {
		_, err = w.Write(input.Bytes)
	}
	if err != nil {
		_ = w.Abort(ctx)
		return UploadDocumentOutput{}, fmt.Errorf("write document artifact: %w", err)
	}
	if err := w.Finalize(ctx); err != nil {
		return UploadDocumentOutput{}, fmt.Errorf("finalize document artifact: %w", err)
	}
	ref = w.Ref()
	// Emptiness is validated on the FINALIZED artifact. Finalize has already
	// set the writer done — Abort cannot remove the bytes now, so delete the
	// unreferenced artifact through the store; no version row or embed job
	// exists yet, so nothing else dangles.
	if ref.ByteSize == 0 {
		_ = s.artifacts.Delete(ctx, ref)
		return UploadDocumentOutput{}, fmt.Errorf("%w: document is empty", ErrInvalidInput)
	}

	// Audience-mode object: the uploader plus the collection's audience can
	// read; collection manage rights gate mutation.
	audience := append([]string{p.ActorID}, c.Audience...)
	if len(input.Audience) > 0 {
		audience = input.Audience
	}
	objectID := input.ObjectID
	if objectID == "" {
		objectID = newID("ragdoc")
	}
	obj, err := s.repo.UpsertObject(ctx, source.Object{
		TenantID:          p.TenantID,
		ID:                objectID,
		Kind:              sourceType,
		NativeID:          sourceID,
		AuthorizationMode: source.AuthorizationAudience,
		OwnerActorID:      p.ActorID,
		Audience:          audience,
		ObservedSeq:       0,
	})
	if err != nil {
		return UploadDocumentOutput{}, err
	}

	provenanceKind := input.ProvenanceKind
	if provenanceKind == "" {
		provenanceKind = "upload"
	}
	prov, _ := json.Marshal(map[string]any{
		"kind": provenanceKind, "input_artifact_ref": input.InputArtifact,
		"result_job_id": input.ResultJobID, "result_artifact_ref": input.ResultArtifact,
		"source_uri": strings.TrimSpace(input.SourceURI),
		"metadata":   nonNilMap(input.Metadata),
	})
	ver := source.ObjectVersion{
		ObjectID:         obj.ID,
		ObservedSeq:      obj.ObservedSeq + 1,
		ArtifactRef:      ref.ID,
		SHA256:           ref.SHA256,
		ByteSize:         ref.ByteSize,
		MimeType:         input.MimeType,
		OriginalFilename: input.FileName,
		Provenance:       string(prov),
		Restrictions:     input.Restrictions,
	}
	if err := s.repo.InsertObjectVersion(ctx, p.TenantID, ver); err != nil {
		return UploadDocumentOutput{}, err
	}
	if err := s.repo.AddCollectionItem(ctx, p.TenantID, c.ID, obj.ID, p.ActorID); err != nil {
		return UploadDocumentOutput{}, err
	}
	versionID := versionRowID(ver)
	gen, err := s.repo.NextGeneration(ctx, p.TenantID, c.ID, versionID)
	if err != nil {
		return UploadDocumentOutput{}, err
	}
	if err := s.enqueueEmbed(ctx, p, c, obj.ID, versionID, ver.ObservedSeq, gen); err != nil {
		return UploadDocumentOutput{}, err
	}
	doc, err := s.repo.GetDocument(ctx, p.TenantID, obj.ID)
	if err != nil {
		return UploadDocumentOutput{}, err
	}
	return UploadDocumentOutput{Document: doc, Chunks: []DocumentChunk{}}, nil
}

func versionRowID(ver source.ObjectVersion) string {
	return fmt.Sprintf("%s#%d", ver.ObjectID, ver.ObservedSeq)
}

// enqueueEmbed stages the generation row and enqueues the fenced embed job.
// The idempotency key binds (object, observed seq, generation) so replays of
// the same version resolve to the same job and a reprocess (new generation)
// is distinct work.
func (s *Service) enqueueEmbed(ctx context.Context, p auth.Principal, c Collection, objectID, versionID string, seq, generation int64) error {
	if err := s.repo.ReplaceVersionChunksAndStage(ctx, p.TenantID, c.ID, objectID, versionID, seq, generation, "" /* profile bound at attempt */, nil); err != nil {
		return err
	}
	// The collection's immutable chunk profile version is FROZEN into the
	// job policy: a later UpdateCollection to a different version cannot
	// retcon a queued run.
	_, _, err := s.jobs.Enqueue(ctx, p, jobs.EnqueueRequest{
		TenantID:       p.TenantID,
		Operation:      OperationEmbed,
		IdempotencyKey: fmt.Sprintf("rag.embed:%s:%d:%d", objectID, seq, generation),
		Subject: jobs.Subject{
			ObjectID:         objectID,
			ObservedRevision: seq,
		},
		InputRef: versionID,
		Policy: map[string]any{
			"collection_id":            c.ID,
			"object_id":                objectID,
			"observed_seq":             seq,
			"generation":               generation,
			"embedding_model":          c.EmbeddingModel,
			"chunk_profile_version_id": c.ChunkProfileVersionID,
		},
	})
	return err
}

// JobHandler is the jobs.Runner handler for OperationEmbed. The fenced
// attempt: load frozen principal → re-check object/version auth →
// MarkDispatched → embed → stage chunks + publish generation → settle. A
// stale attempt can never overwrite a newer version's chunks or generation
// because both the job fence and the generation number must still be the
// latest.
func (s *Service) JobHandler() jobs.HandlerFunc {
	return func(ctx context.Context, claim *jobs.Claim, _ jobs.InputFunc) (string, error) {
		return s.attemptEmbed(ctx, claim)
	}
}

func (s *Service) attemptEmbed(ctx context.Context, claim *jobs.Claim) (string, error) {
	if err := s.ReconcileIndexPoints(ctx, claim.Job().TenantID); err != nil {
		return "", err
	}
	job := claim.Job()
	var p auth.Principal
	if err := json.Unmarshal(job.PrincipalSnapshot, &p); err != nil {
		return "", fmt.Errorf("decode frozen principal: %w", err)
	}
	objectID, _ := job.Policy["object_id"].(string)
	collectionID, _ := job.Policy["collection_id"].(string)
	seq := int64FromAny(job.Policy["observed_seq"])
	generation := int64FromAny(job.Policy["generation"])

	obj, err := s.repo.GetObject(ctx, job.TenantID, objectID)
	if err != nil {
		return "", err
	}
	if obj.DeletedAt != nil || obj.TombstonedAt != nil {
		return "", errors.New("object deleted")
	}
	ver, err := s.repo.GetObjectVersion(ctx, job.TenantID, objectID, seq)
	if err != nil {
		return "", err
	}
	if err := s.authorizer.CanReadVersion(ctx, p, obj, ver, source.ReadOptions{}); err != nil {
		return "", err
	}
	// Superseded-version fence: a newer version exists → this attempt must
	// not write index state. Mark its staged generation superseded.
	latest, err := s.repo.LatestObjectVersion(ctx, job.TenantID, objectID)
	if err != nil {
		return "", err
	}
	c, err := s.repo.GetCollection(ctx, job.TenantID, collectionID)
	if err != nil {
		return "", err
	}
	if latest.ObservedSeq != seq {
		_ = s.repo.MarkGenerationState(ctx, job.TenantID, collectionID, versionRowID(ver), generation, "superseded")
		return versionRowID(ver), nil // complete: nothing to index
	}
	accessRevision := int64(0)
	if obj.ConnectionID != "" {
		accessRevision, _, err = s.repo.LatestCompleteRevision(ctx, job.TenantID, obj.ConnectionID, source.SnapshotGrants)
		if err != nil {
			return "", err
		}
	}
	ref := artifacts.Ref{TenantID: job.TenantID, ID: ver.ArtifactRef, Kind: artifacts.KindOriginal}
	objectRef := source.ObjectRef{TenantID: job.TenantID, ObjectID: objectID, ObservedSeq: seq}
	document, err := s.parser.Parse(ctx, ref, extract.ParseOptions{
		Filename: ver.OriginalFilename, MIMEType: ver.MimeType, ObjectRef: objectRef,
	})
	if err != nil {
		return "", err
	}
	if len(document.Blocks) == 0 {
		return "", errors.New("document has no extractable text")
	}
	document = applyConnectorStructure(document, obj, ver.Provenance)
	parserVersion := "extract-v1"
	if err := s.repo.SaveParsedDocument(ctx, parserVersion, document); err != nil {
		return "", err
	}
	profileVersionID := stringFromPayload(job.Policy, "chunk_profile_version_id")
	if profileVersionID == "" {
		// Queued before the freeze landed: fall back to the collection's
		// stored immutable version, never a hardcoded profile.
		profileVersionID = c.ChunkProfileVersionID
	}
	if profileVersionID == "" {
		return "", errors.New("collection has no chunk profile version")
	}
	// The FROZEN immutable version row drives chunking: its real boundary
	// strategy, table handling and sizes — not the collection's current
	// selection and not a fixed/row-wise construction.
	profileRow, err := s.repo.GetChunkProfile(ctx, job.TenantID, profileVersionID)
	if err != nil {
		return "", fmt.Errorf("load frozen chunk profile %s: %w", profileVersionID, err)
	}
	profile := source.ChunkProfile{
		ID: profileRow.ID, ProfileID: profileRow.ProfileID, Version: profileRow.Version,
		SizeCodePoints: profileRow.SizeCodePoints, OverlapCodePoints: profileRow.OverlapCodePoints,
		BoundaryStrategy: profileRow.BoundaryStrategy, TableHandling: profileRow.TableHandling,
		IncludeParents: profileRow.IncludeParents, PreserveClauses: profileRow.PreserveClauses,
		PreserveAuthorship: profileRow.PreserveAuthorship,
	}
	structuredChunks, err := extract.Chunk(document, profile)
	if err != nil {
		return "", err
	}
	if len(structuredChunks) == 0 {
		return "", errors.New("document has no extractable text")
	}
	// HeaderPath is the real heading ancestry of the chunk's spans, read
	// from the retained parsed blocks at materialization — never post-hoc
	// parsing or invented headings.
	blockByID := make(map[string]source.Block, len(document.Blocks))
	for _, b := range document.Blocks {
		blockByID[b.ID] = b
	}
	headerPathFor := func(spans []source.Span) string {
		for _, span := range spans {
			var parts []string
			seen := map[string]struct{}{}
			block, ok := blockByID[span.BlockID]
			for ok {
				if _, dup := seen[block.ID]; dup {
					break
				}
				seen[block.ID] = struct{}{}
				if block.Kind == "heading" && strings.TrimSpace(block.Text) != "" {
					parts = append([]string{block.Text}, parts...)
				}
				if block.ParentID == "" {
					break
				}
				block, ok = blockByID[block.ParentID]
			}
			if len(parts) > 0 {
				return strings.Join(parts, " > ")
			}
		}
		return ""
	}
	chunkTexts := make([]string, 0, len(structuredChunks))
	for _, chunk := range structuredChunks {
		chunkTexts = append(chunkTexts, chunk.Text)
	}

	if err := claim.MarkDispatched(ctx); err != nil {
		return "", err
	}

	// Priced embedding attempt: the shared executor resolves current actor
	// and input restrictions, reserves the immutable rate, dispatches and
	// settles or retains unknown liability — no direct-provider path.
	vectors, err := s.embed(ctx, p, c.EmbeddingModel, chunkTexts, embedScope{
		Inputs: []source.ObjectRef{{TenantID: job.TenantID, ObjectID: objectID, ObservedSeq: seq}},
		Artifacts: []artifacts.Ref{{
			TenantID: job.TenantID,
			ID:       ver.ArtifactRef,
			Kind:     artifacts.KindOriginal,
		}},
		JobID:     job.ID,
		AttemptID: claim.AttemptID(),
	})
	if err != nil {
		return "", err
	}
	if len(vectors) == 0 || len(vectors[0]) == 0 {
		return "", errors.New("provider returned empty embedding")
	}

	chunks := make([]DocumentChunk, 0, len(chunkTexts))
	points := make([]QdrantPoint, 0, len(chunkTexts))
	var provMeta map[string]any
	_ = json.Unmarshal([]byte(ver.Provenance), &provMeta)
	docMetadata := mapFromPayload(provMeta, "metadata")
	sourceURI := stringFromPayload(provMeta, "source_uri")
	for i, chunkText := range chunkTexts {
		chunkID := newID("ragchunk")
		pointID := newPointID()
		chunk := DocumentChunk{
			ID: chunkID, DocumentID: obj.ID, CollectionID: c.ID, Generation: generation,
			ChunkIndex: i, Content: chunkText, TokenCount: tokenEstimate(chunkText),
			QdrantPointID: pointID, Metadata: map[string]any{"file_name": ver.OriginalFilename},
			HeaderPath: headerPathFor(structuredChunks[i].Spans),
		}
		payload := map[string]any{
			"tenant_id":     job.TenantID,
			"object_ref":    map[string]any{"tenant_id": job.TenantID, "connection_id": obj.ConnectionID, "object_id": obj.ID, "observed_seq": seq},
			"collection_id": c.ID, "document_id": obj.ID, "connection_id": obj.ConnectionID,
			"object_version_id": versionRowID(ver), "observed_seq": seq,
			"generation": generation, "chunk_id": chunkID, "chunk_index": i,
			"content": chunkText, "file_name": ver.OriginalFilename,
			"source_type": obj.Kind, "source_id": obj.NativeID, "source_uri": sourceURI,
			"metadata": docMetadata, "access_revision": accessRevision, "access_enabled": true,
		}
		spanPayload := make([]map[string]any, 0, len(structuredChunks[i].Spans))
		for _, span := range structuredChunks[i].Spans {
			chunk.SpanRefs = append(chunk.SpanRefs, span)
			spanPayload = append(spanPayload, map[string]any{
				"block_id": span.BlockID, "byte_start": span.ByteStart, "byte_end": span.ByteEnd,
			})
		}
		for j, span := range structuredChunks[i].Spans {
			if j == 0 || span.ByteStart < chunk.ByteStart {
				chunk.ByteStart = span.ByteStart
			}
			if span.ByteEnd > chunk.ByteEnd {
				chunk.ByteEnd = span.ByteEnd
			}
		}
		payload["block_spans"] = spanPayload
		chunks = append(chunks, chunk)
		points = append(points, QdrantPoint{
			ID: pointID, Vector: vectors[i], Sparse: sparseVector(chunkText), Payload: payload,
		})
	}

	// Stage the generation: chunks + staging row commit atomically BEFORE
	// any Qdrant write; a crash leaves a staging generation, never a doc
	// claiming indexed state.
	if err := s.repo.ReplaceVersionChunksAndStage(ctx, job.TenantID, c.ID, obj.ID, versionRowID(ver), seq, generation, profileVersionID, chunks); err != nil {
		return "", err
	}
	if err := s.vectorStore.EnsureCollection(ctx, c.QdrantCollection, len(vectors[0])); err != nil {
		return "", err
	}
	if err := s.vectorStore.UpsertPoints(ctx, c.QdrantCollection, points); err != nil {
		return "", err
	}
	// Publish only after the durable chunks and index writes exist.
	if err := s.repo.PublishGenerationFenced(ctx, job.TenantID, c.ID, obj.ID, versionRowID(ver), generation, claim.JobID, claim.Owner, claim.AttemptID(), claim.FenceToken); err != nil {
		return "", err
	}
	if err := s.ReconcileIndexPoints(ctx, job.TenantID); err != nil {
		return "", err
	}
	return versionRowID(ver), nil
}

// embed dispatches one bounded embedding attempt through the shared
// executor; nil-executor tests that never embed stay usable.
func (s *Service) embed(ctx context.Context, p auth.Principal, model string, inputs []string, scope embedScope) ([][]float64, error) {
	if s.executor == nil {
		return nil, errors.New("rag: inference executor is not configured")
	}
	return createEmbeddings(ctx, s.executor, p, model, inputs, scope)
}

// ---------------------------------------------------------------------
// Authorization helpers: no Access struct — the principal drives reads and
// mutations. Admin authority comes from the platform.admin scope, never a
// forged flag.

func collectionReadable(c Collection, p auth.Principal) bool {
	if auth.HasScope(p, "platform.admin") {
		return true
	}
	if !c.IsActive {
		return false
	}
	if c.OwnerActorID != "" && c.OwnerActorID == p.ActorID {
		return true
	}
	for _, aud := range c.Audience {
		if aud == "*" || aud == p.ActorID {
			return true
		}
	}
	return false
}

func collectionManageable(c Collection, p auth.Principal) bool {
	if auth.HasScope(p, "platform.admin") {
		return true
	}
	return c.OwnerActorID != "" && c.OwnerActorID == p.ActorID
}

// collectionAudience maps the wire-stable ACL map onto the stored audience
// list: "public" grants tenant-wide read ("*"); "audience" entries are actor
// IDs verbatim.
func collectionAudience(acl map[string]any) []string {
	out := []string{}
	if acl == nil {
		return out
	}
	if boolACL(acl, "public") {
		out = append(out, "*")
	}
	if raw, ok := acl["audience"]; ok {
		switch vals := raw.(type) {
		case []string:
			out = append(out, vals...)
		case []any:
			for _, v := range vals {
				if s, ok := v.(string); ok && s != "" {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

func normalizeChunkSettings(chunkSize, chunkOverlap int) (int, int, error) {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	if chunkSize < 100 || chunkSize > 16000 {
		return 0, 0, errors.New("chunk_size must be between 100 and 16000")
	}
	if chunkOverlap < 0 {
		return 0, 0, errors.New("chunk_overlap must be zero or greater")
	}
	if chunkOverlap >= chunkSize {
		return 0, 0, errors.New("chunk_overlap must be smaller than chunk_size")
	}
	return chunkSize, chunkOverlap, nil
}

func safeCollectionName(id string) string {
	replacer := strings.NewReplacer("-", "_", ":", "_", "/", "_")
	return replacer.Replace(id)
}

func normalizeACL(acl map[string]any) map[string]any {
	if acl == nil {
		return map[string]any{"public": false, "audience": []string{}}
	}
	if _, ok := acl["public"]; !ok {
		acl["public"] = false
	}
	return acl
}

func nonNilMap(v map[string]any) map[string]any {
	if v == nil {
		return map[string]any{}
	}
	return v
}

func boolACL(acl map[string]any, key string) bool {
	v, ok := acl[key]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

func stringFromPayload(payload map[string]any, key string) string {
	v, _ := payload[key].(string)
	return v
}

func mapFromPayload(payload map[string]any, key string) map[string]any {
	v, ok := payload[key].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return v
}
