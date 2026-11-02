package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// ErrSearchWorkExceeded reports that the authorized enumeration/work bound
// was hit with unexamined rows remaining — never a silent first-N truncation.
// The HTTP layer maps it to a 400-class `search_scope_too_large`.
var ErrSearchWorkExceeded = errors.New("search scope exceeds the authorized work bound")

var ErrUtilityModelNotConfigured = errors.New("utility model is not configured")

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
	vectors map[queryEmbeddingKey][]float64
	scopes  map[queryEmbeddingBinding][]source.ObjectRef
	denied  map[queryEmbeddingBinding]bool
}

func newQueryEmbeddingMemo() *queryEmbeddingMemo {
	return &queryEmbeddingMemo{
		vectors: make(map[queryEmbeddingKey][]float64),
		scopes:  make(map[queryEmbeddingBinding][]source.ObjectRef),
		denied:  make(map[queryEmbeddingBinding]bool),
	}
}

func (m *queryEmbeddingMemo) addCollection(tenantID string, candidates []authorizedCandidate) {
	refs := make([]source.ObjectRef, 0, len(candidates))
	bindings := make(map[queryEmbeddingBinding]struct{})
	for _, candidate := range candidates {
		g := candidate.generation
		bindings[queryEmbeddingBinding{model: g.EmbeddingModel, prefix: g.QueryPrefix, dims: g.EmbeddingDimensions}] = struct{}{}
		refs = append(refs, source.ObjectRef{TenantID: tenantID, ObjectID: candidate.object.ID, ObservedSeq: candidate.version.ObservedSeq})
	}
	for binding := range bindings {
		m.scopes[binding] = append(m.scopes[binding], refs...)
	}
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

// SearchWorkBudget bounds the object/generation authorization work one
// collection may spend per search. It is a var so tests can tighten it.
var SearchWorkBudget = 2000

// Bounds for the cross-collection search page.
const (
	maxSearchCollections = 20
	defaultSearchLimit   = 20
	maxSearchLimit       = 50
	maxQueryRunes        = 512
	rrfConstant          = 60
)

// authorizedCandidate is one fully authorized, currently-published object
// version inside a collection.
type authorizedCandidate struct {
	generation IndexGeneration
	object     source.Object
	version    source.ObjectVersion
}

// authorizedCandidates streams a collection's published generations in a
// deterministic order and authorizes object/version/document reads for each,
// up to the work budget. It reports how many generations were examined
// (authorized or denied — both spend real reads). When more published
// generations exist than the budget allows examining it returns
// ErrSearchWorkExceeded; when the authorized set is exhausted first, results
// are complete.
func (s *Service) authorizedCandidates(ctx context.Context, p auth.Principal, collection Collection, workBudget int) ([]authorizedCandidate, int, error) {
	if workBudget <= 0 || workBudget > SearchWorkBudget {
		workBudget = SearchWorkBudget
	}
	// Fetch one row beyond the budget: its presence is the truthful signal
	// that unexamined rows remain.
	generations, err := s.repo.ListPublishedGenerationsBounded(ctx, p.TenantID, collection.ID, workBudget+1)
	if err != nil {
		return nil, 0, err
	}
	if len(generations) > workBudget {
		return nil, 0, fmt.Errorf("%w: collection %s has more published documents than the %d-object search bound", ErrSearchWorkExceeded, collection.ID, workBudget)
	}
	examined := 0
	candidates := make([]authorizedCandidate, 0, len(generations))
	for _, generation := range generations {
		examined++
		object, err := s.repo.GetObject(ctx, p.TenantID, generation.ObjectID)
		if denied, failure := deniedOrFailed(err); denied {
			continue
		} else if failure != nil {
			return nil, examined, failure
		}
		err = s.authorizer.CanRead(ctx, p, object, source.ReadOptions{})
		if denied, failure := deniedOrFailed(err); denied {
			continue
		} else if failure != nil {
			return nil, examined, failure
		}
		version, err := s.repo.GetObjectVersion(ctx, p.TenantID, generation.ObjectID, object.ObservedSeq)
		if denied, failure := deniedOrFailed(err); denied {
			continue
		} else if failure != nil {
			return nil, examined, failure
		}
		if versionRowID(version) != generation.ObjectVersionID {
			continue
		}
		err = s.authorizer.CanReadVersion(ctx, p, object, version, source.ReadOptions{})
		if denied, failure := deniedOrFailed(err); denied {
			continue
		} else if failure != nil {
			return nil, examined, failure
		}
		doc, err := s.repo.GetDocument(ctx, p.TenantID, generation.ObjectID)
		if denied, failure := deniedOrFailed(err); denied {
			continue
		} else if failure != nil {
			return nil, examined, failure
		}
		// A document being re-embedded (or whose re-embed failed) keeps
		// serving its published generation of the same version.
		if doc.Status == DocumentStatusRemoved || doc.ObjectVersionID != generation.ObjectVersionID {
			continue
		}
		candidates = append(candidates, authorizedCandidate{generation: generation, object: object, version: version})
	}
	return candidates, examined, nil
}

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
	candidates, _, err := s.authorizedCandidates(ctx, p, collection, 0)
	if err != nil {
		return nil, err
	}
	memo := newQueryEmbeddingMemo()
	memo.addCollection(p.TenantID, candidates)
	results, err := s.searchCollection(ctx, p, collection, input, candidates, memo)
	if err != nil {
		return nil, err
	}
	// Candidates were authorized before the vector query ran; re-check the
	// surviving hits so a mid-search revocation cannot leak material.
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

// searchCollection runs one authorized, bounded collection search over the
// pre-authorized candidates: embed, query the vector store within the
// published-generation filter and re-check every returned point against the
// candidate set before materializing results.
func (s *Service) searchCollection(ctx context.Context, p auth.Principal, collection Collection, input SearchInput, candidates []authorizedCandidate, memo *queryEmbeddingMemo) ([]SearchResult, error) {
	query := strings.TrimSpace(input.Query)
	current := make(map[string]authorizedCandidate, len(candidates))
	refs := make([]source.ObjectRef, 0, len(candidates))
	for _, candidate := range candidates {
		current[candidate.generation.ObjectVersionID] = candidate
		refs = append(refs, source.ObjectRef{TenantID: p.TenantID, ObjectID: candidate.object.ID, ObservedSeq: candidate.version.ObservedSeq})
	}
	if len(current) == 0 {
		return []SearchResult{}, nil
	}
	if input.Rewrite && strings.TrimSpace(input.RewriteModel) != "" && utf8.RuneCountInString(query) <= 512 {
		query = s.rewriteQuery(ctx, p, query, refs, input.RewriteModel)
	}
	limit := input.Limit
	if limit <= 0 {
		limit = 5
	}
	if limit > 50 {
		limit = 50
	}
	// Each generation is read from the index it was embedded into: while a
	// collection re-embeds under new settings, documents not yet done are
	// searched in the previous index with the previous model, and the
	// per-index rankings are fused.
	type binding struct {
		index, model, prefix string
		dims                 int
	}
	groups := make(map[binding][]IndexGeneration)
	for _, candidate := range current {
		g := candidate.generation
		b := binding{g.QdrantCollection, g.EmbeddingModel, g.QueryPrefix, g.EmbeddingDimensions}
		groups[b] = append(groups[b], g)
	}
	bindings := make([]binding, 0, len(groups))
	for b := range groups {
		bindings = append(bindings, b)
	}
	sort.Slice(bindings, func(i, j int) bool {
		if bindings[i].index != bindings[j].index {
			return bindings[i].index < bindings[j].index
		}
		if bindings[i].model != bindings[j].model {
			return bindings[i].model < bindings[j].model
		}
		if bindings[i].dims != bindings[j].dims {
			return bindings[i].dims < bindings[j].dims
		}
		return bindings[i].prefix < bindings[j].prefix
	})
	// Canonical source kinds resolve once per connection per collection.
	connectionKinds := make(map[string]string)
	chunkCache := make(map[string]map[string]DocumentChunk)
	lists := make([]rankedSearchList, 0, len(bindings))
	for _, b := range bindings {
		points, err := s.queryIndex(ctx, p, b.index, b.model, b.prefix, b.dims, groups[b], query, input, refs, limit, memo)
		if err != nil {
			return nil, err
		}
		results := make([]SearchResult, 0, len(points))
		for _, point := range points {
			payload := point.Payload
			objectID := stringFromPayload(payload, "document_id")
			versionID := stringFromPayload(payload, "object_version_id")
			generation := int64FromAny(payload["generation"])
			candidate, ok := current[versionID]
			if !ok || candidate.generation.ObjectID != objectID || candidate.generation.Generation != generation {
				continue
			}
			chunkID := stringFromPayload(payload, "chunk_id")
			key := versionID + "\x00" + fmt.Sprint(generation)
			chunksByID, cached := chunkCache[key]
			if !cached {
				chunks, err := s.repo.ListGenerationChunks(ctx, p.TenantID, collection.ID, versionID, generation)
				if err != nil {
					return nil, err
				}
				chunksByID = make(map[string]DocumentChunk, len(chunks))
				for _, chunk := range chunks {
					chunksByID[chunk.ID] = chunk
				}
				chunkCache[key] = chunksByID
			}
			chunk, found := chunksByID[chunkID]
			if !found {
				continue
			}
			results = append(results, SearchResult{DocumentID: objectID, CollectionID: collection.ID, ObjectVersionID: versionID, ChunkID: chunkID, Generation: generation, Spans: chunk.SpanRefs, ObservedSeq: candidate.version.ObservedSeq, ChunkStart: chunk.ChunkIndex, ChunkEnd: chunk.ChunkIndex, Score: point.Score, Content: chunk.Content, SourceURI: stringFromPayload(payload, "source_uri"), FileName: stringFromPayload(payload, "file_name"), HeaderPath: chunk.HeaderPath, Metadata: chunk.Metadata, CollectionName: collection.Name, SourceKind: s.sourceKindFor(ctx, p.TenantID, candidate, connectionKinds)})
		}
		lists = append(lists, rankedSearchList{results: results})
	}
	results := lists[0].results
	if len(lists) > 1 {
		results = fuseRankedResults(lists, limit)
	}
	if input.SelectSections && strings.TrimSpace(input.SectionModel) != "" && len(results) > 0 {
		results = s.selectSections(ctx, p, results, refs, input.SectionModel)
	}
	return results, nil
}

// queryIndex embeds the query with one index's model and size, then searches
// that index within the given generations.
func (s *Service) queryIndex(ctx context.Context, p auth.Principal, index, model, prefix string, dims int, generations []IndexGeneration, query string, input SearchInput, refs []source.ObjectRef, limit int, memo *queryEmbeddingMemo) ([]QdrantSearchPoint, error) {
	filter := FilterBuilder(p.TenantID, generations, input.Filter)
	var vector []float64
	if input.Mode != QueryLexical {
		binding := queryEmbeddingBinding{model: model, prefix: prefix, dims: dims}
		key := queryEmbeddingKey{queryEmbeddingBinding: binding, query: query}
		var found bool
		vector, found = memo.vectors[key]
		if !found {
			scope, cache := memo.scopes[binding]
			if !cache || memo.denied[binding] {
				cache = false
				scope = refs
			}
			vectors, err := s.embed(ctx, p, model, dims, []string{prefix + query}, embedScope{Inputs: scope})
			if cache && err != nil && (errors.Is(err, inference.ErrDenied) || errors.Is(err, source.ErrDenied) || errors.Is(err, inference.ErrNoRoute)) {
				memo.denied[binding] = true
				cache = false
				vectors, err = s.embed(ctx, p, model, dims, []string{prefix + query}, embedScope{Inputs: refs})
			}
			if err != nil {
				return nil, err
			}
			if len(vectors) == 0 || len(vectors[0]) == 0 {
				return nil, errors.New("embedding returned no vector")
			}
			vector = vectors[0]
			if cache {
				memo.vectors[key] = vector
			}
		}
	}
	client, ok := s.vectorStore.(queryClient)
	if !ok {
		if input.Mode == QueryLexical {
			return nil, errors.New("lexical search is unavailable for this vector store")
		}
		return s.vectorStore.SearchHybrid(ctx, index, vector, SparseVector{}, limit, filter)
	}
	if qdrant, ok := s.vectorStore.(*QdrantClient); ok && input.Mode == QueryHybrid {
		hasSparse, _, err := qdrant.collectionVectors(ctx, index)
		if err != nil {
			return nil, err
		}
		if !hasSparse {
			return s.vectorStore.SearchHybrid(ctx, index, vector, SparseVector{}, limit, filter)
		}
	}
	return client.Query(ctx, index, QueryInput{Mode: input.Mode, DenseVector: vector, LexicalText: query, Filter: filter, Limit: limit})
}

// sourceKindFor resolves the canonical source kind of a candidate: the
// source_kind recorded in version provenance, else the connection kind, else
// the provenance kind — never object.Kind (often just "document").
func (s *Service) sourceKindFor(ctx context.Context, tenantID string, candidate authorizedCandidate, connectionKinds map[string]string) string {
	var prov map[string]any
	_ = json.Unmarshal([]byte(candidate.version.Provenance), &prov)
	if kind := stringFromPayload(prov, "source_kind"); kind != "" {
		return kind
	}
	if candidate.object.ConnectionID != "" {
		if kind, ok := connectionKinds[candidate.object.ConnectionID]; ok {
			return kind
		}
		kind := ""
		if conn, err := s.repo.GetConnection(ctx, tenantID, candidate.object.ConnectionID); err == nil {
			kind = conn.Kind
		}
		connectionKinds[candidate.object.ConnectionID] = kind
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
	remaining := SearchWorkBudget
	memo := newQueryEmbeddingMemo()
	candidateSets := make([][]authorizedCandidate, len(selected))
	for i, collection := range selected {
		if remaining <= 0 {
			return SearchReadableResult{}, fmt.Errorf("%w: the request exceeded the %d-object aggregate search bound", ErrSearchWorkExceeded, SearchWorkBudget)
		}
		budget := remaining
		if budget > SearchWorkBudget {
			budget = SearchWorkBudget
		}
		candidates, examined, err := s.authorizedCandidates(ctx, p, collection, budget)
		if err != nil {
			return SearchReadableResult{}, err
		}
		remaining -= examined
		candidateSets[i] = candidates
		memo.addCollection(p.TenantID, candidates)
	}
	for i, collection := range selected {
		input.Query, input.Mode, input.Limit = query, mode, perCollection
		results, err := s.searchCollection(ctx, p, collection, input, candidateSets[i], memo)
		if err != nil {
			return SearchReadableResult{}, err
		}
		if len(results) > 0 {
			lists = append(lists, rankedSearchList{results: results})
		}
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

// rankedSearchList is one collection's bounded ranked results.
type rankedSearchList struct{ results []SearchResult }

// fuseRankedResults merges per-collection ranked lists with reciprocal rank
// fusion and a deterministic tie-break (list order, then rank) — the fused
// order never depends on map iteration or raw cross-collection scores.
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

// reauthorizeResults re-runs current authorization on every returned hit just
// before results are materialized: candidates were authorized before the
// vector query, and a revocation landing mid-search must not leak content,
// filenames or facets. Mirrors authorizedCandidates per hit (object CanRead +
// CanReadVersion + generation still the published current one); dropped hits
// lose results and counts. Published generations are fetched once per
// collection the hits touch.
func (s *Service) reauthorizeResults(ctx context.Context, p auth.Principal, results []SearchResult) ([]SearchResult, error) {
	if len(results) == 0 {
		return results, nil
	}
	type genKey struct {
		objectID, versionID string
		generation          int64
	}
	publishedCache := make(map[string]map[genKey]bool)
	kept := make([]SearchResult, 0, len(results))
	for i := range results {
		hit := results[i]
		object, err := s.repo.GetObject(ctx, p.TenantID, hit.DocumentID)
		if denied, failure := deniedOrFailed(err); denied {
			continue
		} else if failure != nil {
			return nil, failure
		}
		if err := s.authorizer.CanRead(ctx, p, object, source.ReadOptions{}); err != nil {
			continue
		}
		version, err := s.repo.GetObjectVersion(ctx, p.TenantID, hit.DocumentID, object.ObservedSeq)
		if denied, failure := deniedOrFailed(err); denied {
			continue
		} else if failure != nil {
			return nil, failure
		}
		if versionRowID(version) != hit.ObjectVersionID {
			continue
		}
		if err := s.authorizer.CanReadVersion(ctx, p, object, version, source.ReadOptions{}); err != nil {
			continue
		}
		doc, err := s.repo.GetDocument(ctx, p.TenantID, hit.DocumentID)
		if denied, failure := deniedOrFailed(err); denied {
			continue
		} else if failure != nil {
			return nil, failure
		}
		if doc.Status == DocumentStatusRemoved || doc.ObjectVersionID != hit.ObjectVersionID {
			continue
		}
		published, ok := publishedCache[hit.CollectionID]
		if !ok {
			generations, err := s.repo.ListPublishedGenerations(ctx, p.TenantID, hit.CollectionID)
			if err != nil {
				return nil, err
			}
			published = make(map[genKey]bool, len(generations))
			for _, g := range generations {
				published[genKey{objectID: g.ObjectID, versionID: g.ObjectVersionID, generation: g.Generation}] = true
			}
			publishedCache[hit.CollectionID] = published
		}
		if !published[genKey{objectID: hit.DocumentID, versionID: hit.ObjectVersionID, generation: hit.Generation}] {
			continue
		}
		kept = append(kept, hit)
	}
	return kept, nil
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
		return nil, nil
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
		items, err := s.repo.ListCollectionItems(ctx, p.TenantID, hit.CollectionID)
		if err != nil {
			return ContextResult{}, err
		}
		member := false
		for _, id := range items {
			if id == hit.DocumentID {
				member = true
				break
			}
		}
		if !member {
			expanded = append(expanded, SearchResult{DocumentID: hit.DocumentID})
			continue
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
		generations, err := s.repo.ListPublishedGenerations(ctx, p.TenantID, hit.CollectionID)
		if err != nil {
			return ContextResult{}, err
		}
		published := false
		for _, generation := range generations {
			if generation.ObjectVersionID == versionRowID(version) && generation.Generation == hit.Generation && generation.ObjectID == hit.DocumentID {
				published = true
				break
			}
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

func (s *Service) selectSections(ctx context.Context, p auth.Principal, results []SearchResult, refs []source.ObjectRef, model string) []SearchResult {
	eligible := results
	if len(eligible) > 20 {
		eligible = eligible[:20]
	}
	candidates := make([]map[string]string, 0, len(eligible))
	byID := make(map[string]SearchResult, len(eligible))
	for _, result := range eligible {
		text := []rune(result.Content)
		if len(text) > 512 {
			text = text[:512]
		}
		candidates = append(candidates, map[string]string{"evidence_id": result.EvidenceID, "section": result.HeaderPath, "snippet": string(text)})
		byID[result.EvidenceID] = result
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
