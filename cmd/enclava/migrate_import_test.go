package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/llm"
	"enclava-go/internal/rag"
	"enclava-go/internal/store/sqlite"
)

func TestMigrateImportDryRunAndRealImport(t *testing.T) {
	ctx := context.Background()
	inputDir := t.TempDir()
	apiKey := "enc_test_migrate_import_secret_1234567890"
	prefix, hash, algo, err := auth.HashForStorage(apiKey, "sha256", "")
	if err != nil {
		t.Fatalf("hash key: %v", err)
	}
	writeJSONL(t, inputDir, "api_keys.jsonl", map[string]any{
		"id":               101,
		"name":             "migrated",
		"key_prefix":       prefix,
		"key_hash":         hash,
		"key_hash_algo":    algo,
		"scopes":           []string{"models.list", "chatbot"},
		"allowed_chatbots": []string{"migrated_bot"},
		"allowed_agents":   []string{"migrated_agent"},
		"is_active":        true,
		"is_unlimited":     true,
	})
	writeJSONL(t, inputDir, "prompt_templates.jsonl", map[string]any{
		"id":            "prompt_migrated",
		"name":          "Migrated Prompt",
		"system_prompt": "You are migrated.",
		"user_prompt":   "{{.user_input}}",
		"is_active":     true,
	})
	writeJSONL(t, inputDir, "extract_templates.jsonl", map[string]any{
		"id":            "extract_migrated",
		"name":          "Migrated Extract",
		"system_prompt": "Extract.",
		"user_prompt":   "Return JSON.",
		"output_schema": map[string]any{
			"type":     "object",
			"required": []string{"total"},
			"properties": map[string]any{
				"total": map[string]any{"type": "number"},
			},
		},
		"is_active": true,
	})
	writeJSONL(t, inputDir, "chatbots.jsonl", map[string]any{
		"id":                 "chatbot_migrated",
		"name":               "Migrated Bot",
		"prompt_template_id": "prompt_migrated",
		"model":              "migrated-model",
		"external_id":        "migrated_bot",
		"rag_collection_ids": []string{"ragcol_migrated"},
		"is_enabled":         true,
	})
	writeJSONL(t, inputDir, "rag_collections.jsonl", map[string]any{
		"id":                "ragcol_migrated",
		"name":              "Migrated RAG",
		"qdrant_collection": "qdrant_migrated",
		"embedding_model":   "embed-model",
		"chunk_size":        100,
		"chunk_overlap":     10,
		"acl":               map[string]any{"public": true},
		"is_active":         true,
	})
	writeJSONL(t, inputDir, "rag_documents.jsonl", map[string]any{
		"id":                 "ragdoc_migrated",
		"collection_id":      "ragcol_migrated",
		"source_type":        "migration",
		"source_id":          "doc-1",
		"file_name":          "migrated.txt",
		"mime_type":          "text/plain",
		"status":             "ready",
		"qdrant_point_count": 1,
	})

	dryReport, err := migrateImport(ctx, inputDir, filepath.Join(t.TempDir(), "dry-run.db"), true)
	if err != nil {
		t.Fatalf("dry-run import: %v", err)
	}
	if !dryReport.DryRun || rowsForImportFile(dryReport, "api_keys.jsonl") != 1 {
		t.Fatalf("unexpected dry-run report: %#v", dryReport)
	}

	dbPath := filepath.Join(t.TempDir(), "import.db")
	report, err := migrateImport(ctx, inputDir, dbPath, false)
	if err != nil {
		t.Fatalf("real import: %v", err)
	}
	if report.DryRun {
		t.Fatalf("expected real import report: %#v", report)
	}

	db, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open imported db: %v", err)
	}
	defer func() { _ = db.Close() }()

	principal, err := auth.NewService(sqlite.NewAPIKeysRepo(db.SQL), "").Authenticate(ctx, apiKey)
	if err != nil {
		t.Fatalf("authenticate imported key: %v", err)
	}
	if principal.APIKeyID != 101 {
		t.Fatalf("unexpected imported principal: %#v", principal)
	}
	if !auth.ChatbotAllowed(principal, "chatbot_migrated", "migrated_bot") {
		t.Fatalf("expected migrated chatbot allowlist")
	}
	if !auth.AgentAllowed(principal, "migrated_agent") {
		t.Fatalf("expected migrated agent allowlist")
	}
	var outputSchema string
	if err := db.SQL.QueryRowContext(ctx, `SELECT output_schema FROM extract_templates WHERE id = ?`, "extract_migrated").Scan(&outputSchema); err != nil {
		t.Fatalf("query imported extract template: %v", err)
	}
	if outputSchema == "" || outputSchema == "{}" {
		t.Fatalf("expected imported output schema, got %q", outputSchema)
	}
	var chatbotName string
	if err := db.SQL.QueryRowContext(ctx, `SELECT name FROM chatbot_instances WHERE id = ?`, "chatbot_migrated").Scan(&chatbotName); err != nil {
		t.Fatalf("query imported chatbot: %v", err)
	}
	if chatbotName != "Migrated Bot" {
		t.Fatalf("unexpected chatbot name: %s", chatbotName)
	}

	provider := migratedTestProvider{}
	vectorStore := newMigratedVectorStore()
	vectorStore.points["qdrant_migrated"] = []rag.QdrantPoint{{
		ID:     "point_migrated",
		Vector: []float64{1, 2, 3},
		Payload: map[string]any{
			"document_id": "ragdoc_migrated",
			"chunk_id":    "ragchunk_migrated",
			"content":     "migrated rag searchable phrase",
			"file_name":   "migrated.txt",
			"metadata":    map[string]any{"source": "migration"},
		},
	}}
	ragSvc := rag.NewService(sqlite.NewRAGRepo(db.SQL), vectorStore, provider, "embed-model")
	searchResults, err := ragSvc.Search(ctx, rag.Access{APIKeyID: principal.APIKeyID}, rag.SearchInput{
		CollectionID: "ragcol_migrated",
		Query:        "searchable phrase",
		Limit:        5,
	})
	if err != nil {
		t.Fatalf("search imported rag collection: %v", err)
	}
	if len(searchResults) != 1 || !strings.Contains(searchResults[0].Content, "migrated rag searchable phrase") {
		t.Fatalf("unexpected migrated rag results: %#v", searchResults)
	}

	chatbotSvc := chatbot.NewService(sqlite.NewChatbotRepo(db.SQL), sqlite.NewPromptTemplatesRepo(db.SQL), ragSvc, provider, sqlite.NewUsageRepo(db.SQL))
	chatOut, err := chatbotSvc.CreateChatCompletion(ctx, chatbot.Access{
		APIKeyID:           principal.APIKeyID,
		AllowedModels:      principal.AllowedModels,
		AllowedChatbots:    principal.AllowedChatbots,
		AllowedModelsAll:   principal.AllowedModelsAll,
		AllowedChatbotsAll: principal.AllowedChatbotsAll,
	}, "migrated_bot", []byte(`{"model":"client-model","messages":[{"role":"user","content":"Use migrated context"}]}`))
	if err != nil {
		t.Fatalf("call imported chatbot: %v", err)
	}
	if chatOut.Chatbot.ID != "chatbot_migrated" || chatOut.RAGResultCount != 1 || !strings.Contains(string(chatOut.Response), "migrated ok") {
		t.Fatalf("unexpected imported chatbot output: %#v body=%s", chatOut, string(chatOut.Response))
	}
}

func writeJSONL(t *testing.T, dir, name string, rows ...map[string]any) {
	t.Helper()
	file, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	defer func() { _ = file.Close() }()
	for _, row := range rows {
		if err := json.NewEncoder(file).Encode(row); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func rowsForImportFile(report importReport, file string) int {
	for _, item := range report.Files {
		if item.File == file {
			return item.Rows
		}
	}
	return 0
}

func existingForImportFile(report importReport, file string) int {
	for _, item := range report.Files {
		if item.File == file {
			return item.Existing
		}
	}
	return 0
}

func TestMigrateImportConstraintFailuresRollBack(t *testing.T) {
	ctx := context.Background()
	source, err := os.ReadFile("migrate_import.go")
	if err != nil {
		t.Fatalf("read importer: %v", err)
	}
	if strings.Contains(string(source), "INSERT OR IGNORE") {
		t.Fatal("migrate_import.go still contains INSERT OR IGNORE")
	}

	t.Run("missing required value", func(t *testing.T) {
		inputDir := t.TempDir()
		writeJSONL(t, inputDir, "users.jsonl",
			map[string]any{"id": 1, "email": "kept@example.com"},
			map[string]any{"id": 2, "display_name": "no email"},
		)
		dbPath := filepath.Join(t.TempDir(), "missing.db")
		_, err := migrateImport(ctx, inputDir, dbPath, false)
		if err == nil || !strings.Contains(err.Error(), "users.jsonl") || !strings.Contains(err.Error(), "line 2") {
			t.Fatalf("expected file and line error, got %v", err)
		}
		assertImportTablesEmpty(t, dbPath)
	})

	t.Run("dangling foreign key", func(t *testing.T) {
		inputDir := t.TempDir()
		writeJSONL(t, inputDir, "users.jsonl", map[string]any{"id": 1, "email": "kept@example.com"})
		writeJSONL(t, inputDir, "usage_records.jsonl", map[string]any{
			"id":         "usage_dangling",
			"api_key_id": 424242,
			"endpoint":   "/api/v1/chat/completions",
			"cost_cents": 5,
		})
		dbPath := filepath.Join(t.TempDir(), "fk.db")
		_, err := migrateImport(ctx, inputDir, dbPath, false)
		if err == nil || !strings.Contains(err.Error(), "usage_records.jsonl") || !strings.Contains(err.Error(), "line 1") {
			t.Fatalf("expected dangling fk error with file and line, got %v", err)
		}
		assertImportTablesEmpty(t, dbPath)
	})

	t.Run("duplicate natural unique", func(t *testing.T) {
		inputDir := t.TempDir()
		writeJSONL(t, inputDir, "users.jsonl",
			map[string]any{"id": 1, "email": "same@example.com"},
			map[string]any{"id": 2, "email": "same@example.com"},
		)
		dbPath := filepath.Join(t.TempDir(), "unique.db")
		_, err := migrateImport(ctx, inputDir, dbPath, false)
		if err == nil || !strings.Contains(err.Error(), "users.jsonl") || !strings.Contains(err.Error(), "line 2") {
			t.Fatalf("expected unique conflict with file and line, got %v", err)
		}
		assertImportTablesEmpty(t, dbPath)
	})
}

func TestMigrateImportReimportCountsExisting(t *testing.T) {
	ctx := context.Background()
	inputDir := t.TempDir()
	writeJSONL(t, inputDir, "api_keys.jsonl", map[string]any{
		"id":               7,
		"name":             "imported",
		"key_prefix":       "enc_imp",
		"key_hash":         "hash-import-cost-7",
		"total_cost_cents": 99,
	})
	writeJSONL(t, inputDir, "usage_records.jsonl", map[string]any{
		"id":         "usage_import_cost",
		"api_key_id": 7,
		"endpoint":   "/api/v1/chat/completions",
		"cost_cents": 4,
	})
	dbPath := filepath.Join(t.TempDir(), "reimport.db")
	first, err := migrateImport(ctx, inputDir, dbPath, false)
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if rowsForImportFile(first, "api_keys.jsonl") != 1 || existingForImportFile(first, "api_keys.jsonl") != 0 {
		t.Fatalf("unexpected first api key counts: %#v", first)
	}
	if rowsForImportFile(first, "usage_records.jsonl") != 1 || existingForImportFile(first, "usage_records.jsonl") != 0 {
		t.Fatalf("unexpected first usage counts: %#v", first)
	}

	db, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	var costCents, costMicro int64
	if err := db.SQL.QueryRowContext(ctx, `SELECT cost_cents, cost_microcents FROM usage_records WHERE id = ?`, "usage_import_cost").Scan(&costCents, &costMicro); err != nil {
		t.Fatalf("query usage: %v", err)
	}
	if costCents != 4 || costMicro != 4_000_000 {
		t.Fatalf("imported usage cost cents=%d micro=%d", costCents, costMicro)
	}
	var totalCents, totalMicro int64
	if err := db.SQL.QueryRowContext(ctx, `SELECT total_cost_cents, total_cost_microcents FROM api_keys WHERE id = 7`).Scan(&totalCents, &totalMicro); err != nil {
		t.Fatalf("query key totals: %v", err)
	}
	if totalCents != 4 || totalMicro != 4_000_000 {
		t.Fatalf("recomputed key totals cents=%d micro=%d", totalCents, totalMicro)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	second, err := migrateImport(ctx, inputDir, dbPath, false)
	if err != nil {
		t.Fatalf("reimport: %v", err)
	}
	if rowsForImportFile(second, "api_keys.jsonl") != 0 || existingForImportFile(second, "api_keys.jsonl") != 1 {
		t.Fatalf("unexpected reimport api key counts: %#v", second)
	}
	if rowsForImportFile(second, "usage_records.jsonl") != 0 || existingForImportFile(second, "usage_records.jsonl") != 1 {
		t.Fatalf("unexpected reimport usage counts: %#v", second)
	}
}

func assertImportTablesEmpty(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sqlite.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()
	for _, table := range []string{"users", "usage_records", "api_keys"} {
		var count int
		if err := db.SQL.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("%s has %d rows after failed import", table, count)
		}
	}
}

type migratedTestProvider struct{}

func (migratedTestProvider) ListModels(ctx context.Context) (llm.ModelsResponse, error) {
	return llm.ModelsResponse{Object: "list", Data: []llm.Model{{ID: "migrated-model"}, {ID: "embed-model", SupportsEmbeddings: true}}}, nil
}

func (migratedTestProvider) GetModel(ctx context.Context, modelID string) (llm.Model, error) {
	return llm.Model{ID: modelID, Object: "model"}, nil
}

func (migratedTestProvider) CreateChatCompletion(ctx context.Context, body []byte) ([]byte, error) {
	return json.Marshal(map[string]any{
		"id":     "chatcmpl_migrated",
		"object": "chat.completion",
		"model":  "migrated-model",
		"choices": []any{
			map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": "migrated ok",
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5},
	})
}

func (migratedTestProvider) CreateChatCompletionStream(ctx context.Context, body []byte) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("data: [DONE]\n\n")), nil
}

func (migratedTestProvider) CreateEmbeddings(ctx context.Context, body []byte) ([]byte, error) {
	var payload struct {
		Input any `json:"input"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	inputs := []string{}
	switch raw := payload.Input.(type) {
	case string:
		inputs = append(inputs, raw)
	case []any:
		for _, item := range raw {
			inputs = append(inputs, item.(string))
		}
	}
	data := make([]map[string]any, 0, len(inputs))
	for i := range inputs {
		data = append(data, map[string]any{"object": "embedding", "index": i, "embedding": []float64{1, 2, 3}})
	}
	return json.Marshal(map[string]any{
		"object": "list",
		"model":  "embed-model",
		"data":   data,
		"usage":  map[string]any{"prompt_tokens": len(inputs), "total_tokens": len(inputs)},
	})
}

type migratedVectorStore struct {
	points map[string][]rag.QdrantPoint
}

func newMigratedVectorStore() *migratedVectorStore {
	return &migratedVectorStore{points: map[string][]rag.QdrantPoint{}}
}

func (s *migratedVectorStore) EnsureCollection(ctx context.Context, collection string, vectorSize int) error {
	if _, ok := s.points[collection]; !ok {
		s.points[collection] = nil
	}
	return nil
}

func (s *migratedVectorStore) UpsertPoints(ctx context.Context, collection string, points []rag.QdrantPoint) error {
	s.points[collection] = append(s.points[collection], points...)
	return nil
}

func (s *migratedVectorStore) DeleteDocumentPoints(ctx context.Context, collection, documentID string) error {
	kept := []rag.QdrantPoint{}
	for _, point := range s.points[collection] {
		if point.Payload["document_id"] != documentID {
			kept = append(kept, point)
		}
	}
	s.points[collection] = kept
	return nil
}

func (s *migratedVectorStore) DeleteCollection(ctx context.Context, collection string) error {
	delete(s.points, collection)
	return nil
}

func (s *migratedVectorStore) Search(ctx context.Context, collection string, vector []float64, limit int, filter map[string]any) ([]rag.QdrantSearchPoint, error) {
	points := s.points[collection]
	if limit <= 0 || limit > len(points) {
		limit = len(points)
	}
	out := make([]rag.QdrantSearchPoint, 0, limit)
	for _, point := range points[:limit] {
		out = append(out, rag.QdrantSearchPoint{ID: point.ID, Score: 0.99, Payload: point.Payload})
	}
	return out, nil
}
