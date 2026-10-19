package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
	"enclava-go/internal/source"
)

// OperationEmbed is the durable job operation that indexes one immutable
// object version: parse → chunk → embed → stage chunks + publish generation.
const OperationEmbed = "rag.embed"

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

	// Collection items and document projection (latest version + latest
	// index generation state).
	AddCollectionItem(ctx context.Context, tenantID, collectionID, objectID, addedBy string) error
	RemoveCollectionItem(ctx context.Context, tenantID, collectionID, objectID string) error
	ListCollectionItems(ctx context.Context, tenantID, collectionID string) ([]string, error)
	ListDocuments(ctx context.Context, tenantID, collectionID string, limit, offset int) ([]Document, error)
	GetDocument(ctx context.Context, tenantID, objectID string) (Document, error)
	DeleteObjectRows(ctx context.Context, tenantID, objectID string) error

	// Index generations + chunks (single transactional publish boundary).
	// ReplaceVersionChunksAndStage deletes any prior chunk rows for the
	// version, inserts the new chunk set and upserts the (collection,
	// object_version, generation) row in 'staging' state — atomically.
	ReplaceVersionChunksAndStage(ctx context.Context, tenantID, collectionID, objectID, versionID string, seq, generation int64, profileVersionID string, chunks []DocumentChunk) error
	MarkGenerationState(ctx context.Context, tenantID, collectionID, versionID string, generation int64, state string) error
	NextGeneration(ctx context.Context, tenantID, collectionID, versionID string) (int64, error)
	ChunkCountForVersion(ctx context.Context, tenantID, versionID string) (int, error)
	ListChunks(ctx context.Context, tenantID, objectID, versionID string) ([]DocumentChunk, error)
}

type VectorStore interface {
	EnsureCollection(ctx context.Context, collection string, vectorSize int) error
	UpsertPoints(ctx context.Context, collection string, points []QdrantPoint) error
	DeleteDocumentPoints(ctx context.Context, collection, documentID string) error
	DeleteCollection(ctx context.Context, collection string) error
	Search(ctx context.Context, collection string, vector []float64, limit int, filter map[string]any) ([]QdrantSearchPoint, error)
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
	profileVersionID, err := s.repo.EnsureChunkProfile(ctx, p.TenantID, "rag-"+id, chunkSize, chunkOverlap)
	if err != nil {
		return Collection{}, err
	}
	_ = profileVersionID
	c := Collection{
		TenantID:         p.TenantID,
		ID:               id,
		Name:             name,
		Description:      strings.TrimSpace(input.Description),
		OwnerActorID:     p.ActorID,
		QdrantCollection: "rag_" + safeCollectionName(id),
		EmbeddingModel:   embeddingModel,
		ChunkSize:        chunkSize,
		ChunkOverlap:     chunkOverlap,
		ACL:              normalizeACL(input.ACL),
		Audience:         collectionAudience(input.ACL),
		Metadata:         nonNilMap(input.Metadata),
		IsActive:         true,
	}
	if err := s.repo.InsertCollection(ctx, c); err != nil {
		return Collection{}, err
	}
	return s.repo.GetCollection(ctx, p.TenantID, id)
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

func (s *Service) ListDocuments(ctx context.Context, p auth.Principal, collectionID string, limit, offset int) ([]Document, error) {
	c, err := s.GetCollection(ctx, p, collectionID)
	if err != nil {
		return nil, err
	}
	return s.repo.ListDocuments(ctx, p.TenantID, c.ID, limit, offset)
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
	// Tombstone first: reads block immediately; row cleanup follows.
	if err := s.repo.TombstoneObject(ctx, p.TenantID, doc.ID); err != nil {
		return err
	}
	return s.repo.DeleteObjectRows(ctx, p.TenantID, doc.ID)
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

// Search resolves the caller's authorized object set BEFORE querying Qdrant:
// the query itself is scoped, never post-filtered only.
func (s *Service) Search(ctx context.Context, p auth.Principal, input SearchInput) ([]SearchResult, error) {
	query := strings.TrimSpace(input.Query)
	if query == "" {
		return nil, errors.New("query is required")
	}
	c, err := s.GetCollection(ctx, p, strings.TrimSpace(input.CollectionID))
	if err != nil {
		return nil, err
	}
	objectIDs, err := s.repo.ListCollectionItems(ctx, p.TenantID, c.ID)
	if err != nil {
		return nil, err
	}
	allowedVersions := []string{}
	queryInputs := []source.ObjectRef{}
	for _, oid := range objectIDs {
		obj, err := s.repo.GetObject(ctx, p.TenantID, oid)
		if err != nil {
			continue // deleted/unknown objects deny identically
		}
		if err := s.authorizer.CanRead(ctx, p, obj, source.ReadOptions{}); err != nil {
			continue
		}
		doc, err := s.repo.GetDocument(ctx, p.TenantID, oid)
		if err != nil || doc.Status != DocumentStatusReady || doc.ObjectVersionID == "" {
			continue
		}
		allowedVersions = append(allowedVersions, doc.ObjectVersionID)
		queryInputs = append(queryInputs, source.ObjectRef{ObjectID: oid})
	}
	if len(allowedVersions) == 0 {
		return []SearchResult{}, nil
	}
	filter := map[string]any{
		"must": []any{
			map[string]any{"key": "object_version_id", "match": map[string]any{"any": allowedVersions}},
		},
	}
	if len(input.Filter) > 0 {
		filter["must"] = append(filter["must"].([]any), input.Filter)
	}
	vectors, err := s.embed(ctx, p, c.EmbeddingModel, []string{query}, embedScope{Inputs: queryInputs})
	if err != nil {
		return nil, err
	}
	results, err := s.vectorStore.Search(ctx, c.QdrantCollection, vectors[0], input.Limit, filter)
	if err != nil {
		return nil, err
	}
	out := make([]SearchResult, 0, len(results))
	for _, result := range results {
		payload := result.Payload
		out = append(out, SearchResult{
			DocumentID: stringFromPayload(payload, "document_id"),
			ChunkID:    stringFromPayload(payload, "chunk_id"),
			Score:      result.Score,
			Content:    stringFromPayload(payload, "content"),
			FileName:   stringFromPayload(payload, "file_name"),
			Metadata:   mapFromPayload(payload, "metadata"),
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------
// Ingest: durable artifact + object/version row + fenced embed job.

func (s *Service) processNewDocument(ctx context.Context, p auth.Principal, c Collection, input UploadDocumentInput) (UploadDocumentOutput, error) {
	input.FileName = strings.TrimSpace(filepath.Base(input.FileName))
	if input.FileName == "." || input.FileName == "" {
		return UploadDocumentOutput{}, errors.New("file_name is required")
	}
	if len(input.Bytes) == 0 {
		return UploadDocumentOutput{}, errors.New("document is empty")
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

	// Persist bytes BEFORE the object version row and the embed job exist.
	w, ref, err := s.artifacts.Begin(ctx, artifacts.Meta{
		Ref:      artifacts.Ref{TenantID: p.TenantID, Kind: artifacts.KindOriginal},
		MimeType: input.MimeType,
	}, 10*time.Minute, s.maxUploadMB*1024*1024)
	if err != nil {
		return UploadDocumentOutput{}, fmt.Errorf("begin document artifact: %w", err)
	}
	if _, err := w.Write(input.Bytes); err != nil {
		_ = w.Abort(ctx)
		return UploadDocumentOutput{}, fmt.Errorf("write document artifact: %w", err)
	}
	if err := w.Finalize(ctx); err != nil {
		return UploadDocumentOutput{}, fmt.Errorf("finalize document artifact: %w", err)
	}
	ref = w.Ref()

	// Audience-mode object: the uploader plus the collection's audience can
	// read; collection manage rights gate mutation.
	audience := append([]string{p.ActorID}, c.Audience...)
	obj, err := s.repo.UpsertObject(ctx, source.Object{
		TenantID:          p.TenantID,
		ID:                newID("ragdoc"),
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

	sum := sha256.Sum256(input.Bytes)
	prov, _ := json.Marshal(map[string]any{
		"source_uri": strings.TrimSpace(input.SourceURI),
		"metadata":   nonNilMap(input.Metadata),
	})
	ver := source.ObjectVersion{
		ObjectID:         obj.ID,
		ObservedSeq:      obj.ObservedSeq + 1,
		ArtifactRef:      ref.ID,
		SHA256:           hex.EncodeToString(sum[:]),
		ByteSize:         int64(len(input.Bytes)),
		MimeType:         input.MimeType,
		OriginalFilename: input.FileName,
		Provenance:       string(prov),
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
			"collection_id":   c.ID,
			"object_id":       objectID,
			"observed_seq":    seq,
			"generation":      generation,
			"embedding_model": c.EmbeddingModel,
			"chunk_size":      c.ChunkSize,
			"chunk_overlap":   c.ChunkOverlap,
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

	rc, err := s.artifacts.Read(ctx, artifacts.Ref{TenantID: job.TenantID, ID: ver.ArtifactRef, Kind: artifacts.KindOriginal})
	if err != nil {
		return "", fmt.Errorf("read input artifact: %w", err)
	}
	defer func() { _ = rc.Close() }()
	blob, err := readAll(rc)
	if err != nil {
		return "", fmt.Errorf("read artifact bytes: %w", err)
	}
	text, err := ExtractText(ver.OriginalFilename, ver.MimeType, blob)
	if err != nil {
		return "", err
	}
	chunkTexts := ChunkText(text, c.ChunkSize, c.ChunkOverlap)
	if len(chunkTexts) == 0 {
		return "", errors.New("document has no extractable text")
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
		// The Qdrant point id is a UUID the vector store accepts; the durable
		// chunk row id stays a payload reference.
		pointID := newPointID()
		chunks = append(chunks, DocumentChunk{
			ID:            chunkID,
			DocumentID:    obj.ID,
			CollectionID:  c.ID,
			ChunkIndex:    i,
			Content:       chunkText,
			TokenCount:    tokenEstimate(chunkText),
			QdrantPointID: pointID,
			Metadata:      map[string]any{"file_name": ver.OriginalFilename},
		})
		points = append(points, QdrantPoint{
			ID:     pointID,
			Vector: vectors[i],
			Payload: map[string]any{
				"collection_id":     c.ID,
				"document_id":       obj.ID,
				"object_version_id": versionRowID(ver),
				"observed_seq":      seq,
				"generation":        generation,
				"chunk_id":          chunkID,
				"chunk_index":       i,
				"content":           chunkText,
				"file_name":         ver.OriginalFilename,
				"source_type":       obj.Kind,
				"source_id":         obj.NativeID,
				"source_uri":        sourceURI,
				"metadata":          docMetadata,
			},
		})
	}

	profileVersionID, err := s.repo.EnsureChunkProfile(ctx, job.TenantID, "rag-"+c.ID, c.ChunkSize, c.ChunkOverlap)
	if err != nil {
		return "", err
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
	if err := s.repo.MarkGenerationState(ctx, job.TenantID, c.ID, versionRowID(ver), generation, "published"); err != nil {
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
