package inference

import (
	"context"
	"sync"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/llm"
)

// modelLimits is the per-model catalog truth a policy decision needs:
// context window and the provider-declared generation ceiling. Zero means
// the catalog did not report it — callers skip the clamp entirely.
type modelLimits struct {
	contextLength int64
	maxOutput     int64
}

type modelContextKey struct {
	service                            *Service
	tenant, endpoint, model, signature string
}

type modelContextEntry struct {
	limits  modelLimits
	expires time.Time
	ready   chan struct{}
}

var modelContexts = struct {
	sync.Mutex
	entries map[modelContextKey]*modelContextEntry
}{entries: make(map[modelContextKey]*modelContextEntry)}

func (s *Service) ModelContextLength(ctx context.Context, p auth.Principal, model, endpointID string) (int64, error) {
	if p.TenantID == "" || p.ActorID == "" {
		return 0, ErrDenied
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	endpoint, err := s.route(lookupCtx, p, Request{Model: model, EndpointID: endpointID}, CapabilityChat)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if lookupCtx.Err() != nil {
			return 0, nil
		}
		return 0, err
	}
	upstreamModel, _ := upstreamModelFor(endpoint, model)
	limits, err := s.modelLimitsFor(ctx, lookupCtx, endpoint, upstreamModel)
	if err != nil {
		return 0, err
	}
	return limits.contextLength, nil
}

// modelLimitsFor resolves the routed model's catalog limits through the
// per-endpoint cache — one list per endpoint/model per TTL, never per
// request. lookupCtx bounds the fetch; ctx is the caller's.
func (s *Service) modelLimitsFor(ctx, lookupCtx context.Context, endpoint Endpoint, upstreamModel string) (modelLimits, error) {
	key := modelContextKey{service: s, tenant: endpoint.TenantID, endpoint: endpoint.ID, model: upstreamModel, signature: transportSignature(endpoint)}
	modelContexts.Lock()
	if entry := modelContexts.entries[key]; entry != nil {
		if entry.ready != nil {
			ready := entry.ready
			modelContexts.Unlock()
			select {
			case <-ready:
				return entry.limits, nil
			case <-lookupCtx.Done():
				return modelLimits{}, ctx.Err()
			}
		}
		if time.Now().Before(entry.expires) {
			limits := entry.limits
			modelContexts.Unlock()
			return limits, nil
		}
	}
	if len(modelContexts.entries) >= 1024 {
		for oldKey, old := range modelContexts.entries {
			if old.ready == nil {
				delete(modelContexts.entries, oldKey)
				break
			}
		}
		if len(modelContexts.entries) >= 1024 {
			modelContexts.Unlock()
			return modelLimits{}, nil
		}
	}
	entry := &modelContextEntry{ready: make(chan struct{})}
	modelContexts.entries[key] = entry
	modelContexts.Unlock()
	result := make(chan modelLimits, 1)
	go func() {
		var limits modelLimits
		up, err := s.transportFor(lookupCtx, endpoint)
		if err == nil {
			if lister, ok := up.(interface {
				ListModels(context.Context) (llm.ModelsResponse, error)
			}); ok {
				models, listErr := lister.ListModels(lookupCtx)
				if listErr == nil {
					for _, item := range models.Data {
						if item.ID == upstreamModel {
							limits = modelLimits{contextLength: item.ContextLength, maxOutput: item.MaxOutputTokens}
							break
						}
					}
				}
			}
		}
		result <- limits
	}()
	var limits modelLimits
	select {
	case limits = <-result:
	case <-lookupCtx.Done():
	}
	ttl := time.Minute
	if limits.contextLength > 0 || limits.maxOutput > 0 {
		ttl = 5 * time.Minute
	}
	modelContexts.Lock()
	entry.limits = limits
	entry.expires = time.Now().Add(ttl)
	close(entry.ready)
	entry.ready = nil
	if ctx.Err() != nil {
		delete(modelContexts.entries, key)
	}
	modelContexts.Unlock()
	if ctx.Err() != nil {
		return modelLimits{}, ctx.Err()
	}
	return limits, nil
}

// modelLimitsCached returns the routed model's catalog limits only when the
// per-endpoint cache already holds them. The request path must never
// construct a transport or call upstream to clamp, so a miss (absent,
// expired or still-fetching entry) means no clamp — the same outcome as an
// uncataloged model.
func (s *Service) modelLimitsCached(endpoint Endpoint, upstreamModel string) (modelLimits, bool) {
	key := modelContextKey{service: s, tenant: endpoint.TenantID, endpoint: endpoint.ID, model: upstreamModel, signature: transportSignature(endpoint)}
	modelContexts.Lock()
	defer modelContexts.Unlock()
	entry := modelContexts.entries[key]
	if entry == nil || entry.ready != nil || !time.Now().Before(entry.expires) {
		return modelLimits{}, false
	}
	return entry.limits, true
}
