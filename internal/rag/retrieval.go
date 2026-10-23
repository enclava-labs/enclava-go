package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

func (s *Service) Search(ctx context.Context, p auth.Principal, input SearchInput) ([]SearchResult, error) {
	query := strings.TrimSpace(input.Query)
	if query == "" {
		return nil, errors.New("query is required")
	}
	if input.Mode == "" {
		input.Mode = QueryHybrid
	}
	if input.Mode != QueryHybrid && input.Mode != QueryLexical && input.Mode != QuerySemantic {
		return nil, fmt.Errorf("unsupported search mode %q", input.Mode)
	}
	collection, err := s.GetCollection(ctx, p, strings.TrimSpace(input.CollectionID))
	if err != nil {
		return nil, err
	}
	objectIDs, err := s.repo.ListCollectionItems(ctx, p.TenantID, collection.ID)
	if err != nil {
		return nil, err
	}
	generations, err := s.repo.ListPublishedGenerations(ctx, p.TenantID, collection.ID)
	if err != nil {
		return nil, err
	}
	items := make(map[string]bool, len(objectIDs))
	for _, id := range objectIDs {
		items[id] = true
	}
	current := make(map[string]IndexGeneration)
	refs := make([]source.ObjectRef, 0, len(generations))
	for _, generation := range generations {
		if !items[generation.ObjectID] || generation.State != "published" || generation.Tombstoned {
			continue
		}
		object, err := s.repo.GetObject(ctx, p.TenantID, generation.ObjectID)
		if denied, failure := deniedOrFailed(err); denied {
			continue
		} else if failure != nil {
			return nil, failure
		}
		err = s.authorizer.CanRead(ctx, p, object, source.ReadOptions{})
		if denied, failure := deniedOrFailed(err); denied {
			continue
		} else if failure != nil {
			return nil, failure
		}
		version, err := s.repo.GetObjectVersion(ctx, p.TenantID, generation.ObjectID, object.ObservedSeq)
		if denied, failure := deniedOrFailed(err); denied {
			continue
		} else if failure != nil {
			return nil, failure
		}
		if versionRowID(version) != generation.ObjectVersionID {
			continue
		}
		err = s.authorizer.CanReadVersion(ctx, p, object, version, source.ReadOptions{})
		if denied, failure := deniedOrFailed(err); denied {
			continue
		} else if failure != nil {
			return nil, failure
		}
		doc, err := s.repo.GetDocument(ctx, p.TenantID, generation.ObjectID)
		if denied, failure := deniedOrFailed(err); denied {
			continue
		} else if failure != nil {
			return nil, failure
		}
		if doc.Status != DocumentStatusReady || doc.ObjectVersionID != generation.ObjectVersionID {
			continue
		}
		current[generation.ObjectVersionID] = generation
		refs = append(refs, source.ObjectRef{TenantID: p.TenantID, ObjectID: object.ID, ObservedSeq: version.ObservedSeq})
	}
	if len(current) == 0 {
		return []SearchResult{}, nil
	}
	if input.Rewrite && strings.TrimSpace(input.RewriteModel) != "" && utf8.RuneCountInString(query) <= 512 {
		query = s.rewriteQuery(ctx, p, query, refs, input.RewriteModel)
	}
	filterGenerations := make([]IndexGeneration, 0, len(current))
	for _, generation := range current {
		filterGenerations = append(filterGenerations, generation)
	}
	filter := FilterBuilder(p.TenantID, filterGenerations, input.Filter)
	var vector []float64
	if input.Mode != QueryLexical {
		vectors, err := s.embed(ctx, p, collection.EmbeddingModel, []string{query}, embedScope{Inputs: refs})
		if err != nil {
			return nil, err
		}
		if len(vectors) == 0 || len(vectors[0]) == 0 {
			return nil, errors.New("embedding returned no vector")
		}
		vector = vectors[0]
	}
	limit := input.Limit
	if limit <= 0 {
		limit = 5
	}
	if limit > 50 {
		limit = 50
	}
	var points []QdrantSearchPoint
	if client, ok := s.vectorStore.(queryClient); ok {
		if qdrant, ok := s.vectorStore.(*QdrantClient); ok && input.Mode == QueryHybrid {
			hasSparse, _, err := qdrant.collectionVectors(ctx, collection.QdrantCollection)
			if err != nil {
				return nil, err
			}
			if !hasSparse {
				points, err = s.vectorStore.SearchHybrid(ctx, collection.QdrantCollection, vector, SparseVector{}, limit, filter)
			} else {
				points, err = client.Query(ctx, collection.QdrantCollection, QueryInput{Mode: input.Mode, DenseVector: vector, LexicalText: query, Filter: filter, Limit: limit})
			}
			if err != nil {
				return nil, err
			}
		} else {
			points, err = client.Query(ctx, collection.QdrantCollection, QueryInput{Mode: input.Mode, DenseVector: vector, LexicalText: query, Filter: filter, Limit: limit})
			if err != nil {
				return nil, err
			}
		}
	} else {
		if input.Mode == QueryLexical {
			return nil, errors.New("lexical search is unavailable for this vector store")
		}
		points, err = s.vectorStore.SearchHybrid(ctx, collection.QdrantCollection, vector, SparseVector{}, limit, filter)
		if err != nil {
			return nil, err
		}
	}
	results := make([]SearchResult, 0, len(points))
	chunkCache := make(map[string]map[string]DocumentChunk)
	for _, point := range points {
		payload := point.Payload
		objectID := stringFromPayload(payload, "document_id")
		versionID := stringFromPayload(payload, "object_version_id")
		generation := int64FromAny(payload["generation"])
		allowed, ok := current[versionID]
		if !ok || allowed.ObjectID != objectID || allowed.Generation != generation {
			continue
		}
		object, err := s.repo.GetObject(ctx, p.TenantID, objectID)
		if denied, failure := deniedOrFailed(err); denied {
			continue
		} else if failure != nil {
			return nil, failure
		}
		err = s.authorizer.CanRead(ctx, p, object, source.ReadOptions{})
		if denied, failure := deniedOrFailed(err); denied {
			continue
		} else if failure != nil {
			return nil, failure
		}
		version, err := s.repo.GetObjectVersion(ctx, p.TenantID, objectID, object.ObservedSeq)
		if denied, failure := deniedOrFailed(err); denied {
			continue
		} else if failure != nil {
			return nil, failure
		}
		if versionRowID(version) != versionID {
			continue
		}
		err = s.authorizer.CanReadVersion(ctx, p, object, version, source.ReadOptions{})
		if denied, failure := deniedOrFailed(err); denied {
			continue
		} else if failure != nil {
			return nil, failure
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
		results = append(results, SearchResult{DocumentID: objectID, CollectionID: collection.ID, ObjectVersionID: versionID, ChunkID: chunkID, Generation: generation, Spans: chunk.SpanRefs, ObservedSeq: version.ObservedSeq, ChunkStart: chunk.ChunkIndex, ChunkEnd: chunk.ChunkIndex, Score: point.Score, Content: chunk.Content, SourceURI: stringFromPayload(payload, "source_uri"), FileName: stringFromPayload(payload, "file_name"), HeaderPath: chunk.HeaderPath, Metadata: chunk.Metadata})
	}
	if input.SelectSections && strings.TrimSpace(input.SectionModel) != "" && len(results) > 0 {
		results = s.selectSections(ctx, p, results, refs, input.SectionModel)
	}
	// Evidence persists only for results that survive selection, so
	// discarded hits never strand unreachable evidence rows.
	for i := range results {
		if results[i].ChunkID == "" {
			continue
		}
		evidence := EvidenceRecord{TenantID: p.TenantID, ID: newID("evidence"), CollectionID: results[i].CollectionID, ObjectID: results[i].DocumentID, ObjectVersionID: results[i].ObjectVersionID, ObservedSeq: results[i].ObservedSeq, Generation: results[i].Generation, ChunkID: results[i].ChunkID, SpanRefs: results[i].Spans}
		if err := s.repo.SaveEvidence(ctx, evidence); err != nil {
			return nil, err
		}
		results[i].EvidenceID = evidence.ID
	}
	return results, nil
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
		Estimate: billing.Usage{InputUnits: int64((utf8.RuneCountInString(query)+3)/4 + 64), OutputUnits: 128},
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
		Estimate: billing.Usage{InputUnits: 2048, OutputUnits: 256},
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
