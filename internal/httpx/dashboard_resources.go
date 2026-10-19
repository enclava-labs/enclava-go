package httpx

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/connectors"
	"enclava-go/internal/llm"
	"enclava-go/internal/prompts"
	"enclava-go/internal/rag"
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
	Snapshot    llm.ReadinessSnapshot
	Models      []llm.Model
	Attestation any
	Error       string
}

// Legacy billing/pricing view rows — the billing repo itself ports in 01-17;
// these stay wire-identical to the sqlite rows until then.
type dashboardProviderPricing struct {
	ID                       int64
	ProviderID               string
	ModelID                  string
	Currency                 string
	InputTokenCentsPer1K     float64
	OutputTokenCentsPer1K    float64
	EmbeddingTokenCentsPer1K float64
	EffectiveAt              time.Time
	CreatedAt                time.Time
}

type dashboardPricingAudit struct {
	ID                string
	ProviderPricingID *int64
	Action            string
	ActorType         string
	ActorID           string
	Before            map[string]any
	After             map[string]any
	CreatedAt         time.Time
}

type dashboardBillingAudit struct {
	ID          string
	APIKeyID    *int64
	SubjectType string
	SubjectID   string
	Action      string
	ActorType   string
	ActorID     string
	AmountCents *int64
	Metadata    map[string]any
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
	Sources     []connectors.Source
	Jobs        []connectors.SyncJob
	Collections []rag.Collection
	Success     string
	Error       string
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
	a.renderDashboardResourcePage(w, r, "chatbots", "Chatbots", "External chatbot endpoints and model bindings", "/dashboard/partials/chatbots", "dashboard-chatbots", "Loading chatbots...")
}

func (a *App) handleDashboardPagePromptTemplates(w http.ResponseWriter, r *http.Request) {
	a.renderDashboardResourcePage(w, r, "prompt-templates", "Prompt Templates", "Reusable system and user prompts", "/dashboard/partials/prompt-templates", "dashboard-prompt-templates", "Loading prompt templates...")
}

func (a *App) handleDashboardPageConnectors(w http.ResponseWriter, r *http.Request) {
	a.renderDashboardResourcePage(w, r, "connectors", "Connectors", "First-party source sync and RAG ingestion", "/dashboard/partials/connectors", "dashboard-connectors", "Loading connectors...")
}

func (a *App) renderDashboardResourcePage(w http.ResponseWriter, r *http.Request, activePage, pageTitle, description, partialPath, mountID, loadingText string) {
	if r.Method != http.MethodGet {
		WriteMethodNotAllowed(w)
		return
	}
	if a.dashboardPasswordAuthEnabled() && !a.dashboardSessionValid(r) {
		http.Redirect(w, r, "/dashboard/login", http.StatusSeeOther)
		return
	}
	data := dashboardResourcePageData{
		dashboardPageCommon: a.dashboardPageData(activePage, pageTitle),
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
	if a.pricingRepo == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "pricing is not configured", "server_error", "not_configured")
		return
	}
	pricing, err := a.pricingRepo.List(r.Context(), "", "", 100, 0)
	if err != nil {
		log.Printf("failed to load dashboard pricing: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_pricing_failed")
		return
	}
	audit, err := a.pricingRepo.ListPricingAudit(r.Context(), 20, 0)
	if err != nil {
		log.Printf("failed to load dashboard pricing audit: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_pricing_audit_failed")
		return
	}
	pricingView := make([]dashboardProviderPricing, 0, len(pricing))
	for _, row := range pricing {
		pricingView = append(pricingView, dashboardProviderPricing{
			ID: row.ID, ProviderID: row.ProviderID, ModelID: row.ModelID, Currency: row.Currency,
			InputTokenCentsPer1K: row.InputTokenCentsPer1K, OutputTokenCentsPer1K: row.OutputTokenCentsPer1K,
			EmbeddingTokenCentsPer1K: row.EmbeddingTokenCentsPer1K, EffectiveAt: row.EffectiveAt, CreatedAt: row.CreatedAt,
		})
	}
	auditView := make([]dashboardPricingAudit, 0, len(audit))
	for _, row := range audit {
		auditView = append(auditView, dashboardPricingAudit{
			ID: row.ID, ProviderPricingID: row.ProviderPricingID, Action: row.Action,
			ActorType: row.ActorType, ActorID: row.ActorID, Before: row.Before, After: row.After, CreatedAt: row.CreatedAt,
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
	if a.billingAuditRepo == nil {
		WriteOpenAIError(w, http.StatusNotImplemented, "billing audit is not configured", "server_error", "not_configured")
		return
	}
	audit, err := a.billingAuditRepo.List(r.Context(), nil, "", 100, 0)
	if err != nil {
		log.Printf("failed to load dashboard billing audit: %v", err)
		WriteOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "dashboard_billing_audit_failed")
		return
	}
	auditView := make([]dashboardBillingAudit, 0, len(audit))
	for _, row := range audit {
		auditView = append(auditView, dashboardBillingAudit{
			ID: row.ID, APIKeyID: row.APIKeyID, SubjectType: row.SubjectType, SubjectID: row.SubjectID,
			Action: row.Action, ActorType: row.ActorType, ActorID: row.ActorID,
			AmountCents: row.AmountCents, Metadata: row.Metadata, CreatedAt: row.CreatedAt,
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
	data := dashboardConnectorsData{Sources: sources, Jobs: jobs, Success: successMessage, Error: errorMessage}
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
	case "upload_document":
		collectionID := strings.TrimSpace(r.FormValue("collection_id"))
		file, header, err := r.FormFile("file")
		if err != nil {
			return "", "File is required"
		}
		defer func() { _ = file.Close() }()
		content, err := io.ReadAll(file)
		if err != nil {
			return "", "Failed to read uploaded file"
		}
		mimeType := header.Header.Get("Content-Type")
		if strings.TrimSpace(mimeType) == "" {
			mimeType = "application/octet-stream"
		}
		if _, err := a.ragSvc.UploadDocument(r.Context(), p, collectionID, rag.UploadDocumentInput{
			FileName: filepath.Base(header.Filename),
			MimeType: mimeType,
			Bytes:    content,
		}); err != nil {
			return "", err.Error()
		}
		return "Document uploaded", ""
	case "reprocess_document":
		documentID := strings.TrimSpace(r.FormValue("document_id"))
		if documentID == "" {
			return "", "Document ID is required"
		}
		if _, err := a.ragSvc.ReprocessDocument(r.Context(), p, documentID); err != nil {
			return "", err.Error()
		}
		return "Document reprocessed", ""
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
	case "sync_source":
		sourceID := strings.TrimSpace(r.FormValue("source_id"))
		if _, err := a.connectorSvc.SyncSource(r.Context(), p, sourceID); err != nil {
			return "", err.Error()
		}
		return "Connector sync started", ""
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
	config, err := dashboardJSONMap(r.FormValue("config_json"))
	if err != nil {
		return nil, err
	}
	content := strings.TrimSpace(r.FormValue("content"))
	if content == "" {
		return config, nil
	}
	config["documents"] = []any{map[string]any{
		"file_name":  r.FormValue("file_name"),
		"mime_type":  "text/plain",
		"content":    content,
		"source_id":  r.FormValue("source_id"),
		"source_uri": r.FormValue("source_uri"),
	}}
	return config, nil
}
