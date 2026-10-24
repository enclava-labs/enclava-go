package httpx

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/connectors"
	"enclava-go/internal/inference"
	"enclava-go/internal/llm"
	"enclava-go/internal/prompts"
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
)

type dashboardResourcePageData struct {
	dashboardPageCommon
	Heading     string
	Description string
	PartialPath string
	MountID     string
	LoadingText string
}

type dashboardProvidersData struct {
	Snapshot       llm.ReadinessSnapshot
	Models         []llm.Model
	Attestation    any
	Endpoints      []inference.Endpoint
	CredentialRefs []auth.SecretRefMeta
	PriceRows      []postgres.PriceRow
	CanManage      bool
	TenantID       string
	Error          string
}

// Billing view rows carry the canonical billing types — the sections read
// billing_prices/billing_price_audit/billing_audit via a.billingRepo.
type dashboardProviderPricing struct {
	ID            string
	SubjectType   string
	SubjectID     string
	Capability    string
	Unit          string
	RateMicros    int64
	UnitQuantity  int64
	Currency      string
	EffectiveFrom time.Time
	CreatedAt     time.Time
}

type dashboardPricingAudit struct {
	ID        string
	PriceID   string
	Action    string
	ActorType string
	ActorID   string
	Detail    map[string]any
	CreatedAt time.Time
}

type dashboardBillingAudit struct {
	ID          string
	SubjectType string
	SubjectID   string
	Action      string
	ActorType   string
	ActorID     string
	Detail      map[string]any
	CreatedAt   time.Time
}

type dashboardPricingData struct {
	Pricing []dashboardProviderPricing
	Audit   []dashboardPricingAudit
}

type dashboardBillingAuditData struct {
	Audit []dashboardBillingAudit
}

type dashboardRAGData struct {
	Collections []rag.Collection
	Documents   []rag.Document
	Success     string
	Error       string
}

type dashboardChatbotsData struct {
	Chatbots        []chatbot.Instance
	PromptTemplates []prompts.Template
	Collections     []rag.Collection
	Success         string
	Error           string
}

type dashboardPromptTemplatesData struct {
	Templates []prompts.Template
	Success   string
	Error     string
}

type dashboardConnectorsData struct {
	Sources             []connectors.Source
	Jobs                []connectors.SyncJob
	Collections         []rag.Collection
	Bindings            map[string][]source.SourceIdentity
	CredentialRefs      map[string][]auth.SecretRefMeta
	Success             string
	Error               string
	CanManageBindings   bool
	Capabilities        map[string]connectors.AdapterCapabilities
	LatestJobs          map[string]connectors.SyncJob
	Streams             map[string]map[string]connectors.StreamState
	PermissionFreshness map[string]map[string]time.Time
}

func (a *App) handleDashboardPageProviders(w http.ResponseWriter, r *http.Request) {
	a.renderDashboardResourcePage(w, r, "providers", "Providers", "Provider routing and attestation", "/dashboard/partials/providers", "dashboard-providers", "Loading providers...")
}

func (a *App) handleDashboardPagePricing(w http.ResponseWriter, r *http.Request) {
	a.renderDashboardResourcePage(w, r, "pricing", "Pricing", "Provider model rates and pricing audit", "/dashboard/partials/pricing", "dashboard-pricing", "Loading pricing...")
}

func (a *App) handleDashboardPageBillingAudit(w http.ResponseWriter, r *http.Request) {
	a.renderDashboardResourcePage(w, r, "billing-audit", "Billing Audit", "Billing and budget-related audit trail", "/dashboard/partials/billing-audit", "dashboard-billing-audit", "Loading billing audit...")
}

func (a *App) handleDashboardPageRAG(w http.ResponseWriter, r *http.Request) {
	a.renderDashboardResourcePage(w, r, "rag", "RAG", "Collections, embedding models, and document indexes", "/dashboard/partials/rag", "dashboard-rag", "Loading RAG...")
}

func (a *App) handleDashboardPageChatbots(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	if err := dashboardTemplates.ExecuteTemplate(w, "page-agents", a.dashboardPageData(r, "chatbots", "Assistants")); err != nil {
		log.Printf("failed to render assistant dashboard: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_agents_page_failed")
	}
}

func (a *App) handleDashboardPagePromptTemplates(w http.ResponseWriter, r *http.Request) {
	a.renderDashboardResourcePage(w, r, "prompt-templates", "Prompt Templates", "Reusable system and user prompts", "/dashboard/partials/prompt-templates", "dashboard-prompt-templates", "Loading prompt templates...")
}

func (a *App) handleDashboardPageConnectors(w http.ResponseWriter, r *http.Request) {
	a.renderDashboardResourcePage(w, r, "connectors", "Connectors", "First-party source sync and RAG ingestion", "/dashboard/partials/connectors", "dashboard-connectors", "Loading connectors...")
}

func (a *App) handleDashboardPageMCP(w http.ResponseWriter, r *http.Request) {
	if p, ok := auth.PrincipalFromContext(r.Context()); !ok || !auth.HasScope(p, "platform.admin") {
		WriteForbidden(w, "tenant administrator access required")
		return
	}
	a.renderDashboardResourcePage(w, r, "mcp", "MCP Tools", "Manage approved outbound MCP servers and tool schemas", "/dashboard/partials/mcp", "dashboard-mcp", "Loading MCP servers...")
}

func (a *App) handleDashboardMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	if p, ok := auth.PrincipalFromContext(r.Context()); !ok || !auth.HasScope(p, "platform.admin") {
		WriteForbidden(w, "tenant administrator access required")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTemplates.ExecuteTemplate(w, "partial-mcp", nil); err != nil {
		log.Printf("failed to render MCP dashboard partial: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_mcp_partial_failed")
	}
}

func (a *App) renderDashboardResourcePage(w http.ResponseWriter, r *http.Request, activePage, pageTitle, description, partialPath, mountID, loadingText string) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	data := dashboardResourcePageData{
		dashboardPageCommon: a.dashboardPageData(r, activePage, pageTitle),
		Heading:             pageTitle,
		Description:         description,
		PartialPath:         partialPath,
		MountID:             mountID,
		LoadingText:         loadingText,
	}
	if err := dashboardTemplates.ExecuteTemplate(w, "page-resource", data); err != nil {
		log.Printf("failed to render dashboard %s page: %v", activePage, err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_resource_page_render_failed")
	}
}

func (a *App) handleDashboardProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	data := dashboardProvidersData{}
	if principal, ok := auth.PrincipalFromContext(r.Context()); ok {
		data.TenantID = principal.TenantID
		data.CanManage = auth.HasScope(principal, "pricing.manage")
		if a.executor != nil {
			data.CredentialRefs = a.executor.CredentialReferences(principal.TenantID)
		}
	}
	if a.prober != nil {
		data.Snapshot = a.prober.Snapshot()
	}
	if attested, ok := a.provider.(llm.AttestationProvider); ok {
		data.Attestation = attested.AttestationStatus()
	}
	if a.provider != nil {
		models, err := a.provider.ListModels(r.Context())
		if err != nil {
			data.Error = "Provider model list failed"
			log.Printf("failed to load dashboard provider models: %v", err)
		} else {
			data.Models = models.Data
		}
	}
	if a.endpoints != nil {
		tenantID, ok := a.dashboardTenantID(w, r)
		if !ok {
			return
		}
		endpoints, err := a.endpoints.List(r.Context(), tenantID)
		if err != nil {
			data.Error = "Endpoint list failed"
			log.Printf("failed to load dashboard endpoints: %v", err)
		} else {
			data.Endpoints = endpoints
		}
	}
	if a.billingRepo != nil && data.TenantID != "" {
		endpointPrices, endpointErr := a.billingRepo.ListPrices(r.Context(), data.TenantID, "endpoint", "", "", 200, 0)
		modelPrices, modelErr := a.billingRepo.ListPrices(r.Context(), data.TenantID, "model", "", "", 200, 0)
		if endpointErr != nil || modelErr != nil {
			data.Error = "Provider pricing status unavailable"
			log.Printf("failed to load provider pricing status: endpoint=%v model=%v", endpointErr, modelErr)
		} else {
			data.PriceRows = append(endpointPrices, modelPrices...)
		}
	}
	if err := dashboardTemplates.ExecuteTemplate(w, "partial-providers", data); err != nil {
		log.Printf("failed to render dashboard providers: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_providers_render_failed")
	}
}

func (a *App) handleDashboardPricing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	if a.billingRepo == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "pricing is not configured", "server_error", "not_configured")
		return
	}
	tenantID, ok := a.dashboardTenantID(w, r)
	if !ok {
		return
	}
	pricing, err := a.billingRepo.ListPrices(r.Context(), tenantID, "", "", "", 100, 0)
	if err != nil {
		log.Printf("failed to load dashboard pricing: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_pricing_failed")
		return
	}
	audit, err := a.billingRepo.ListPriceAudit(r.Context(), tenantID, 20, 0)
	if err != nil {
		log.Printf("failed to load dashboard pricing audit: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_pricing_audit_failed")
		return
	}
	pricingView := make([]dashboardProviderPricing, 0, len(pricing))
	for _, row := range pricing {
		pricingView = append(pricingView, dashboardProviderPricing{
			ID: row.ID, SubjectType: row.SubjectType, SubjectID: row.SubjectID,
			Capability: row.Capability, Unit: string(row.Unit), RateMicros: row.RateMicros,
			UnitQuantity: row.UnitQuantity, Currency: row.Currency,
			EffectiveFrom: row.EffectiveFrom, CreatedAt: row.CreatedAt,
		})
	}
	auditView := make([]dashboardPricingAudit, 0, len(audit))
	for _, row := range audit {
		auditView = append(auditView, dashboardPricingAudit{
			ID: row.ID, PriceID: row.PriceID, Action: row.Action,
			ActorType: row.ActorType, ActorID: row.ActorID, Detail: row.Detail, CreatedAt: row.CreatedAt,
		})
	}
	if err := dashboardTemplates.ExecuteTemplate(w, "partial-pricing", dashboardPricingData{Pricing: pricingView, Audit: auditView}); err != nil {
		log.Printf("failed to render dashboard pricing: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_pricing_render_failed")
	}
}

func (a *App) handleDashboardBillingAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	if a.billingRepo == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "billing audit is not configured", "server_error", "not_configured")
		return
	}
	tenantID, ok := a.dashboardTenantID(w, r)
	if !ok {
		return
	}
	audit, err := a.billingRepo.ListBillingAudit(r.Context(), tenantID, "", "", 100, 0)
	if err != nil {
		log.Printf("failed to load dashboard billing audit: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_billing_audit_failed")
		return
	}
	auditView := make([]dashboardBillingAudit, 0, len(audit))
	for _, row := range audit {
		auditView = append(auditView, dashboardBillingAudit{
			ID: row.ID, SubjectType: row.SubjectType, SubjectID: row.SubjectID,
			Action: row.Action, ActorType: row.ActorType, ActorID: row.ActorID,
			Detail: row.Detail, CreatedAt: row.CreatedAt,
		})
	}
	if err := dashboardTemplates.ExecuteTemplate(w, "partial-billing-audit", dashboardBillingAuditData{Audit: auditView}); err != nil {
		log.Printf("failed to render dashboard billing audit: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_billing_audit_render_failed")
	}
}

func (a *App) handleDashboardRAG(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		WriteMethodNotAllowed(w)
		return
	}
	if a.ragSvc == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "rag is not configured", "server_error", "not_configured")
		return
	}
	p, ok := a.extractPrincipal(w, r)
	if !ok {
		return
	}
	successMessage := ""
	errorMessage := ""
	if r.Method == http.MethodPost {
		successMessage, errorMessage = a.handleDashboardRAGAction(r, p)
	}
	collections, err := a.ragSvc.ListCollections(r.Context(), p)
	if err != nil {
		log.Printf("failed to load dashboard rag collections: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_rag_failed")
		return
	}
	documents := make([]rag.Document, 0)
	for _, collection := range collections {
		docs, err := a.ragSvc.ListDocuments(r.Context(), p, collection.ID, 100, 0)
		if err != nil {
			log.Printf("failed to load dashboard rag documents collection_id=%s: %v", collection.ID, err)
			continue
		}
		documents = append(documents, docs...)
	}
	if err := dashboardTemplates.ExecuteTemplate(w, "partial-rag", dashboardRAGData{Collections: collections, Documents: documents, Success: successMessage, Error: errorMessage}); err != nil {
		log.Printf("failed to render dashboard rag: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_rag_render_failed")
	}
}

func (a *App) handleDashboardRAGDocumentDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	p, ok := a.extractPrincipal(w, r)
	if !ok {
		return
	}
	if a.ragSvc == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "rag is not configured", "server_error", "not_configured")
		return
	}
	documentID := strings.Trim(strings.TrimPrefix(r.URL.Path, "/dashboard/rag/documents/"), "/")
	documentID = strings.TrimSuffix(documentID, "/download")
	documentID = strings.Trim(documentID, "/")
	if documentID == "" {
		WriteOpenAIError(w, http.StatusBadRequest, "document id is required", "invalid_request_error", "missing_document")
		return
	}
	doc, blob, err := a.ragSvc.DownloadDocument(r.Context(), p, documentID)
	if err != nil {
		writeRAGNotFoundOrError(w, err, "dashboard_rag_document_download_failed")
		return
	}
	w.Header().Set("Content-Type", doc.MimeType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", doc.FileName))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob)
}

func (a *App) handleDashboardChatbots(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		WriteMethodNotAllowed(w)
		return
	}
	if a.chatbotSvc == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "chatbots are not configured", "server_error", "not_configured")
		return
	}
	successMessage := ""
	errorMessage := ""
	p, ok := a.extractPrincipal(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodPost {
		successMessage, errorMessage = a.handleDashboardChatbotAction(r, p)
	}
	chatbots, err := a.chatbotSvc.List(r.Context(), p)
	if err != nil {
		log.Printf("failed to load dashboard chatbots: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_chatbots_failed")
		return
	}
	data := dashboardChatbotsData{Chatbots: chatbots, Success: successMessage, Error: errorMessage}
	if a.promptSvc != nil {
		data.PromptTemplates, _ = a.promptSvc.List(r.Context(), p)
	}
	if a.ragSvc != nil {
		data.Collections, _ = a.ragSvc.ListCollections(r.Context(), p)
	}
	if err := dashboardTemplates.ExecuteTemplate(w, "partial-chatbots", data); err != nil {
		log.Printf("failed to render dashboard chatbots: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_chatbots_render_failed")
	}
}

func (a *App) handleDashboardPromptTemplates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		WriteMethodNotAllowed(w)
		return
	}
	if a.promptSvc == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "prompt templates are not configured", "server_error", "not_configured")
		return
	}
	p, ok := a.extractPrincipal(w, r)
	if !ok {
		return
	}
	successMessage := ""
	errorMessage := ""
	if r.Method == http.MethodPost {
		successMessage, errorMessage = a.handleDashboardPromptTemplateAction(r, p)
	}
	templates, err := a.promptSvc.List(r.Context(), p)
	if err != nil {
		log.Printf("failed to load dashboard prompt templates: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_prompt_templates_failed")
		return
	}
	if err := dashboardTemplates.ExecuteTemplate(w, "partial-prompt-templates", dashboardPromptTemplatesData{Templates: templates, Success: successMessage, Error: errorMessage}); err != nil {
		log.Printf("failed to render dashboard prompt templates: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_prompt_templates_render_failed")
	}
}

func (a *App) handleDashboardConnectors(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		WriteMethodNotAllowed(w)
		return
	}
	if a.connectorSvc == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "connectors are not configured", "server_error", "not_configured")
		return
	}
	p, ok := a.extractPrincipal(w, r)
	if !ok {
		return
	}
	successMessage := ""
	errorMessage := ""
	if r.Method == http.MethodPost {
		successMessage, errorMessage = a.handleDashboardConnectorAction(r, p)
	}
	sources, err := a.connectorSvc.ListSources(r.Context(), p, "", 100, 0)
	if err != nil {
		log.Printf("failed to load dashboard connector sources: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_connectors_failed")
		return
	}
	jobs, err := a.connectorSvc.ListSyncJobs(r.Context(), p, "", "", 25, 0)
	if err != nil {
		log.Printf("failed to load dashboard connector jobs: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_connector_jobs_failed")
		return
	}
	data := dashboardConnectorsData{Sources: sources, Jobs: jobs, Success: successMessage, Error: errorMessage, Bindings: map[string][]source.SourceIdentity{}, CanManageBindings: auth.HasScope(p, auth.ScopePlatformAdmin)}
	data.CredentialRefs = map[string][]auth.SecretRefMeta{}
	for _, kind := range []string{"github", "slack", "teams", "notion", "google_drive"} {
		data.CredentialRefs[kind] = a.connectorSvc.CredentialRefs(p, kind)
	}
	data.Capabilities = map[string]connectors.AdapterCapabilities{}
	for _, connection := range sources {
		if caps, err := a.connectorSvc.GetCapabilities(r.Context(), p, connection.ID); err == nil {
			data.Capabilities[connection.ID] = caps
		}
	}
	data.LatestJobs = map[string]connectors.SyncJob{}
	for _, job := range jobs {
		if _, exists := data.LatestJobs[job.ConnectorSourceID]; !exists {
			data.LatestJobs[job.ConnectorSourceID] = job
		}
	}
	data.Streams = map[string]map[string]connectors.StreamState{}
	data.PermissionFreshness = map[string]map[string]time.Time{}
	for _, connection := range sources {
		streams := map[string]connectors.StreamState{}
		for _, stream := range []string{"content", "access", "membership"} {
			if state, err := a.connectorSvc.GetStreamState(r.Context(), p, connection.ID, stream); err == nil {
				streams[stream] = state
			}
		}
		data.Streams[connection.ID] = streams
		freshness := map[string]time.Time{}
		if a.sourceRepo != nil {
			if _, at, err := a.sourceRepo.LatestCompleteRevision(r.Context(), p.TenantID, connection.ID, source.SnapshotGrants); err == nil {
				freshness["access"] = at
			}
			if _, at, err := a.sourceRepo.LatestCompleteRevision(r.Context(), p.TenantID, connection.ID, source.SnapshotMembership); err == nil {
				freshness["membership"] = at
			}
		}
		data.PermissionFreshness[connection.ID] = freshness
	}
	if data.CanManageBindings {
		for _, connection := range sources {
			rows, err := a.connectorSvc.ListSourceBindings(r.Context(), p, connection.ID)
			if err == nil {
				data.Bindings[connection.ID] = rows
			}
		}
	}
	data.Collections, _ = a.ragSvc.ListCollections(r.Context(), p)
	if err := dashboardTemplates.ExecuteTemplate(w, "partial-connectors", data); err != nil {
		log.Printf("failed to render dashboard connectors: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_connectors_render_failed")
	}
}

func (a *App) handleDashboardRAGAction(r *http.Request, p auth.Principal) (string, string) {
	if err := parseDashboardForm(r, a.maxUploadMB); err != nil {
		return "", "Invalid form data"
	}
	switch strings.TrimSpace(r.FormValue("action")) {
	case "upload_document":
		if r.MultipartForm == nil || len(r.MultipartForm.File) != 1 || len(r.MultipartForm.File["file"]) != 1 {
			return "", "Exactly one file is required"
		}
		defer func() { _ = r.MultipartForm.RemoveAll() }()
		collectionID := strings.TrimSpace(r.FormValue("collection_id"))
		file, header, err := r.FormFile("file")
		if err != nil {
			return "", "File is required"
		}
		defer file.Close()
		maxBytes := a.maxUploadMB * 1024 * 1024
		if header.Size <= 0 || header.Size > maxBytes {
			return "", "File size is outside the allowed limit"
		}
		data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
		if err != nil || int64(len(data)) > maxBytes {
			return "", "Unable to read file within the allowed limit"
		}
		mimeType := header.Header.Get("Content-Type")
		if strings.TrimSpace(mimeType) == "" {
			mimeType = "application/octet-stream"
		}
		if _, err := a.ragSvc.UploadDocument(r.Context(), p, collectionID, rag.UploadDocumentInput{FileName: header.Filename, MimeType: mimeType, Bytes: data}); err != nil {
			return "", err.Error()
		}
		return "Document upload queued", ""
	case "create_collection":
		chunkSize := dashboardFormInt(r, "chunk_size", rag.DefaultChunkSize)
		chunkOverlap := dashboardFormInt(r, "chunk_overlap", rag.DefaultChunkOverlap)
		if _, err := a.ragSvc.CreateCollection(r.Context(), p, rag.CreateCollectionInput{
			Name:           r.FormValue("name"),
			Description:    r.FormValue("description"),
			EmbeddingModel: r.FormValue("embedding_model"),
			ChunkSize:      chunkSize,
			ChunkOverlap:   chunkOverlap,
			ACL:            map[string]any{"public": strings.TrimSpace(r.FormValue("public")) == "on"},
		}); err != nil {
			return "", err.Error()
		}
		return "RAG collection created", ""
	case "delete_document":
		documentID := strings.TrimSpace(r.FormValue("document_id"))
		if documentID == "" {
			return "", "Document ID is required"
		}
		if err := a.ragSvc.DeleteDocument(r.Context(), p, documentID); err != nil {
			return "", err.Error()
		}
		return "Document deleted", ""
	case "delete_collection":
		collectionID := strings.TrimSpace(r.FormValue("collection_id"))
		if collectionID == "" {
			return "", "Collection ID is required"
		}
		if err := a.ragSvc.DeleteCollection(r.Context(), p, collectionID); err != nil {
			return "", err.Error()
		}
		return "RAG collection deleted", ""
	default:
		return "", "Invalid action"
	}
}

func (a *App) handleDashboardChatbotAction(r *http.Request, p auth.Principal) (string, string) {
	if err := parseDashboardForm(r, a.maxUploadMB); err != nil {
		return "", "Invalid form data"
	}
	switch strings.TrimSpace(r.FormValue("action")) {
	case "", "create_chatbot":
		ragIDs := []string{}
		if id := strings.TrimSpace(r.FormValue("rag_collection_id")); id != "" {
			ragIDs = append(ragIDs, id)
		}
		settings, err := dashboardJSONMap(r.FormValue("settings_json"))
		if err != nil {
			return "", "Settings must be valid JSON"
		}
		if toolsRaw := strings.TrimSpace(r.FormValue("tools_json")); toolsRaw != "" {
			var tools []string
			if err := json.Unmarshal([]byte(toolsRaw), &tools); err != nil {
				return "", "Tool IDs must be a JSON array of strings"
			}
			settings["tools"] = tools
		}
		if _, err := a.chatbotSvc.Create(r.Context(), p, chatbot.CreateChatbotInput{
			Name:             r.FormValue("name"),
			Description:      r.FormValue("description"),
			PromptTemplateID: r.FormValue("prompt_template_id"),
			ProviderID:       r.FormValue("provider_id"),
			Model:            r.FormValue("model"),
			RAGCollectionIDs: ragIDs,
			Settings:         settings,
			ExternalID:       r.FormValue("external_id"),
		}); err != nil {
			return "", err.Error()
		}
		return "Chatbot created", ""
	case "set_enabled":
		id := strings.TrimSpace(r.FormValue("chatbot_id"))
		enabled := strings.TrimSpace(r.FormValue("enabled")) == "true"
		if id == "" {
			return "", "Chatbot ID is required"
		}
		if _, err := a.chatbotSvc.Update(r.Context(), p, id, chatbot.UpdateChatbotInput{IsEnabled: &enabled}); err != nil {
			return "", err.Error()
		}
		if enabled {
			return "Chatbot enabled", ""
		}
		return "Chatbot disabled", ""
	case "delete_chatbot":
		id := strings.TrimSpace(r.FormValue("chatbot_id"))
		if id == "" {
			return "", "Chatbot ID is required"
		}
		if err := a.chatbotSvc.Delete(r.Context(), p, id); err != nil {
			return "", err.Error()
		}
		return "Chatbot deleted", ""
	default:
		return "", "Invalid action"
	}
}

func (a *App) handleDashboardPromptTemplateAction(r *http.Request, p auth.Principal) (string, string) {
	if err := parseDashboardForm(r, a.maxUploadMB); err != nil {
		return "", "Invalid form data"
	}
	vars, err := dashboardJSONMap(r.FormValue("variables_schema"))
	if err != nil {
		return "", "Variables schema must be valid JSON"
	}
	if _, err := a.promptSvc.Create(r.Context(), p, prompts.CreateTemplateInput{
		ID:              r.FormValue("id"),
		Name:            r.FormValue("name"),
		Description:     r.FormValue("description"),
		SystemPrompt:    r.FormValue("system_prompt"),
		UserPrompt:      r.FormValue("user_prompt"),
		VariablesSchema: vars,
		IsDefault:       strings.TrimSpace(r.FormValue("is_default")) == "on",
	}); err != nil {
		return "", err.Error()
	}
	return "Prompt template created", ""
}

func (a *App) handleDashboardConnectorAction(r *http.Request, p auth.Principal) (string, string) {
	if err := parseDashboardForm(r, a.maxUploadMB); err != nil {
		return "", "Invalid form data"
	}
	switch strings.TrimSpace(r.FormValue("action")) {
	case "sync_source":
		sourceID := strings.TrimSpace(r.FormValue("source_id"))
		if sourceID == "" {
			return "", "Connector source ID is required"
		}
		if _, err := a.connectorSvc.SyncSource(r.Context(), p, sourceID); err != nil {
			return "", err.Error()
		}
		return "Connector sync queued", ""
	case "cancel_sync":
		jobID := strings.TrimSpace(r.FormValue("job_id"))
		if jobID == "" {
			return "", "Connector sync job ID is required"
		}
		if err := a.connectorSvc.CancelSyncJob(r.Context(), p, jobID); err != nil {
			return "", err.Error()
		}
		return "Connector sync cancellation requested", ""
	case "bind_source_principal":
		_, err := a.connectorSvc.BindSourcePrincipal(r.Context(), p, connectors.BindSourcePrincipalInput{
			ConnectionID: r.FormValue("source_id"), NativeUserID: r.FormValue("native_user_id"),
			MappedUserID: r.FormValue("mapped_user_id"), VerificationEvidence: r.FormValue("verification_evidence"),
		})
		if err != nil {
			return "", err.Error()
		}
		return "Source principal binding verified and saved", ""
	case "revoke_source_binding":
		if err := a.connectorSvc.RevokeConnectionSourceBinding(r.Context(), p, r.FormValue("source_id"), r.FormValue("binding_id")); err != nil {
			return "", err.Error()
		}
		return "Source principal binding revoked", ""
	case "create_source":
		config, err := dashboardConnectorConfig(r)
		if err != nil {
			return "", err.Error()
		}
		if _, err := a.connectorSvc.CreateSource(r.Context(), p, connectors.CreateSourceInput{
			Type:               r.FormValue("type"),
			Name:               r.FormValue("name"),
			TargetCollectionID: r.FormValue("target_collection_id"),
			Config:             config,
			CredentialRef:      r.FormValue("credential_ref"),
		}); err != nil {
			return "", err.Error()
		}
		return "Connector source created", ""
	case "set_source_enabled":
		sourceID := strings.TrimSpace(r.FormValue("source_id"))
		enabled := strings.TrimSpace(r.FormValue("enabled")) == "true"
		if sourceID == "" {
			return "", "Connector source ID is required"
		}
		if _, err := a.connectorSvc.UpdateSource(r.Context(), p, sourceID, connectors.UpdateSourceInput{IsEnabled: &enabled}); err != nil {
			return "", err.Error()
		}
		if enabled {
			return "Connector source enabled", ""
		}
		return "Connector source disabled", ""
	case "delete_source":
		sourceID := strings.TrimSpace(r.FormValue("source_id"))
		if sourceID == "" {
			return "", "Connector source ID is required"
		}
		if err := a.connectorSvc.DeleteSource(r.Context(), p, sourceID); err != nil {
			return "", err.Error()
		}
		return "Connector source deleted", ""
	default:
		return "", "Invalid action"
	}
}

func parseDashboardForm(r *http.Request, maxUploadMB int64) error {
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		return r.ParseMultipartForm(maxUploadMB * 1024 * 1024)
	}
	return r.ParseForm()
}

func dashboardFormInt(r *http.Request, key string, fallback int) int {
	raw := strings.TrimSpace(r.FormValue(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}

func dashboardJSONMap(raw string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]any{}, nil
	}
	out := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func dashboardConnectorConfig(r *http.Request) (map[string]any, error) {
	config := map[string]any{"sync_interval": r.FormValue("sync_interval")}
	split := func(key string) []any {
		parts := strings.FieldsFunc(r.FormValue(key), func(r rune) bool { return r == ',' || r == '\n' || r == '\r' })
		out := make([]any, 0, len(parts))
		for _, part := range parts {
			if value := strings.TrimSpace(part); value != "" {
				out = append(out, value)
			}
		}
		return out
	}
	switch r.FormValue("type") {
	case "github":
		config["owner"], config["repo"], config["ref"], config["paths"] = r.FormValue("owner"), r.FormValue("repo"), r.FormValue("ref"), split("paths")
	case "slack":
		channels := split("channel_ids")
		if len(channels) != 1 {
			return nil, fmt.Errorf("Slack supports one configured channel per connection")
		}
		config["channel_id"] = channels[0]
		config["reconcile"] = "true"
	case "teams":
		config["tenant_id"], config["team_id"], config["chat_ids"] = r.FormValue("tenant_id"), r.FormValue("team_id"), split("chat_ids")
		channels := split("channel_ids")
		channelMap := map[string]any{}
		if len(channels) > 0 {
			channelMap[r.FormValue("team_id")] = channels
		}
		config["channel_ids"] = channelMap
	case "notion":
		audience := strings.TrimSpace(r.FormValue("notion_audience"))
		members := split("notion_members")
		if audience == "" || len(members) == 0 {
			return nil, fmt.Errorf("Notion published_audience requires an audience ID and at least one Enclava user")
		}
		selected := []any{}
		for _, id := range split("page_ids") {
			selected = append(selected, map[string]any{"page_id": id, "audiences": []any{map[string]any{"id": audience, "members": members}}})
		}
		config["selected_pages"] = selected
		config["authorization_mode"] = "published_audience"
	case "google_drive":
		config["folder_id"], config["drive_id"], config["mime_types"] = r.FormValue("folder_id"), r.FormValue("drive_id"), split("mime_types")
	default:
		return nil, fmt.Errorf("unsupported connector type")
	}
	return config, nil
}
