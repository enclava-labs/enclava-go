package llm

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"
)

type ModelCacheProvider struct {
	upstream Provider
	ttl      time.Duration
	provider string

	mu       sync.RWMutex
	models   ModelsResponse
	modelMap map[string]Model
	cachedAt time.Time
}

func NewModelCacheProvider(upstream Provider, ttl time.Duration, provider string) *ModelCacheProvider {
	return &ModelCacheProvider{
		upstream: upstream,
		ttl:      ttl,
		provider: provider,
		modelMap: map[string]Model{},
	}
}

func (p *ModelCacheProvider) ListModels(ctx context.Context) (ModelsResponse, error) {
	if p.ttl > 0 {
		p.mu.RLock()
		if !p.cachedAt.IsZero() && time.Since(p.cachedAt) < p.ttl {
			cached := cloneModelsResponse(p.models)
			p.mu.RUnlock()
			return cached, nil
		}
		p.mu.RUnlock()
	}
	return p.refreshModels(ctx)
}

func (p *ModelCacheProvider) GetModel(ctx context.Context, modelID string) (Model, error) {
	if p.ttl > 0 {
		p.mu.RLock()
		if !p.cachedAt.IsZero() && time.Since(p.cachedAt) < p.ttl {
			if model, ok := p.modelMap[modelID]; ok {
				p.mu.RUnlock()
				return cloneModel(model), nil
			}
		}
		p.mu.RUnlock()
	}
	model, err := p.upstream.GetModel(ctx, modelID)
	if err != nil {
		return Model{}, err
	}
	model = NormalizeModel(model, p.provider)
	p.mu.Lock()
	p.modelMap[model.ID] = model
	p.mu.Unlock()
	return cloneModel(model), nil
}

// cloneModel deep-copies a cached Model so callers cannot mutate shared
// per-endpoint observations in place.
func cloneModel(m Model) Model {
	out := m
	out.InputModalities = append([]string(nil), m.InputModalities...)
	out.OutputModalities = append([]string(nil), m.OutputModalities...)
	out.Tasks = append([]string(nil), m.Tasks...)
	if m.Metadata != nil {
		md := make(map[string]any, len(m.Metadata))
		for k, v := range m.Metadata {
			md[k] = v
		}
		out.Metadata = md
	}
	return out
}

func cloneModelsResponse(r ModelsResponse) ModelsResponse {
	out := r
	out.Data = make([]Model, len(r.Data))
	for i, m := range r.Data {
		out.Data[i] = cloneModel(m)
	}
	return out
}

func (p *ModelCacheProvider) CreateChatCompletion(ctx context.Context, body []byte) ([]byte, error) {
	return p.upstream.CreateChatCompletion(ctx, body)
}

func (p *ModelCacheProvider) CreateChatCompletionStream(ctx context.Context, body []byte) (io.ReadCloser, error) {
	return p.upstream.CreateChatCompletionStream(ctx, body)
}

func (p *ModelCacheProvider) CreateEmbeddings(ctx context.Context, body []byte) ([]byte, error) {
	return p.upstream.CreateEmbeddings(ctx, body)
}

func (p *ModelCacheProvider) DoResponsesRequest(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	responses, ok := p.upstream.(ResponsesProvider)
	if !ok {
		return nil, &UpstreamError{StatusCode: 501, Message: "upstream provider does not support responses"}
	}
	return responses.DoResponsesRequest(ctx, method, path, body)
}

func (p *ModelCacheProvider) DoResponsesStream(ctx context.Context, method, path string, body []byte) (io.ReadCloser, error) {
	responses, ok := p.upstream.(ResponsesProvider)
	if !ok {
		return nil, &UpstreamError{StatusCode: 501, Message: "upstream provider does not support responses"}
	}
	return responses.DoResponsesStream(ctx, method, path, body)
}

func (p *ModelCacheProvider) DoPassthroughRequest(ctx context.Context, method, path string, body []byte, headers http.Header) (PassthroughResponse, error) {
	passthrough, ok := p.upstream.(PassthroughProvider)
	if !ok {
		return PassthroughResponse{}, &UpstreamError{StatusCode: 501, Message: "upstream provider does not support passthrough requests"}
	}
	return passthrough.DoPassthroughRequest(ctx, method, path, body, headers)
}

func (p *ModelCacheProvider) Health(ctx context.Context) error {
	if health, ok := p.upstream.(HealthProvider); ok {
		if err := health.Health(ctx); err != nil {
			return err
		}
		return nil
	}
	_, err := p.upstream.ListModels(ctx)
	return err
}

func (p *ModelCacheProvider) InvalidateModelCache() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.models = ModelsResponse{}
	p.modelMap = map[string]Model{}
	p.cachedAt = time.Time{}
}

func (p *ModelCacheProvider) VerifyAttestation(ctx context.Context) (AttestationStatus, error) {
	attested, ok := p.upstream.(AttestationProvider)
	if !ok {
		return AttestationStatus{Enabled: false, Provider: p.provider}, nil
	}
	return attested.VerifyAttestation(ctx)
}

func (p *ModelCacheProvider) AttestationStatus() AttestationStatus {
	attested, ok := p.upstream.(AttestationProvider)
	if !ok {
		return AttestationStatus{Enabled: false, Provider: p.provider}
	}
	return attested.AttestationStatus()
}

func (p *ModelCacheProvider) refreshModels(ctx context.Context) (ModelsResponse, error) {
	models, err := p.upstream.ListModels(ctx)
	if err != nil {
		return ModelsResponse{}, err
	}
	models = NormalizeModels(models, p.provider)
	modelMap := make(map[string]Model, len(models.Data))
	for _, model := range models.Data {
		modelMap[model.ID] = model
	}
	p.mu.Lock()
	p.models = models
	p.modelMap = modelMap
	p.cachedAt = time.Now()
	p.mu.Unlock()
	return cloneModelsResponse(models), nil
}
