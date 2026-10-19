package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/inference"
	"enclava-go/internal/source"
)

// maxEmbeddingInputs caps batch size by item count; maxEmbeddingBatchUnits
// caps the summed estimate so one batch always sits inside the routed
// embeddings operation bound — a route-level limit is enforced by the
// executor, never assumed from a fixed item count.
const (
	maxEmbeddingInputs     = 2048
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
func createEmbeddings(ctx context.Context, executor Executor, p auth.Principal, model string, inputs []string, scope embedScope) ([][]float64, error) {
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
	return vectors, nil
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
