package llm

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type cacheTestProvider struct {
	listCalls atomic.Int32
	models    ModelsResponse
}

func (p *cacheTestProvider) ListModels(ctx context.Context) (ModelsResponse, error) {
	p.listCalls.Add(1)
	return p.models, nil
}

func (p *cacheTestProvider) GetModel(ctx context.Context, modelID string) (Model, error) {
	return Model{ID: modelID}, nil
}

func (p *cacheTestProvider) CreateChatCompletion(ctx context.Context, body []byte) ([]byte, error) {
	return nil, nil
}

func (p *cacheTestProvider) CreateChatCompletionStream(ctx context.Context, body []byte) (io.ReadCloser, error) {
	return nil, nil
}

func (p *cacheTestProvider) CreateEmbeddings(ctx context.Context, body []byte) ([]byte, error) {
	return nil, nil
}

func TestModelCacheProviderCachesAndInvalidatesModels(t *testing.T) {
	upstream := &cacheTestProvider{
		models: ModelsResponse{Data: []Model{{ID: "chat-model", Tasks: []string{"chat"}}}},
	}
	provider := NewModelCacheProvider(upstream, time.Minute, "test-provider")

	first, err := provider.ListModels(context.Background())
	if err != nil {
		t.Fatalf("list models first: %v", err)
	}
	second, err := provider.ListModels(context.Background())
	if err != nil {
		t.Fatalf("list models second: %v", err)
	}
	if upstream.listCalls.Load() != 1 {
		t.Fatalf("expected one upstream call before invalidation, got %d", upstream.listCalls.Load())
	}
	if first.Data[0].Provider != "test-provider" || !second.Data[0].SupportsStreaming {
		t.Fatalf("expected normalized cached model metadata: %#v %#v", first.Data[0], second.Data[0])
	}
	model, err := provider.GetModel(context.Background(), "chat-model")
	if err != nil {
		t.Fatalf("get cached model: %v", err)
	}
	if model.ID != "chat-model" {
		t.Fatalf("unexpected cached model: %#v", model)
	}

	provider.InvalidateModelCache()
	if _, err := provider.ListModels(context.Background()); err != nil {
		t.Fatalf("list models after invalidation: %v", err)
	}
	if upstream.listCalls.Load() != 2 {
		t.Fatalf("expected upstream call after invalidation, got %d", upstream.listCalls.Load())
	}
}

func TestModelCacheProviderReturnsIndependentCopies(t *testing.T) {
	upstream := &cacheTestProvider{
		models: ModelsResponse{Data: []Model{{
			ID:              "chat-model",
			InputModalities: []string{"text"},
			Metadata:        map[string]any{"k": "v"},
		}}},
	}
	provider := NewModelCacheProvider(upstream, time.Minute, "test-provider")

	first, err := provider.ListModels(context.Background())
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	// Mutate the returned data in place — the cache must not be corrupted.
	first.Data[0].ID = "tampered"
	first.Data[0].InputModalities[0] = "injected"
	first.Data[0].Metadata["k"] = "tampered"
	if len(first.Data) > 0 {
		first.Data = append(first.Data, Model{ID: "phantom"})
	}

	second, err := provider.ListModels(context.Background())
	if err != nil {
		t.Fatalf("list models again: %v", err)
	}
	if upstream.listCalls.Load() != 1 {
		t.Fatalf("expected one upstream call, got %d", upstream.listCalls.Load())
	}
	if len(second.Data) != 1 || second.Data[0].ID != "chat-model" {
		t.Fatalf("caller mutation corrupted cache: %#v", second.Data)
	}
	if second.Data[0].InputModalities[0] != "text" || second.Data[0].Metadata["k"] != "v" {
		t.Fatalf("caller mutation leaked into cached model: %#v", second.Data[0])
	}

	got, err := provider.GetModel(context.Background(), "chat-model")
	if err != nil {
		t.Fatalf("get model: %v", err)
	}
	got.InputModalities[0] = "mutated"
	again, err := provider.GetModel(context.Background(), "chat-model")
	if err != nil {
		t.Fatalf("get model again: %v", err)
	}
	if again.InputModalities[0] != "text" {
		t.Fatalf("GetModel caller mutation leaked into cache: %#v", again)
	}
}

func TestModelCacheProviderConcurrentRefreshAndInstances(t *testing.T) {
	upstream := &cacheTestProvider{
		models: ModelsResponse{Data: []Model{{ID: "m1"}}},
	}
	a := NewModelCacheProvider(upstream, time.Minute, "endpoint-a")
	b := NewModelCacheProvider(&cacheTestProvider{
		models: ModelsResponse{Data: []Model{{ID: "m2"}}},
	}, time.Minute, "endpoint-b")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = a.ListModels(context.Background())
			_, _ = a.GetModel(context.Background(), "m1")
		}()
	}
	wg.Wait()

	a.InvalidateModelCache()
	modelsB, err := b.ListModels(context.Background())
	if err != nil {
		t.Fatalf("endpoint-b list: %v", err)
	}
	if len(modelsB.Data) != 1 || modelsB.Data[0].ID != "m2" {
		t.Fatalf("invalidating endpoint-a leaked into endpoint-b: %#v", modelsB.Data)
	}
	b.mu.RLock()
	_, leaked := b.modelMap["m1"]
	b.mu.RUnlock()
	if leaked {
		t.Fatal("endpoint-b inherited endpoint-a's model observations")
	}
}
