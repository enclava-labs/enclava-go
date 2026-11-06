package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/extract"
	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
	"enclava-go/internal/llm"
	"enclava-go/internal/source"
)

const (
	OperationEmbed          = "rag.embed"
	OperationIngest         = "rag.ingest"
	OperationAccessBackfill = "rag.access_backfill"
	OperationReembedScan    = "rag.reembed_scan"
)

// ErrProfileNotFound marks a chunk_profile_version_id that does not exist in
// the calling tenant — a caller-input failure (HTTP 400), distinct from
// infrastructure errors.
var ErrProfileNotFound = errors.New("chunk profile version not found")

// ErrGenerationSuperseded reports a generation that may no longer publish: a
// newer generation of the same document already is, or replaced it.
var ErrGenerationSuperseded = errors.New("generation superseded by a newer one")

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
	CollectionHasObject(ctx context.Context, tenantID, collectionID, objectID string) (bool, error)
	ListDocuments(ctx context.Context, tenantID, collectionID, status string, limit, offset int) ([]Document, error)
	ListFailedDocuments(ctx context.Context, tenantID string, limit, offset int) ([]Document, error)
	ListTenantDocuments(ctx context.Context, tenantID string, limit, offset int) ([]Document, error)
	// TenantDocumentObjectIDs returns one object id per live collection
	// item (duplicates kept, liveness = ListTenantDocuments' projection) —
	// the flat read CountDocuments authorizes instead of paging full rows.
	TenantDocumentObjectIDs(ctx context.Context, tenantID string) ([]string, error)
	CountDocuments(ctx context.Context, tenantID string, excludeRemoved bool) (int64, error)
	LiveCollectionSourceIDs(ctx context.Context, tenantID, collectionID string) (map[string]struct{}, error)
	GetDocument(ctx context.Context, tenantID, objectID string) (Document, error)
	// GetObjects reads many canonical objects in one query — one map entry
	// per found id, keyed by object id, same row projection as GetObject.
	// A missing id has no entry; callers decide the not-found outcome.
	GetObjects(ctx context.Context, tenantID string, ids []string) (map[string]source.Object, error)
	CreateUploadIfNew(ctx context.Context, collectionID, actorID string, object source.Object, version source.ObjectVersion, excludedIDs []string) (string, error)
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
	ReconcilePendingIndexes(ctx context.Context, tenantID string, limit int, cleanup func(IndexReconciliation) error) (int, error)
	ReserveKnowledgeImport(ctx context.Context, tenantID, collectionID, importRef, objectID, kind string) (reservedObjectID string, complete bool, err error)
	ListGenerationsForConnection(ctx context.Context, tenantID, connectionID string) ([]IndexGeneration, error)
	// Search reads bindings and connections without enumerating documents,
	// then checks each hit against its published generation.
	DistinctIndexBindings(ctx context.Context, tenantID, collectionID string) ([]IndexBinding, error)
	PublishedGenerationForVersion(ctx context.Context, tenantID, collectionID, versionID string, generation int64) (bool, error)
	CurrentPublishedGeneration(ctx context.Context, tenantID, collectionID, objectID, versionID string) (int64, error)
	// Access-projection backlog: published generations whose index points
	// predate the acl/published projection, their marker flip, and the
	// tenants that still need the backfill job.
	ListUnprojectedGenerations(ctx context.Context, tenantID string, limit int) ([]IndexGeneration, error)
	ProjectGenerationAccess(ctx context.Context, tenantID, collectionID, versionID string, generation int64, project func() error) error
	TenantsNeedingAccessBackfill(ctx context.Context) ([]string, error)
	RecordKnowledgeImport(ctx context.Context, tenantID, collectionID, importRef, objectID, kind string) error
	GetStoredResult(ctx context.Context, tenantID, jobID string) (StoredResult, error)
	PublishGenerationFenced(ctx context.Context, tenantID, collectionID, objectID, versionID string, generation int64, jobID, leaseOwner, attemptID string, fenceToken int64) error
	// BindGeneration records the index a staged generation is written to.
	BindGeneration(ctx context.Context, tenantID, versionID string, generation int64, c Collection) error
	// LiveIndexNames lists indexes bound to a published or staging
	// generation of a live document; ForgetRetiredIndex drops one name from
	// the collection's retired list once its index is gone.
	LiveIndexNames(ctx context.Context, tenantID, collectionID string) (map[string]struct{}, error)
	ForgetRetiredIndex(ctx context.Context, tenantID, collectionID, name string) error
	// DocumentsToReembed lists live documents whose published generation was
	// embedded under other settings than the collection's current index and
	// document-name choice, with no newer attempt queued or running.
	DocumentsToReembed(ctx context.Context, tenantID string, c Collection) ([]Document, error)
	ReembedProgressDocuments(ctx context.Context, tenantID string, c Collection) (pending []Document, total []Document, err error)
	PendingReembed(ctx context.Context, tenantID string, c Collection) (bool, error)
	LatestReusableGeneration(ctx context.Context, tenantID, collectionID, versionID, profileVersionID string) (int64, []string, error)
	SetGenerationWarnings(ctx context.Context, tenantID, collectionID, versionID string, generation int64, warnings []string) error
}

type VectorStore interface {
	EnsureCollection(ctx context.Context, collection string, vectorSize int) error
	UpsertPoints(ctx context.Context, collection string, points []QdrantPoint) error
	DeleteDocumentPoints(ctx context.Context, collection, documentID string) error
	DeleteGenerationPoints(ctx context.Context, collection, versionID string, generation int64) error
	DeleteConnectionPoints(ctx context.Context, collection, connectionID string) error
	SetPayload(ctx context.Context, collection string, filter map[string]any, payload map[string]any) error
	DeleteCollection(ctx context.Context, collection string) error
	SearchHybrid(ctx context.Context, collection string, vector []float64, limit int, filter map[string]any) ([]QdrantSearchPoint, error)
}

// Executor is the shared authorized inference boundary every embedding
// attempt dispatches through; *inference.Service satisfies it.
type Executor interface {
	Execute(ctx context.Context, p auth.Principal, req inference.Request) (*inference.Result, error)
}

type Service struct {
	repo                       Repository
	vectorStore                VectorStore
	executor                   Executor
	defaultEmbeddingModel      string
	defaultEmbeddingDimensions int
	utilityModel               string
	defaultModel               func(ctx context.Context, tenantID string) (string, error)
	indexRetirementSoak        time.Duration
	artifacts                  *artifacts.Store
	parser                     *extract.Parser
	jobs                       *jobs.Service
	authorizer                 *source.Authorizer
	maxUploadMB                int64
}

// NewService builds the ported rag.Service. artifactStore and jobSvc may be
// nil only in narrow unit tests — durable ingest and priced embeddings
// require them; the executor is the single priced dispatch boundary and is
// required for any embedding operation.
func NewService(repo Repository, vectorStore VectorStore, executor Executor, defaultEmbeddingModel string, artifactStore *artifacts.Store, jobSvc *jobs.Service) *Service {
	if strings.TrimSpace(defaultEmbeddingModel) == "" {
		defaultEmbeddingModel = "qwen3-embedding-4b"
	}
	return &Service{
		repo:                  repo,
		vectorStore:           vectorStore,
		executor:              executor,
		defaultEmbeddingModel: defaultEmbeddingModel,
		artifacts:             artifactStore,
		parser:                extract.NewParser(artifactStore),
		jobs:                  jobSvc,
		authorizer:            source.NewAuthorizer(repo),
		maxUploadMB:           512,
	}
}

// SetDefaultEmbeddingDimensions sets the vector size a collection on the
// default model gets when it picks none (0 = the model's full output).
func (s *Service) SetDefaultEmbeddingDimensions(n int) { s.defaultEmbeddingDimensions = n }

func (s *Service) SetIndexRetirementSoak(d time.Duration) { s.indexRetirementSoak = d }

// DefaultEmbedding reports the deployment default model and its vector size.
func (s *Service) DefaultEmbedding() (string, int) {
	return s.defaultEmbeddingModel, s.defaultEmbeddingDimensions
}

// embeddingDimensions resolves a collection's vector size: an explicit choice
// wins; otherwise the default model gets the deployment default and any other
// model its full output, since only Matryoshka-trained models shorten safely.
func (s *Service) embeddingDimensions(model string, requested *int) (int, error) {
	if requested == nil {
		if model == s.defaultEmbeddingModel {
			return s.defaultEmbeddingDimensions, nil
		}
		return 0, nil
	}
	if n := *requested; n != 0 && (n < 32 || n > 8192) {
		return 0, fmt.Errorf("%w: embedding_dimensions must be 0 (full size) or between 32 and 8192", ErrInvalidInput)
	}
	return *requested, nil
}

// probeEmbedding embeds one short string so a model that is unreachable or
// returns fewer dimensions than asked fails when the collection is
// configured, not at its first ingest.
func (s *Service) probeEmbedding(ctx context.Context, p auth.Principal, model string, dims int) error {
	if s.executor == nil {
		return nil
	}
	if _, err := s.embed(ctx, p, model, dims, []string{"embedding probe"}, embedScope{}); err != nil {
		return fmt.Errorf("%w: cannot embed with %q: %v", ErrInvalidInput, model, err)
	}
	return nil
}

// newIndexName names a fresh Qdrant collection for a settings change. Names
// are never reused, so cleanup of a retired index can never hit a collection
// that switched back to the same settings.
func newIndexName(collectionID string) string {
	return safeCollectionName(newID("rag_" + collectionID))
}

// reembedPending queues a new generation for every live document still
// published under other settings, with none already in flight. The set is
// one snapshot, so concurrent deletes cannot shift it, and it is derived
// from the generations themselves, so saving the settings again resumes
// whatever an interrupted change left undone.
func (s *Service) reembedPending(ctx context.Context, p auth.Principal, c Collection) error {
	docs, err := s.repo.DocumentsToReembed(ctx, p.TenantID, c)
	if err != nil {
		return err
	}
	// Same rule as a manual reprocess: only documents the caller can read.
	readErrs, err := s.authorizeDocuments(ctx, p, docs)
	if err != nil {
		return err
	}
	for i, doc := range docs {
		if denied, failure := deniedOrFailed(readErrs[i]); failure != nil {
			return failure
		} else if denied {
			continue
		}
		if err := s.reembedDocument(ctx, p, c, doc.ID, doc.ObjectVersionID, doc.ObservedSeq); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) enqueueReembedScan(ctx context.Context, p auth.Principal, c Collection) error {
	pending, err := s.repo.PendingReembed(ctx, p.TenantID, c)
	if err != nil || !pending {
		return err
	}
	if s.jobs == nil {
		return errors.New("rag: jobs are not configured")
	}
	// A completed scan's identity must never be reused — saving the same
	// settings twice still needs a fresh scan. Only a queued scan dedupes: a
	// running one already froze its document list, so the save must schedule
	// another pass for the documents it already skipped.
	active, err := s.jobs.ListTenantActivity(ctx, p, []string{OperationReembedScan}, []string{jobs.StatusQueued}, 100)
	if err != nil {
		return err
	}
	for _, job := range active {
		if stringFromPayload(job.Policy, "collection_id") == c.ID {
			return nil
		}
	}
	_, _, err = s.jobs.Enqueue(ctx, p, jobs.EnqueueRequest{
		TenantID: p.TenantID, Operation: OperationReembedScan,
		IdempotencyKey: fmt.Sprintf("rag.reembed_scan:%s:%s", c.ID, newID("scan")),
		InputRef:       c.ID, Policy: map[string]any{"collection_id": c.ID},
		Lane: jobs.LaneBackground,
	})
	return err
}

func (s *Service) ReembedScanJobHandler() jobs.HandlerFunc {
	return func(ctx context.Context, claim *jobs.Claim, _ jobs.InputFunc) (string, error) {
		job := claim.Job()
		var p auth.Principal
		if err := json.Unmarshal(job.PrincipalSnapshot, &p); err != nil {
			return "", fmt.Errorf("decode frozen principal: %w", err)
		}
		id := stringFromPayload(job.Policy, "collection_id")
		if p.TenantID != job.TenantID || id == "" || job.InputRef != id {
			return "", source.ErrDenied
		}
		c, err := s.GetCollection(ctx, p, id)
		if err != nil {
			return "", err
		}
		if !collectionManageable(c, p) {
			return "", source.ErrDenied
		}
		if err := s.reembedPending(ctx, p, c); err != nil {
			return "", err
		}
		return id, nil
	}
}

func (s *Service) ReembedProgress(ctx context.Context, p auth.Principal, collectionID string) (pending, total int64, err error) {
	c, err := s.GetCollection(ctx, p, collectionID)
	if err != nil {
		return 0, 0, err
	}
	if !collectionManageable(c, p) {
		return 0, 0, source.ErrDenied
	}
	pendingDocs, totalDocs, err := s.repo.ReembedProgressDocuments(ctx, p.TenantID, c)
	if err != nil {
		return 0, 0, err
	}
	pendingIDs := make(map[string]struct{}, len(pendingDocs))
	for _, doc := range pendingDocs {
		pendingIDs[doc.ID] = struct{}{}
	}
	readErrs, err := s.authorizeDocuments(ctx, p, totalDocs)
	if err != nil {
		return 0, 0, err
	}
	for i, doc := range totalDocs {
		if denied, failure := deniedOrFailed(readErrs[i]); failure != nil {
			return 0, 0, failure
		} else if denied {
			continue
		}
		total++
		if _, ok := pendingIDs[doc.ID]; ok {
			pending++
		}
	}
	return pending, total, nil
}

// reembedDocument queues the document's next generation under c's settings.
func (s *Service) reembedDocument(ctx context.Context, p auth.Principal, c Collection, objectID, versionID string, seq int64) error {
	gen, err := s.repo.NextGeneration(ctx, p.TenantID, c.ID, versionID)
	if err != nil {
		return err
	}
	return s.enqueueEmbed(ctx, p, c, objectID, versionID, seq, gen, jobs.LaneBackground)
}

// retireIndexes drops the collection's retired indexes that no live
// generation reads or writes any more. Best effort: a failure keeps the name
// listed, and the next publish or delete retries it.
func (s *Service) retireIndexes(ctx context.Context, tenantID, collectionID string) {
	if err := s.dropUnusedIndexes(ctx, tenantID, collectionID); err != nil {
		log.Printf("rag: retire indexes of %s: %v", collectionID, err)
	}
}

func (s *Service) dropUnusedIndexes(ctx context.Context, tenantID, collectionID string) error {
	c, err := s.repo.GetCollection(ctx, tenantID, collectionID)
	if err != nil || len(c.RetiredQdrantCollections) == 0 || s.vectorStore == nil {
		return err
	}
	live, err := s.repo.LiveIndexNames(ctx, tenantID, collectionID)
	if err != nil {
		return err
	}
	counter, ok := s.vectorStore.(interface {
		CountPoints(context.Context, string) (int64, error)
	})
	if !ok {
		return nil
	}
	published, err := s.repo.ListPublishedGenerations(ctx, tenantID, collectionID)
	if err != nil {
		return err
	}
	for _, generation := range published {
		if generation.QdrantCollection == c.QdrantCollection {
			count, err := counter.CountPoints(ctx, c.QdrantCollection)
			if err != nil || count <= 0 {
				return err
			}
			break
		}
	}
	soak := s.indexRetirementSoak
	if soak == 0 {
		soak = 24 * time.Hour
	}
	for _, name := range c.RetiredQdrantCollections {
		if _, used := live[name]; used || name == c.QdrantCollection {
			continue
		}
		since, known := c.RetiredIndexSince[name]
		if soak > 0 && (!known || time.Since(since) < soak) {
			continue
		}
		if err := s.vectorStore.DeleteCollection(ctx, name); err != nil {
			return err
		}
		if err := s.repo.ForgetRetiredIndex(ctx, tenantID, collectionID, name); err != nil {
			return err
		}
	}
	return nil
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
	embeddingDimensions, err := s.embeddingDimensions(embeddingModel, input.EmbeddingDimensions)
	if err != nil {
		return Collection{}, err
	}
	// Only a shortened size can be wrong for a model; check it up front.
	if embeddingDimensions > 0 {
		if err := s.probeEmbedding(ctx, p, embeddingModel, embeddingDimensions); err != nil {
			return Collection{}, err
		}
	}
	queryPrefix, passagePrefix := defaultEmbeddingPrefixes(embeddingModel)
	if input.QueryPrefix != nil {
		queryPrefix = *input.QueryPrefix
	}
	if input.PassagePrefix != nil {
		passagePrefix = *input.PassagePrefix
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
		QueryPrefix:           queryPrefix,
		PassagePrefix:         passagePrefix,
		EmbeddingDimensions:   embeddingDimensions,
		EmbedTitles:           input.EmbedTitles == nil || *input.EmbedTitles,
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
	model, dims := c.EmbeddingModel, c.EmbeddingDimensions
	passagePrefix := c.PassagePrefix
	if input.EmbeddingModel != nil {
		c.EmbeddingModel = strings.TrimSpace(*input.EmbeddingModel)
	}
	if c.EmbeddingModel == "" {
		return Collection{}, errors.New("embedding_model is required")
	}
	if input.EmbeddingDimensions != nil || c.EmbeddingModel != model {
		if c.EmbeddingDimensions, err = s.embeddingDimensions(c.EmbeddingModel, input.EmbeddingDimensions); err != nil {
			return Collection{}, err
		}
	}
	if c.EmbeddingModel != model {
		c.QueryPrefix, c.PassagePrefix = defaultEmbeddingPrefixes(c.EmbeddingModel)
	}
	if input.QueryPrefix != nil {
		c.QueryPrefix = *input.QueryPrefix
	}
	if input.PassagePrefix != nil {
		c.PassagePrefix = *input.PassagePrefix
	}
	if c.EmbeddingModel != model || c.EmbeddingDimensions != dims || c.PassagePrefix != passagePrefix {
		// One Qdrant collection holds one model's vectors at one size: the new
		// settings get their own index while every document re-embeds in the
		// background, and search reads the previous index until each is done.
		if err := s.probeEmbedding(ctx, p, c.EmbeddingModel, c.EmbeddingDimensions); err != nil {
			return Collection{}, err
		}
		c.QdrantCollection = newIndexName(c.ID)
	}
	if input.EmbedTitles != nil {
		// Only passages change: documents re-embed in place.
		c.EmbedTitles = *input.EmbedTitles
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
	updated, err := s.repo.GetCollection(ctx, p.TenantID, id)
	if err != nil {
		return Collection{}, err
	}
	if err := s.enqueueReembedScan(ctx, p, updated); err != nil {
		return Collection{}, err
	}
	// Drops an old index nothing reads any more — at once for an empty
	// collection, and a retry point for an earlier failed cleanup.
	s.retireIndexes(ctx, p.TenantID, c.ID)
	return updated, nil
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
		for _, name := range append([]string{c.QdrantCollection}, c.RetiredQdrantCollections...) {
			if err := s.vectorStore.DeleteCollection(ctx, name); err != nil {
				return err
			}
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
	batch := 400
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
		readErrs, err := s.authorizeDocuments(ctx, p, rows)
		if err != nil {
			return nil, err
		}
		for i, d := range rows {
			// Removed metadata is manage-gated above; live rows pass the
			// same source authorization as every other content read.
			if d.Status != DocumentStatusRemoved {
				if err := readErrs[i]; err != nil {
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

// authorizeDocuments is the batch form of authorizeDocumentRead: one object
// fetch for the whole document set plus CanReadMany, which costs a constant
// number of queries per connection instead of several per document. Result
// i applies to docs[i]; a document whose object row is missing yields the
// same not-found outcome GetObject gives the single-read path.
func (s *Service) authorizeDocuments(ctx context.Context, p auth.Principal, docs []Document) ([]error, error) {
	ids := make([]string, 0, len(docs))
	for _, d := range docs {
		ids = append(ids, d.ID)
	}
	errsByID, err := s.authorizeObjectIDs(ctx, p, ids)
	if err != nil {
		return nil, err
	}
	errs := make([]error, len(docs))
	for i, d := range docs {
		errs[i] = errsByID[d.ID]
	}
	return errs, nil
}

// authorizeObjectIDs is the id-keyed core of authorizeDocuments: every
// distinct id is fetched and authorized once, the outcome keyed by object
// id so callers working over id lists (including duplicates) share it.
func (s *Service) authorizeObjectIDs(ctx context.Context, p auth.Principal, ids []string) (map[string]error, error) {
	objects, err := s.repo.GetObjects(ctx, p.TenantID, ids)
	if err != nil {
		return nil, err
	}
	errsByID := make(map[string]error, len(ids))
	var batch []source.Object
	var batchIDs []string
	for _, id := range ids {
		if _, done := errsByID[id]; done {
			continue
		}
		obj, ok := objects[id]
		if !ok {
			errsByID[id] = source.ErrDenied
			continue
		}
		errsByID[id] = source.ErrDenied // provisional until CanReadMany fills it
		batch = append(batch, obj)
		batchIDs = append(batchIDs, id)
	}
	readErrs := s.authorizer.CanReadMany(ctx, p, batch, source.ReadOptions{})
	for j, id := range batchIDs {
		errsByID[id] = readErrs[j]
	}
	return errsByID, nil
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
	// The failed filter recomputes every candidate per query, so one large
	// scan batch avoids the O(N²) of paging small windows; the loop still
	// covers tenants beyond it.
	const batch = 5000
	var authorized []Document
	for rawOffset := 0; len(authorized) < limit; {
		rows, err := s.repo.ListFailedDocuments(ctx, p.TenantID, batch, rawOffset)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		readErrs, err := s.authorizeDocuments(ctx, p, rows)
		if err != nil {
			return nil, err
		}
		for i, d := range rows {
			if err := readErrs[i]; err != nil {
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
	ids, err := s.repo.TenantDocumentObjectIDs(ctx, p.TenantID)
	if err != nil {
		return 0, err
	}
	// Each returned id is one live collection item; authorizing each
	// distinct object once keeps multi-collection reads counted per item.
	var count int64
	const chunk = 2000
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		readErrs, err := s.authorizeObjectIDs(ctx, p, ids[start:end])
		if err != nil {
			return 0, err
		}
		for _, id := range ids[start:end] {
			if err := readErrs[id]; err != nil {
				if errors.Is(err, source.ErrDenied) {
					continue
				}
				return 0, err
			}
			count++
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
		for _, name := range append([]string{c.QdrantCollection}, c.RetiredQdrantCollections...) {
			if err := s.vectorStore.DeleteDocumentPoints(ctx, name, doc.ID); err != nil {
				return err
			}
		}
	}
	// Soft delete: the object tombstone blocks reads (and evidence) while
	// the collection item keeps removed management metadata. Object/version
	// rows are retained — the management Removed view never re-exposes
	// content, so nothing is lost by keeping the records.
	if err := s.repo.TombstoneObject(ctx, p.TenantID, doc.ID); err != nil {
		return err
	}
	if err := s.repo.MarkCollectionItemRemoved(ctx, p.TenantID, doc.CollectionID, doc.ID); err != nil {
		return err
	}
	// The last document still on a retired index may just have left it.
	s.retireIndexes(ctx, p.TenantID, doc.CollectionID)
	return nil
}

// PurgeConnectionPoints drops every index point the connection's synced
// objects left behind, across each generation's bound index. The DB rows go
// away separately (DeleteConnection); a nil vector store has nothing to purge.
func (s *Service) PurgeConnectionPoints(ctx context.Context, tenantID, connectionID string) error {
	if s.vectorStore == nil {
		return nil
	}
	generations, err := s.repo.ListGenerationsForConnection(ctx, tenantID, connectionID)
	if err != nil {
		return err
	}
	seen := map[string]struct{}{}
	for _, generation := range generations {
		if generation.QdrantCollection == "" {
			continue
		}
		if _, dup := seen[generation.QdrantCollection]; dup {
			continue
		}
		seen[generation.QdrantCollection] = struct{}{}
		if err := s.vectorStore.DeleteConnectionPoints(ctx, generation.QdrantCollection, connectionID); err != nil {
			return err
		}
	}
	return nil
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
	if err := s.enqueueEmbed(ctx, p, c, obj.ID, doc.ObjectVersionID, doc.ObservedSeq, gen, jobs.LaneInteractive); err != nil {
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
	if input.ObjectID == "" && strings.TrimSpace(input.SourceID) == "" {
		return s.persistUpload(ctx, p, c, input, ref, sourceType, sourceID)
	}
	return s.persistDocumentArtifact(ctx, p, c, input, ref, sourceType, sourceID)
}

func (s *Service) persistUpload(ctx context.Context, p auth.Principal, c Collection, input UploadDocumentInput, ref artifacts.Ref, sourceType, sourceID string) (UploadDocumentOutput, error) {
	obj := source.Object{
		TenantID: p.TenantID, ID: newID("ragdoc"), Kind: sourceType, NativeID: sourceID,
		AuthorizationMode: source.AuthorizationCollection, OwnerActorID: p.ActorID,
	}
	kind := input.ProvenanceKind
	if kind == "" {
		kind = "upload"
	}
	prov, _ := json.Marshal(map[string]any{
		"kind": kind, "input_artifact_ref": input.InputArtifact,
		"result_job_id": input.ResultJobID, "result_artifact_ref": input.ResultArtifact,
		"source_uri": strings.TrimSpace(input.SourceURI), "metadata": nonNilMap(input.Metadata),
	})
	ver := source.ObjectVersion{
		ObjectID: obj.ID, ObservedSeq: 1, ArtifactRef: ref.ID, SHA256: ref.SHA256,
		ByteSize: ref.ByteSize, MimeType: input.MimeType, OriginalFilename: input.FileName,
		Provenance: string(prov), Restrictions: input.Restrictions,
	}
	var excludedIDs []string
	for {
		existingID, err := s.repo.CreateUploadIfNew(ctx, c.ID, p.ActorID, obj, ver, excludedIDs)
		if err != nil {
			_ = s.artifacts.Delete(context.WithoutCancel(ctx), ref)
			return UploadDocumentOutput{}, err
		}
		if existingID == "" {
			break
		}
		doc, err := s.repo.GetDocument(ctx, p.TenantID, existingID)
		if err == nil {
			err = s.authorizeDocumentRead(ctx, p, doc)
		}
		if err == nil {
			_ = s.artifacts.Delete(context.WithoutCancel(ctx), ref)
			return UploadDocumentOutput{Document: doc, Chunks: []DocumentChunk{}, Deduplicated: true}, nil
		}
		if !errors.Is(err, source.ErrDenied) {
			_ = s.artifacts.Delete(context.WithoutCancel(ctx), ref)
			return UploadDocumentOutput{}, err
		}
		excludedIDs = append(excludedIDs, existingID)
	}
	versionID := versionRowID(ver)
	gen, err := s.repo.NextGeneration(ctx, p.TenantID, c.ID, versionID)
	if err != nil {
		return UploadDocumentOutput{}, err
	}
	if err := s.enqueueEmbed(ctx, p, c, obj.ID, versionID, ver.ObservedSeq, gen, jobs.LaneInteractive); err != nil {
		return UploadDocumentOutput{}, err
	}
	doc, err := s.repo.GetDocument(ctx, p.TenantID, obj.ID)
	if err != nil {
		return UploadDocumentOutput{}, err
	}
	return UploadDocumentOutput{Document: doc, Chunks: []DocumentChunk{}}, nil
}

func (s *Service) persistDocumentArtifact(ctx context.Context, p auth.Principal, c Collection, input UploadDocumentInput, ref artifacts.Ref, sourceType, sourceID string) (UploadDocumentOutput, error) {
	// Imports and source-identity updates retain their existing versioning path.

	// Collection-mode object: readable by the principals that can read the
	// collection; the uploader owns the row for mutation bookkeeping.
	objectID := input.ObjectID
	if objectID == "" {
		objectID = newID("ragdoc")
	}
	obj, err := s.repo.UpsertObject(ctx, source.Object{
		TenantID:          p.TenantID,
		ID:                objectID,
		Kind:              sourceType,
		NativeID:          sourceID,
		AuthorizationMode: source.AuthorizationCollection,
		OwnerActorID:      p.ActorID,
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
	if err := s.enqueueEmbed(ctx, p, c, obj.ID, versionID, ver.ObservedSeq, gen, jobs.LaneInteractive); err != nil {
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
// is distinct work. lane: interactive for uploads and reprocess, background
// for connector ingest and re-embeds.
func (s *Service) enqueueEmbed(ctx context.Context, p auth.Principal, c Collection, objectID, versionID string, seq, generation int64, lane jobs.Lane) error {
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
		Lane: lane,
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
		if obj.ConnectionID != "" && errors.Is(err, source.ErrDenied) {
			return "", fmt.Errorf("%w: the source's creator can no longer open its collection; share the collection with them, then sync again", err)
		}
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
	// A retried attempt whose generation already published (the previous run
	// died scheduling its successor) skips the billed re-embed — the staged
	// row would reject it as immutable anyway — and converges on the same
	// post-publish requeue check.
	published, err := s.repo.PublishedGenerationForVersion(ctx, job.TenantID, c.ID, versionRowID(ver), generation)
	if err != nil {
		return "", err
	}
	if published {
		docs, err := s.repo.DocumentsToReembed(ctx, job.TenantID, c)
		if err != nil {
			return "", jobs.Retryable(fmt.Errorf("check stale generation: %w", err))
		}
		for _, doc := range docs {
			if doc.ID != obj.ID {
				continue
			}
			if err := s.reembedDocument(ctx, p, c, obj.ID, versionRowID(ver), seq); err != nil {
				return "", jobs.Retryable(fmt.Errorf("requeue %s under current settings: %w", obj.ID, err))
			}
			break
		}
		if err := s.ReconcileIndexPoints(ctx, job.TenantID); err != nil {
			return "", err
		}
		s.retireIndexes(ctx, job.TenantID, c.ID)
		return versionRowID(ver), nil
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
	previous, warnings, err := s.repo.LatestReusableGeneration(ctx, job.TenantID, c.ID, versionRowID(ver), profileVersionID)
	if err != nil {
		return "", err
	}
	var document source.Document
	var structuredChunks []source.Chunk
	var previousChunks []DocumentChunk
	if previous != 0 {
		previousChunks, err = s.repo.ListGenerationChunks(ctx, job.TenantID, c.ID, versionRowID(ver), previous)
		if err != nil {
			return "", err
		}
		for _, chunk := range previousChunks {
			structuredChunks = append(structuredChunks, source.Chunk{Ordinal: chunk.ChunkIndex, Text: chunk.Content, Spans: chunk.SpanRefs})
		}
	} else {
		ref := artifacts.Ref{TenantID: job.TenantID, ID: ver.ArtifactRef, Kind: artifacts.KindOriginal}
		objectRef := source.ObjectRef{TenantID: job.TenantID, ObjectID: objectID, ObservedSeq: seq}
		document, err = s.parser.Parse(ctx, ref, extract.ParseOptions{
			Filename: ver.OriginalFilename, MIMEType: ver.MimeType, ObjectRef: objectRef,
		})
		if err != nil {
			return "", err
		}
		if len(document.Blocks) == 0 {
			return "", errors.New("document has no extractable text")
		}
		document = applyConnectorStructure(document, ver.Provenance)
		if err := s.repo.SaveParsedDocument(ctx, "extract-v1", document); err != nil {
			return "", err
		}
		structuredChunks, err = extract.Chunk(document, profileRow)
		if err != nil {
			return "", err
		}
		warnings = append(warnings, document.ParseWarnings...)
		for _, chunk := range structuredChunks {
			warnings = append(warnings, chunk.Warnings...)
		}
	}
	if len(structuredChunks) == 0 {
		return "", errors.New("document has no extractable text")
	}
	seenWarnings := make(map[string]bool, len(warnings))
	uniqueWarnings := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		if !seenWarnings[warning] {
			seenWarnings[warning] = true
			uniqueWarnings = append(uniqueWarnings, warning)
		}
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
	embedTexts := make([]string, 0, len(structuredChunks))
	searchTexts := make([]string, 0, len(structuredChunks))
	for _, chunk := range structuredChunks {
		chunkTexts = append(chunkTexts, chunk.Text)
		embedding, search := embeddingTexts(c, obj, ver, chunk.Text, profileRow.SizeCodePoints)
		embedTexts = append(embedTexts, embedding)
		searchTexts = append(searchTexts, search)
	}

	if err := claim.MarkDispatched(ctx); err != nil {
		return "", err
	}

	// Priced embedding attempt: the shared executor resolves current actor
	// and input restrictions, reserves the immutable rate, dispatches and
	// settles or retains unknown liability — no direct-provider path.
	vectors, err := s.embed(ctx, p, c.EmbeddingModel, c.EmbeddingDimensions, embedTexts, embedScope{
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
		return "", retryEmbedError(err)
	}
	if len(vectors) != len(chunkTexts) || len(vectors) == 0 || len(vectors[0]) == 0 {
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
		if previous != 0 {
			chunk.ChunkIndex = previousChunks[i].ChunkIndex
			chunk.HeaderPath = previousChunks[i].HeaderPath
		}
		payload := map[string]any{
			"tenant_id":     job.TenantID,
			"object_ref":    map[string]any{"tenant_id": job.TenantID, "connection_id": obj.ConnectionID, "object_id": obj.ID, "observed_seq": seq},
			"collection_id": c.ID, "document_id": obj.ID, "connection_id": obj.ConnectionID,
			"object_version_id": versionRowID(ver), "observed_seq": seq,
			"generation": generation, "chunk_id": chunkID, "chunk_index": chunk.ChunkIndex,
			"content": chunkText, "search_text": searchTexts[i], "file_name": ver.OriginalFilename,
			"source_type": obj.Kind, "source_id": obj.NativeID, "source_uri": sourceURI,
			"metadata": docMetadata, "access_enabled": true,
			"published": false,
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
			ID: pointID, Vector: vectors[i], Payload: payload,
		})
	}

	// Stage the generation: chunks + staging row commit atomically BEFORE
	// any Qdrant write; a crash leaves a staging generation, never a doc
	// claiming indexed state.
	if err := s.repo.ReplaceVersionChunksAndStage(ctx, job.TenantID, c.ID, obj.ID, versionRowID(ver), seq, generation, profileVersionID, chunks); err != nil {
		if errors.Is(err, ErrGenerationSuperseded) {
			return versionRowID(ver), nil // a newer generation owns the document
		}
		return "", err
	}
	if err := s.repo.SetGenerationWarnings(ctx, job.TenantID, c.ID, versionRowID(ver), generation, uniqueWarnings); err != nil {
		return "", err
	}
	// The generation records the index this attempt writes to — the settings
	// read at its start, even if the collection switches meanwhile.
	if err := s.repo.BindGeneration(ctx, job.TenantID, versionRowID(ver), generation, c); err != nil {
		if errors.Is(err, ErrGenerationSuperseded) {
			return versionRowID(ver), nil
		}
		return "", err
	}
	// An index retired and already dropped while this attempt embedded must
	// not be recreated: retry under the collection's current settings.
	current, err := s.repo.GetCollection(ctx, job.TenantID, c.ID)
	if err != nil {
		return "", err
	}
	if current.QdrantCollection != c.QdrantCollection && !slices.Contains(current.RetiredQdrantCollections, c.QdrantCollection) {
		return "", jobs.Retryable(fmt.Errorf("collection %s moved to a new index during the attempt", c.ID))
	}
	if err := s.vectorStore.EnsureCollection(ctx, c.QdrantCollection, len(vectors[0])); err != nil {
		return "", retryVectorError(err)
	}
	if err := s.vectorStore.UpsertPoints(ctx, c.QdrantCollection, points); err != nil {
		return "", retryVectorError(err)
	}
	err = s.repo.ProjectGenerationAccess(ctx, job.TenantID, c.ID, versionRowID(ver), generation, func() error {
		object, err := s.repo.GetObject(ctx, job.TenantID, obj.ID)
		if err != nil {
			return err
		}
		tokens, err := s.authorizer.ReadTokens(ctx, object)
		if err != nil {
			return err
		}
		if err := s.vectorStore.SetPayload(ctx, c.QdrantCollection, map[string]any{"must": []map[string]any{
			{"key": "object_version_id", "match": map[string]any{"value": versionRowID(ver)}},
			{"key": "generation", "match": map[string]any{"value": generation}},
		}}, map[string]any{"acl": tokens, "published": true}); err != nil {
			return retryVectorError(err)
		}
		return s.repo.PublishGenerationFenced(ctx, job.TenantID, c.ID, obj.ID, versionRowID(ver), generation, claim.JobID, claim.Owner, claim.AttemptID(), claim.FenceToken)
	})
	if errors.Is(err, ErrGenerationSuperseded) {
		// A newer generation published first: drop what this one wrote.
		if err := s.vectorStore.DeleteGenerationPoints(ctx, c.QdrantCollection, versionRowID(ver), generation); err != nil {
			log.Printf("rag: drop superseded generation %s/%d: %v", versionRowID(ver), generation, err)
		}
		s.retireIndexes(ctx, job.TenantID, c.ID)
		return versionRowID(ver), nil
	}
	if err != nil {
		return "", err
	}
	if err := s.ReconcileIndexPoints(ctx, job.TenantID); err != nil {
		return "", err
	}
	// The settings changed after this attempt read them: it published under
	// the old ones, so queue the document again under the current settings.
	if err := s.requeueForCurrentSettings(ctx, p, obj.ID, versionRowID(ver), seq, c); err != nil {
		return "", err
	}
	s.retireIndexes(ctx, job.TenantID, c.ID)
	return versionRowID(ver), nil
}

// requeueForCurrentSettings enqueues the document's next generation when the
// collection's current settings differ from the ones bound at this attempt's
// start. A settings scan may already have skipped this in-flight document,
// so a transient re-read or enqueue failure returns retryable — reporting
// success would leave the stale generation pending forever. A collection
// that no longer exists denies the read: no successor is needed, so the
// attempt completes.
func (s *Service) requeueForCurrentSettings(ctx context.Context, p auth.Principal, objectID, versionID string, seq int64, bound Collection) error {
	current, err := s.repo.GetCollection(ctx, p.TenantID, bound.ID)
	if err != nil {
		if errors.Is(err, source.ErrDenied) {
			s.retireIndexes(ctx, p.TenantID, bound.ID)
			return nil
		}
		return jobs.Retryable(fmt.Errorf("reload collection for re-embed: %w", err))
	}
	if current.QdrantCollection == bound.QdrantCollection && current.EmbedTitles == bound.EmbedTitles && current.PassagePrefix == bound.PassagePrefix {
		return nil
	}
	if err := s.reembedDocument(ctx, p, current, objectID, versionID, seq); err != nil {
		return jobs.Retryable(fmt.Errorf("requeue %s under current settings: %w", objectID, err))
	}
	return nil
}

func retryVectorError(err error) error {
	var networkError net.Error
	if errors.Is(err, ErrVectorStoreUnavailable) || errors.As(err, &networkError) {
		return jobs.Retryable(err)
	}
	return err
}

// retryEmbedError marks transient embedding upstream failures retryable;
// context cancellation, 4xx answers and config errors stay fatal.
func retryEmbedError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var networkError net.Error
	var upstream *llm.UpstreamError
	switch {
	case errors.As(err, &networkError),
		errors.Is(err, io.EOF),
		errors.Is(err, io.ErrUnexpectedEOF):
		return jobs.Retryable(err)
	case errors.As(err, &upstream) && (upstream.StatusCode == 429 || upstream.StatusCode >= 500):
		return jobs.Retryable(err)
	}
	return err
}

// embed dispatches one bounded embedding attempt through the shared
// executor; nil-executor tests that never embed stay usable.
func (s *Service) embed(ctx context.Context, p auth.Principal, model string, dims int, inputs []string, scope embedScope) ([][]float64, error) {
	if s.executor == nil {
		return nil, errors.New("rag: inference executor is not configured")
	}
	return createEmbeddings(ctx, s.executor, p, model, dims, inputs, scope)
}

// ---------------------------------------------------------------------
// Authorization helpers: no Access struct — the principal drives reads and
// mutations. Admin authority comes from the platform.admin scope, never a
// forged flag.

func collectionReadable(c Collection, p auth.Principal) bool {
	return source.CollectionReadable(source.CollectionAccess{
		ID: c.ID, OwnerActorID: c.OwnerActorID, Audience: c.Audience, IsActive: c.IsActive,
	}, p)
}

// CollectionWritable reports whether p can mutate collection c (owner or
// platform admin) — the same rule collectionManageable enforces on every
// write path, exported for read-only UI affordances.
func CollectionWritable(c Collection, p auth.Principal) bool {
	return collectionManageable(c, p)
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
