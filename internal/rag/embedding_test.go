package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/inference"
)

// capExecutor mimics an OpenAI-compatible embeddings provider behind the
// adapter's 8 MiB JSON response cap: a batch whose serialized vectors
// (~21 bytes per float on vLLM) would exceed it fails like the transport.
type capExecutor struct {
	dim     int
	batches []int
}

func (e *capExecutor) Execute(_ context.Context, _ auth.Principal, req inference.Request) (*inference.Result, error) {
	var payload struct {
		Input []string `json:"input"`
	}
	if err := json.Unmarshal(req.Payload, &payload); err != nil {
		return nil, err
	}
	n := len(payload.Input)
	e.batches = append(e.batches, n)
	if n*e.dim*21 > 8<<20 {
		return nil, errors.New("upstream response body exceeds maximum size (limit 8388608 bytes)")
	}
	vector := strings.TrimSuffix(strings.Repeat("0,", e.dim), ",")
	var b strings.Builder
	b.WriteString(`{"object":"list","data":[`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"index":%d,"embedding":[%s]}`, i, vector)
	}
	fmt.Fprintf(&b, `],"usage":{"prompt_tokens":%d,"total_tokens":%d}}`, n, n)
	return &inference.Result{Body: io.NopCloser(strings.NewReader(b.String()))}, nil
}

// TestCreateEmbeddingsStaysUnderProviderResponseCap: a book-sized document
// embedded with a 2,560-dimension model batches under the provider's
// response cap instead of failing the whole ingest.
func TestCreateEmbeddingsStaysUnderProviderResponseCap(t *testing.T) {
	exec := &capExecutor{dim: 2560}
	inputs := make([]string, 1300)
	for i := range inputs {
		inputs[i] = fmt.Sprintf("chunk %d of a long novel", i)
	}
	vectors, err := createEmbeddings(context.Background(), exec, auth.Principal{TenantID: "t", ActorID: "a"}, "qwen3-embedding-4b", 0, inputs, embedScope{})
	if err != nil {
		t.Fatalf("embed 1300 chunks at 2560 dims: %v (batch sizes %v)", err, exec.batches)
	}
	if len(vectors) != len(inputs) || len(vectors[len(vectors)-1]) != 2560 {
		t.Fatalf("got %d vectors, last dim %d", len(vectors), len(vectors[len(vectors)-1]))
	}
}

// TestCreateEmbeddingsRejectsSizeAboveModel: a collection asking for more
// dimensions than its model returns fails instead of indexing short vectors.
func TestCreateEmbeddingsRejectsSizeAboveModel(t *testing.T) {
	_, err := createEmbeddings(context.Background(), &capExecutor{dim: 768}, auth.Principal{TenantID: "t", ActorID: "a"}, "nomic", 1024, []string{"x"}, embedScope{})
	if err == nil || !strings.Contains(err.Error(), "returns 768 dimensions") {
		t.Fatalf("1024 dims from a 768-dimension model = %v, want a size error", err)
	}
}

func TestDefaultEmbeddingPrefixes(t *testing.T) {
	instruct := "Instruct: Given a web search query, retrieve relevant passages that answer the query\nQuery: "
	bge := "Represent this sentence for searching relevant passages: "
	tests := []struct {
		model, query, passage string
	}{
		{"Qwen/QWEN3-Embedding-4B", instruct, ""},
		{"org/multilingual-e5-large-instruct", instruct, ""},
		{"nomic-ai/nomic-embed-text-v1.5", "search_query: ", "search_document: "},
		{"intfloat/e5-large-v2", "query: ", "passage: "},
		{"multilingual-e5-small", "query: ", "passage: "},
		{"BAAI/bge-large-en-v1.5", bge, ""},
		{"snowflake-arctic-embed-m", bge, ""},
		{"mxbai-embed-large-v1", bge, ""},
		{"bge-large-zh", "", ""},
		{"unknown", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			query, passage := defaultEmbeddingPrefixes(tt.model)
			if query != tt.query || passage != tt.passage {
				t.Fatalf("got %q, %q; want %q, %q", query, passage, tt.query, tt.passage)
			}
		})
	}
}
