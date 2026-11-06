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
	"sync"
	"syscall"
	"time"
	// Budget windows use IANA zones; the alpine runtime image ships no zoneinfo.
	_ "time/tzdata"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/config"
	"enclava-go/internal/connectors"
	"enclava-go/internal/credentials"
	"enclava-go/internal/extract"
	"enclava-go/internal/httpx"
	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
	"enclava-go/internal/llm"
	"enclava-go/internal/mcp"
	extractmodule "enclava-go/internal/modules/extract"
	"enclava-go/internal/prompts"
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run dispatches argv so tests can exercise it. Help paths return before any
// configuration loading (OPS-I01): `enclava --help|-h|help` prints usage and
// `enclava bootstrap -h` surfaces flag.ErrHelp as success.
func run(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			printUsage()
			return 0
		case "bootstrap":
			if err := runBootstrap(context.Background(), args[1:]); err != nil {
				log.Printf("bootstrap error: %v", err)
				return 1
			}
			return 0
		}
	}
	if err := serve(); err != nil {
		log.Printf("%v", err)
		return 1
	}
	return 0
}

func printUsage() {
	fmt.Fprint(os.Stderr, `Usage: enclava [command]

Commands:
  (none)      start the server
  bootstrap   create the first tenant and admin user

Options:
  -h, --help  show this help
`)
}

func serve() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config error: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// CORE-02/CORE-05: the ONLY database open is PostgreSQL — there is no
	// SQLite path anywhere in the composition root.
	pgDB, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("postgres init error: %w", err)
	}
	defer func() {
		if err := pgDB.Close(); err != nil {
			log.Printf("postgres close failed: %v", err)
		}
	}()

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("data dir init error: %w", err)
	}

	identityRepo := postgres.NewIdentityRepo(pgDB)
	promptRepo := postgres.NewPromptsRepo(pgDB)
	chatbotRepo := postgres.NewChatbotRepo(pgDB)
	billingRepo := postgres.NewBillingRepo(pgDB)
	endpointsRepo := postgres.NewEndpointsRepo(pgDB)
	extractRepo := postgres.NewExtractRepo(pgDB)
	jobsRepoPG := postgres.NewJobsRepo(pgDB)
	artifactRepo := postgres.NewArtifactRepo(pgDB)
	chunkProfilesRepo := postgres.NewChunkProfilesRepo(pgDB)
	ragRepo := postgres.NewRAGRepo(pgDB)
	bindingsRepo := postgres.NewResponseBindingsRepo(pgDB)
	mcpRepo := postgres.NewMCPToolsRepo(pgDB)
	sourceRepo := postgres.NewSourceRepo(pgDB)
	auditRepo := postgres.NewSecurityAuditRepo(pgDB)

	// Durable artifacts under <data>/artifacts, scratch under <data>/scratch —
	// sibling roots, since the store rejects nested disjoint roots.
	artifactStore, err := artifacts.NewStore(cfg.DataDir+"/artifacts", cfg.DataDir+"/scratch", artifactRepo)
	if err != nil {
		return fmt.Errorf("artifact store init error: %w", err)
	}
	// D-10: reconcile crash leftovers before serving; a reconcile failure
	// fails startup rather than serving over a suspect durable root.
	if err := artifactStore.Reconcile(ctx); err != nil {
		return fmt.Errorf("artifact reconcile error: %w", err)
	}
	native := extract.ProbeNative()
	log.Printf("native document processing capabilities: PDF=%t OCR=%t languages=%v reason=%s", native.PDF, native.OCR, native.Languages, native.Reason)

	jobsSvc := jobs.NewService(jobsRepoPG, 0)
	billingSvc := billing.NewService(billingRepo)

	provider, err := buildProvider(cfg)
	if err != nil {
		return fmt.Errorf("provider init error: %w", err)
	}
	prober := llm.NewProber(provider, cfg.ProviderProbeInterval, cfg.ProviderProbeTimeout)
	go prober.Start(ctx)

	// The env-configured provider is seeded as a canonical endpoint record:
	// model aliases discovered from the provider, vision capability for
	// extraction, and a credential_ref backed by the operator bindings only.
	envBinding, err := ensureEnvEndpoint(ctx, identityRepo, endpointsRepo, cfg, provider)
	if err != nil {
		return fmt.Errorf("provider config seed error: %w", err)
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
		return fmt.Errorf("secret binding error: %w", err)
	}
	mcpTransport := &mcp.StreamableHTTP{Credential: secretResolver, ClientName: "enclava", ClientVersion: "1"}
	mcpExecutor := &mcp.Executor{Repository: mcpRepo, Caller: chatbot.MCPCaller{}, Transport: mcpTransport}
	admission := inference.NewAdmission(inference.AdmissionConfig{
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
	})
	inferenceSvc, err := inference.NewService(inference.Deps{
		Endpoints:     endpointsRepo,
		Sources:       sourceRepo,
		Authorizer:    source.NewAuthorizer(sourceRepo),
		Billing:       billingSvc,
		Secrets:       secretResolver,
		Artifacts:     artifactRepo,
		Conversations: postgres.NewChatbotConversationStore(chatbotRepo),
		ModelSettings: postgres.NewModelSettingsRepo(pgDB),
	}, inference.WithAdmission(admission), inference.WithDefaultMaxOutputTokens(cfg.InferenceDefaultMaxOutputTokens))
	if err != nil {
		return fmt.Errorf("inference service init error: %w", err)
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
	extractSvc.SetSourceRepository(sourceRepo)
	extractSvc.SetDefaultMaxOutputTokens(cfg.ExtractMaxOutputTokens)
	extractSvc.SetDefaultModelResolver(inferenceSvc.DefaultModel)
	if err := extractSvc.ReconcileDeletedJobs(ctx); err != nil {
		return fmt.Errorf("extract deletion recovery error: %w", err)
	}
	// Seed the bootstrap tenant's default extract templates and settings row
	// through the owned service.
	bootstrapTenantID, err := identityRepo.GetTenantBySlug(ctx, "bootstrap")
	if err != nil {
		return fmt.Errorf("bootstrap tenant lookup error: %w", err)
	}
	bootstrapPrincipal := auth.Principal{TenantID: bootstrapTenantID, ActorID: "bootstrap"}
	if err := extractSvc.SeedDefaults(ctx, bootstrapPrincipal); err != nil {
		return fmt.Errorf("extract template seed error: %w", err)
	}
	if _, err := extractSvc.GetSettings(ctx, bootstrapPrincipal); err != nil {
		return fmt.Errorf("extract settings seed error: %w", err)
	}

	qdrantClient := rag.NewQdrantClient(cfg.QdrantURL, cfg.UpstreamTimeout)
	ragSvc := rag.NewService(ragRepo, qdrantClient, inferenceSvc, cfg.DefaultEmbeddingModel, artifactStore, jobsSvc)
	ragSvc.SetDefaultEmbeddingDimensions(cfg.DefaultEmbeddingDimensions)
	ragSvc.SetUtilityModel(cfg.UtilityModel)
	ragSvc.SetDefaultModelResolver(inferenceSvc.DefaultModel)
	ragSvc.SetIndexRetirementSoak(cfg.IndexRetirementSoak)
	previewSvc := extract.NewPreviewService(chunkProfilesRepo, jobsRepoPG, jobsSvc, artifactStore, sourceRepo, cfg.MaxUploadSizeMB)
	// Dashboard-managed credentials first, SECRET_BINDINGS as the fallback.
	credentialSvc, err := credentials.NewService(postgres.NewCredentialsRepo(pgDB), cfg.SecretsKey, secretResolver, cfg.PublicBaseURL)
	if err != nil {
		return err
	}
	connectorSvc := connectors.NewService(sourceRepo, jobsSvc, jobsRepoPG)
	connectorSvc.SetCredentialResolver(credentialSvc)
	connectorSvc.SetPrincipalResolver(authSvc.PrincipalForActor)
	connectorSvc.SetPointPurger(ragSvc.PurgeConnectionPoints)
	adapterFactories := connectors.AdapterFactories{
		connectors.SourceKindGitHub: func(c source.Connection) (connectors.Adapter, error) {
			return connectors.NewGitHubAdapter(c, credentialSvc)
		},
		connectors.SourceKindSlack: func(c source.Connection) (connectors.Adapter, error) {
			return connectors.NewSlackAdapter(c, credentialSvc)
		},
		connectors.SourceKindTeams: func(c source.Connection) (connectors.Adapter, error) {
			return connectors.NewTeamsAdapter(c, credentialSvc)
		},
		connectors.SourceKindNotion: func(c source.Connection) (connectors.Adapter, error) {
			return connectors.NewNotionAdapter(c, credentialSvc)
		},
		connectors.SourceKindGoogleDrive: func(source.Connection) (connectors.Adapter, error) {
			return connectors.NewGDriveAdapter(connectorSvc), nil
		},
	}
	connectorSvc.SetSyncDependencies(adapterFactories, postgres.NewConnectorObservationsRepo(pgDB), artifactStore, sourceRepo)

	chatbotSvc := chatbot.NewService(chatbotRepo, promptRepo, ragSvc, inferenceSvc)
	chatbotSvc.ConfigureRuns(postgres.NewChatbotRunsRepo(pgDB), jobsSvc, extractSvc, authSvc)
	chatbotSvc.ConfigureMCP(chatbot.MCPRunTools{Executor: mcpExecutor, Authorizer: inferenceSvc})

	// Workers: one durable queue, two lanes with their own capacity, so bulk
	// connector work (syncs and their ingest/embed fan-out, access
	// projections, re-embed scans) never starves uploads, extraction,
	// previews or agent runs.
	jobHandlers := map[string]jobs.HandlerFunc{
		extract.OperationProcess:    extractSvc.JobHandler(),
		extract.OperationVision:     extractSvc.JobHandler(),
		extract.OperationPreview:    previewSvc.JobHandler(),
		rag.OperationEmbed:          ragSvc.JobHandler(),
		rag.OperationIngest:         ragSvc.IngestJobHandler(),
		rag.OperationAccessBackfill: ragSvc.AccessBackfillJobHandler(),
		rag.OperationReembedScan:    ragSvc.ReembedScanJobHandler(),
		connectors.OperationSync:    connectorSvc.JobHandler(),
		chatbot.RunOperation:        chatbotSvc.JobHandler(),
	}
	runners := []*jobs.Runner{
		jobs.NewRunner(jobsSvc, artifactStore, jobHandlers, jobs.RunnerConfig{JobTimeout: cfg.JobTimeout, Lane: jobs.LaneInteractive}),
		// Syncs are few and long: two slots, so one big source never blocks another.
		jobs.NewRunner(jobsSvc, artifactStore, jobHandlers, jobs.RunnerConfig{JobTimeout: cfg.JobTimeout, Lane: jobs.LaneSync, Concurrency: 2}),
		jobs.NewRunner(jobsSvc, artifactStore, jobHandlers, jobs.RunnerConfig{JobTimeout: cfg.JobTimeout, Lane: jobs.LaneBackground}),
	}
	runnerCtx, stopRunner := context.WithCancel(context.Background())
	go previewSvc.RunRetention(runnerCtx)
	runnerDone := make(chan struct{})
	go func() {
		defer close(runnerDone)
		var wg sync.WaitGroup
		for _, runner := range runners {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := runner.Run(runnerCtx); err != nil {
					log.Printf("jobs runner error: %v", err)
				}
			}()
		}
		wg.Wait()
	}()

	go ragSvc.RunAccessBackfillLoop(runnerCtx, time.Minute)

	// Scheduled connector syncs: per-source sync_interval (default 1h, min
	// 5m, "off" disables) re-enqueues connector.sync for due sources.
	schedulerDone := make(chan struct{})
	go func() {
		defer close(schedulerDone)
		connectors.NewScheduler(connectorSvc).Run(runnerCtx)
	}()

	// Stale holds: a crash between reserve and settle would otherwise keep a
	// held reservation counting against budgets forever. An hour outlives
	// every legitimate attempt (stream header 5m + wall 30m, unary 5m).
	sweeperDone := make(chan struct{})
	go func() {
		defer close(sweeperDone)
		tick := time.NewTicker(5 * time.Minute)
		defer tick.Stop()
		for {
			released, unknown, err := billingSvc.SweepStaleHolds(runnerCtx, time.Now().Add(-time.Hour))
			if err != nil && runnerCtx.Err() == nil {
				log.Printf("stale hold sweep failed: %v", err)
			} else if released+unknown > 0 {
				log.Printf("stale hold sweep: %d released (never dispatched), %d marked unknown", released, unknown)
			}
			select {
			case <-runnerCtx.Done():
				return
			case <-tick.C:
			}
		}
	}()

	// Bulk job history (syncs and their ingest/embed fan-out) is trimmed after
	// JOB_RETENTION; user-facing jobs (chat runs, extractions) are kept.
	retentionDone := make(chan struct{})
	go func() {
		defer close(retentionDone)
		bulkOps := []string{rag.OperationIngest, rag.OperationEmbed, rag.OperationAccessBackfill, rag.OperationReembedScan, connectors.OperationSync, "rag.access"}
		tick := time.NewTicker(time.Hour)
		defer tick.Stop()
		for {
			purged, err := jobsSvc.PurgeFinished(runnerCtx, cfg.JobRetention, bulkOps)
			if err != nil && runnerCtx.Err() == nil {
				log.Printf("job retention sweep failed: %v", err)
			} else if purged > 0 {
				log.Printf("job retention sweep: %d finished jobs removed", purged)
			}
			select {
			case <-runnerCtx.Done():
				return
			case <-tick.C:
			}
		}
	}()

	promptSvc := prompts.NewService(promptRepo, chatbotSvc)

	// Browser dashboard login runs on the server-session handlers from 01-12;
	// the configured slug resolves per request so `enclava bootstrap` order is free.
	sessionAuth := httpx.NewSessionAuth(authSvc, auth.NewOIDCService(identityRepo, auth.EnvSecretResolver), cfg.SessionTTL, httpx.DashboardAuthTemplates(), cfg.SessionTenantSlug)
	sessionAuth.SetSecurityAudit(auditRepo)
	sessionAuth.SetTrustProxy(cfg.TrustProxyHeaders)

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
	app.ConfigureMCP(mcpRepo, mcpTransport)
	app.ConfigureActivity(jobsSvc)
	app.ConfigureCredentials(credentialSvc)
	app.SetSecurityAudit(auditRepo)
	app.ConfigureVectorStoreProbe(httpx.ProbeHTTP(cfg.QdrantURL + "/collections"))
	app.ConfigureMetrics(cfg.MetricsToken, cfg.MetricsAuthDisabled, jobsRepoPG, admission)

	// The extract module mounts its API routes on the app's mux — every
	// processing call it makes funnels through the shared executor.
	extractmodule.New(extractSvc, previewSvc, cfg.MaxUploadSizeMB).Register(app.Mux(), app)

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
	<-schedulerDone
	<-sweeperDone
	<-retentionDone
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
		if closeErr := srv.Close(); closeErr != nil {
			log.Printf("forced server close failed: %v", closeErr)
		}
	}
	log.Printf("shutdown complete")
	return nil
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
		endpoint.BaseOrigin = envOrigin(cfg.UpstreamOpenAIBaseURL)
	case "openai_compatible":
		endpoint.BaseOrigin = envOrigin(cfg.OpenAICompatibleBaseURL)
	case "redpill":
		endpoint.BaseOrigin = envOrigin(cfg.RedPillBaseURL)
	case "tinfoil":
		endpoint.BaseOrigin = "https://inference.tinfoil.sh"
		if cfg.TinfoilEnclave != "" {
			endpoint.BaseOrigin = "https://" + cfg.TinfoilEnclave
			endpoint.EnclaveRepo = cfg.TinfoilRepo
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

// envOrigin reduces a configured provider URL to the origin endpoint records
// store: both "https://host" and the documented "https://host/v1" forms give
// "https://host", and every transport re-adds /v1 (inference.defaultTransport).
func envOrigin(raw string) string {
	return strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(raw), "/"), "/v1")
}

// envAdapter builds the env provider's transport under the same /v1 contract
// as the seeded endpoint record. A cleartext (http://) origin mirrors
// runtimeProviderConfig's AuthorizedPrivate marking: the operator explicitly
// configured this origin, so the adapter is approved for the same private
// target the seeded endpoint record carries.
func envAdapter(baseURL, apiKey string, timeout time.Duration, provider string) *llm.OpenAICompatibleAdapter {
	a := llm.NewOpenAICompatibleAdapter(envOrigin(baseURL)+"/v1", apiKey, timeout).WithProviderName(provider)
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(baseURL)), "http://") {
		a.WithAllowPrivateEndpoint()
	}
	return a
}
