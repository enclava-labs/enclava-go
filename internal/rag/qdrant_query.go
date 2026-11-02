package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// QueryMode selects the retrieval branch without requiring an embedding for lexical search.
type QueryMode string

const (
	QueryHybrid   QueryMode = "hybrid"
	QueryLexical  QueryMode = "lexical"
	QuerySemantic QueryMode = "semantic"
)

type QueryInput struct {
	DenseVector []float64
	LexicalText string
	Filter      map[string]any
	Limit       int
	Offset      int
	Mode        QueryMode
}

// AccessFilter binds a query to the tenant's published, access-enabled
// points visible to the principal's token set. Unprojected points (written
// before the acl/published projection) pass to the authoritative post-check
// via is_empty so a backfill can never hide a document. Authorization
// itself is re-checked per hit against PostgreSQL — this filter is a
// prefilter, never the decision.
func AccessFilter(tenantID string, tokens []string, narrower map[string]any) map[string]any {
	aclShould := []any{map[string]any{"is_empty": map[string]any{"key": "acl"}}}
	if len(tokens) > 0 {
		aclShould = append([]any{map[string]any{"key": "acl", "match": map[string]any{"any": tokens}}}, aclShould...)
	}
	publishedShould := []any{
		map[string]any{"key": "published", "match": map[string]any{"value": true}},
		map[string]any{"is_empty": map[string]any{"key": "published"}},
	}
	must := []any{
		map[string]any{"key": "tenant_id", "match": map[string]any{"value": tenantID}},
		map[string]any{"key": "access_enabled", "match": map[string]any{"value": true}},
		map[string]any{"should": aclShould},
		map[string]any{"should": publishedShould},
	}
	if narrower != nil {
		must = append(must, narrower)
	}
	return map[string]any{"must": must}
}

// Query submits one branch or an explicit dense+sparse RRF query to Qdrant.
func (c *QdrantClient) Query(ctx context.Context, collection string, in QueryInput) ([]QdrantSearchPoint, error) {
	if in.Limit <= 0 {
		in.Limit = 5
	}
	if in.Mode == "" {
		in.Mode = QueryHybrid
	}
	if in.Mode != QueryLexical && in.Mode != QuerySemantic && in.Mode != QueryHybrid {
		return nil, fmt.Errorf("unsupported query mode %q", in.Mode)
	}
	if (in.Mode == QuerySemantic || in.Mode == QueryHybrid) && len(in.DenseVector) == 0 {
		return nil, fmt.Errorf("dense vector is required")
	}
	if (in.Mode == QueryLexical || in.Mode == QueryHybrid) && strings.TrimSpace(in.LexicalText) == "" {
		return nil, fmt.Errorf("lexical text is required")
	}
	path := "/collections/" + url.PathEscape(collection) + "/points/query"
	payload := map[string]any{"limit": in.Limit, "with_payload": true}
	prefetchLimit := in.Limit
	if in.Offset > 0 {
		prefetchLimit += in.Offset
	}
	dense := map[string]any{"query": in.DenseVector, "using": "dense", "filter": in.Filter, "limit": prefetchLimit}
	lexical := map[string]any{"query": map[string]any{"text": in.LexicalText, "model": "qdrant/bm25"}, "using": "text", "filter": in.Filter, "limit": prefetchLimit}
	switch in.Mode {
	case QueryHybrid:
		payload["prefetch"] = []any{dense, lexical}
		payload["query"] = map[string]any{"fusion": "rrf"}
	case QueryLexical:
		payload["query"] = lexical["query"]
		payload["using"], payload["filter"] = lexical["using"], lexical["filter"]
	case QuerySemantic:
		payload["query"], payload["using"], payload["filter"] = dense["query"], dense["using"], dense["filter"]
	}
	if in.Offset > 0 {
		payload["offset"] = in.Offset
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	status, response, err := c.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, fmt.Errorf("qdrant query failed: status=%d body=%s", status, response)
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(response, &envelope); err != nil {
		return nil, fmt.Errorf("decode qdrant query response: %w", err)
	}
	var result struct {
		Points []QdrantSearchPoint `json:"points"`
	}
	if len(envelope.Result) > 0 && envelope.Result[0] == '[' {
		if err := json.Unmarshal(envelope.Result, &result.Points); err != nil {
			return nil, err
		}
	} else if err := json.Unmarshal(envelope.Result, &result); err != nil {
		return nil, err
	}
	return result.Points, nil
}
