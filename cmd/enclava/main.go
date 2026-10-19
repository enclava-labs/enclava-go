package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/config"
	"enclava-go/internal/connectors"
	"enclava-go/internal/extract"
	"enclava-go/internal/httpx"
	"enclava-go/internal/llm"
	extractmodule "enclava-go/internal/modules/extract"
	"enclava-go/internal/prompts"
	"enclava-go/internal/rag"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/store/sqlite"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "migrate-import" {
		if err := runMigrateImport(context.Background(), os.Args[2:]); err != nil {
			log.Fatalf("migrate-import error: %v", err)
		}
		return
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pgDB, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("postgres init error: %v", err)
	}
	defer func() {
		if err := pgDB.Close(); err != nil {
			log.Printf("postgres close failed: %v", err)
		}
	}()
	identityRepo := postgres.NewIdentityRepo(pgDB)

	// Legacy SQLite store keeps unported services running until their port
	// plans land (01-22 removes the whole open).
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("data dir init error: %v", err)
	}
	db, err := sqlite.Open(ctx, cfg.DataDir+"/enclava-legacy.db")
	if err != nil {
		log.Fatalf("database init error: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Printf("database close failed: %v", err)
		}
	}()

	apiKeysRepo := sqlite.NewAPIKeysRepo(db.SQL)
	templatesRepo := sqlite.NewExtractTemplatesRepo(db.SQL)
	settingsRepo := sqlite.NewExtractSettingsRepo(db.SQL)
	jobsRepo := sqlite.NewExtractJobsRepo(db.SQL)
	usageRepo := sqlite.NewUsageRepo(db.SQL)
	ragRepo := sqlite.NewRAGRepo(db.SQL)
	promptRepo := sqlite.NewPromptTemplatesRepo(db.SQL)
	chatbotRepo := sqlite.NewChatbotRepo(db.SQL)
	pricingRepo := sqlite.NewPricingRepo(db.SQL)
	budgetsRepo := sqlite.NewBudgetsRepo(db.SQL)
	billingAuditRepo := sqlite.NewBillingAuditRepo(db.SQL)
	providersRepo := sqlite.NewProvidersRepo(db.SQL)
	connectorsRepo := sqlite.NewConnectorsRepo(db.SQL)

	if err := settingsRepo.EnsureDefaultRow(ctx, cfg.MaxUploadSizeMB); err != nil {
		log.Fatalf("settings init error: %v", err)
	}
	if err := templatesRepo.EnsureDefaults(ctx, extract.DefaultTemplates()); err != nil {
		log.Fatalf("template seed error: %v", err)
	}
	if err := promptRepo.EnsureDefaults(ctx, prompts.DefaultTemplates()); err != nil {
		log.Fatalf("prompt template seed error: %v", err)
	}
	if err := providersRepo.EnsureDefault(ctx, runtimeProviderConfig(cfg)); err != nil {
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
		templatesRepo,
		settingsRepo,
		jobsRepo,
		provider,
		cfg.MaxUploadSizeMB,
	)
	qdrantClient := rag.NewQdrantClient(cfg.QdrantURL, cfg.UpstreamTimeout)
	ragSvc := rag.NewService(ragRepo, qdrantClient, provider, cfg.DefaultEmbeddingModel)
	promptSvc := prompts.NewService(promptRepo)
	chatbotSvc := chatbot.NewService(chatbotRepo, promptRepo, ragSvc, provider, usageRepo)
	connectorSvc := connectors.NewService(connectorsRepo, ragSvc)
	connectorSvc.StartScheduler(ctx, cfg.ConnectorSyncInterval)

	app := httpx.NewApp(
		authSvc,
		provider,
		cfg.Provider,
		prober,
		apiKeysRepo,
		jobsRepo,
		templatesRepo,
		settingsRepo,
		extractSvc,
		ragSvc,
		promptSvc,
		chatbotSvc,
		connectorSvc,
		usageRepo,
		pricingRepo,
		budgetsRepo,
		billingAuditRepo,
		providersRepo,
		cfg.APIKeyHashAlgo,
		cfg.APIKeyPepper,
		cfg.APIKeyPrefix,
		cfg.CORSAllowedOrigins,
		cfg.AdminEmail,
		cfg.AdminPassword,
		cfg.MaxUploadSizeMB,
		cfg.TrustProxyHeaders,
	)
	ragSvc.SetSpendGuard(app)

	extractMod := extractmodule.New(extractSvc, cfg.MaxUploadSizeMB)
	extractMod.Register(app.Mux(), app)

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

func runtimeProviderConfig(cfg config.Config) sqlite.ProviderConfig {
	provider := sqlite.ProviderConfig{
		ID:            cfg.Provider,
		Type:          cfg.Provider,
		Name:          cfg.Provider,
		CredentialRef: cfg.Provider + "_env",
		Config:        map[string]any{},
		IsEnabled:     true,
		IsDefault:     true,
	}
	switch cfg.Provider {
	case "privatemode":
		provider.Name = "PrivateMode"
		provider.BaseURL = cfg.UpstreamOpenAIBaseURL
	case "openai_compatible":
		provider.Name = "OpenAI Compatible"
		provider.BaseURL = cfg.OpenAICompatibleBaseURL
	case "redpill":
		provider.Name = "RedPill"
		provider.BaseURL = cfg.RedPillBaseURL
	case "tinfoil":
		provider.Name = "Tinfoil"
		provider.BaseURL = "https://inference.tinfoil.sh/v1"
		if cfg.TinfoilEnclave != "" {
			provider.BaseURL = "https://" + cfg.TinfoilEnclave + "/v1"
			provider.Config["enclave"] = cfg.TinfoilEnclave
			provider.Config["repo"] = cfg.TinfoilRepo
		}
		provider.Config["verify_at_start"] = cfg.TinfoilVerifyAtStart
		provider.Config["verify_per_call"] = cfg.TinfoilVerifyPerCall
		provider.Config["verify_max_age"] = cfg.TinfoilVerifyMaxAge.String()
	}
	return provider
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
