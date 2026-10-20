package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

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
	status, _, err := c.do(ctx, http.MethodGet, "/collections/"+url.PathEscape(collection), nil)
	if err == nil && status < 300 {
		return nil
	}
	if err != nil && status != http.StatusNotFound {
		return err
	}
	if status != http.StatusNotFound && status != 0 {
		return fmt.Errorf("qdrant collection check failed with status %d", status)
	}

	body, err := json.Marshal(map[string]any{
		"vectors":        map[string]any{"dense": map[string]any{"size": vectorSize, "distance": "Cosine"}},
		"sparse_vectors": map[string]any{"text": map[string]any{}},
	})
	if err != nil {
		return err
	}
	status, resp, err := c.do(ctx, http.MethodPut, "/collections/"+url.PathEscape(collection), body)
	if err != nil {
		return err
	}
	if status >= 300 {
		return fmt.Errorf("qdrant create collection failed: status=%d body=%s", status, string(resp))
	}
	return nil
}

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
				named["text"] = point.Sparse
			}
			vectors = named
		}
		wirePoints = append(wirePoints, map[string]any{"id": point.ID, "vector": vectors, "payload": point.Payload})
	}
	body, err := json.Marshal(map[string]any{"points": wirePoints})
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
		return 0, nil, fmt.Errorf("qdrant request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read qdrant response: %w", err)
	}
	return resp.StatusCode, respBody, nil
}
