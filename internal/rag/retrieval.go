package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"unicode/utf8"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/inference"
	"enclava-go/internal/source"
)

type queryClient interface {
	Query(context.Context, string, QueryInput) ([]QdrantSearchPoint, error)
}

// ErrSearchWorkExceeded reports an explicit request-size bound (the
// readable-collection cap) was hit — never a silent first-N truncation.
// The HTTP layer maps it to a 400-class `search_scope_too_large`.
var ErrSearchWorkExceeded = errors.New("search scope exceeds the request bound")

var ErrUtilityModelNotConfigured = errors.New("utility model is not configured")

var errSearchBindingDenied = errors.New("search embedding binding denied")

func (s *Service) SetUtilityModel(model string) {
	s.utilityModel = strings.TrimSpace(model)
}

type queryEmbeddingBinding struct {
	model, prefix string
	dims          int
}

type queryEmbeddingKey struct {
	queryEmbeddingBinding
	query string
}

type queryEmbeddingMemo struct {
	vectors  map[queryEmbeddingKey][]float64
	searched bool
}

func newQueryEmbeddingMemo() *queryEmbeddingMemo {
	return &queryEmbeddingMemo{vectors: make(map[queryEmbeddingKey][]float64)}
}

func (s *Service) searchModels(input *SearchInput) error {
	if input.Rewrite && strings.TrimSpace(input.RewriteModel) == "" {
		input.RewriteModel = s.utilityModel
		if strings.TrimSpace(input.RewriteModel) == "" {
			return ErrUtilityModelNotConfigured
		}
	}
	if input.SelectSections && strings.TrimSpace(input.SectionModel) == "" {
		input.SectionModel = s.utilityModel
		if strings.TrimSpace(input.SectionModel) == "" {
			return ErrUtilityModelNotConfigured
		}
	}
	return nil
}

// Bounds for the cross-collection search page.
const (
	maxSearchCollections = 20
	defaultSearchLimit   = 20
	maxSearchLimit       = 50
	maxQueryRunes        = 512
	rrfConstant          = 60
)

func (s *Service) Search(ctx context.Context, p auth.Principal, input SearchInput) ([]SearchResult, error) {
	if err := validateSearchQueryMode(input.Query, input.Mode); err != nil {
		return nil, err
	}
	if err := s.searchModels(&input); err != nil {
		return nil, err
	}
	collection, err := s.GetCollection(ctx, p, strings.TrimSpace(input.CollectionID))
	if err != nil {
		return nil, err
	}
	results, err := s.searchCollection(ctx, p, collection, input, newQueryEmbeddingMemo())
	if err != nil {
		return nil, err
	}
	results, err = s.reauthorizeResults(ctx, p, results)
	if err != nil {
		return nil, err
	}
	// Evidence persists only for results that survive selection, so
	// discarded hits never strand unreachable evidence rows.
	if err := s.persistResultEvidence(ctx, p, results); err != nil {
		return nil, err
	}
	return results, nil
}

func validateSearchQueryMode(query string, mode QueryMode) error {
	if strings.TrimSpace(query) == "" {
		return fmt.Errorf("%w: query is required", ErrInvalidInput)
	}
	if mode == "" {
		return nil
	}
	if mode != QueryHybrid && mode != QueryLexical && mode != QuerySemantic {
		return fmt.Errorf("%w: unsupported search mode %q", ErrInvalidInput, mode)
	}
	return nil
}

// searchCollection runs one bounded collection search: the vector store is
// filtered by tenant + the principal's access tokens (points lacking the
// projection pass to the post-check), and every returned hit is
// authoritatively re-checked against PostgreSQL before it can surface —
// revocation is effective at read time, never at projection time.
func (s *Service) searchCollection(ctx context.Context, p auth.Principal, collection Collection, input SearchInput, memo *queryEmbeddingMemo) ([]SearchResult, error) {
	query := strings.TrimSpace(input.Query)
	// The bindings a search must hit come from the published generations'
	// index records — one aggregate read, never a document enumeration.
	bindings, err := s.repo.DistinctIndexBindings(ctx, p.TenantID, collection.ID)
	if err != nil {
		return nil, err
	}
	if len(bindings) == 0 {
		return []SearchResult{}, nil
	}
	sort.Slice(bindings, func(i, j int) bool {
		if bindings[i].QdrantCollection != bindings[j].QdrantCollection {
			return bindings[i].QdrantCollection < bindings[j].QdrantCollection
		}
		if bindings[i].EmbeddingModel != bindings[j].EmbeddingModel {
			return bindings[i].EmbeddingModel < bindings[j].EmbeddingModel
		}
		if bindings[i].EmbeddingDimensions != bindings[j].EmbeddingDimensions {
			return bindings[i].EmbeddingDimensions < bindings[j].EmbeddingDimensions
		}
		return bindings[i].QueryPrefix < bindings[j].QueryPrefix
	})
	// The prefilter token set is bounded by the collection's connections,
	// never by its documents.
	connectionIDs, err := s.repo.ListCollectionConnectionIDs(ctx, p.TenantID, collection.ID)
	if err != nil {
		return nil, err
	}
	tokens, err := s.authorizer.QueryTokens(ctx, p, connectionIDs)
	if err != nil {
		return nil, err
	}
	if input.Rewrite && strings.TrimSpace(input.RewriteModel) != "" && utf8.RuneCountInString(query) <= 512 {
		query = s.rewriteQuery(ctx, p, query, nil, input.RewriteModel)
	}
	limit := input.Limit
	if limit <= 0 {
		limit = 5
	}
	if limit > 50 {
		limit = 50
	}
	filter := AccessFilter(p.TenantID, tokens, input.Filter)
	// Canonical source kinds resolve once per connection per collection.
	connectionKinds := make(map[string]string)
	chunkCache := make(map[string]map[string]DocumentChunk)
	lists := make([]rankedSearchList, 0, len(bindings))
	var denied error
	for _, b := range bindings {
		results, err := s.queryIndex(ctx, p, collection, b, query, input, filter, limit, memo, connectionKinds, chunkCache)
		if err != nil {
			if errors.Is(err, errSearchBindingDenied) {
				denied = err
				slog.WarnContext(ctx, "search embedding binding unavailable", "collection", collection.ID, "index", b.QdrantCollection, "model", b.EmbeddingModel)
				continue
			}
			return nil, err
		}
		memo.searched = true
		lists = append(lists, rankedSearchList{results: results})
	}
	if len(lists) == 0 {
		return nil, denied
	}
	results := lists[0].results
	if len(lists) > 1 {
		results = fuseRankedResults(lists, limit)
	}
	if input.SelectSections && strings.TrimSpace(input.SectionModel) != "" && len(results) > 0 {
		results = s.selectSections(ctx, p, results, input.SectionModel)
	}
	return results, nil
}

// Search pages this deep per binding while authorized hits are short of
// the limit — over-fetch covers prefilter-only matches the post-check drops.
const (
	searchFetchFactor = 4
	searchFetchMax    = 200
	searchFetchRounds = 3
)

// queryIndex embeds the query with one binding's model and size, then pages
// the index under the access filter, authorizing and materializing every
// hit until the limit is satisfied or the round budget runs out.
func (s *Service) queryIndex(ctx context.Context, p auth.Principal, collection Collection, b IndexBinding, query string, input SearchInput, filter map[string]any, limit int, memo *queryEmbeddingMemo, connectionKinds map[string]string, chunkCache map[string]map[string]DocumentChunk) ([]SearchResult, error) {
	var vector []float64
	if input.Mode != QueryLexical {
		binding := queryEmbeddingBinding{model: b.EmbeddingModel, prefix: b.QueryPrefix, dims: b.EmbeddingDimensions}
		key := queryEmbeddingKey{queryEmbeddingBinding: binding, query: query}
		var found bool
		vector, found = memo.vectors[key]
		if !found {
			vectors, err := s.embed(ctx, p, b.EmbeddingModel, b.EmbeddingDimensions, []string{b.QueryPrefix + query}, embedScope{})
			if err != nil {
				if errors.Is(err, inference.ErrDenied) || errors.Is(err, inference.ErrNoRoute) {
					return nil, fmt.Errorf("%w: %w", errSearchBindingDenied, err)
				}
				return nil, err
			}
			if len(vectors) == 0 || len(vectors[0]) == 0 {
				return nil, errors.New("embedding returned no vector")
			}
			vector = vectors[0]
			memo.vectors[key] = vector
		}
	}
	client, pages := s.vectorStore.(queryClient)
	if !pages && input.Mode == QueryLexical {
		return nil, errors.New("lexical search is unavailable for this vector store")
	}
	fetch := searchFetchFactor * limit
	if fetch > searchFetchMax {
		fetch = searchFetchMax
	}
	results := make([]SearchResult, 0, limit)
	seen := make(map[string]struct{}, fetch)
	for round := 0; round < searchFetchRounds && len(results) < limit; round++ {
		var points []QdrantSearchPoint
		var err error
		if !pages {
			points, err = s.vectorStore.SearchHybrid(ctx, b.QdrantCollection, vector, SparseVector{}, fetch, filter)
		} else {
			if qdrant, ok := s.vectorStore.(*QdrantClient); ok && input.Mode == QueryHybrid && round == 0 {
				hasSparse, _, ferr := qdrant.collectionVectors(ctx, b.QdrantCollection)
				if ferr != nil {
					return nil, ferr
				}
				if !hasSparse {
					points, err = s.vectorStore.SearchHybrid(ctx, b.QdrantCollection, vector, SparseVector{}, fetch, filter)
					pages = false
				}
			}
			if pages {
				points, err = client.Query(ctx, b.QdrantCollection, QueryInput{Mode: input.Mode, DenseVector: vector, LexicalText: query, Filter: filter, Limit: fetch, Offset: round * fetch})
			}
		}
		if err != nil {
			return nil, err
		}
		for _, point := range points {
			if _, dup := seen[fmt.Sprint(point.ID)]; dup {
				continue
			}
			seen[fmt.Sprint(point.ID)] = struct{}{}
			result, keep, err := s.authorizeHit(ctx, p, collection, point, connectionKinds, chunkCache)
			if err != nil {
				return nil, err
			}
			if keep {
				results = append(results, *result)
			}
		}
		if !pages || len(points) < fetch {
			break // a short page means the filtered index is exhausted
		}
	}
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

// authorizeHit is THE authoritative post-check for one prefiltered point:
// the same reads the old enumeration performed, run only for hits the
// vector query actually returned — object CanRead, current version +
// CanReadVersion, live document status, and the hit generation still being
// that version's published generation in this collection (a per-hit
// lookup, not a listing). Anything failing a check is dropped; the point
// itself then materializes the chunk result.
func (s *Service) authorizeHit(ctx context.Context, p auth.Principal, collection Collection, point QdrantSearchPoint, connectionKinds map[string]string, chunkCache map[string]map[string]DocumentChunk) (*SearchResult, bool, error) {
	payload := point.Payload
	objectID := stringFromPayload(payload, "document_id")
	versionID := stringFromPayload(payload, "object_version_id")
	generation := int64FromAny(payload["generation"])
	if objectID == "" || versionID == "" || generation <= 0 {
		return nil, false, nil
	}
	object, err := s.repo.GetObject(ctx, p.TenantID, objectID)
	if denied, failure := deniedOrFailed(err); denied {
		return nil, false, nil
	} else if failure != nil {
		return nil, false, failure
	}
	if err := s.authorizer.CanRead(ctx, p, object, source.ReadOptions{}); err != nil {
		return nil, false, nil
	}
	version, err := s.repo.GetObjectVersion(ctx, p.TenantID, objectID, object.ObservedSeq)
	if denied, failure := deniedOrFailed(err); denied {
		return nil, false, nil
	} else if failure != nil {
		return nil, false, failure
	}
	if versionRowID(version) != versionID {
		return nil, false, nil
	}
	if err := s.authorizer.CanReadVersion(ctx, p, object, version, source.ReadOptions{}); err != nil {
		return nil, false, nil
	}
	doc, err := s.repo.GetDocument(ctx, p.TenantID, objectID)
	if denied, failure := deniedOrFailed(err); denied {
		return nil, false, nil
	} else if failure != nil {
		return nil, false, failure
	}
	if doc.Status == DocumentStatusRemoved || doc.ObjectVersionID != versionID {
		return nil, false, nil
	}
	published, err := s.repo.PublishedGenerationForVersion(ctx, p.TenantID, collection.ID, versionID, generation)
	if err != nil {
		return nil, false, err
	}
	if !published {
		return nil, false, nil
	}
	chunkID := stringFromPayload(payload, "chunk_id")
	key := collection.ID + "\x00" + versionID + "\x00" + fmt.Sprint(generation)
	chunksByID, cached := chunkCache[key]
	if !cached {
		chunks, err := s.repo.ListGenerationChunks(ctx, p.TenantID, collection.ID, versionID, generation)
		if err != nil {
			return nil, false, err
		}
		chunksByID = make(map[string]DocumentChunk, len(chunks))
		for _, chunk := range chunks {
			chunksByID[chunk.ID] = chunk
		}
		chunkCache[key] = chunksByID
	}
	chunk, found := chunksByID[chunkID]
	if !found {
		return nil, false, nil
	}
	result := SearchResult{DocumentID: objectID, CollectionID: collection.ID, ObjectVersionID: versionID, ChunkID: chunkID, Generation: generation, Spans: chunk.SpanRefs, ObservedSeq: version.ObservedSeq, ChunkStart: chunk.ChunkIndex, ChunkEnd: chunk.ChunkIndex, Score: point.Score, Content: chunk.Content, SourceURI: stringFromPayload(payload, "source_uri"), FileName: stringFromPayload(payload, "file_name"), HeaderPath: chunk.HeaderPath, Metadata: chunk.Metadata, CollectionName: collection.Name, SourceKind: s.sourceKindFor(ctx, p.TenantID, object, version, connectionKinds)}
	return &result, true, nil
}

func (s *Service) reauthorizeResults(ctx context.Context, p auth.Principal, results []SearchResult) ([]SearchResult, error) {
	retained := make([]SearchResult, 0, len(results))
	kinds := make(map[string]string)
	chunks := make(map[string]map[string]DocumentChunk)
	for _, result := range results {
		point := QdrantSearchPoint{Payload: map[string]any{
			"document_id": result.DocumentID, "object_version_id": result.ObjectVersionID,
			"generation": result.Generation, "chunk_id": result.ChunkID,
		}}
		current, keep, err := s.authorizeHit(ctx, p, Collection{ID: result.CollectionID}, point, kinds, chunks)
		if err != nil {
			return nil, err
		}
		if keep && current.ObservedSeq == result.ObservedSeq {
			retained = append(retained, result)
		}
	}
	return retained, nil
}

// sourceKindFor resolves the canonical source kind of a hit's document:
// the source_kind recorded in version provenance, else the connection
// kind, else the provenance kind — never object.Kind (often just
// "document").
func (s *Service) sourceKindFor(ctx context.Context, tenantID string, object source.Object, version source.ObjectVersion, connectionKinds map[string]string) string {
	var prov map[string]any
	_ = json.Unmarshal([]byte(version.Provenance), &prov)
	if kind := stringFromPayload(prov, "source_kind"); kind != "" {
		return kind
	}
	if object.ConnectionID != "" {
		if kind, ok := connectionKinds[object.ConnectionID]; ok {
			return kind
		}
		kind := ""
		if conn, err := s.repo.GetConnection(ctx, tenantID, object.ConnectionID); err == nil {
			kind = conn.Kind
		}
		connectionKinds[object.ConnectionID] = kind
		if kind != "" {
			return kind
		}
	}
	if kind := stringFromPayload(prov, "kind"); kind != "" {
		return kind
	}
	return "unknown"
}

// SearchReadable searches every collection the caller may read (or the
// readable subset named by explicit IDs) and fuses the per-collection ranked
// lists with reciprocal rank fusion — rank positions are comparable across
// collections, raw scores never are. Facet counts describe exactly the fused
// bounded result set after authorization: denied collections contribute
// neither results nor counts.
func (s *Service) SearchReadable(ctx context.Context, p auth.Principal, query string, mode QueryMode, collectionIDs []string, limit int) (SearchReadableResult, error) {
	return s.SearchReadableWithOptions(ctx, p, query, mode, collectionIDs, limit, SearchReadableOptions{})
}

type SearchReadableOptions struct {
	Rewrite        bool
	SelectSections bool
}

func (s *Service) SearchReadableWithOptions(ctx context.Context, p auth.Principal, query string, mode QueryMode, collectionIDs []string, limit int, options SearchReadableOptions) (SearchReadableResult, error) {
	input := SearchInput{Rewrite: options.Rewrite, SelectSections: options.SelectSections}
	if err := s.searchModels(&input); err != nil {
		return SearchReadableResult{}, err
	}
	if strings.TrimSpace(query) == "" {
		return SearchReadableResult{}, fmt.Errorf("%w: query is required", ErrInvalidInput)
	}
	if utf8.RuneCountInString(query) > maxQueryRunes {
		return SearchReadableResult{}, fmt.Errorf("%w: query exceeds %d characters", ErrInvalidInput, maxQueryRunes)
	}
	if mode == "" {
		mode = QueryHybrid
	}
	if mode != QueryHybrid && mode != QueryLexical && mode != QuerySemantic {
		return SearchReadableResult{}, fmt.Errorf("%w: unsupported search mode %q", ErrInvalidInput, mode)
	}
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}
	perCollection := 4 * limit
	if perCollection > 100 {
		perCollection = 100
	}
	readable, err := s.ListCollections(ctx, p)
	if err != nil {
		return SearchReadableResult{}, err
	}
	selected, err := selectReadableCollections(readable, collectionIDs)
	if err != nil {
		return SearchReadableResult{}, err
	}
	lists := make([]rankedSearchList, 0, len(selected))
	memo := newQueryEmbeddingMemo()
	var denied error
	for _, collection := range selected {
		input.Query, input.Mode, input.Limit = query, mode, perCollection
		results, err := s.searchCollection(ctx, p, collection, input, memo)
		if errors.Is(err, errSearchBindingDenied) {
			denied = err
			continue
		}
		if err != nil {
			return SearchReadableResult{}, err
		}
		if len(results) > 0 {
			lists = append(lists, rankedSearchList{results: results})
		}
	}
	if !memo.searched && denied != nil {
		return SearchReadableResult{}, denied
	}
	results := fuseRankedResults(lists, limit)
	results, err = s.reauthorizeResults(ctx, p, results)
	if err != nil {
		return SearchReadableResult{}, err
	}
	// Evidence persists only for results that survive fusion, and locations
	// resolve once over the fused set — the same retained-block path the
	// evidence views use.
	if err := s.persistResultEvidence(ctx, p, results); err != nil {
		return SearchReadableResult{}, err
	}
	locations, err := s.resultLocations(ctx, p.TenantID, results)
	if err != nil {
		return SearchReadableResult{}, err
	}
	for i := range results {
		results[i].Locations = locations[i]
	}
	facets := sourceKindFacets(results)
	return SearchReadableResult{Results: results, Facets: facets}, nil
}

// selectReadableCollections applies the explicit-ID rule: an explicit
// collection_ids entry is caller-supplied, so an unknown or unauthorized ID
// is denied outright; an absent selection means every readable collection.
func selectReadableCollections(readable []Collection, collectionIDs []string) ([]Collection, error) {
	explicit := make([]string, 0, len(collectionIDs))
	seen := make(map[string]struct{}, len(collectionIDs))
	for _, id := range collectionIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		explicit = append(explicit, id)
	}
	if len(explicit) == 0 {
		if len(readable) > maxSearchCollections {
			return nil, fmt.Errorf("%w: %d readable collections exceed the %d-collection search bound", ErrSearchWorkExceeded, len(readable), maxSearchCollections)
		}
		return readable, nil
	}
	if len(explicit) > maxSearchCollections {
		return nil, fmt.Errorf("%w: %d collection_ids exceed the %d-collection search bound", ErrSearchWorkExceeded, len(explicit), maxSearchCollections)
	}
	byID := make(map[string]Collection, len(readable))
	for _, c := range readable {
		byID[c.ID] = c
	}
	selected := make([]Collection, 0, len(explicit))
	for _, id := range explicit {
		c, ok := byID[id]
		if !ok {
			return nil, source.ErrDenied
		}
		selected = append(selected, c)
	}
	return selected, nil
}

// rankedSearchList is one bounded ranked results list.
type rankedSearchList struct {
	results []SearchResult
}

// fuseRankedResults merges ranked lists with reciprocal rank fusion and a
// deterministic tie-break (list order, then rank) — the fused order never
// depends on map iteration or raw cross-collection scores.
func fuseRankedResults(lists []rankedSearchList, limit int) []SearchResult {
	type key struct {
		collectionID string
		versionID    string
		chunkID      string
	}
	scores := make(map[key]float64)
	order := make(map[key]int)
	for listIndex, list := range lists {
		for rank, result := range list.results {
			id := key{result.CollectionID, result.ObjectVersionID, result.ChunkID}
			scores[id] += 1 / float64(rrfConstant+rank+1)
			if _, seen := order[id]; !seen {
				order[id] = listIndex<<32 | rank
			}
		}
	}
	results := make([]SearchResult, 0, len(scores))
	for id := range scores {
		listIndex := int(uint64(order[id]) >> 32)
		rank := int(uint64(order[id]) & 0xffffffff)
		results = append(results, lists[listIndex].results[rank])
	}
	sort.Slice(results, func(i, j int) bool {
		ki := key{results[i].CollectionID, results[i].ObjectVersionID, results[i].ChunkID}
		kj := key{results[j].CollectionID, results[j].ObjectVersionID, results[j].ChunkID}
		if scores[ki] != scores[kj] {
			return scores[ki] > scores[kj]
		}
		return order[ki] < order[kj]
	})
	if len(results) > limit {
		results = results[:limit]
	}
	return results
}

// persistResultEvidence mints server evidence for every surviving result.
func (s *Service) persistResultEvidence(ctx context.Context, p auth.Principal, results []SearchResult) error {
	for i := range results {
		if results[i].ChunkID == "" {
			continue
		}
		evidence := EvidenceRecord{TenantID: p.TenantID, ID: newID("evidence"), CollectionID: results[i].CollectionID, ObjectID: results[i].DocumentID, ObjectVersionID: results[i].ObjectVersionID, ObservedSeq: results[i].ObservedSeq, Generation: results[i].Generation, ChunkID: results[i].ChunkID, SpanRefs: results[i].Spans}
		if err := s.repo.SaveEvidence(ctx, evidence); err != nil {
			return err
		}
		results[i].EvidenceID = evidence.ID
	}
	return nil
}

// resultLocations batch-resolves span block locators over the fused result
// set — the same retained-block resolution the evidence views use; blocks
// that were never parsed resolve to nothing and are never invented.
func (s *Service) resultLocations(ctx context.Context, tenantID string, results []SearchResult) ([][]source.Locator, error) {
	blockIDs := make([]string, 0, len(results))
	seen := make(map[string]struct{})
	for _, result := range results {
		for _, span := range result.Spans {
			if span.BlockID == "" {
				continue
			}
			if _, dup := seen[span.BlockID]; dup {
				continue
			}
			seen[span.BlockID] = struct{}{}
			blockIDs = append(blockIDs, span.BlockID)
		}
	}
	if len(blockIDs) == 0 {
		return make([][]source.Locator, len(results)), nil
	}
	locators, err := s.repo.ListBlockLocators(ctx, tenantID, blockIDs)
	if err != nil {
		return nil, err
	}
	out := make([][]source.Locator, len(results))
	for i, result := range results {
		for _, span := range result.Spans {
			if locator, ok := locators[span.BlockID]; ok {
				out[i] = append(out[i], locator)
			}
		}
	}
	return out, nil
}

// sourceKindFacets counts the fused result set by canonical source kind,
// ordered by kind for a deterministic payload.
func sourceKindFacets(results []SearchResult) []SourceFacet {
	counts := make(map[string]int, len(results))
	for _, result := range results {
		counts[result.SourceKind]++
	}
	facets := make([]SourceFacet, 0, len(counts))
	for kind, count := range counts {
		facets = append(facets, SourceFacet{SourceKind: kind, Count: count})
	}
	sort.Slice(facets, func(i, j int) bool { return facets[i].SourceKind < facets[j].SourceKind })
	return facets
}

// BuildContext rechecks each source and expands to adjacent and parent-path chunks from its published generation.
func (s *Service) BuildContext(ctx context.Context, p auth.Principal, candidates []SearchResult, characterLimit int) (ContextResult, error) {
	var expanded []SearchResult
	for _, hit := range candidates {
		object, err := s.repo.GetObject(ctx, p.TenantID, hit.DocumentID)
		if denied, failure := deniedOrFailed(err); denied {
			expanded = append(expanded, SearchResult{DocumentID: hit.DocumentID})
			continue
		} else if failure != nil {
			return ContextResult{}, failure
		}
		if denied, failure := deniedOrFailed(s.authorizer.CanRead(ctx, p, object, source.ReadOptions{})); denied {
			expanded = append(expanded, SearchResult{DocumentID: hit.DocumentID})
			continue
		} else if failure != nil {
			return ContextResult{}, failure
		}
		version, err := s.repo.GetObjectVersion(ctx, p.TenantID, hit.DocumentID, object.ObservedSeq)
		if denied, failure := deniedOrFailed(err); denied {
			expanded = append(expanded, SearchResult{DocumentID: hit.DocumentID})
			continue
		} else if failure != nil {
			return ContextResult{}, failure
		}
		if hit.ObjectVersionID != "" && versionRowID(version) != hit.ObjectVersionID {
			expanded = append(expanded, SearchResult{DocumentID: hit.DocumentID})
			continue
		}
		if denied, failure := deniedOrFailed(s.authorizer.CanReadVersion(ctx, p, object, version, source.ReadOptions{})); denied {
			expanded = append(expanded, SearchResult{DocumentID: hit.DocumentID})
			continue
		} else if failure != nil {
			return ContextResult{}, failure
		}
		published, err := s.repo.PublishedGenerationForVersion(ctx, p.TenantID, hit.CollectionID, versionRowID(version), hit.Generation)
		if err != nil {
			return ContextResult{}, err
		}
		if !published {
			expanded = append(expanded, SearchResult{DocumentID: hit.DocumentID})
			continue
		}
		chunks, err := s.repo.ListGenerationChunks(ctx, p.TenantID, hit.CollectionID, versionRowID(version), hit.Generation)
		if err != nil {
			return ContextResult{}, err
		}
		center := -1
		for i := range chunks {
			if chunks[i].ID == hit.ChunkID {
				center = i
				break
			}
		}
		if center < 0 {
			continue
		}
		indices := map[int]struct{}{center: {}}
		if center > 0 {
			indices[center-1] = struct{}{}
		}
		if center+1 < len(chunks) {
			indices[center+1] = struct{}{}
		}
		parentPath := chunks[center].HeaderPath
		if slash := strings.LastIndex(parentPath, "/"); slash >= 0 {
			parentPath = parentPath[:slash]
		}
		if parentPath != "" {
			for i := range chunks {
				if chunks[i].HeaderPath == parentPath {
					indices[i] = struct{}{}
					break
				}
			}
		}
		for i := range chunks {
			if _, ok := indices[i]; !ok {
				continue
			}
			chunk := chunks[i]
			evidenceID := hit.EvidenceID
			if i != center {
				record := EvidenceRecord{TenantID: p.TenantID, ID: newID("evidence"), CollectionID: hit.CollectionID, ObjectID: hit.DocumentID, ObjectVersionID: versionRowID(version), ObservedSeq: version.ObservedSeq, Generation: hit.Generation, ChunkID: chunk.ID, SpanRefs: chunk.SpanRefs}
				if err := s.repo.SaveEvidence(ctx, record); err != nil {
					return ContextResult{}, err
				}
				evidenceID = record.ID
			}
			expanded = append(expanded, SearchResult{DocumentID: hit.DocumentID, CollectionID: hit.CollectionID, ObjectVersionID: versionRowID(version), ChunkID: chunk.ID, Generation: hit.Generation, Spans: chunk.SpanRefs, EvidenceID: evidenceID, Content: chunk.Content})
		}
	}
	return BuildContext(ctx, p, expanded, characterLimit, contextAuthorizerFunc(func(ctx context.Context, p auth.Principal, id string) bool {
		object, err := s.repo.GetObject(ctx, p.TenantID, id)
		return err == nil && s.authorizer.CanRead(ctx, p, object, source.ReadOptions{}) == nil
	})), nil
}

type contextAuthorizerFunc func(context.Context, auth.Principal, string) bool

func (f contextAuthorizerFunc) CanReadObject(ctx context.Context, p auth.Principal, id string) bool {
	return f(ctx, p, id)
}

func (s *Service) rewriteQuery(ctx context.Context, p auth.Principal, query string, refs []source.ObjectRef, model string) string {
	payload, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": "Rewrite the search query concisely. Return only the rewritten query."},
			{"role": "user", "content": query},
		},
		"max_tokens":  128,
		"temperature": 0,
	})
	if err != nil || s.executor == nil {
		return query
	}
	result, err := s.executor.Execute(ctx, p, inference.Request{
		Capability: inference.CapabilityChat, Model: model, Payload: payload, Inputs: refs,
		Estimate: billing.Usage{InputUnits: int64((utf8.RuneCountInString(query)+3)/4 + 64)},
	})
	if err != nil || result == nil || result.Body == nil {
		return query
	}
	defer result.Body.Close()
	response, err := io.ReadAll(io.LimitReader(result.Body, 1<<20))
	if err != nil {
		return query
	}
	var envelope struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(response, &envelope) != nil || len(envelope.Choices) == 0 {
		return query
	}
	rewritten := strings.TrimSpace(envelope.Choices[0].Message.Content)
	if rewritten == "" || utf8.RuneCountInString(rewritten) > 512 {
		return query
	}
	return rewritten
}

func (s *Service) selectSections(ctx context.Context, p auth.Principal, results []SearchResult, model string) []SearchResult {
	eligible := results
	if len(eligible) > 20 {
		eligible = eligible[:20]
	}
	candidates := make([]map[string]string, 0, len(eligible))
	byID := make(map[string]SearchResult, len(eligible))
	refs := make([]source.ObjectRef, 0, len(eligible))
	seenRefs := make(map[source.ObjectRef]struct{}, len(eligible))
	for i, result := range eligible {
		ref := source.ObjectRef{TenantID: p.TenantID, ObjectID: result.DocumentID, ObservedSeq: result.ObservedSeq}
		if _, ok := seenRefs[ref]; !ok {
			refs = append(refs, ref)
			seenRefs[ref] = struct{}{}
		}
		text := []rune(result.Content)
		if len(text) > 512 {
			text = text[:512]
		}
		id := fmt.Sprint(i)
		candidates = append(candidates, map[string]string{"evidence_id": id, "section": result.HeaderPath, "snippet": string(text)})
		byID[id] = result
	}
	candidatePayload, err := json.Marshal(candidates)
	if err != nil {
		return results
	}
	payload, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": "Select relevant evidence IDs only. Return a JSON array of IDs from the supplied candidates."},
			{"role": "user", "content": string(candidatePayload)},
		},
		"max_tokens":  256,
		"temperature": 0,
	})
	if err != nil || s.executor == nil {
		return results
	}
	result, err := s.executor.Execute(ctx, p, inference.Request{
		Capability: inference.CapabilityChat, Model: model, Payload: payload, Inputs: refs,
		Estimate: billing.Usage{InputUnits: 2048},
	})
	if err != nil || result == nil || result.Body == nil {
		return results
	}
	defer result.Body.Close()
	response, err := io.ReadAll(io.LimitReader(result.Body, 1<<20))
	if err != nil {
		return results
	}
	var envelope struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(response, &envelope) != nil || len(envelope.Choices) == 0 {
		return results
	}
	var ids []string
	if json.Unmarshal([]byte(strings.TrimSpace(envelope.Choices[0].Message.Content)), &ids) != nil || len(ids) == 0 {
		return results
	}
	selected := make([]SearchResult, 0, len(ids))
	seen := map[string]struct{}{}
	for _, id := range ids {
		if candidate, ok := byID[id]; ok {
			if _, duplicate := seen[id]; !duplicate {
				selected = append(selected, candidate)
				seen[id] = struct{}{}
			}
		}
	}
	if len(selected) == 0 {
		return results
	}
	return selected
}

func deniedOrFailed(err error) (bool, error) {
	if err == nil {
		return false, nil
	}
	if errors.Is(err, source.ErrDenied) {
		return true, nil
	}
	return false, err
}
