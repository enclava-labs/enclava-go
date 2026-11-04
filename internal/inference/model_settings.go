package inference

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"

	"enclava-go/internal/auth"
	"enclava-go/internal/llm"
)

// ModelSetting is the tenant's policy for one chat model alias. A model
// without a stored setting is available, not the default and has no fallback.
type ModelSetting struct {
	Model         string
	Available     bool
	Default       bool
	FallbackModel string
}

// ModelSettingsRepository persists model settings; PutModelSetting upserts
// and, when the setting is the default, clears any other default atomically.
type ModelSettingsRepository interface {
	ListModelSettings(ctx context.Context, tenantID string) ([]ModelSetting, error)
	GetModelSetting(ctx context.Context, tenantID, model string) (ModelSetting, bool, error)
	PutModelSetting(ctx context.Context, tenantID string, setting ModelSetting) error
}

// ModelCatalogEntry is one chat model alias served by the tenant's endpoints
// with its settings, as the Models page and API show it.
type ModelCatalogEntry struct {
	Model         string   `json:"model"`
	Endpoints     []string `json:"endpoints"`
	Routable      bool     `json:"routable"`
	Available     bool     `json:"available"`
	Default       bool     `json:"default"`
	FallbackModel string   `json:"fallback_model"`
}

// ModelSettingPatch changes only the fields that are set.
type ModelSettingPatch struct {
	Available     *bool   `json:"available,omitempty"`
	Default       *bool   `json:"default,omitempty"`
	FallbackModel *string `json:"fallback_model,omitempty"`
}

var (
	// ErrModelUnavailable denies a model the tenant withheld from people;
	// it is an ErrDenied, so callers keep the indistinguishable denial.
	ErrModelUnavailable = fmt.Errorf("%w: model is not available", ErrDenied)
	// ErrUnknownModel reports a model no endpoint serves for chat.
	ErrUnknownModel = errors.New("inference: unknown chat model")
	// ErrInvalidModelSetting reports a rejected settings change.
	ErrInvalidModelSetting = errors.New("inference: invalid model setting")
)

// maxFallbackHops bounds a fallback chain (A → B → C) so a misconfigured
// cycle can never loop.
const maxFallbackHops = 2

// canManageModels reports principals that configure models; they keep access
// to models withheld from people so they can test them.
func canManageModels(p auth.Principal) bool {
	return auth.HasScope(p, "pricing.manage") || auth.HasScope(p, auth.ScopePlatformAdmin)
}

// chatFamily reports capabilities model settings govern. Embeddings are
// excluded: their vector space is tied to each knowledge collection.
func chatFamily(capability string) bool {
	switch capability {
	case CapabilityChat, CapabilityVision, CapabilityResponses, CapabilityCompact:
		return true
	}
	return false
}

func servesChat(e Endpoint) bool {
	return capabilityAdvertised(e, CapabilityChat) || capabilityAdvertised(e, CapabilityVision) || capabilityAdvertised(e, CapabilityResponses)
}

// ModelCatalog lists every chat model alias bound on the tenant's endpoints
// with its settings, sorted by alias.
func (s *Service) ModelCatalog(ctx context.Context, tenantID string) ([]ModelCatalogEntry, error) {
	endpoints, err := s.endpoints.List(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list endpoints: %w", err)
	}
	byModel := map[string]*ModelCatalogEntry{}
	for _, e := range endpoints {
		if !servesChat(e) {
			continue
		}
		for _, capability := range e.Capabilities {
			alias, ok := modelAliasOf(capability)
			if !ok {
				continue
			}
			entry := byModel[alias]
			if entry == nil {
				entry = &ModelCatalogEntry{Model: alias, Available: true, Endpoints: []string{}}
				byModel[alias] = entry
			}
			entry.Endpoints = append(entry.Endpoints, e.ID)
			entry.Routable = entry.Routable || e.Enabled
		}
	}
	if s.modelSettings != nil {
		settings, err := s.modelSettings.ListModelSettings(ctx, tenantID)
		if err != nil {
			return nil, fmt.Errorf("list model settings: %w", err)
		}
		for _, setting := range settings {
			if entry := byModel[setting.Model]; entry != nil {
				entry.Available, entry.Default, entry.FallbackModel = setting.Available, setting.Default, setting.FallbackModel
			}
		}
	}
	out := make([]ModelCatalogEntry, 0, len(byModel))
	for _, entry := range byModel {
		out = append(out, *entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out, nil
}

// UpdateModelSetting applies patch to one served chat model. The fallback
// must be another served chat model; "" clears it.
func (s *Service) UpdateModelSetting(ctx context.Context, tenantID, model string, patch ModelSettingPatch) (ModelCatalogEntry, error) {
	if s.modelSettings == nil {
		return ModelCatalogEntry{}, errors.New("inference: model settings are not configured")
	}
	catalog, err := s.ModelCatalog(ctx, tenantID)
	if err != nil {
		return ModelCatalogEntry{}, err
	}
	known := make(map[string]ModelCatalogEntry, len(catalog))
	for _, entry := range catalog {
		known[entry.Model] = entry
	}
	entry, ok := known[model]
	if !ok {
		return ModelCatalogEntry{}, ErrUnknownModel
	}
	if patch.Available != nil {
		entry.Available = *patch.Available
	}
	if patch.Default != nil {
		entry.Default = *patch.Default
	}
	if patch.FallbackModel != nil {
		fallback := strings.TrimSpace(*patch.FallbackModel)
		if fallback == model {
			return ModelCatalogEntry{}, fmt.Errorf("%w: a model cannot fall back to itself", ErrInvalidModelSetting)
		}
		if _, served := known[fallback]; fallback != "" && !served {
			return ModelCatalogEntry{}, fmt.Errorf("%w: fallback %q is not a chat model served by an endpoint", ErrInvalidModelSetting, fallback)
		}
		entry.FallbackModel = fallback
	}
	if err := s.modelSettings.PutModelSetting(ctx, tenantID, ModelSetting{
		Model: model, Available: entry.Available, Default: entry.Default, FallbackModel: entry.FallbackModel,
	}); err != nil {
		return ModelCatalogEntry{}, err
	}
	return entry, nil
}

// DefaultModel returns the tenant's default chat model alias, "" when unset.
func (s *Service) DefaultModel(ctx context.Context, tenantID string) (string, error) {
	if s.modelSettings == nil {
		return "", nil
	}
	settings, err := s.modelSettings.ListModelSettings(ctx, tenantID)
	if err != nil {
		return "", fmt.Errorf("list model settings: %w", err)
	}
	for _, setting := range settings {
		if setting.Default {
			return setting.Model, nil
		}
	}
	return "", nil
}

// UnavailableModels returns the chat models withheld from p; model managers
// get none so they can still test them.
func (s *Service) UnavailableModels(ctx context.Context, p auth.Principal) (map[string]bool, error) {
	out := map[string]bool{}
	if s.modelSettings == nil || canManageModels(p) {
		return out, nil
	}
	settings, err := s.modelSettings.ListModelSettings(ctx, p.TenantID)
	if err != nil {
		return nil, fmt.Errorf("list model settings: %w", err)
	}
	for _, setting := range settings {
		if !setting.Available {
			out[setting.Model] = true
		}
	}
	return out, nil
}

// checkModelAvailable denies a chat-family request for a model withheld from p.
func (s *Service) checkModelAvailable(ctx context.Context, p auth.Principal, model, capability string) error {
	if s.modelSettings == nil || model == "" || !chatFamily(capability) || canManageModels(p) {
		return nil
	}
	setting, ok, err := s.modelSettings.GetModelSetting(ctx, p.TenantID, model)
	if err != nil {
		return fmt.Errorf("model settings: %w", err)
	}
	if ok && !setting.Available {
		return ErrModelUnavailable
	}
	return nil
}

// fallbackEligible reports failures where the requested model provably did
// not run the request, so another model may answer instead: no route or
// availability for it, nothing sent to its endpoint, or an upstream refusal
// or server error. Failures after request bytes may have been processed are
// never retried elsewhere.
func fallbackEligible(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, ErrNoRoute) || errors.Is(err, ErrModelUnavailable) || llm.IsNotDispatched(err) {
		return true
	}
	var upstream *llm.UpstreamError
	if errors.As(err, &upstream) {
		switch upstream.StatusCode {
		case http.StatusNotFound, http.StatusRequestTimeout, http.StatusTooManyRequests:
			return true
		}
		return upstream.StatusCode >= http.StatusInternalServerError
	}
	return false
}

// Execute is the single entry point every model call must use: resolve policy,
// route, reserve, dispatch, settle-or-retain. The principal's authority is
// never widened by request fields; the returned body is caller-owned. A chat
// request whose model cannot answer follows the model's configured fallback
// (at most maxFallbackHops), each attempt under the full policy of its model.
// Requests pinned to an endpoint never fall back.
func (s *Service) Execute(ctx context.Context, p auth.Principal, req Request) (*Result, error) {
	result, err := s.executeOnce(ctx, p, req)
	if err == nil || s.modelSettings == nil || req.EndpointID != "" ||
		(req.Capability != CapabilityChat && req.Capability != CapabilityVision) {
		return result, err
	}
	requested, firstErr := req.Model, err
	tried := map[string]bool{req.Model: true}
	for hop := 0; hop < maxFallbackHops && fallbackEligible(err) && ctx.Err() == nil; hop++ {
		setting, ok, lookupErr := s.modelSettings.GetModelSetting(ctx, p.TenantID, req.Model)
		if lookupErr != nil || !ok || setting.FallbackModel == "" || tried[setting.FallbackModel] {
			break
		}
		next := setting.FallbackModel
		tried[next] = true
		log.Printf("inference fallback: tenant=%s model %q -> %q after: %v", p.TenantID, req.Model, next, err)
		req.Model = next
		var fallback *Result
		if fallback, err = s.executeOnce(ctx, p, req); err == nil {
			fallback.FallbackFrom = requested
			return fallback, nil
		}
	}
	return nil, firstErr
}
