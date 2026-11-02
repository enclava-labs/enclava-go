package rag

import (
	"context"
	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/inference"
	"enclava-go/internal/source"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"unicode/utf8"
)

func defaultEmbeddingPrefixes(model string) (query, passage string) {
	model = strings.ToLower(model)
	if i := strings.LastIndexByte(model, '/'); i >= 0 {
		model = model[i+1:]
	}
	switch {
	case strings.HasPrefix(model, "qwen3-embedding"), model == "multilingual-e5-large-instruct":
		return "Instruct: Given a web search query, retrieve relevant passages that answer the query\nQuery: ", ""
	case strings.HasPrefix(model, "nomic-embed-text"):
		return "search_query: ", "search_document: "
	case strings.HasPrefix(model, "e5-"), strings.HasPrefix(model, "multilingual-e5-"):
		return "query: ", "passage: "
	case strings.HasPrefix(model, "bge-") && strings.Contains(model, "-en"),
		strings.HasPrefix(model, "snowflake-arctic-embed"), strings.HasPrefix(model, "mxbai-embed-large"):
		return "Represent this sentence for searching relevant passages: ", ""
	default:
		return "", ""
	}
}

func tokenEstimate(text string) int {
	if text == "" {
		return 0
	}
	return (utf8.RuneCountInString(text) + 3) / 4
}

// maxEmbeddingInputs caps batch size by item count; maxEmbeddingBatchUnits
// caps the summed estimate so one batch always sits inside the routed
// embeddings operation bound — a route-level limit is enforced by the
// executor, never assumed from a fixed item count. The item cap also bounds
// the provider's JSON response, which grows with inputs × dimensions
// (~21 bytes per float, measured on vLLM): 64 keeps vectors up to ~6000
// dimensions under the 8 MiB provider response limit — 2048 overflowed it
// for a 2560-dimension model on any document past ~150 chunks.
const (
	maxEmbeddingInputs     = 64
	maxEmbeddingBatchUnits = 1 << 20
)

type embeddingResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// embedScope is the authorization/accounting context shared by every batch
// of one embedding attempt: the same initiating or delegated principal,
// immutable input references and durable job linkage.
type embedScope struct {
	Inputs    []source.ObjectRef
	Artifacts []artifacts.Ref
	JobID     string
	AttemptID string
}

// createEmbeddings batches embedding calls through the shared executor —
// policy resolution, pricing, dispatch and settlement all live inside
// inference.Service.Execute; there is no direct-provider or key-zero path.
// dims > 0 keeps only each vector's first dims components (see
// truncateEmbedding); 0 keeps the model's full output.
func createEmbeddings(ctx context.Context, executor Executor, p auth.Principal, model string, dims int, inputs []string, scope embedScope) ([][]float64, error) {
	vectors := make([][]float64, len(inputs))
	var dim int
	for offset := 0; offset < len(inputs); {
		// Bound each batch by both item count and summed unit estimate.
		units := int64(0)
		end := offset
		for end < len(inputs) && end-offset < maxEmbeddingInputs {
			u := int64(tokenEstimate(inputs[end]))
			if units+u > maxEmbeddingBatchUnits && end > offset {
				break
			}
			units += u
			end++
		}
		batchVectors, err := createEmbeddingBatch(ctx, executor, p, model, inputs[offset:end], units, scope)
		if err != nil {
			return nil, err
		}
		for i, vec := range batchVectors {
			if dim == 0 {
				dim = len(vec)
			} else if len(vec) != dim {
				return nil, fmt.Errorf("embedding dimension mismatch: got %d, want %d", len(vec), dim)
			}
			vectors[offset+i] = vec
		}
		offset = end
	}
	if dims > 0 {
		if dims > dim {
			return nil, fmt.Errorf("embedding model %q returns %d dimensions, fewer than the collection's %d", model, dim, dims)
		}
		for i := range vectors {
			vectors[i] = truncateEmbedding(vectors[i], dims)
		}
	}
	return vectors, nil
}

// truncateEmbedding keeps the first dims components rescaled to unit length:
// Matryoshka-trained models front-load information, so the prefix is itself
// an embedding — the same vector a server-side `dimensions` request returns.
// Done here because not every server supports that parameter (vLLM rejects it).
func truncateEmbedding(vec []float64, dims int) []float64 {
	vec = vec[:dims]
	var sum float64
	for _, v := range vec {
		sum += v * v
	}
	if sum == 0 {
		return vec
	}
	norm := math.Sqrt(sum)
	for i := range vec {
		vec[i] /= norm
	}
	return vec
}

func createEmbeddingBatch(ctx context.Context, executor Executor, p auth.Principal, model string, inputs []string, estimate int64, scope embedScope) ([][]float64, error) {
	payload, err := json.Marshal(map[string]any{"input": inputs})
	if err != nil {
		return nil, fmt.Errorf("marshal embeddings request: %w", err)
	}
	res, err := executor.Execute(ctx, p, inference.Request{
		Capability: inference.CapabilityEmbeddings,
		Model:      model,
		Payload:    payload,
		Inputs:     scope.Inputs,
		Artifacts:  scope.Artifacts,
		Estimate:   billing.Usage{InputUnits: estimate},
		JobID:      scope.JobID,
		AttemptID:  scope.AttemptID,
	})
	if err != nil {
		return nil, fmt.Errorf("provider embeddings: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.SettlementError != nil {
		return nil, fmt.Errorf("settle reservation: %w", res.SettlementError)
	}
	respBody, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("read embeddings response: %w", err)
	}
	var resp embeddingResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("decode embeddings response: %w", err)
	}
	if len(resp.Data) != len(inputs) {
		return nil, fmt.Errorf("embedding count mismatch: got %d, want %d", len(resp.Data), len(inputs))
	}
	vectors := make([][]float64, len(inputs))
	seen := make([]bool, len(inputs))
	for _, item := range resp.Data {
		if item.Index < 0 || item.Index >= len(vectors) {
			return nil, fmt.Errorf("embedding response has invalid index %d", item.Index)
		}
		if seen[item.Index] {
			return nil, fmt.Errorf("embedding response has duplicate index %d", item.Index)
		}
		if len(item.Embedding) == 0 {
			return nil, fmt.Errorf("embedding response has empty vector at index %d", item.Index)
		}
		seen[item.Index] = true
		vectors[item.Index] = item.Embedding
	}
	for i, ok := range seen {
		if !ok {
			return nil, fmt.Errorf("embedding response missing index %d", i)
		}
	}
	return vectors, nil
}

func readAll(r io.Reader) ([]byte, error) {
	const maxDocBytes = 512 << 20
	limited := io.LimitReader(r, maxDocBytes+1)
	out, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(out)) > maxDocBytes {
		return nil, fmt.Errorf("document exceeds %d bytes", maxDocBytes)
	}
	return out, nil
}

func int64FromAny(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
}
