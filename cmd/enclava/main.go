package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/config"
	"enclava-go/internal/connectors"
	"enclava-go/internal/extract"
	"enclava-go/internal/httpx"
	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
	"enclava-go/internal/llm"
	extractmodule "enclava-go/internal/modules/extract"
	"enclava-go/internal/prompts"
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "bootstrap" {
		if err := runBootstrap(context.Background(), os.Args[2:]); err != nil {
			log.Fatalf("bootstrap error: %v", err)
		}
		return
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// CORE-02/CORE-05: the ONLY database open is PostgreSQL — there is no
	// SQLite path anywhere in the composition root.
	pgDB, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("postgres init error: %v", err)
	}
	defer func() {
		if err := pgDB.Close(); err != nil {
			log.Printf("postgres close failed: %v", err)
		}
	}()

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("data dir init error: %v", err)
	}

	identityRepo := postgres.NewIdentityRepo(pgDB)
	promptRepo := postgres.NewPromptsRepo(pgDB)
	chatbotRepo := postgres.NewChatbotRepo(pgDB)
	billingRepo := postgres.NewBillingRepo(pgDB)
	endpointsRepo := postgres.NewEndpointsRepo(pgDB)
	extractRepo := postgres.NewExtractRepo(pgDB)
	jobsRepoPG := postgres.NewJobsRepo(pgDB)
	artifactRepo := postgres.NewArtifactRepo(pgDB)
	ragRepo := postgres.NewRAGRepo(pgDB)
	bindingsRepo := postgres.NewResponseBindingsRepo(pgDB)
	sourceRepo := postgres.NewSourceRepo(pgDB)

	// Durable artifacts under <data>/artifacts, scratch under <data>/scratch —
	// sibling roots, since the store rejects nested disjoint roots.
	artifactStore, err := artifacts.NewStore(cfg.DataDir+"/artifacts", cfg.DataDir+"/scratch", artifactRepo)
	if err != nil {
		log.Fatalf("artifact store init error: %v", err)
	}
	// D-10: reconcile crash leftovers before serving; a reconcile failure
	// fails startup rather than serving over a suspect durable root.
	if err := artifactStore.Reconcile(ctx); err != nil {
		log.Fatalf("artifact reconcile error: %v", err)
	}

	jobsSvc := jobs.NewService(jobsRepoPG, 0)
	billingSvc := billing.NewService(billingRepo)

	provider, err := buildProvider(cfg)
	if err != nil {
		log.Fatalf("provider init error: %v", err)
	}
	prober := llm.NewProber(provider, cfg.ProviderProbeInterval, cfg.ProviderProbeTimeout)
	go prober.Start(ctx)

	// The env-configured provider is seeded as a canonical endpoint record:
	// model aliases discovered from the provider, vision capability for
	// extraction, and a credential_ref backed by the operator bindings only.
	envBinding, err := ensureEnvEndpoint(ctx, identityRepo, endpointsRepo, cfg, provider)
	if err != nil {
		log.Fatalf("provider config seed error: %v", err)
	}

	// The shared executor is the ONLY priced model-dispatch boundary (INF-02):
	// extraction attempts, worker jobs and any later inference surface all
	// resolve policy, reserve, dispatch and settle through it. Secrets resolve
	// through operator bindings plus the env-key equivalence returned above —
	// never a stored ref as an env name.
	secretBindings := cfg.SecretBindings
	if envBinding != nil {
		dup := false
		for _, b := range cfg.SecretBindings {
			if b.RefID == envBinding.RefID && b.TenantID == envBinding.TenantID &&
				b.Purpose == envBinding.Purpose {
				dup = true
				break
			}
		}
		if !dup {
			secretBindings = append(secretBindings, *envBinding)
		}
	}
	secretResolver, err := auth.NewSecretResolver(secretBindings)
	if err != nil {
		log.Fatalf("secret binding error: %v", err)
	}
	inferenceSvc, err := inference.NewService(inference.Deps{
		Endpoints:     endpointsRepo,
		Sources:       sourceRepo,
		Authorizer:    source.NewAuthorizer(sourceRepo, 0),
		Billing:       billingSvc,
		Secrets:       secretResolver,
		Artifacts:     artifactRepo,
		Conversations: postgres.NewChatbotConversationStore(chatbotRepo),
	}, inference.WithAdmission(inference.NewAdmission(inference.AdmissionConfig{
		Execute: inference.ClassLimit{
			MaxActive: cfg.InferenceMaxActive,
			MaxQueued: cfg.InferenceMaxQueued,
			PerTenant: cfg.InferencePerTenant,
		},
		Manage: inference.ClassLimit{
			MaxActive: cfg.InferenceManageActive,
			MaxQueued: cfg.InferenceManageQueued,
		},
		FanOut: inference.ClassLimit{
			MaxActive: cfg.InferenceFanOutActive,
			MaxQueued: cfg.InferenceFanOutQueued,
		},
	})))
	if err != nil {
		log.Fatalf("inference service init error: %v", err)
	}
	defer func() {
		if err := inferenceSvc.Close(); err != nil {
			log.Printf("inference close failed: %v", err)
		}
	}()

	authSvc := auth.NewService(identityRepo, cfg.APIKeyPepper)

	extractSvc := extract.NewService(
		extractRepo,
		extractRepo,
		jobsRepoPG,
		jobsSvc,
		artifactStore,
		inferenceSvc,
		endpointsRepo,
		cfg.MaxUploadSizeMB,
	)
	// Seed the bootstrap tenant's default extract templates and settings row
	// through the owned service.
	bootstrapTenantID, err := identityRepo.GetTenantBySlug(ctx, "bootstrap")
	if err != nil {
		log.Fatalf("bootstrap tenant lookup error: %v", err)
	}
	bootstrapPrincipal := auth.Principal{TenantID: bootstrapTenantID, ActorID: "bootstrap"}
	if err := extractSvc.SeedDefaults(ctx, bootstrapPrincipal); err != nil {
		log.Fatalf("extract template seed error: %v", err)
	}
	if _, err := extractSvc.GetSettings(ctx, bootstrapPrincipal); err != nil {
		log.Fatalf("extract settings seed error: %v", err)
	}

	qdrantClient := rag.NewQdrantClient(cfg.QdrantURL, cfg.UpstreamTimeout)
	ragSvc := rag.NewService(ragRepo, qdrantClient, inferenceSvc, cfg.DefaultEmbeddingModel, artifactStore, jobsSvc)

	// Worker: extract and embed job handlers dispatch through the shared
	// executor — leased fenced claims only, no synchronous provider path.
	jobsRunner := jobs.NewRunner(jobsSvc, artifactStore, map[string]jobs.HandlerFunc{
		extract.OperationProcess: extractSvc.JobHandler(),
		rag.OperationEmbed:       ragSvc.JobHandler(),
	}, jobs.RunnerConfig{})
	runnerCtx, stopRunner := context.WithCancel(context.Background())
	runnerDone := make(chan struct{})
	go func() {
		defer close(runnerDone)
		if err := jobsRunner.Run(runnerCtx); err != nil {
			log.Printf("jobs runner error: %v", err)
		}
	}()

	chatbotSvc := chatbot.NewService(chatbotRepo, promptRepo, ragSvc, inferenceSvc)
	promptSvc := prompts.NewService(promptRepo, chatbotSvc)
	connectorSvc := connectors.NewService(sourceRepo, jobsSvc, jobsRepoPG, ragSvc)

	// Browser dashboard login runs on the server-session handlers from 01-12;
	// the configured slug resolves per request so `enclava bootstrap` order is free.
	sessionAuth := httpx.NewSessionAuth(authSvc, auth.NewOIDCService(identityRepo, auth.EnvSecretResolver), cfg.SessionTTL, httpx.DashboardTemplates(), cfg.SessionTenantSlug)

	app := httpx.NewApp(
		authSvc,
		provider,
		cfg.Provider,
		prober,
		identityRepo,
		extractSvc,
		ragSvc,
		promptSvc,
		chatbotSvc,
		connectorSvc,
		billingSvc,
		billingRepo,
		endpointsRepo,
		cfg.APIKeyHashAlgo,
		cfg.APIKeyPepper,
		cfg.APIKeyPrefix,
		cfg.CORSAllowedOrigins,
		sessionAuth,
		cfg.MaxUploadSizeMB,
		cfg.TrustProxyHeaders,
		inferenceSvc,
		bindingsRepo,
		httpx.GateConfig{
			MaxInflight:  cfg.InferenceMaxInflight,
			MaxQueued:    cfg.InferenceMaxHTTPQueue,
			MaxBodyBytes: cfg.InferenceMaxBodyBytes,
		},
		sourceRepo,
		artifactRepo,
	)

	// The extract module mounts its API routes on the app's mux — every
	// processing call it makes funnels through the shared executor.
	extractmodule.New(extractSvc, cfg.MaxUploadSizeMB).Register(app.Mux(), app)

	srv := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           app.Handler(),
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	go func() {
		log.Printf("enclava-go listening on %s", cfg.Addr())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	// Drain order: runner first (releases fenced leases / requeues), then HTTP.
	stopRunner()
	<-runnerDone
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
		if closeErr := srv.Close(); closeErr != nil {
			log.Printf("forced server close failed: %v", closeErr)
		}
	}
	log.Printf("shutdown complete")
}

// runtimeProviderConfig translates the env provider settings into the
// canonical endpoint record seeded at startup. Assurance claims stay empty —
// env config never implies verification. The vision capability is advertised
// because extraction payload parts route as vision through the executor.
func runtimeProviderConfig(cfg config.Config) inference.Endpoint {
	endpoint := inference.Endpoint{
		ID:           cfg.Provider,
		Protocol:     cfg.Provider,
		Capabilities: []string{inference.CapabilityChat, inference.CapabilityEmbeddings, inference.CapabilityVision, inference.CapabilityResponses, inference.CapabilityCompact},
		Enabled:      true,
	}
	switch cfg.Provider {
	case "privatemode":
		endpoint.BaseOrigin = cfg.UpstreamOpenAIBaseURL
	case "openai_compatible":
		endpoint.BaseOrigin = cfg.OpenAICompatibleBaseURL
	case "redpill":
		endpoint.BaseOrigin = cfg.RedPillBaseURL
	case "tinfoil":
		endpoint.BaseOrigin = "https://inference.tinfoil.sh"
		if cfg.TinfoilEnclave != "" {
			endpoint.BaseOrigin = "https://" + cfg.TinfoilEnclave
		}
	}
	// Env-configured private origins (http) must be explicitly authorized.
	if strings.HasPrefix(strings.ToLower(endpoint.BaseOrigin), "http://") {
		endpoint.AuthorizedPrivate = true
	}
	return endpoint
}

// envCredentialName names the operator-owned environment variable that holds
// the env provider's API key — trusted configuration, never a request or
// stored credential_ref value.
func envCredentialName(cfg config.Config) string {
	switch cfg.Provider {
	case "privatemode", "openai":
		return "PRIVATEMODE_API_KEY"
	case "openai_compatible":
		if os.Getenv("OPENAI_COMPATIBLE_API_KEY") != "" {
			return "OPENAI_COMPATIBLE_API_KEY"
		}
		return "PRIVATEMODE_API_KEY"
	case "redpill":
		return "REDPILL_API_KEY"
	case "tinfoil":
		return "TINFOIL_API_KEY"
	}
	return ""
}

// ensureEnvEndpoint create-or-gets the env provider's endpoint row on the
// bootstrap tenant so transports/executor see a canonical record. Model
// aliases discovered from the provider become "model:" capability bindings;
// when the operator set the provider's environment key, an equivalent
// inference-purpose binding is returned for the resolver (an operator
// SECRET_BINDINGS entry for the same ref wins instead).
func ensureEnvEndpoint(ctx context.Context, identity *postgres.IdentityRepo, endpoints *postgres.EndpointsRepo, cfg config.Config, provider llm.Provider) (*auth.SecretBinding, error) {
	tenantID, err := identity.GetTenantBySlug(ctx, "bootstrap")
	if err != nil {
		if !errors.Is(err, auth.ErrNotFound) {
			return nil, fmt.Errorf("lookup bootstrap tenant: %w", err)
		}
		tenantID, err = identity.CreateTenant(ctx, "bootstrap", "Bootstrap")
		if err != nil {
			return nil, fmt.Errorf("create bootstrap tenant: %w", err)
		}
	}
	seed := runtimeProviderConfig(cfg)
	seed.TenantID = tenantID

	mctx, cancel := context.WithTimeout(ctx, cfg.ProviderProbeTimeout)
	defer cancel()
	if models, err := provider.ListModels(mctx); err == nil {
		for _, m := range models.Data {
			if strings.TrimSpace(m.ID) != "" {
				seed.Capabilities = append(seed.Capabilities, inference.ModelCapabilityPrefix+m.ID)
			}
		}
	} else {
		log.Printf("model discovery for env endpoint failed: %v", err)
	}

	var binding *auth.SecretBinding
	refID := cfg.Provider + "_env"
	if envName := envCredentialName(cfg); envName != "" && os.Getenv(envName) != "" {
		if origin, err := auth.CanonicalizeSecretOrigin(seed.BaseOrigin); err == nil {
			seed.CredentialRef = refID
			binding = &auth.SecretBinding{
				RefID: refID, TenantID: tenantID, EnvName: envName,
				Purpose: auth.SecretPurposeInference, Origins: []string{origin},
			}
		}
	}
	if _, err := endpoints.Get(ctx, tenantID, seed.ID); err == nil {
		return binding, nil
	} else if !errors.Is(err, inference.ErrEndpointNotFound) {
		return nil, fmt.Errorf("lookup env endpoint: %w", err)
	}
	if err := endpoints.Create(ctx, seed); err != nil {
		return nil, fmt.Errorf("seed env endpoint: %w", err)
	}
	return binding, nil
}

func buildProvider(cfg config.Config) (llm.Provider, error) {
	switch cfg.Provider {
	case "privatemode", "openai":
		return llm.NewModelCacheProvider(envAdapter(
			cfg.UpstreamOpenAIBaseURL,
			cfg.UpstreamOpenAIAPIKey,
			cfg.UpstreamTimeout, cfg.Provider), cfg.ProviderModelCacheTTL, cfg.Provider), nil
	case "openai_compatible":
		return llm.NewModelCacheProvider(envAdapter(
			cfg.OpenAICompatibleBaseURL,
			cfg.OpenAICompatibleAPIKey,
			cfg.UpstreamTimeout, cfg.Provider), cfg.ProviderModelCacheTTL, cfg.Provider), nil
	case "redpill":
		return llm.NewModelCacheProvider(envAdapter(
			cfg.RedPillBaseURL,
			cfg.RedPillAPIKey,
			cfg.UpstreamTimeout, cfg.Provider), cfg.ProviderModelCacheTTL, cfg.Provider), nil
	case "tinfoil":
		provider, err := llm.NewTinfoilProvider(
			cfg.TinfoilAPIKey,
			cfg.TinfoilEnclave,
			cfg.TinfoilRepo,
			cfg.UpstreamTimeout,
			cfg.TinfoilVerifyAtStart,
			cfg.TinfoilVerifyPerCall,
			cfg.TinfoilVerifyMaxAge,
		)
		if err != nil {
			return nil, err
		}
		if attested, ok := any(provider).(llm.AttestationProvider); ok {
			status := attested.AttestationStatus()
			if status.Verified {
				log.Printf("tinfoil attestation verified: enclave=%s repo=%s digest=%s", status.Enclave, status.Repo, status.Digest)
			}
		}
		return llm.NewModelCacheProvider(provider, cfg.ProviderModelCacheTTL, cfg.Provider), nil
	default:
		return nil, fmt.Errorf("unsupported INFERENCE_PROVIDER %q", cfg.Provider)
	}
}

// envAdapter builds the env provider's transport. A cleartext (http://)
// origin mirrors runtimeProviderConfig's AuthorizedPrivate marking: the
// operator explicitly configured this origin, so the adapter is approved
// for the same private target the seeded endpoint record carries.
func envAdapter(baseURL, apiKey string, timeout time.Duration, provider string) *llm.OpenAICompatibleAdapter {
	a := llm.NewOpenAICompatibleAdapter(baseURL, apiKey, timeout).WithProviderName(provider)
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(baseURL)), "http://") {
		a.WithAllowPrivateEndpoint()
	}
	return a
}
