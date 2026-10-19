package extractmodule

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"enclava-go/internal/auth"
	extractsvc "enclava-go/internal/extract"
	"enclava-go/internal/httpx"
	"enclava-go/internal/llm"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/store/sqlite"
	"enclava-go/internal/testdb"
)

func TestExtractTemplateIDFromRequestAcceptsLegacyAliases(t *testing.T) {
	for _, tc := range []struct {
		name string
		form url.Values
		want string
	}{
		{name: "template_id", form: url.Values{"template_id": {"invoice"}}, want: "invoice"},
		{name: "template", form: url.Values{"template": {"receipt"}}, want: "receipt"},
		{name: "extract_template_id", form: url.Values{"extract_template_id": {"expense"}}, want: "expense"},
		{name: "template_id wins", form: url.Values{"template_id": {"invoice"}, "template": {"receipt"}}, want: "invoice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/v1/extract/process", nil)
			req.Form = tc.form
			if got := extractTemplateIDFromRequest(req); got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}

func TestHandleProcessRecordsPricedExtractUsage(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "extract-usage.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Authentication resolves against an isolated PostgreSQL identity store.
	pgdb := testdb.New(t)
	tenant := testdb.SeedTenant(t, pgdb, "extract-usage")
	user := testdb.SeedUser(t, pgdb, tenant, "extract@example.com", nil)
	apiKey, _ := testdb.SeedAPIKey(t, pgdb, tenant, testdb.SeedKeySpec{
		Name:        "extract-usage",
		OwnerUserID: user.ID,
		Scopes:      []string{"extract"},
	})
	keysRepo := sqlite.NewAPIKeysRepo(db.SQL)
	identityRepo := postgres.NewIdentityRepo(pgdb)
	// Mirror the deterministic legacy numeric handle: usage_records still
	// FK-references sqlite api_keys.id until those stores are ported.
	seeded, err := auth.NewService(identityRepo, "").Authenticate(ctx, apiKey)
	if err != nil {
		t.Fatalf("authenticate seeded key: %v", err)
	}
	if _, err := db.SQL.ExecContext(ctx,
		`INSERT INTO api_keys (id, name, key_prefix, key_hash, is_active) VALUES (?, 'extract-usage', '', '', 1)`,
		seeded.APIKeyID); err != nil {
		t.Fatalf("mirror legacy key row: %v", err)
	}

	templatesRepo := sqlite.NewExtractTemplatesRepo(db.SQL)
	if err := templatesRepo.Upsert(ctx, sqlite.ExtractTemplate{
		ID:            "invoice",
		Name:          "Invoice",
		SystemPrompt:  "Extract invoice data.",
		UserPrompt:    "Return JSON.",
		Model:         "priced-extract",
		ContextSchema: map[string]any{},
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"total": map[string]any{"type": "number"},
			},
		},
		IsActive: true,
	}); err != nil {
		t.Fatalf("upsert extract template: %v", err)
	}
	settingsRepo := sqlite.NewExtractSettingsRepo(db.SQL)
	if err := settingsRepo.EnsureDefaultRow(ctx, 5); err != nil {
		t.Fatalf("ensure extract settings: %v", err)
	}
	if _, err := sqlite.NewPricingRepo(db.SQL).Create(ctx, sqlite.ProviderPricing{
		ProviderID:            "extract-provider",
		ModelID:               "priced-extract",
		Currency:              "USD",
		InputTokenCentsPer1K:  10,
		OutputTokenCentsPer1K: 20,
		EffectiveAt:           time.Now().Add(-time.Minute).UTC(),
	}, sqlite.PricingAuditLog{ID: "priceaudit_extract_usage_test"}); err != nil {
		t.Fatalf("create pricing: %v", err)
	}

	provider := &extractHandlerProvider{
		response: []byte(`{"id":"c","model":"priced-extract","usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500},"choices":[{"message":{"content":"{\"total\":12.5}"}}]}`),
	}
	extractService := extractsvc.NewService(
		templatesRepo,
		settingsRepo,
		sqlite.NewExtractJobsRepo(db.SQL),
		provider,
		5,
	)
	app := httpx.NewApp(
		auth.NewService(identityRepo, ""),
		provider,
		"extract-provider",
		nil,
		keysRepo,
		sqlite.NewExtractJobsRepo(db.SQL),
		templatesRepo,
		settingsRepo,
		extractService,
		nil,
		nil,
		nil,
		nil,
		sqlite.NewUsageRepo(db.SQL),
		sqlite.NewPricingRepo(db.SQL),
		sqlite.NewBudgetsRepo(db.SQL),
		nil,
		nil,
		"sha256",
		"",
		"enc_",
		nil,
		"",
		"",
		5,
		false,
	)
	handlers := NewHandlers(extractService, 5, app)
	principal, err := auth.NewService(identityRepo, "").Authenticate(ctx, apiKey)
	if err != nil {
		t.Fatalf("authenticate key: %v", err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("template_id", "invoice"); err != nil {
		t.Fatalf("write template field: %v", err)
	}
	fileWriter, err := writer.CreateFormFile("file", "invoice.pdf")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fileWriter.Write([]byte("%PDF-1.7 fake")); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/extract/process", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()

	handlers.HandleProcess(rec, req, principal)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var endpoint, model, providerID string
	var promptTokens, completionTokens, totalTokens, costCents int64
	if err := db.SQL.QueryRowContext(ctx, `
		SELECT endpoint, model, provider_id, prompt_tokens, completion_tokens, total_tokens, cost_cents
		FROM usage_records
		WHERE api_key_id = ?
	`, principal.APIKeyID).Scan(&endpoint, &model, &providerID, &promptTokens, &completionTokens, &totalTokens, &costCents); err != nil {
		t.Fatalf("query usage record: %v", err)
	}
	if endpoint != "/api/v1/extract/process" || model != "priced-extract" || providerID != "extract-provider" {
		t.Fatalf("unexpected usage identity: endpoint=%s model=%s provider=%s", endpoint, model, providerID)
	}
	if promptTokens != 1000 || completionTokens != 500 || totalTokens != 1500 || costCents != 20 {
		t.Fatalf("unexpected priced usage: prompt=%d completion=%d total=%d cost=%d", promptTokens, completionTokens, totalTokens, costCents)
	}
}

type extractHandlerProvider struct {
	response []byte
}

func (p *extractHandlerProvider) ListModels(ctx context.Context) (llm.ModelsResponse, error) {
	return llm.ModelsResponse{Object: "list", Data: []llm.Model{{ID: "priced-extract", SupportsVision: true}}}, nil
}

func (p *extractHandlerProvider) GetModel(ctx context.Context, modelID string) (llm.Model, error) {
	return llm.Model{ID: modelID}, nil
}

func (p *extractHandlerProvider) CreateChatCompletion(ctx context.Context, body []byte) ([]byte, error) {
	return p.response, nil
}

func (p *extractHandlerProvider) CreateChatCompletionStream(ctx context.Context, body []byte) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(nil)), nil
}

func (p *extractHandlerProvider) CreateEmbeddings(ctx context.Context, body []byte) ([]byte, error) {
	return json.Marshal(map[string]any{
		"object": "list",
		"data":   []any{map[string]any{"object": "embedding", "index": 0, "embedding": []float64{0.1}}},
		"model":  "priced-extract",
		"usage":  map[string]any{"prompt_tokens": 1, "total_tokens": 1},
	})
}
