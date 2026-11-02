package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrVectorStoreUnavailable = errors.New("vector store unavailable")

type QdrantClient struct {
	baseURL string
	client  *http.Client
}

type SparseVector struct {
	Indices []uint32  `json:"indices"`
	Values  []float32 `json:"values"`
}

type QdrantPoint struct {
	ID      string         `json:"id"`
	Vector  []float64      `json:"-"`
	Sparse  SparseVector   `json:"-"`
	Payload map[string]any `json:"payload"`
}

func NewQdrantClient(baseURL string, timeout time.Duration) *QdrantClient {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &QdrantClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: timeout},
	}
}

func (c *QdrantClient) EnsureCollection(ctx context.Context, collection string, vectorSize int) error {
	if strings.TrimSpace(collection) == "" {
		return fmt.Errorf("qdrant collection is required")
	}
	if vectorSize <= 0 {
		return fmt.Errorf("vector size must be positive")
	}
	compatible, status, err := c.verifyCollectionSchema(ctx, collection, vectorSize, false)
	if err != nil {
		return err
	}
	if compatible {
		return c.ensurePayloadIndexes(ctx, collection)
	}
	if status != http.StatusNotFound {
		return fmt.Errorf("qdrant collection %q has incompatible dense/BM25 schema", collection)
	}

	body, err := json.Marshal(map[string]any{
		"vectors":        map[string]any{"dense": map[string]any{"size": vectorSize, "distance": "Cosine"}},
		"sparse_vectors": map[string]any{"text": map[string]any{"modifier": "idf"}},
	})
	if err != nil {
		return err
	}
	status, resp, err := c.do(ctx, http.MethodPut, "/collections/"+url.PathEscape(collection), body)
	if err != nil {
		return err
	}
	if status == http.StatusConflict {
		// A concurrent attempt created the same index first: accept it when
		// the winner's schema is the one this caller requires.
		compatible, _, err = c.verifyCollectionSchema(ctx, collection, vectorSize, true)
		if err != nil {
			return err
		}
		if !compatible {
			return fmt.Errorf("qdrant collection %q has incompatible dense/BM25 schema", collection)
		}
	} else if status >= 300 {
		return fmt.Errorf("qdrant create collection failed: status=%d body=%s", status, string(resp))
	}
	return c.ensurePayloadIndexes(ctx, collection)
}

// verifyCollectionSchema reports whether the existing collection carries the
// required dense/BM25 schema; status is the GET's HTTP status (0 on
// transport error).
func (c *QdrantClient) verifyCollectionSchema(ctx context.Context, collection string, vectorSize int, retryMissing bool) (bool, int, error) {
	for attempt := 0; ; attempt++ {
		compatible, status, err := c.checkCollectionSchema(ctx, collection, vectorSize)
		retry := status >= 500 || (retryMissing && status == http.StatusNotFound)
		if !retry {
			return compatible, status, err
		}
		if attempt == 4 {
			if err == nil {
				err = fmt.Errorf("%w: qdrant collection %q not yet visible", ErrVectorStoreUnavailable, collection)
			}
			return false, status, err
		}
		timer := time.NewTimer(100 * time.Millisecond << attempt)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, status, ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *QdrantClient) checkCollectionSchema(ctx context.Context, collection string, vectorSize int) (compatible bool, status int, err error) {
	status, resp, err := c.do(ctx, http.MethodGet, "/collections/"+url.PathEscape(collection), nil)
	if err != nil {
		if status != http.StatusNotFound {
			return false, status, err
		}
		return false, status, nil
	}
	if status == http.StatusNotFound {
		return false, status, nil
	}
	if status >= 300 {
		return false, status, fmt.Errorf("qdrant collection check failed with status %d", status)
	}
	var envelope struct {
		Result struct {
			Config struct {
				Params struct {
					Vectors map[string]struct {
						Size     int    `json:"size"`
						Distance string `json:"distance"`
					} `json:"vectors"`
					SparseVectors map[string]json.RawMessage `json:"sparse_vectors"`
				} `json:"params"`
			} `json:"config"`
		} `json:"result"`
	}
	if decodeErr := json.Unmarshal(resp, &envelope); decodeErr != nil {
		return false, status, fmt.Errorf("decode existing qdrant schema: %w", decodeErr)
	}
	dense, hasDense := envelope.Result.Config.Params.Vectors["dense"]
	sparse, hasSparse := envelope.Result.Config.Params.SparseVectors["text"]
	var sparseConfig struct {
		Modifier string `json:"modifier"`
	}
	_ = json.Unmarshal(sparse, &sparseConfig)
	return hasDense && dense.Size == vectorSize && dense.Distance == "Cosine" && hasSparse && sparseConfig.Modifier == "idf", status, nil
}

// payloadIndexes are the keyword/bool payload indexes every search filter
// relies on; created idempotently so collections made before the access
// projection gain them on first touch. tenant_id and collection ids share
// the index: uuid covers both, keyword does not index uuids.
var payloadIndexes = []struct{ key, schema string }{
	{"tenant_id", "keyword"},
	{"acl", "keyword"},
	{"published", "bool"},
	{"access_enabled", "bool"},
}

func (c *QdrantClient) ensurePayloadIndexes(ctx context.Context, collection string) error {
	for _, index := range payloadIndexes {
		body, err := json.Marshal(map[string]any{"field_name": index.key, "field_schema": index.schema})
		if err != nil {
			return err
		}
		status, resp, err := c.do(ctx, http.MethodPut, "/collections/"+url.PathEscape(collection)+"/index", body)
		if err != nil {
			return err
		}
		if status >= 300 && status != http.StatusConflict {
			return fmt.Errorf("qdrant create payload index %q failed: status=%d body=%s", index.key, status, string(resp))
		}
	}
	return nil
}

// qdrantUpsertBatch bounds one upsert request: Qdrant refuses JSON bodies
// over 32 MiB by default and a 2560-dimension point serializes to ~58 KB, so
// a book-sized document (1,247 points ≈ 72 MB) must be split. Upserts are
// idempotent by point id, so a failed batch is safely retried whole.
const qdrantUpsertBatch = 128

func (c *QdrantClient) UpsertPoints(ctx context.Context, collection string, points []QdrantPoint) error {
	hasSparse, namedDense, err := c.collectionVectors(ctx, collection)
	if err != nil {
		return err
	}
	wirePoints := make([]map[string]any, 0, len(points))
	for _, point := range points {
		var vectors any = point.Vector
		if namedDense {
			named := map[string]any{"dense": point.Vector}
			if hasSparse {
				text, ok := point.Payload["search_text"].(string)
				if !ok {
					text, ok = point.Payload["content"].(string)
				}
				if ok {
					named["text"] = map[string]any{"text": text, "model": "qdrant/bm25"}
				} else {
					named["text"] = point.Sparse
				}
			}
			vectors = named
		}
		wirePoints = append(wirePoints, map[string]any{"id": point.ID, "vector": vectors, "payload": point.Payload})
	}
	for start := 0; start < len(wirePoints); start += qdrantUpsertBatch {
		body, err := json.Marshal(map[string]any{"points": wirePoints[start:min(start+qdrantUpsertBatch, len(wirePoints))]})
		if err != nil {
			return err
		}
		status, resp, err := c.do(ctx, http.MethodPut, "/collections/"+url.PathEscape(collection)+"/points?wait=true", body)
		if err != nil {
			return err
		}
		if status >= 300 {
			return fmt.Errorf("qdrant upsert failed: status=%d body=%s", status, string(resp))
		}
	}
	return nil
}

func (c *QdrantClient) DeleteDocumentPoints(ctx context.Context, collection, documentID string) error {
	body, err := json.Marshal(map[string]any{
		"filter": map[string]any{
			"must": []map[string]any{
				{"key": "document_id", "match": map[string]any{"value": documentID}},
			},
		},
	})
	if err != nil {
		return err
	}
	status, resp, err := c.do(ctx, http.MethodPost, "/collections/"+url.PathEscape(collection)+"/points/delete?wait=true", body)
	if err != nil {
		return err
	}
	if status >= 300 && status != http.StatusNotFound {
		return fmt.Errorf("qdrant delete document points failed: status=%d body=%s", status, string(resp))
	}
	return nil
}

func (c *QdrantClient) DeleteGenerationPoints(ctx context.Context, collection, versionID string, generation int64) error {
	filter := map[string]any{"must": []map[string]any{
		{"key": "object_version_id", "match": map[string]any{"value": versionID}},
		{"key": "generation", "match": map[string]any{"value": generation}},
	}}
	body, err := json.Marshal(map[string]any{"filter": filter})
	if err != nil {
		return err
	}
	status, resp, err := c.do(ctx, http.MethodPost, "/collections/"+url.PathEscape(collection)+"/points/delete?wait=true", body)
	if err != nil {
		return err
	}
	if status >= 300 && status != http.StatusNotFound {
		return fmt.Errorf("qdrant delete generation points failed: status=%d body=%s", status, string(resp))
	}
	return nil
}

func (c *QdrantClient) SetPayload(ctx context.Context, collection string, filter map[string]any, payload map[string]any) error {
	body, err := json.Marshal(map[string]any{"filter": filter, "payload": payload})
	if err != nil {
		return err
	}
	status, resp, err := c.do(ctx, http.MethodPost, "/collections/"+url.PathEscape(collection)+"/points/payload?wait=true", body)
	if err != nil {
		return err
	}
	if status >= 300 && status != http.StatusNotFound {
		return fmt.Errorf("qdrant set payload failed: status=%d body=%s", status, string(resp))
	}
	return nil
}

func (c *QdrantClient) DeleteCollection(ctx context.Context, collection string) error {
	status, resp, err := c.do(ctx, http.MethodDelete, "/collections/"+url.PathEscape(collection), nil)
	if err != nil {
		return err
	}
	if status >= 300 && status != http.StatusNotFound {
		return fmt.Errorf("qdrant delete collection failed: status=%d body=%s", status, string(resp))
	}
	return nil
}

func (c *QdrantClient) CountPoints(ctx context.Context, collection string) (int64, error) {
	status, resp, err := c.do(ctx, http.MethodPost, "/collections/"+url.PathEscape(collection)+"/points/count", []byte(`{"exact":true}`))
	if err != nil {
		return 0, err
	}
	if status >= 300 {
		return 0, fmt.Errorf("qdrant count failed: status=%d body=%s", status, string(resp))
	}
	var result struct {
		Result *struct {
			Count *int64 `json:"count"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return 0, fmt.Errorf("decode qdrant count: %w", err)
	}
	if result.Result == nil || result.Result.Count == nil {
		return 0, fmt.Errorf("decode qdrant count: missing count")
	}
	return *result.Result.Count, nil
}

type QdrantSearchPoint struct {
	ID      any            `json:"id"`
	Score   float64        `json:"score"`
	Payload map[string]any `json:"payload"`
}

func (c *QdrantClient) Search(ctx context.Context, collection string, vector []float64, limit int, filter map[string]any) ([]QdrantSearchPoint, error) {
	return c.SearchHybrid(ctx, collection, vector, SparseVector{}, limit, filter)
}

func (c *QdrantClient) SearchHybrid(ctx context.Context, collection string, vector []float64, sparse SparseVector, limit int, filter map[string]any) ([]QdrantSearchPoint, error) {
	hasSparse, namedDense, err := c.collectionVectors(ctx, collection)
	if err != nil {
		return nil, err
	}
	hasSparse = hasSparse && len(sparse.Indices) > 0
	return c.search(ctx, collection, vector, sparse, limit, filter, hasSparse, namedDense)
}

func (c *QdrantClient) search(ctx context.Context, collection string, vector []float64, sparse SparseVector, limit int, filter map[string]any, hasSparse, namedDense bool) ([]QdrantSearchPoint, error) {
	if limit <= 0 {
		limit = 5
	}
	var payload map[string]any
	path := "/collections/" + url.PathEscape(collection) + "/points/search"
	if hasSparse {
		prefetch := []map[string]any{
			{"query": vector, "using": "dense", "limit": limit, "filter": filter},
			{"query": sparse, "using": "text", "limit": limit, "filter": filter},
		}
		payload = map[string]any{"prefetch": prefetch, "query": map[string]any{"fusion": "rrf"}, "limit": limit, "with_payload": true}
		path = "/collections/" + url.PathEscape(collection) + "/points/query"
	} else if namedDense {
		payload = map[string]any{"query": vector, "using": "dense", "limit": limit, "with_payload": true}
		if len(filter) > 0 {
			payload["filter"] = filter
		}
		path = "/collections/" + url.PathEscape(collection) + "/points/query"
	} else {
		payload = map[string]any{"vector": vector, "limit": limit, "with_payload": true}
		if len(filter) > 0 {
			payload["filter"] = filter
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	status, resp, err := c.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, fmt.Errorf("qdrant search failed: status=%d body=%s", status, string(resp))
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Status string          `json:"status"`
	}
	if err := json.Unmarshal(resp, &envelope); err != nil {
		return nil, fmt.Errorf("decode qdrant search response: %w", err)
	}
	var points []QdrantSearchPoint
	if len(envelope.Result) > 0 && envelope.Result[0] == '[' {
		err = json.Unmarshal(envelope.Result, &points)
	} else {
		var queryResult struct {
			Points []QdrantSearchPoint `json:"points"`
		}
		err = json.Unmarshal(envelope.Result, &queryResult)
		points = queryResult.Points
	}
	if err != nil {
		return nil, fmt.Errorf("decode qdrant search results: %w", err)
	}
	return points, nil
}

func (c *QdrantClient) collectionVectors(ctx context.Context, collection string) (hasSparse, namedDense bool, err error) {
	status, resp, err := c.do(ctx, http.MethodGet, "/collections/"+url.PathEscape(collection), nil)
	if err != nil {
		return false, false, err
	}
	if status >= 300 {
		return false, false, fmt.Errorf("qdrant collection schema failed: status=%d body=%s", status, string(resp))
	}
	var envelope struct {
		Result struct {
			Config struct {
				Params struct {
					Vectors       json.RawMessage            `json:"vectors"`
					SparseVectors map[string]json.RawMessage `json:"sparse_vectors"`
				} `json:"params"`
			} `json:"config"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &envelope); err != nil {
		return false, false, fmt.Errorf("decode qdrant collection schema: %w", err)
	}
	var named map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Result.Config.Params.Vectors, &named); err == nil {
		_, namedDense = named["dense"]
	}
	_, hasSparse = envelope.Result.Config.Params.SparseVectors["text"]
	return hasSparse, namedDense, nil
}

func (c *QdrantClient) do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, ctx.Err()
		}
		return 0, nil, fmt.Errorf("%w: qdrant request failed: %w", ErrVectorStoreUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("%w: read qdrant response: %w", ErrVectorStoreUnavailable, err)
	}
	if resp.StatusCode >= 500 {
		return resp.StatusCode, respBody, fmt.Errorf("%w: qdrant request failed: status=%d body=%s", ErrVectorStoreUnavailable, resp.StatusCode, respBody)
	}
	return resp.StatusCode, respBody, nil
}
