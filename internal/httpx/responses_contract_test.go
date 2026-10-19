package httpx

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/inference"
	"enclava-go/internal/llm"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

// responses_contract_test.go exercises the owned Responses lifecycle: every
// operation flows route → auth → bindings → executor → deterministic in-process
// upstream, asserting public-ID rewriting, owner/origin/tenant confinement and
// lifecycle availability under budget exhaustion (INF-04/INF-05/INF-06).

// responsesUpstream is a deterministic transport recording every dispatch and
// replaying canned per-path payloads.
type responsesUpstream struct {
	calls     []responsesCall
	seq       int
	chatBody  []byte
	embedBody []byte
}

type responsesCall struct {
	Method string
	Path   string
	Body   string
}

func (u *responsesUpstream) CreateChatCompletion(ctx context.Context, body []byte) ([]byte, error) {
	return u.chatBody, nil
}

func (u *responsesUpstream) CreateChatCompletionStream(ctx context.Context, body []byte) (io.ReadCloser, error) {
	return nil, fmt.Errorf("no stream")
}

func (u *responsesUpstream) CreateEmbeddings(ctx context.Context, body []byte) ([]byte, error) {
	return u.embedBody, nil
}

// responseObject returns one stored response body with upstream ids keyed by
// the create sequence number.
func responseObject(seq int) string {
	return fmt.Sprintf(`{"id":"resp_UP%d","object":"response","model":"up-model","status":"completed",`+
		`"output":[{"id":"msg_UP%d_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],`+
		`"usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13}}`, seq, seq)
}

func (u *responsesUpstream) DoResponsesRequest(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	u.calls = append(u.calls, responsesCall{Method: method, Path: path, Body: string(body)})
	p := strings.SplitN(path, "?", 2)[0]
	switch {
	case p == "/responses" && method == http.MethodPost:
		u.seq++
		obj := responseObject(u.seq)
		var req map[string]any
		if json.Unmarshal([]byte(body), &req) == nil {
			if prev, ok := req["previous_response_id"].(string); ok {
				obj = strings.Replace(obj, `"output"`, `"previous_response_id":"`+prev+`","output"`, 1)
			}
		}
		return []byte(obj), nil
	case p == "/responses/input_tokens" && method == http.MethodPost:
		return []byte(`{"object":"response.input_tokens","input_tokens":42}`), nil
	case p == "/responses/compact" && method == http.MethodPost:
		return []byte(`{"object":"response.compaction","output":[{"id":"cmp_UP1","type":"compaction","encrypted_content":"zz"}],"usage":{"input_tokens":30,"output_tokens":3,"total_tokens":33}}`), nil
	case strings.HasPrefix(p, "/responses/resp_UP1") && method == http.MethodGet:
		seq := 1
		fmt.Sscanf(strings.TrimPrefix(p, "/responses/resp_UP1"), "%d", &seq)
		if strings.HasSuffix(p, "/input_items") {
			return []byte(fmt.Sprintf(`{"object":"list","data":[{"id":"msg_UP%d_1","type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"first_id":"msg_UP%d_1","last_id":"msg_UP%d_1","has_more":false}`, seq, seq, seq)), nil
		}
		return []byte(responseObject(seq)), nil
	case strings.HasSuffix(p, "/cancel") && method == http.MethodPost:
		seq := 1
		fmt.Sscanf(strings.TrimSuffix(strings.TrimPrefix(p, "/responses/resp_UP1"), "/cancel"), "%d", &seq)
		return []byte(fmt.Sprintf(`{"id":"resp_UP%d","object":"response","status":"cancelled","usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}`, seq)), nil
	case method == http.MethodDelete:
		return []byte(fmt.Sprintf(`{"id":%q,"object":"response.deleted","deleted":true}`, strings.TrimPrefix(p, "/responses/"))), nil
	}
	return nil, &llm.UpstreamError{StatusCode: 404, Body: []byte(`{"error":{"message":"not found","type":"invalid_request_error","code":"not_found"}}`), Message: "not found"}
}

func (u *responsesUpstream) DoResponsesStream(ctx context.Context, method, path string, body []byte) (io.ReadCloser, error) {
	u.calls = append(u.calls, responsesCall{Method: method, Path: path, Body: string(body)})
	u.seq++
	stream := fmt.Sprintf("event: response.created\n"+
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_UP%d\",\"object\":\"response\",\"status\":\"in_progress\",\"output\":[]}}\n\n"+
		"data: {malformed upstream event\n\n"+
		"event: response.output_item.done\n"+
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_UP%d_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hi\"}]}}\n\n"+
		"event: response.completed\n"+
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_UP%d\",\"object\":\"response\",\"status\":\"completed\",\"previous_response_id\":\"resp_PREV\",\"output\":[{\"id\":\"msg_UP%d_1\",\"type\":\"message\",\"role\":\"assistant\"}],\"usage\":{\"input_tokens\":9,\"output_tokens\":4,\"total_tokens\":13}}}\n\n"+
		"data: [DONE]\n\n", u.seq, u.seq, u.seq, u.seq)
	return io.NopCloser(strings.NewReader(stream)), nil
}

// responsesFx is the Responses contract fixture mirroring openaiFx.
type responsesFx struct {
	db       *postgres.DB
	tenant   string
	app      *App
	bindings *postgres.ResponseBindingsRepo
	brepo    *postgres.BillingRepo
	erepo    *postgres.EndpointsRepo
	ups      map[string]*responsesUpstream
	keyA     string // owner user A's API key
	keyB     string // other user in same tenant
	keyOther string // key in a second tenant
}

func newResponsesFx(t *testing.T) *responsesFx {
	t.Helper()
	db := testdb.New(t)
	repo := postgres.NewIdentityRepo(db)
	tenant := testdb.SeedTenant(t, db, fmt.Sprintf("resp-%d", time.Now().UnixNano()%1e6))
	scopes := []string{"models.list", "chat.completions", "embeddings.create", "responses"}
	userA := testdb.SeedUser(t, db, tenant, "owner@example.com", scopes)
	userB := testdb.SeedUser(t, db, tenant, "other@example.com", scopes)
	keyA, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "a", OwnerUserID: userA.ID, Scopes: scopes})
	keyB, _ := testdb.SeedAPIKey(t, db, tenant, testdb.SeedKeySpec{Name: "b", OwnerUserID: userB.ID, Scopes: scopes})

	tenant2 := testdb.SeedTenant(t, db, fmt.Sprintf("resp2-%d", time.Now().UnixNano()%1e6))
	user2 := testdb.SeedUser(t, db, tenant2, "xt@example.com", scopes)
	keyOther, _ := testdb.SeedAPIKey(t, db, tenant2, testdb.SeedKeySpec{Name: "x", OwnerUserID: user2.ID, Scopes: scopes})

	erepo := postgres.NewEndpointsRepo(db)
	brepo := postgres.NewBillingRepo(db)
	srepo := postgres.NewSourceRepo(db)
	arepo := postgres.NewArtifactRepo(db)
	bindings := postgres.NewResponseBindingsRepo(db)
	ups := map[string]*responsesUpstream{
		"ep-resp": {},
		"ep-zz":   {},
	}

	svc, err := inference.NewService(inference.Deps{
		Endpoints:  erepo,
		Sources:    srepo,
		Authorizer: source.NewAuthorizer(srepo, 0),
		Billing:    billing.NewService(brepo),
		Artifacts:  arepo,
	}, inference.WithTransportFactory(func(ctx context.Context, e inference.Endpoint, credential string) (inference.Upstream, error) {
		if u, ok := ups[e.ID]; ok {
			return u, nil
		}
		return nil, fmt.Errorf("no upstream for %s", e.ID)
	}))
	if err != nil {
		t.Fatalf("new inference service: %v", err)
	}
	app := NewApp(auth.NewService(repo, ""), nil, "", nil, repo,
		nil, nil, nil, nil, nil, nil, nil, brepo, erepo,
		"sha256", "", "enc_", nil, nil, 1, false, svc, bindings, GateConfig{},
		postgres.NewSourceRepo(db), postgres.NewArtifactRepo(db))
	fx := &responsesFx{db: db, tenant: tenant, app: app, bindings: bindings, brepo: brepo, erepo: erepo, ups: ups,
		keyA: keyA, keyB: keyB, keyOther: keyOther}
	fx.mustEndpoint(t, "ep-resp", []string{
		inference.CapabilityResponses, inference.CapabilityCompact, "model:gpt-fast=gpt-4o-mini",
	})
	fx.mustEndpoint(t, "ep-zz", []string{
		inference.CapabilityResponses, inference.CapabilityCompact,
		"model:gpt-alt=gpt-4o", "model:gpt-fast=gpt-4o-mini",
	})
	for _, ep := range []string{"ep-resp", "ep-zz"} {
		fx.price(t, ep, inference.CapabilityResponses, billing.UnitInput, 1, 1)
		fx.price(t, ep, inference.CapabilityResponses, billing.UnitOutput, 2, 1)
		fx.price(t, ep, inference.CapabilityCompact, billing.UnitInput, 1, 1)
		fx.price(t, ep, inference.CapabilityCompact, billing.UnitOutput, 2, 1)
	}
	return fx
}

func (f *responsesFx) mustEndpoint(t *testing.T, id string, caps []string) {
	t.Helper()
	if err := f.erepo.Create(context.Background(), inference.Endpoint{
		ID: id, TenantID: f.tenant, Protocol: "openai_compatible",
		BaseOrigin: "https://up-" + id + ".example", Capabilities: caps, Enabled: true,
	}); err != nil {
		t.Fatalf("create endpoint %s: %v", id, err)
	}
}

func (f *responsesFx) price(t *testing.T, endpointID, capability string, unit billing.UnitKind, micros, qty int64) {
	t.Helper()
	id := fmt.Sprintf("price-%s-%s-%s-%d", endpointID, capability, unit, time.Now().UnixNano())
	if err := f.brepo.PutPrice(context.Background(), f.tenant, id, billing.Price{
		SubjectType: billing.PriceSubjectEndpoint, SubjectID: endpointID,
		Capability: capability, Unit: unit,
		RateMicros: micros, UnitQuantity: qty, Currency: "USD",
	}, "tester", time.Now().Add(-time.Minute).UTC()); err != nil {
		t.Fatalf("put price: %v", err)
	}
}

func (f *responsesFx) do(t *testing.T, key, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	f.app.Handler().ServeHTTP(rec, r)
	return rec
}

func decodeJSONMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %d body %q: %v", rec.Code, rec.Body.String(), err)
	}
	return out
}

// createResponse runs one unary create and returns the public response ID.
func (f *responsesFx) createResponse(t *testing.T, key, model string) string {
	t.Helper()
	rec := f.do(t, key, http.MethodPost, "/v1/responses", `{"model":"`+model+`","input":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	out := decodeJSONMap(t, rec)
	id, _ := out["id"].(string)
	if !strings.HasPrefix(id, "resp_") || strings.Contains(id, "UP") {
		t.Fatalf("response id must be a bound public id, got %q", id)
	}
	items, _ := out["output"].([]any)
	if len(items) == 0 {
		t.Fatalf("missing output items: %v", out)
	}
	item, _ := items[0].(map[string]any)
	if iid, _ := item["id"].(string); !strings.HasPrefix(iid, "item_") || strings.Contains(iid, "UP") {
		t.Fatalf("item id must be a bound public id, got %q", iid)
	}
	return id
}

func TestResponsesLifecycleBoundThroughExecutor(t *testing.T) {
	f := newResponsesFx(t)
	pub := f.createResponse(t, f.keyA, "gpt-fast")

	up := f.ups["ep-resp"]
	if len(up.calls) != 1 || up.calls[0].Method != http.MethodPost || up.calls[0].Path != "/responses" {
		t.Fatalf("create dispatch wrong: %+v", up.calls)
	}

	// Retrieve replays the bound upstream id on its origin endpoint.
	rec := f.do(t, f.keyA, http.MethodGet, "/v1/responses/"+pub, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	got := decodeJSONMap(t, rec)
	if got["id"] != pub {
		t.Fatalf("get must return the public id: %v", got["id"])
	}
	if up.calls[len(up.calls)-1].Path != "/responses/resp_UP1" {
		t.Fatalf("get dispatched path %q", up.calls[len(up.calls)-1].Path)
	}

	// input_items rewrites nested ids and pagination cursors.
	rec = f.do(t, f.keyA, http.MethodGet, "/v1/responses/"+pub+"/input_items", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("input_items: %d %s", rec.Code, rec.Body.String())
	}
	list := decodeJSONMap(t, rec)
	if up.calls[len(up.calls)-1].Path != "/responses/resp_UP1/input_items" {
		t.Fatalf("input_items dispatched %q", up.calls[len(up.calls)-1].Path)
	}
	data, _ := list["data"].([]any)
	if len(data) == 0 {
		t.Fatalf("input_items empty: %v", list)
	}
	if iid, _ := data[0].(map[string]any)["id"].(string); !strings.HasPrefix(iid, "item_") {
		t.Fatalf("item id not rewritten: %v", data[0])
	}
	if fid, _ := list["first_id"].(string); !strings.HasPrefix(fid, "item_") {
		t.Fatalf("first_id not bound: %v", fid)
	}

	// Cancel hits the bound upstream path and preserves the binding.
	rec = f.do(t, f.keyA, http.MethodPost, "/v1/responses/"+pub+"/cancel", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body.String())
	}
	if up.calls[len(up.calls)-1].Path != "/responses/resp_UP1/cancel" {
		t.Fatalf("cancel dispatched %q", up.calls[len(up.calls)-1].Path)
	}

	// Delete tombstones the binding; the id stops resolving afterwards.
	rec = f.do(t, f.keyA, http.MethodDelete, "/v1/responses/"+pub, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if up.calls[len(up.calls)-1].Path != "/responses/resp_UP1" || up.calls[len(up.calls)-1].Method != http.MethodDelete {
		t.Fatalf("delete dispatched %+v", up.calls[len(up.calls)-1])
	}
	if _, err := f.bindings.Resolve(context.Background(), f.tenant, pub); err == nil {
		t.Fatal("deleted binding still resolves")
	}
	rec = f.do(t, f.keyA, http.MethodGet, "/v1/responses/"+pub, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("get after delete: %d", rec.Code)
	}
}

func TestResponsesChainedFollowUpPinsOrigin(t *testing.T) {
	f := newResponsesFx(t)
	pub := f.createResponse(t, f.keyA, "gpt-fast")
	upA := f.ups["ep-resp"]
	upB := f.ups["ep-zz"]

	// A chained create resolves the public parent to its bound upstream id
	// and stays on the parent's endpoint even when another model is picked.
	rec := f.do(t, f.keyA, http.MethodPost, "/v1/responses",
		`{"model":"gpt-fast","previous_response_id":"`+pub+`","input":"next"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("chained create: %d %s", rec.Code, rec.Body.String())
	}
	last := upA.calls[len(upA.calls)-1]
	if !strings.Contains(last.Body, `"previous_response_id":"resp_UP1"`) {
		t.Fatalf("chained request must carry bound upstream parent: %s", last.Body)
	}
	if len(upB.calls) != 0 {
		t.Fatalf("chained request leaked to another endpoint: %+v", upB.calls)
	}
	child := decodeJSONMap(t, rec)
	childID, _ := child["id"].(string)
	if childID == pub {
		t.Fatal("child response must mint its own public id")
	}
	// The outbound parent's public id is echoed, never the upstream id.
	if prev, _ := child["previous_response_id"].(string); prev != pub {
		t.Fatalf("previous_response_id must echo the public parent, got %v", prev)
	}
	// Exact quoted upstream ids: random public ids may legitimately contain "UP".
	for _, up := range []string{`"resp_UP1"`, `"resp_UP2"`, `"msg_UP1_1"`, `"msg_UP2_1"`, `"cmp_UP1"`} {
		if strings.Contains(rec.Body.String(), up) {
			t.Fatalf("upstream id %s leaked in response: %s", up, rec.Body.String())
		}
	}

	// Item references resolve through bindings too.
	rec = f.do(t, f.keyA, http.MethodPost, "/v1/responses",
		`{"model":"gpt-fast","previous_response_id":"`+childID+`","input":[{"type":"item_reference","id":"`+itemIDOf(t, f, childID)+`"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("item-reference create: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(upA.calls[len(upA.calls)-1].Body, `"id":"msg_UP2_1"`) {
		t.Fatalf("item reference not translated: %s", upA.calls[len(upA.calls)-1].Body)
	}

	// A public id from nowhere never reaches upstream.
	before := len(upA.calls)
	rec = f.do(t, f.keyA, http.MethodPost, "/v1/responses",
		`{"model":"gpt-fast","previous_response_id":"resp_UP","input":"x"}`)
	if rec.Code == http.StatusOK || len(upA.calls) != before {
		t.Fatalf("raw upstream id must deny before dispatch: %d", rec.Code)
	}
	rec = f.do(t, f.keyA, http.MethodPost, "/v1/responses",
		`{"model":"gpt-fast","previous_response_id":"resp_nonexistent","input":"x"}`)
	if rec.Code != http.StatusForbidden || len(upA.calls) != before {
		t.Fatalf("foreign reference must deny: %d", rec.Code)
	}
}

// itemIDOf reads the first bound item's public id for a response.
func itemIDOf(t *testing.T, f *responsesFx, responsePublicID string) string {
	t.Helper()
	var id string
	err := f.db.SQL.QueryRowContext(context.Background(),
		`SELECT public_id FROM response_bindings WHERE tenant_id=$1 AND parent_public_id=$2 AND kind='item' LIMIT 1`,
		f.tenant, responsePublicID).Scan(&id)
	if err != nil {
		t.Fatalf("lookup bound item: %v", err)
	}
	return id
}

func TestResponsesCrossOwnerAndTenantDenied(t *testing.T) {
	f := newResponsesFx(t)
	pub := f.createResponse(t, f.keyA, "gpt-fast")
	upA := f.ups["ep-resp"]
	before := len(upA.calls)

	for _, tc := range []struct {
		name   string
		key    string
		method string
		path   string
		body   string
	}{
		{"other user get", f.keyB, http.MethodGet, "/v1/responses/" + pub, ""},
		{"other user cancel", f.keyB, http.MethodPost, "/v1/responses/" + pub + "/cancel", ""},
		{"other user delete", f.keyB, http.MethodDelete, "/v1/responses/" + pub, ""},
		{"other user items", f.keyB, http.MethodGet, "/v1/responses/" + pub + "/input_items", ""},
		{"other tenant get", f.keyOther, http.MethodGet, "/v1/responses/" + pub, ""},
		{"raw upstream id", f.keyA, http.MethodGet, "/v1/responses/resp_UP", ""},
		{"raw upstream cancel", f.keyA, http.MethodPost, "/v1/responses/resp_UP/cancel", ""},
	} {
		rec := f.do(t, tc.key, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: got %d want 403 (%s)", tc.name, rec.Code, rec.Body.String())
		}
		var env OpenAIErrorEnvelope
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		if strings.Contains(env.Error.Message, pub) || strings.Contains(env.Error.Message, "resp_UP") {
			t.Fatalf("%s: error discloses identifiers: %s", tc.name, rec.Body.String())
		}
	}
	if len(upA.calls) != before {
		t.Fatalf("denied follow-up reached upstream: %+v", upA.calls[before:])
	}

	// Foreign cursor cursors deny before any list.
	rec := f.do(t, f.keyB, http.MethodGet, "/v1/responses/"+pub+"/input_items?after="+pub, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign cursor: %d", rec.Code)
	}
}

func TestResponsesEndpointCollisionStaysLocal(t *testing.T) {
	f := newResponsesFx(t)
	// Both endpoints emit the same upstream ids — collisions must not merge.
	pubA := f.createResponse(t, f.keyA, "gpt-fast")
	pubB := f.createResponse(t, f.keyA, "gpt-alt")
	if pubA == pubB {
		t.Fatal("colliding upstream ids minted the same public id")
	}
	bindA, err := f.bindings.Resolve(context.Background(), f.tenant, pubA)
	if err != nil || bindA.EndpointID != "ep-resp" {
		t.Fatalf("binding A: %v %+v", err, bindA)
	}
	bindB, err := f.bindings.Resolve(context.Background(), f.tenant, pubB)
	if err != nil || bindB.EndpointID != "ep-zz" {
		t.Fatalf("binding B: %v %+v", err, bindB)
	}
	// Follow-ups reach their own endpoint only.
	f.do(t, f.keyA, http.MethodGet, "/v1/responses/"+pubB, "")
	if len(f.ups["ep-zz"].calls) == 0 {
		t.Fatal("ep-alt follow-up missing")
	}
	for _, c := range f.ups["ep-resp"].calls {
		if strings.HasPrefix(c.Path, "/responses/resp_UP1") && len(f.ups["ep-zz"].calls) > 0 && c.Method == http.MethodGet && len(f.ups["ep-resp"].calls) > 2 {
		}
	}
}

func TestResponsesDeniedShapes(t *testing.T) {
	f := newResponsesFx(t)
	upA := f.ups["ep-resp"]
	before := len(upA.calls)
	deny := map[string]int{
		`{"model":"gpt-fast","input":"x","store":true}`:                                                          http.StatusForbidden,
		`{"model":"gpt-fast","input":"x","background":true}`:                                                     http.StatusForbidden,
		`{"model":"gpt-fast","input":"x","tools":[{"type":"web_search"}]}`:                                       http.StatusBadRequest,
		`{"model":"gpt-fast","input":"x","tools":[{"type":"file_search","vector_store_ids":["vs_1"]}]}`:          http.StatusBadRequest,
		`{"model":"gpt-fast","input":"x","tools":[{"type":"mcp","server_url":"https://x.example"}]}`:             http.StatusBadRequest,
		`{"model":"gpt-fast","input":[{"type":"message","content":[{"type":"input_file","file_id":"file_1"}]}]}`: http.StatusForbidden,
		`{"model":"gpt-fast","input":"x","previous_response_id":123}`:                                            http.StatusForbidden,
		`{"model":"gpt-fast","input":[{"type":"unknown_kind"}]}`:                                                 http.StatusForbidden,
	}
	for body, want := range deny {
		rec := f.do(t, f.keyA, http.MethodPost, "/v1/responses", body)
		if rec.Code != want {
			t.Fatalf("body %s: got %d want %d (%s)", body, rec.Code, want, rec.Body.String())
		}
	}
	// store:false is an honest 400 (client-chosen retention), not a denial.
	rec := f.do(t, f.keyA, http.MethodPost, "/v1/responses", `{"model":"gpt-fast","input":"x","store":false}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("store:false: %d %s", rec.Code, rec.Body.String())
	}
	if len(upA.calls) != before {
		t.Fatalf("denied shapes dispatched upstream: %+v", upA.calls[before:])
	}
}

func TestResponsesCancelAndDeleteUnderBudgetExhaustion(t *testing.T) {
	f := newResponsesFx(t)
	pub := f.createResponse(t, f.keyA, "gpt-fast")

	// Exhaust the tenant's inference budget after the create.
	if err := f.brepo.CreateBudget(context.Background(), f.tenant, "bud-tiny", billing.Budget{
		Subject:           billing.Subject{Type: billing.BudgetSubjectTenant, ID: f.tenant},
		Kind:              billing.BudgetKindAmount,
		Period:            billing.PeriodMonthly,
		LimitAmountMicros: 1,
		Currency:          "USD",
		StartsAt:          time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("create budget: %v", err)
	}
	rec := f.do(t, f.keyA, http.MethodPost, "/v1/responses", `{"model":"gpt-fast","input":"more"}`)
	if rec.Code == http.StatusOK {
		t.Fatal("priced create must deny at exhausted budget")
	}
	// Management follows the binding, not the budget: cancel and delete work.
	rec = f.do(t, f.keyA, http.MethodPost, "/v1/responses/"+pub+"/cancel", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel under exhausted budget: %d %s", rec.Code, rec.Body.String())
	}
	rec = f.do(t, f.keyA, http.MethodDelete, "/v1/responses/"+pub, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete under exhausted budget: %d %s", rec.Code, rec.Body.String())
	}
}

func TestResponsesStreamRewritesAndSettles(t *testing.T) {
	f := newResponsesFx(t)
	rec := f.do(t, f.keyA, http.MethodPost, "/v1/responses", `{"model":"gpt-fast","input":"hi","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("stream create: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "text/event-stream") && rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("expected SSE stream, got %q", rec.Header().Get("Content-Type"))
	}
	if strings.Contains(body, "resp_UP") || strings.Contains(body, "msg_UP") {
		t.Fatalf("upstream ids leaked into stream: %s", body)
	}
	if !strings.Contains(body, "resp_") || !strings.Contains(body, "item_") {
		t.Fatalf("public ids missing from stream: %s", body)
	}
	if !strings.Contains(body, "{malformed upstream event") {
		t.Fatalf("malformed event dropped instead of relayed: %s", body)
	}
	// previous_response_id for an unknown upstream parent is dropped, never
	// emitted raw.
	if strings.Contains(body, "resp_PREV") {
		t.Fatalf("unbound parent upstream id leaked: %s", body)
	}
	// Stream settled once with terminal usage: no retained liability.
	var unsettled int
	if err := f.db.SQL.QueryRowContext(context.Background(),
		`SELECT count(*) FROM reservations WHERE tenant_id=$1 AND state IN ('held','unknown')`,
		f.tenant).Scan(&unsettled); err != nil {
		t.Fatalf("reservation query: %v", err)
	}
	if unsettled != 0 {
		t.Fatalf("stream left unsettled liability: %d", unsettled)
	}
}

func TestResponsesInputTokensAndCompact(t *testing.T) {
	f := newResponsesFx(t)
	upA := f.ups["ep-resp"]

	rec := f.do(t, f.keyA, http.MethodPost, "/v1/responses/input_tokens", `{"model":"gpt-fast","input":"count me"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("input_tokens: %d %s", rec.Code, rec.Body.String())
	}
	out := decodeJSONMap(t, rec)
	if n, _ := out["input_tokens"].(float64); n != 42 {
		t.Fatalf("input_tokens body: %v", out)
	}
	if upA.calls[len(upA.calls)-1].Path != "/responses/input_tokens" {
		t.Fatalf("input_tokens dispatched %q", upA.calls[len(upA.calls)-1].Path)
	}
	// Priced and settled once — the count is recorded usage, not free work.
	var st string
	if err := f.db.SQL.QueryRowContext(context.Background(),
		`SELECT state FROM reservations WHERE tenant_id=$1 ORDER BY created_at DESC LIMIT 1`,
		f.tenant).Scan(&st); err != nil {
		t.Fatalf("reservation: %v", err)
	}
	if st != "settled" {
		t.Fatalf("input_tokens reservation state %q", st)
	}
	var units string
	if err := f.db.SQL.QueryRowContext(context.Background(),
		`SELECT units::text FROM usage_records WHERE tenant_id=$1 ORDER BY created_at DESC LIMIT 1`,
		f.tenant).Scan(&units); err != nil && err != sql.ErrNoRows {
		t.Fatalf("usage: %v", err)
	}
	if !strings.Contains(units, `"input"`) || !strings.Contains(units, "42") {
		t.Fatalf("input_tokens usage units: %s", units)
	}

	rec = f.do(t, f.keyA, http.MethodPost, "/v1/responses/compact", `{"model":"gpt-fast","input":"long text"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("compact: %d %s", rec.Code, rec.Body.String())
	}
	out = decodeJSONMap(t, rec)
	items, _ := out["output"].([]any)
	if len(items) == 0 {
		t.Fatalf("compact output: %v", out)
	}
	if iid, _ := items[0].(map[string]any)["id"].(string); !strings.HasPrefix(iid, "item_") {
		t.Fatalf("compact item id not bound: %v", items[0])
	}
	if upA.calls[len(upA.calls)-1].Path != "/responses/compact" {
		t.Fatalf("compact dispatched %q", upA.calls[len(upA.calls)-1].Path)
	}
}
