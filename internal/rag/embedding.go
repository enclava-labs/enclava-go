package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"enclava-go/internal/llm"
)

const maxEmbeddingInputs = 2048

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

// createEmbeddings batches embedding calls over maxEmbeddingInputs. Budget
// admission and usage settlement live in the caller (billing.Reserve →
// provider → billing.Settle) — never in a spend guard with an APIKeyID-zero
// bypass.
func createEmbeddings(ctx context.Context, provider llm.Provider, model string, inputs []string) ([][]float64, embeddingResponse, error) {
	vectors := make([][]float64, len(inputs))
	var summed embeddingResponse
	var dim int
	for offset := 0; offset < len(inputs); offset += maxEmbeddingInputs {
		end := offset + maxEmbeddingInputs
		if end > len(inputs) {
			end = len(inputs)
		}
		batch := inputs[offset:end]
		batchVectors, resp, err := createEmbeddingBatch(ctx, provider, model, batch)
		if err != nil {
			return nil, embeddingResponse{}, err
		}
		for i, vec := range batchVectors {
			if dim == 0 {
				dim = len(vec)
			} else if len(vec) != dim {
				return nil, embeddingResponse{}, fmt.Errorf("embedding dimension mismatch: got %d, want %d", len(vec), dim)
			}
			vectors[offset+i] = vec
		}
		summed.Usage.PromptTokens += resp.Usage.PromptTokens
		summed.Usage.TotalTokens += resp.Usage.TotalTokens
	}
	return vectors, summed, nil
}

func createEmbeddingBatch(ctx context.Context, provider llm.Provider, model string, inputs []string) ([][]float64, embeddingResponse, error) {
	payload := map[string]any{
		"model": model,
		"input": inputs,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, embeddingResponse{}, fmt.Errorf("marshal embeddings request: %w", err)
	}
	respBody, err := provider.CreateEmbeddings(ctx, body)
	if err != nil {
		return nil, embeddingResponse{}, fmt.Errorf("provider embeddings: %w", err)
	}
	var resp embeddingResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, embeddingResponse{}, fmt.Errorf("decode embeddings response: %w", err)
	}
	if len(resp.Data) != len(inputs) {
		return nil, embeddingResponse{}, fmt.Errorf("embedding count mismatch: got %d, want %d", len(resp.Data), len(inputs))
	}
	vectors := make([][]float64, len(inputs))
	seen := make([]bool, len(inputs))
	for _, item := range resp.Data {
		if item.Index < 0 || item.Index >= len(vectors) {
			return nil, embeddingResponse{}, fmt.Errorf("embedding response has invalid index %d", item.Index)
		}
		if seen[item.Index] {
			return nil, embeddingResponse{}, fmt.Errorf("embedding response has duplicate index %d", item.Index)
		}
		if len(item.Embedding) == 0 {
			return nil, embeddingResponse{}, fmt.Errorf("embedding response has empty vector at index %d", item.Index)
		}
		seen[item.Index] = true
		vectors[item.Index] = item.Embedding
	}
	for i, ok := range seen {
		if !ok {
			return nil, embeddingResponse{}, fmt.Errorf("embedding response missing index %d", i)
		}
	}
	return vectors, resp, nil
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
