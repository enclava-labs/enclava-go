package rag_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

type evalQuery struct {
	Query    string   `json:"query"`
	Expected []string `json:"expected"`
}

type evalMetrics struct {
	Variant string  `json:"variant"`
	Recall1 float64 `json:"recall_at_1"`
	Recall5 float64 `json:"recall_at_5"`
	MRR     float64 `json:"mrr"`
	ShareA  float64 `json:"share_a_top_5,omitempty"`
}

func TestRetrievalEval(t *testing.T) {
	base := strings.TrimSpace(os.Getenv("OPENAI_CONTRACT_BASE_URL"))
	if base == "" || strings.TrimSpace(os.Getenv("OPENAI_CONTRACT_API_KEY")) == "" {
		t.Skip("OPENAI_CONTRACT_BASE_URL/OPENAI_CONTRACT_API_KEY not set; live embedding upstream not requested")
	}
	qdrantURL := envOrFatal(t, "QDRANT_INTEGRATION_URL")
	envOrFatal(t, "ENCLAVA_TEST_DATABASE_URL")
	model := strings.TrimSpace(os.Getenv("ENCLAVA_TEST_EMBEDDING_MODEL"))
	if model == "" {
		model = "text-embedding-3-small"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "rag-eval")
	user := testdb.SeedUser(t, db, tenant, "rag-eval@example.test", nil)
	p := auth.Principal{TenantID: tenant, ActorID: user.ID, ActorKind: auth.ActorKindUser, UserID: user.ID, CredentialID: "key_test", CredentialKind: auth.CredentialKindAPIKey, AllowedModelsAll: true, AllowedEndpointsAll: true}
	origin, err := auth.CanonicalizeSecretOrigin(base)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := auth.NewSecretResolver([]auth.SecretBinding{{RefID: "eval-key", TenantID: tenant, Purpose: auth.SecretPurposeInference, EnvName: "OPENAI_CONTRACT_API_KEY", Origins: []string{origin}}})
	if err != nil {
		t.Fatal(err)
	}
	endpoints := postgres.NewEndpointsRepo(db)
	if err := endpoints.Create(ctx, inference.Endpoint{ID: "eval-endpoint", TenantID: tenant, Protocol: "openai_compatible", BaseOrigin: origin, CredentialRef: "eval-key", Capabilities: []string{inference.CapabilityEmbeddings, "model:" + model}, AuthorizedPrivate: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	billingRepo := postgres.NewBillingRepo(db)
	for _, unit := range []billing.UnitKind{billing.UnitInput, billing.UnitCachedInput} {
		if err := billingRepo.PutPrice(ctx, tenant, "eval-"+string(unit), billing.Price{SubjectType: billing.PriceSubjectEndpoint, SubjectID: "eval-endpoint", Capability: inference.CapabilityEmbeddings, Unit: unit, RateMicros: 1, UnitQuantity: 1, Currency: "USD"}, "tester", time.Now().UTC().Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	sources := postgres.NewSourceRepo(db)
	executor, err := inference.NewService(inference.Deps{Endpoints: endpoints, Sources: sources, Authorizer: source.NewAuthorizer(sources, 0), Billing: billing.NewService(billingRepo), Secrets: resolver, Artifacts: postgres.NewArtifactRepo(db)})
	if err != nil {
		t.Fatal(err)
	}
	probe, err := executor.Execute(ctx, p, inference.Request{Capability: inference.CapabilityEmbeddings, Model: model, Payload: json.RawMessage(`{"input":["dimension probe"]}`), Estimate: billing.Usage{InputUnits: 10}})
	if err != nil {
		t.Fatalf("embedding dimension probe: %v", err)
	}
	var response struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if probe.SettlementError != nil {
		t.Fatal(probe.SettlementError)
	}
	if err := json.NewDecoder(probe.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if err := probe.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if len(response.Data) != 1 || len(response.Data[0].Embedding) == 0 {
		t.Fatal("dimension probe returned no embedding")
	}
	dimensions := len(response.Data[0].Embedding)
	jobsSvc := jobs.NewService(postgres.NewJobsRepo(db), 0)
	store, err := artifacts.NewStore(t.TempDir(), t.TempDir(), postgres.NewArtifactRepo(db))
	if err != nil {
		t.Fatal(err)
	}
	vector := rag.NewQdrantClient(qdrantURL, 15*time.Second)
	svc := rag.NewService(postgres.NewRAGRepo(db), vector, executor, model, store, jobsSvc)
	evalFS := os.DirFS("testdata/eval")
	corpus, err := fs.Glob(evalFS, "corpus/*.md")
	if err != nil || len(corpus) == 0 {
		t.Fatalf("read corpus: %v", err)
	}
	data, err := fs.ReadFile(evalFS, "queries.json")
	if err != nil {
		t.Fatal(err)
	}
	var queries []evalQuery
	if err := json.Unmarshal(data, &queries); err != nil || len(queries) == 0 {
		t.Fatalf("read queries: %v", err)
	}
	known := make(map[string]bool, len(corpus))
	for _, member := range corpus {
		known[path.Base(member)] = true
	}
	for _, q := range queries {
		if q.Query == "" || len(q.Expected) == 0 {
			t.Fatalf("invalid query: %+v", q)
		}
		for _, name := range q.Expected {
			if !known[name] {
				t.Fatalf("unknown expected document %q", name)
			}
		}
	}
	create := func(titles bool, dims int, prefix *string, files []string) (rag.Collection, map[string]string) {
		t.Helper()
		c, err := svc.CreateCollection(ctx, p, rag.CreateCollectionInput{Name: "Eval " + ragSafeName(), EmbeddingModel: model, EmbeddingDimensions: &dims, EmbedTitles: &titles, QueryPrefix: prefix, ChunkSize: 2000, ChunkOverlap: 0, ACL: map[string]any{"public": true}})
		if err != nil {
			t.Fatalf("create collection: %v", err)
		}
		t.Cleanup(func() {
			if err := vector.DeleteCollection(context.Background(), c.QdrantCollection); err != nil {
				t.Errorf("delete Qdrant collection: %v", err)
			}
		})
		ids := make(map[string]string, len(files))
		for _, member := range files {
			content, err := fs.ReadFile(evalFS, member)
			if err != nil {
				t.Fatal(err)
			}
			upload, err := svc.UploadDocument(ctx, p, c.ID, rag.UploadDocumentInput{FileName: path.Base(member), MimeType: "text/markdown", Bytes: content})
			if err != nil {
				t.Fatalf("upload %s: %v", member, err)
			}
			ids[upload.Document.ID] = path.Base(member)
			claim, err := jobsSvc.Lease(ctx, "eval-worker", 5*time.Minute)
			if err != nil || claim == nil {
				t.Fatalf("lease %s: %v", member, err)
			}
			ref, err := svc.JobHandler()(ctx, claim, nil)
			if err != nil {
				_ = claim.Fail(ctx, err, jobs.FailureFatal)
				t.Fatalf("index %s: %v", member, err)
			}
			if _, err := claim.Complete(ctx, ref); err != nil {
				t.Fatalf("complete %s: %v", member, err)
			}
		}
		return c, ids
	}
	measure := func(name string, collections []string, ids map[string]string, mode rag.QueryMode, mixing bool) evalMetrics {
		t.Helper()
		m := evalMetrics{Variant: name}
		var a, total int
		for _, q := range queries {
			var results []rag.SearchResult
			if mixing {
				found, err := svc.SearchReadable(ctx, p, q.Query, mode, collections, 5)
				if err != nil {
					t.Fatalf("search %s: %v", name, err)
				}
				results = found.Results
			} else {
				var err error
				results, err = svc.Search(ctx, p, rag.SearchInput{CollectionID: collections[0], Query: q.Query, Mode: mode, Limit: 5})
				if err != nil {
					t.Fatalf("search %s: %v", name, err)
				}
			}
			expected := make(map[string]bool, len(q.Expected))
			for _, file := range q.Expected {
				expected[file] = true
			}
			matched := make(map[string]bool)
			for i, result := range results {
				if mixing {
					total++
					if result.CollectionID == collections[0] {
						a++
					}
				}
				file := ids[result.DocumentID]
				if expected[file] && !matched[file] {
					matched[file] = true
					if len(matched) == 1 {
						m.MRR += 1 / float64(i+1)
					}
					if i == 0 {
						m.Recall1 += 1 / float64(len(expected))
					}
					m.Recall5 += 1 / float64(len(expected))
				}
			}
		}
		m.Recall1 /= float64(len(queries))
		m.Recall5 /= float64(len(queries))
		m.MRR /= float64(len(queries))
		if total > 0 {
			m.ShareA = float64(a) / float64(total)
		}
		return m
	}
	var metrics []evalMetrics
	var baseline evalMetrics
	for _, titles := range []bool{true, false} {
		for _, dims := range []int{0, 1024} {
			if dims > dimensions {
				continue
			}
			for _, empty := range []bool{false, true} {
				var prefix *string
				prefixName := "registry"
				if empty {
					value := ""
					prefix = &value
					prefixName = "empty"
				}
				c, ids := create(titles, dims, prefix, corpus)
				for _, mode := range []rag.QueryMode{rag.QueryHybrid, rag.QuerySemantic, rag.QueryLexical} {
					name := fmt.Sprintf("titles=%t dims=%d prefix=%s mode=%s", titles, dims, prefixName, mode)
					m := measure(name, []string{c.ID}, ids, mode, false)
					metrics = append(metrics, m)
					if titles && dims == 0 && !empty && mode == rag.QueryHybrid {
						baseline = m
					}
				}
			}
		}
	}
	cut := len(corpus) / 10
	if cut == 0 {
		t.Fatal("corpus too small for mixing measurement")
	}
	ca, idsA := create(true, 0, nil, corpus[:cut])
	cb, idsB := create(true, 0, nil, corpus[cut:])
	for id, file := range idsB {
		idsA[id] = file
	}
	mix := measure("mix titles=true dims=0 prefix=registry mode=hybrid", []string{ca.ID, cb.ID}, idsA, rag.QueryHybrid, true)
	metrics = append(metrics, mix)
	t.Logf("%-66s %8s %8s %8s", "variant", "recall@1", "recall@5", "MRR")
	for _, m := range metrics {
		t.Logf("%-66s %8.3f %8.3f %8.3f", m.Variant, m.Recall1, m.Recall5, m.MRR)
	}
	t.Logf("mixing vs baseline: recall@1 %.3f vs %.3f; recall@5 %.3f vs %.3f; MRR %.3f vs %.3f; A share of top-5 %.3f", mix.Recall1, baseline.Recall1, mix.Recall5, baseline.Recall5, mix.MRR, baseline.MRR, mix.ShareA)
	out, err := json.MarshalIndent(struct {
		Model      string        `json:"model"`
		Dimensions int           `json:"dimensions"`
		Metrics    []evalMetrics `json:"metrics"`
	}{model, dimensions, metrics}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "retrieval-eval.json")
	if err := os.WriteFile(output, out, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("evaluation JSON: %s", output)
	if baseline.Recall5 <= 0 {
		t.Fatal("baseline recall@5 is zero; retrieval pipeline is broken")
	}
}
