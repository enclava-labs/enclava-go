package inference_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"

	"enclava-go/internal/inference"
	"enclava-go/internal/llm"
	"enclava-go/internal/store/postgres"
)

// modelStatusUpstream answers like fakeUpstream except for upstream model ids
// listed in status, which fail with that HTTP status (-1: never sent).
type modelStatusUpstream struct {
	*fakeUpstream
	mu     sync.Mutex
	status map[string]int
	models []string
}

func (u *modelStatusUpstream) CreateChatCompletion(ctx context.Context, body []byte) ([]byte, error) {
	var sent struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &sent)
	u.mu.Lock()
	u.models = append(u.models, sent.Model)
	code := u.status[sent.Model]
	u.mu.Unlock()
	if code < 0 {
		return nil, &llm.NotDispatched{Err: errors.New("dial: connection refused")}
	}
	if code != 0 {
		return nil, &llm.UpstreamError{StatusCode: code, Body: []byte(`{"error":{"message":"refused"}}`)}
	}
	return u.fakeUpstream.CreateChatCompletion(ctx, body)
}

func (u *modelStatusUpstream) dispatched() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.models...)
}

// newFallbackFx serves "primary" and "backup" from one priced endpoint with
// model settings enabled.
func newFallbackFx(t *testing.T) (*fx, *modelStatusUpstream) {
	t.Helper()
	f := newFx(t)
	up := &modelStatusUpstream{fakeUpstream: f.upstream, status: map[string]int{}}
	f.upstreamOverride = up
	f.modelSettings = postgres.NewModelSettingsRepo(f.db)
	f.build(t)
	f.mkEndpoint(t, "model:primary=primary-up", "model:backup=backup-up", "model:spare=spare-up")
	f.flatPrices(t)
	return f, up
}

func (f *fx) setModel(t *testing.T, model string, patch inference.ModelSettingPatch) {
	t.Helper()
	if _, err := f.svc.UpdateModelSetting(f.ctx, f.tenant, model, patch); err != nil {
		t.Fatalf("update %s: %v", model, err)
	}
}

func (f *fx) chat(model string) (*inference.Result, error) {
	req := f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)
	req.Model = model
	res, err := f.svc.Execute(f.ctx, f.principal, req)
	if err == nil {
		_ = res.Body.Close()
	}
	return res, err
}

func TestModelFallbackFollowsRefusalsButNeverAmbiguousFailures(t *testing.T) {
	f, up := newFallbackFx(t)
	backup := "backup"
	f.setModel(t, "primary", inference.ModelSettingPatch{FallbackModel: &backup})

	up.status["primary-up"] = http.StatusServiceUnavailable
	res, err := f.chat("primary")
	if err != nil || res.FallbackFrom != "primary" || res.Model != "backup-up" {
		t.Fatalf("503 did not fall back: res=%+v err=%v", res, err)
	}

	// An endpoint that was never reached (nothing sent) also falls back.
	up.status["primary-up"] = -1
	if res, err := f.chat("primary"); err != nil || res.FallbackFrom != "primary" {
		t.Fatalf("unreachable endpoint did not fall back: res=%+v err=%v", res, err)
	}

	// A client error is the request's fault: the fallback would fail alike.
	up.status["primary-up"] = http.StatusBadRequest
	before := len(up.dispatched())
	if _, err := f.chat("primary"); err == nil || len(up.dispatched()) != before+1 {
		t.Fatalf("400 must not fall back: err=%v dispatched=%v", err, up.dispatched()[before:])
	}

	// Without a fallback setting the requested model's failure is returned.
	up.status["primary-up"] = 0
	up.status["spare-up"] = http.StatusServiceUnavailable
	var upstream *llm.UpstreamError
	if _, err := f.chat("spare"); !errors.As(err, &upstream) || upstream.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("spare without fallback: %v", err)
	}
}

func TestModelFallbackChainIsBoundedAndPinsNeverFallBack(t *testing.T) {
	f, up := newFallbackFx(t)
	backup, primary := "backup", "primary"
	f.setModel(t, "primary", inference.ModelSettingPatch{FallbackModel: &backup})
	f.setModel(t, "backup", inference.ModelSettingPatch{FallbackModel: &primary})
	up.status["primary-up"] = http.StatusBadGateway
	up.status["backup-up"] = http.StatusBadGateway

	var upstream *llm.UpstreamError
	if _, err := f.chat("primary"); !errors.As(err, &upstream) || len(up.dispatched()) != 2 {
		t.Fatalf("cycle must stop after one hop: err=%v dispatched=%v", err, up.dispatched())
	}

	req := f.chatRequest(`{"messages":[{"role":"user","content":"hi"}]}`)
	req.Model, req.EndpointID = "primary", "ep-1"
	if _, err := f.svc.Execute(f.ctx, f.principal, req); err == nil || len(up.dispatched()) != 3 {
		t.Fatalf("pinned request fell back: err=%v dispatched=%v", err, up.dispatched())
	}
}

func TestUnavailableModelsAreWithheldFromPeopleNotManagers(t *testing.T) {
	f, up := newFallbackFx(t)
	off, backup := false, "backup"
	f.setModel(t, "primary", inference.ModelSettingPatch{Available: &off})

	if _, err := f.chat("primary"); !errors.Is(err, inference.ErrModelUnavailable) || !errors.Is(err, inference.ErrDenied) {
		t.Fatalf("people reached an unavailable model: %v", err)
	}
	models, err := f.svc.PricedModelSelector(f.ctx, f.principal, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range models {
		if m.ID == "primary" {
			t.Fatalf("unavailable model offered to people: %+v", models)
		}
	}

	// Its fallback answers people instead.
	f.setModel(t, "primary", inference.ModelSettingPatch{FallbackModel: &backup})
	if res, err := f.chat("primary"); err != nil || res.FallbackFrom != "primary" || up.dispatched()[len(up.dispatched())-1] != "backup-up" {
		t.Fatalf("unavailable model did not fall back: res=%+v err=%v", res, err)
	}

	// Model managers keep testing it directly.
	f.principal.Scopes = map[string]struct{}{"pricing.manage": {}}
	if res, err := f.chat("primary"); err != nil || res.FallbackFrom != "" || res.Model != "primary-up" {
		t.Fatalf("manager could not use the unavailable model: res=%+v err=%v", res, err)
	}
}

func TestModelSettingsValidationAndSingleDefault(t *testing.T) {
	f, _ := newFallbackFx(t)
	self, unknown, on := "primary", "nope", true
	if _, err := f.svc.UpdateModelSetting(f.ctx, f.tenant, "primary", inference.ModelSettingPatch{FallbackModel: &self}); !errors.Is(err, inference.ErrInvalidModelSetting) {
		t.Fatalf("self fallback: %v", err)
	}
	if _, err := f.svc.UpdateModelSetting(f.ctx, f.tenant, "primary", inference.ModelSettingPatch{FallbackModel: &unknown}); !errors.Is(err, inference.ErrInvalidModelSetting) {
		t.Fatalf("unknown fallback: %v", err)
	}
	if _, err := f.svc.UpdateModelSetting(f.ctx, f.tenant, "ghost", inference.ModelSettingPatch{Default: &on}); !errors.Is(err, inference.ErrUnknownModel) {
		t.Fatalf("unknown model: %v", err)
	}
	f.setModel(t, "primary", inference.ModelSettingPatch{Default: &on})
	f.setModel(t, "backup", inference.ModelSettingPatch{Default: &on})
	if def, err := f.svc.DefaultModel(f.ctx, f.tenant); err != nil || def != "backup" {
		t.Fatalf("default = %q, %v; want backup", def, err)
	}
	catalog, err := f.svc.ModelCatalog(f.ctx, f.tenant)
	if err != nil {
		t.Fatal(err)
	}
	defaults := 0
	for _, entry := range catalog {
		if entry.Default {
			defaults++
		}
		if !entry.Available || !entry.Routable || len(entry.Endpoints) != 1 {
			t.Fatalf("catalog entry = %+v", entry)
		}
	}
	if len(catalog) != 4 || defaults != 1 { // gpt-fast comes from the fixture endpoint
		t.Fatalf("catalog = %+v", catalog)
	}
}
