package inference

import (
	"context"
	"sync"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/llm"
)

type modelContextKey struct {
	service                            *Service
	tenant, endpoint, model, signature string
}

type modelContextEntry struct {
	length  int64
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
	key := modelContextKey{service: s, tenant: p.TenantID, endpoint: endpoint.ID, model: upstreamModel, signature: transportSignature(endpoint)}
	modelContexts.Lock()
	if entry := modelContexts.entries[key]; entry != nil {
		if entry.ready != nil {
			ready := entry.ready
			modelContexts.Unlock()
			select {
			case <-ready:
				return entry.length, nil
			case <-lookupCtx.Done():
				return 0, ctx.Err()
			}
		}
		if time.Now().Before(entry.expires) {
			length := entry.length
			modelContexts.Unlock()
			return length, nil
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
			return 0, nil
		}
	}
	entry := &modelContextEntry{ready: make(chan struct{})}
	modelContexts.entries[key] = entry
	modelContexts.Unlock()
	result := make(chan int64, 1)
	go func() {
		var length int64
		up, err := s.transportFor(lookupCtx, endpoint)
		if err == nil {
			if lister, ok := up.(interface {
				ListModels(context.Context) (llm.ModelsResponse, error)
			}); ok {
				models, listErr := lister.ListModels(lookupCtx)
				if listErr == nil {
					for _, item := range models.Data {
						if item.ID == upstreamModel && item.ContextLength > 0 {
							length = item.ContextLength
							break
						}
					}
				}
			}
		}
		result <- length
	}()
	var length int64
	select {
	case length = <-result:
	case <-lookupCtx.Done():
	}
	ttl := time.Minute
	if length > 0 {
		ttl = 5 * time.Minute
	}
	modelContexts.Lock()
	entry.length = length
	entry.expires = time.Now().Add(ttl)
	close(entry.ready)
	entry.ready = nil
	if ctx.Err() != nil {
		delete(modelContexts.entries, key)
	}
	modelContexts.Unlock()
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	return length, nil
}
