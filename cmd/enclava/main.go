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
	"enclava-go/internal/prompts"
	"enclava-go/internal/rag"
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
	if err := ensureEnvEndpoint(ctx, identityRepo, endpointsRepo, runtimeProviderConfig(cfg)); err != nil {
		log.Fatalf("provider config seed error: %v", err)
	}

	authSvc := auth.NewService(identityRepo, cfg.APIKeyPepper)
	provider, err := buildProvider(cfg)
	if err != nil {
		log.Fatalf("provider init error: %v", err)
	}
	prober := llm.NewProber(provider, cfg.ProviderProbeInterval, cfg.ProviderProbeTimeout)
	go prober.Start(ctx)

	extractSvc := extract.NewService(
		extractRepo,
		extractRepo,
		jobsRepoPG,
		jobsSvc,
		artifactStore,
		billingSvc,
		provider,
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

	// Infrastructure-only runner: no operation handlers are registered until
	// the executor plans (01-25+) mount them; it exists to drain fenced
	// claims cleanly at shutdown.
	jobsRunner := jobs.NewRunner(jobsSvc, artifactStore, map[string]jobs.HandlerFunc{}, jobs.RunnerConfig{})
	runnerCtx, stopRunner := context.WithCancel(context.Background())
	runnerDone := make(chan struct{})
	go func() {
		defer close(runnerDone)
		if err := jobsRunner.Run(runnerCtx); err != nil {
			log.Printf("jobs runner error: %v", err)
		}
	}()

	qdrantClient := rag.NewQdrantClient(cfg.QdrantURL, cfg.UpstreamTimeout)
	ragSvc := rag.NewService(ragRepo, qdrantClient, provider, cfg.DefaultEmbeddingModel, artifactStore, jobsSvc, billingSvc)
	chatbotSvc := chatbot.NewService(chatbotRepo, promptRepo, ragSvc, provider, nil)
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
		nil, // usageRepo: every route that consumed it is unmounted (removed in 01-36)
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
	)

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
// env config never implies verification.
func runtimeProviderConfig(cfg config.Config) inference.Endpoint {
	endpoint := inference.Endpoint{
		ID:            cfg.Provider,
		Protocol:      cfg.Provider,
		Capabilities:  []string{"chat/completions", "embeddings"},
		CredentialRef: cfg.Provider + "_env",
		Enabled:       true,
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

// ensureEnvEndpoint create-or-gets the env provider's endpoint row on the
// bootstrap tenant so transports/executor see a canonical record.
func ensureEnvEndpoint(ctx context.Context, identity *postgres.IdentityRepo, endpoints *postgres.EndpointsRepo, seed inference.Endpoint) error {
	tenantID, err := identity.GetTenantBySlug(ctx, "bootstrap")
	if err != nil {
		if !errors.Is(err, auth.ErrNotFound) {
			return fmt.Errorf("lookup bootstrap tenant: %w", err)
		}
		tenantID, err = identity.CreateTenant(ctx, "bootstrap", "Bootstrap")
		if err != nil {
			return fmt.Errorf("create bootstrap tenant: %w", err)
		}
	}
	seed.TenantID = tenantID
	if _, err := endpoints.Get(ctx, tenantID, seed.ID); err == nil {
		return nil
	} else if !errors.Is(err, inference.ErrEndpointNotFound) {
		return fmt.Errorf("lookup env endpoint: %w", err)
	}
	if err := endpoints.Create(ctx, seed); err != nil {
		return fmt.Errorf("seed env endpoint: %w", err)
	}
	return nil
}

func buildProvider(cfg config.Config) (llm.Provider, error) {
	switch cfg.Provider {
	case "privatemode", "openai":
		return llm.NewModelCacheProvider(llm.NewOpenAICompatibleAdapter(
			cfg.UpstreamOpenAIBaseURL,
			cfg.UpstreamOpenAIAPIKey,
			cfg.UpstreamTimeout,
		).WithProviderName(cfg.Provider), cfg.ProviderModelCacheTTL, cfg.Provider), nil
	case "openai_compatible":
		return llm.NewModelCacheProvider(llm.NewOpenAICompatibleAdapter(
			cfg.OpenAICompatibleBaseURL,
			cfg.OpenAICompatibleAPIKey,
			cfg.UpstreamTimeout,
		).WithProviderName(cfg.Provider), cfg.ProviderModelCacheTTL, cfg.Provider), nil
	case "redpill":
		return llm.NewModelCacheProvider(llm.NewOpenAICompatibleAdapter(
			cfg.RedPillBaseURL,
			cfg.RedPillAPIKey,
			cfg.UpstreamTimeout,
		).WithProviderName(cfg.Provider), cfg.ProviderModelCacheTTL, cfg.Provider), nil
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
