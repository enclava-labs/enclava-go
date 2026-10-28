package chatbot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/extract"
	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
	"enclava-go/internal/llm"
	"enclava-go/internal/prompts"
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
)

// Repository is the tenant-scoped persistence boundary over the chatbots,
// conversations and messages domain tables. Implemented by
// postgres.ChatbotRepo.
type Repository interface {
	Create(ctx context.Context, b Instance) error
	List(ctx context.Context, tenantID string) ([]Instance, error)
	GetByID(ctx context.Context, tenantID, id string) (Instance, error)
	GetByExternalID(ctx context.Context, tenantID, externalID string) (Instance, error)
	Update(ctx context.Context, b Instance) error
	Delete(ctx context.Context, tenantID, id string) error
	CreateConversation(ctx context.Context, c Conversation) error
	GetConversation(ctx context.Context, tenantID, id string) (Conversation, error)
	AddMessage(ctx context.Context, m Message) error
	ListMessages(ctx context.Context, tenantID, conversationID string) ([]Message, error)
	// ListConversations returns the owner's newest-first conversation
	// summaries under the opaque keyset cursor (own-only, admins included).
	ListConversations(ctx context.Context, tenantID, ownerActorID string, limit int, cursor string) ([]ConversationSummary, string, error)
	// CountConversations counts one owner's conversations; the owner binds
	// to the caller at the service boundary — never a request parameter.
	CountConversations(ctx context.Context, tenantID, ownerActorID string) (int64, error)
}

type RunRepository interface {
	CreateRun(ctx context.Context, run AssistantRun) error
	GetRun(ctx context.Context, tenantID, id string) (AssistantRun, error)
	UpdateRun(ctx context.Context, run AssistantRun) error
	UpdateRunFenced(ctx context.Context, run AssistantRun, owner string, fence int64) error
	AddRunAttempt(ctx context.Context, attempt AssistantRunAttempt, owner string, fence int64) error
	UpdateRunAttempt(ctx context.Context, attempt AssistantRunAttempt, owner string, fence int64) error
	ListRunAttempts(ctx context.Context, tenantID, runID string) ([]AssistantRunAttempt, error)
	AddRunMessage(ctx context.Context, message Message, jobID, owner string, fence int64) error
	// ListFailedRuns derives terminal status from the BOUND JOB so
	// pre-dispatch failures and queued cancels surface without a worker.
	ListFailedRuns(ctx context.Context, tenantID, ownerActorID string, limit int) ([]AssistantRun, error)
}

type MCPInputAuthorizer interface {
	AuthorizeMCPInput(context.Context, auth.Principal, []source.ObjectRef) error
}

type MCPToolDispatcher interface {
	Definitions(context.Context, auth.Principal, auth.Principal, Instance) ([]map[string]any, error)
	Invoke(context.Context, auth.Principal, auth.Principal, Instance, string, json.RawMessage, []source.ObjectRef) (BuiltinResult, error)
}

type RunPrincipalResolver interface {
	CurrentPrincipal(ctx context.Context, frozen auth.Principal) (auth.Principal, error)
	PrincipalForActor(ctx context.Context, tenantID, actorID string) (auth.Principal, error)
}

type EvidenceResolver interface {
	ResolveEvidence(ctx context.Context, p auth.Principal, ids []string) ([]rag.EvidenceResolution, error)
}

type Extractor interface {
	Process(ctx context.Context, p auth.Principal, input extract.ProcessInput) (extract.ProcessOutput, error)
	GetJob(ctx context.Context, p auth.Principal, jobID string) (extract.JobDetail, error)
}

type PromptRepository interface {
	GetByID(ctx context.Context, tenantID, id string) (prompts.Template, error)
	GetDefault(ctx context.Context, tenantID string) (prompts.Template, error)
}

// RAGSearcher is the rag.Service search surface. The request principal is
// passed through unchanged so a caller can never forge collection authority.
type RAGSearcher interface {
	Search(ctx context.Context, p auth.Principal, input rag.SearchInput) ([]rag.SearchResult, error)
	BuildContext(ctx context.Context, p auth.Principal, candidates []rag.SearchResult, characterLimit int) (rag.ContextResult, error)
}

type ragCollectionLister interface {
	ListCollections(context.Context, auth.Principal) ([]rag.Collection, error)
}

// Executor is the single authorized priced model-dispatch boundary;
// *inference.Service satisfies it. The assistant never calls a provider or
// records billing usage directly — reservation, dispatch and settlement all
// happen inside Execute under the caller's principal.
type Executor interface {
	Execute(ctx context.Context, p auth.Principal, req inference.Request) (*inference.Result, error)
}

type Service struct {
	repo              Repository
	prompts           PromptRepository
	rag               RAGSearcher
	executor          Executor
	mcpTools          MCPToolDispatcher
	runRepo           RunRepository
	jobSvc            *jobs.Service
	extractor         Extractor
	principalResolver RunPrincipalResolver
}

func (s *Service) ConfigureMCP(dispatcher MCPToolDispatcher) { s.mcpTools = dispatcher }

func NewService(repo Repository, promptRepo PromptRepository, ragSvc RAGSearcher, executor Executor) *Service {
	return &Service{repo: repo, prompts: promptRepo, rag: ragSvc, executor: executor}
}

// ConfigureRuns connects chatbot runs to durable jobs and current caller authority.
func (s *Service) ConfigureRuns(repo RunRepository, jobSvc *jobs.Service, extractor Extractor, resolver RunPrincipalResolver) {
	s.runRepo, s.jobSvc, s.extractor, s.principalResolver = repo, jobSvc, extractor, resolver
}

// maxOutputEstimate is the declared upper bound for generated units; it
// matches the executor's operation bound for chat/vision.
const maxOutputEstimate = 1 << 17

func requireTenant(p auth.Principal) error {
	if strings.TrimSpace(p.TenantID) == "" {
		return fmt.Errorf("%w: principal tenant is required", source.ErrDenied)
	}
	return nil
}

// actorKey is the conversation-owner identity.
func actorKey(p auth.Principal) string {
	return strings.TrimSpace(p.ActorID)
}

// chatbotAllowed is the single stored-grant bound for assistant access:
// List/Get/run AND the Update/Delete mutations require it (D-25). Neither
// platform.admin scope nor record ownership substitutes for a grant —
// ownership only narrows mutations (chatbotOwnerAllowed). The stored
// audience is an ADDITIONAL restriction on top of grants (D-07/D-25): the
// caller must be an audience member (or "*") unless they are the
// platform.admin or the assistant owner — those bypass the audience term
// only, never the grant check.
func chatbotAllowed(p auth.Principal, bot Instance) bool {
	if !chatbotOwnerAllowed(p, bot) && !contains(bot.Audience, actorKey(p)) && !contains(bot.Audience, "*") {
		return false
	}
	ids := []string{bot.ID}
	if ext := strings.TrimSpace(bot.ExternalID); ext != "" {
		ids = append(ids, ext)
	}
	return auth.ChatbotAllowed(p, ids...) || auth.AgentAllowed(p, ids...)
}
func chatbotOwnerAllowed(p auth.Principal, bot Instance) bool {
	return auth.HasScope(p, "platform.admin") || bot.OwnerActorID != "" && bot.OwnerActorID == actorKey(p)
}

func (s *Service) ownerPrincipal(ctx context.Context, p auth.Principal, bot Instance) (auth.Principal, error) {
	if auth.HasScope(p, "platform.admin") || actorKey(p) == bot.OwnerActorID {
		return p, nil
	}
	if s.principalResolver == nil {
		return auth.Principal{}, source.ErrDenied
	}
	owner, err := s.principalResolver.PrincipalForActor(ctx, p.TenantID, bot.OwnerActorID)
	if err != nil || owner.TenantID != p.TenantID || actorKey(owner) != bot.OwnerActorID {
		return auth.Principal{}, fmt.Errorf("chatbot: assistant owner authority is unavailable: %w", source.ErrDenied)
	}
	return owner, nil
}

// VisiblePromptTemplateIDs implements prompts.VisibleTemplateIDs: the set of
// prompt template ids attached to chatbots the principal may see.
func (s *Service) VisiblePromptTemplateIDs(ctx context.Context, p auth.Principal) (map[string]struct{}, error) {
	bots, err := s.List(ctx, p)
	if err != nil {
		return nil, err
	}
	allowed := map[string]struct{}{}
	for _, bot := range bots {
		if id := strings.TrimSpace(bot.PromptTemplateID); id != "" {
			allowed[id] = struct{}{}
		}
	}
	return allowed, nil
}

func (s *Service) Create(ctx context.Context, p auth.Principal, input CreateChatbotInput) (Instance, error) {
	if err := requireTenant(p); err != nil {
		return Instance{}, err
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return Instance{}, errors.New("name is required")
	}
	model := strings.TrimSpace(input.Model)
	if model == "" {
		return Instance{}, errors.New("model is required")
	}
	if !auth.ModelAllowed(p, model) {
		return Instance{}, ErrModelNotAllowed
	}
	id := newID("chatbot")
	externalID, err := normalizeExternalID(input.ExternalID, id)
	if err != nil {
		return Instance{}, err
	}
	enabled := true
	if input.IsEnabled != nil {
		enabled = *input.IsEnabled
	}
	audience, err := normalizeAudience(input.Audience)
	if err != nil {
		return Instance{}, err
	}
	if audience == nil {
		// Omitted audience degrades safely to owner-only.
		audience = []string{p.ActorID}
	}
	b := Instance{
		TenantID:         p.TenantID,
		ID:               id,
		Name:             name,
		Description:      strings.TrimSpace(input.Description),
		PromptTemplateID: strings.TrimSpace(input.PromptTemplateID),
		ProviderID:       strings.TrimSpace(input.ProviderID),
		Model:            model,
		RAGCollectionIDs: normalizeStringList(input.RAGCollectionIDs),
		Settings:         nonNilMap(input.Settings),
		Audience:         audience,
		ExternalID:       externalID,
		IsEnabled:        enabled,
		OwnerActorID:     p.ActorID,
	}
	if err := s.repo.Create(ctx, b); err != nil {
		return Instance{}, err
	}
	return s.repo.GetByID(ctx, p.TenantID, id)
}

// List returns only chatbots the principal's allowlists grant AND whose
// stored audience admits them (owner/platform.admin bypass the audience
// term only). An empty allowlist denies; only explicit all-mode is
// unrestricted on the grant side.
func (s *Service) List(ctx context.Context, p auth.Principal) ([]Instance, error) {
	if err := requireTenant(p); err != nil {
		return nil, err
	}
	bots, err := s.repo.List(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	out := make([]Instance, 0, len(bots))
	for _, bot := range bots {
		if chatbotAllowed(p, bot) {
			out = append(out, bot)
		}
	}
	return out, nil
}

func (s *Service) Get(ctx context.Context, p auth.Principal, id string) (Instance, error) {
	if err := requireTenant(p); err != nil {
		return Instance{}, err
	}
	bot, err := s.repo.GetByID(ctx, p.TenantID, strings.TrimSpace(id))
	if err != nil {
		return Instance{}, err
	}
	if !chatbotAllowed(p, bot) {
		return Instance{}, sql.ErrNoRows
	}
	return bot, nil
}

func (s *Service) Update(ctx context.Context, p auth.Principal, id string, input UpdateChatbotInput) (Instance, error) {
	if err := requireTenant(p); err != nil {
		return Instance{}, err
	}
	b, err := s.repo.GetByID(ctx, p.TenantID, strings.TrimSpace(id))
	if err != nil {
		return Instance{}, err
	}
	// Mutation gate: record ownership AND the stored assistant grant must
	// both pass — a granted non-owner and a revoked owner both get
	// not-found.
	if !chatbotOwnerAllowed(p, b) || !chatbotAllowed(p, b) {
		return Instance{}, sql.ErrNoRows
	}
	if input.Name != nil {
		b.Name = strings.TrimSpace(*input.Name)
	}
	if b.Name == "" {
		return Instance{}, errors.New("name is required")
	}
	if input.Description != nil {
		b.Description = strings.TrimSpace(*input.Description)
	}
	if input.PromptTemplateID != nil {
		b.PromptTemplateID = strings.TrimSpace(*input.PromptTemplateID)
	}
	if input.ProviderID != nil {
		b.ProviderID = strings.TrimSpace(*input.ProviderID)
	}
	if input.Model != nil {
		b.Model = strings.TrimSpace(*input.Model)
	}
	if b.Model == "" {
		return Instance{}, errors.New("model is required")
	}
	if !auth.ModelAllowed(p, b.Model) {
		return Instance{}, ErrModelNotAllowed
	}
	if input.RAGCollectionIDs != nil {
		b.RAGCollectionIDs = normalizeStringList(input.RAGCollectionIDs)
	}
	if input.Audience != nil {
		audience, err := normalizeAudience(input.Audience)
		if err != nil {
			return Instance{}, err
		}
		b.Audience = audience
	}
	if input.Settings != nil {
		b.Settings = nonNilMap(input.Settings)
	}
	if input.ExternalID != nil {
		externalID, err := normalizeExternalID(*input.ExternalID, b.ID)
		if err != nil {
			return Instance{}, err
		}
		b.ExternalID = externalID
	}
	if input.IsEnabled != nil {
		b.IsEnabled = *input.IsEnabled
	}
	if err := s.repo.Update(ctx, b); err != nil {
		return Instance{}, err
	}
	return s.repo.GetByID(ctx, p.TenantID, b.ID)
}

func (s *Service) Delete(ctx context.Context, p auth.Principal, id string) error {
	if err := requireTenant(p); err != nil {
		return err
	}
	b, err := s.repo.GetByID(ctx, p.TenantID, strings.TrimSpace(id))
	if err != nil {
		return err
	}
	// Same dual gate as Update: ownership AND the stored assistant grant.
	if !chatbotOwnerAllowed(p, b) || !chatbotAllowed(p, b) {
		return sql.ErrNoRows
	}
	return s.repo.Delete(ctx, p.TenantID, b.ID)
}

func (s *Service) ExternalModels(ctx context.Context, p auth.Principal, chatbotRef string) (llm.ModelsResponse, error) {
	bot, err := s.getByRef(ctx, p, chatbotRef)
	if err != nil {
		return llm.ModelsResponse{}, err
	}
	if !bot.IsEnabled {
		return llm.ModelsResponse{}, ErrDisabled
	}
	if !chatbotAllowed(p, bot) {
		return llm.ModelsResponse{}, ErrChatbotNotAllowed
	}
	if !auth.ModelAllowed(p, bot.Model) {
		return llm.ModelsResponse{}, ErrModelNotAllowed
	}
	// Model catalog access goes through the executor's endpoint binding —
	// this endpoint reports the assistant's configured alias only.
	return llm.ModelsResponse{
		Object: "list",
		Data:   []llm.Model{{ID: bot.Model, Object: "model"}},
	}, nil
}

func (s *Service) CreateChatCompletion(ctx context.Context, p auth.Principal, chatbotRef string, rawRequest []byte) (CompletionOutput, error) {
	prepared, err := s.PrepareChatCompletion(ctx, p, chatbotRef, rawRequest, false)
	if err != nil {
		return CompletionOutput{}, err
	}
	res, err := s.executePrepared(ctx, p, prepared)
	if err != nil {
		return CompletionOutput{}, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.SettlementError != nil {
		return CompletionOutput{}, fmt.Errorf("settle reservation: %w", res.SettlementError)
	}
	resp, err := io.ReadAll(res.Body)
	if err != nil {
		return CompletionOutput{}, fmt.Errorf("read provider response: %w", err)
	}
	if err := s.PersistAssistantMessage(ctx, p, prepared, extractAssistantContent(resp)); err != nil {
		return CompletionOutput{}, err
	}
	resp = appendCompletionMetadata(resp, prepared.Chatbot.ID, prepared.Conversation.ID)
	return CompletionOutput{
		Response:       resp,
		Conversation:   prepared.Conversation,
		Chatbot:        prepared.Chatbot,
		Usage:          res.Usage,
		RAGResultCount: prepared.RAGResultCount,
	}, nil
}

func (s *Service) PrepareChatCompletion(ctx context.Context, p auth.Principal, chatbotRef string, rawRequest []byte, stream bool) (PreparedCompletion, error) {
	if err := requireTenant(p); err != nil {
		return PreparedCompletion{}, err
	}
	bot, err := s.getByRef(ctx, p, chatbotRef)
	if err != nil {
		return PreparedCompletion{}, err
	}
	if !bot.IsEnabled {
		return PreparedCompletion{}, ErrDisabled
	}
	if !chatbotAllowed(p, bot) {
		return PreparedCompletion{}, ErrChatbotNotAllowed
	}
	if !auth.ModelAllowed(p, bot.Model) {
		return PreparedCompletion{}, ErrModelNotAllowed
	}
	var payload map[string]any
	if err := json.Unmarshal(rawRequest, &payload); err != nil {
		return PreparedCompletion{}, invalidInput(fmt.Errorf("invalid JSON payload: %w", err))
	}
	var req chatCompletionRequest
	if err := json.Unmarshal(rawRequest, &req); err != nil {
		return PreparedCompletion{}, invalidInput(fmt.Errorf("invalid chat payload: %w", err))
	}
	if len(req.Messages) == 0 {
		return PreparedCompletion{}, invalidInput(errors.New("messages are required"))
	}
	userInput, userIndex := latestUserInput(req.Messages)
	if strings.TrimSpace(userInput) == "" || userIndex < 0 {
		return PreparedCompletion{}, invalidInput(errors.New("at least one user message is required"))
	}
	userMessage := map[string]any{"role": "user", "content": contentToString(req.Messages[userIndex]["content"])}
	conversation, err := s.ensureConversation(ctx, p, bot, req)
	if err != nil {
		return PreparedCompletion{}, err
	}
	historyMessages, historyRefs, historyEvidenceIDs, err := s.authorizedHistory(ctx, p, conversation.ID)
	if err != nil {
		return PreparedCompletion{}, err
	}
	// Redacted turns (revoked evidence) contribute no text to inference.
	history := make([]map[string]any, 0, len(historyMessages))
	for _, message := range historyMessages {
		if strings.TrimSpace(message.Content) == "" {
			continue
		}
		history = append(history, map[string]any{"role": message.Role, "content": message.Content})
	}
	messages := append(history, userMessage)
	userIndex = len(messages) - 1

	owner := p
	if s.rag != nil && len(bot.RAGCollectionIDs) > 0 {
		owner, err = s.ownerPrincipal(ctx, p, bot)
		if err != nil {
			return PreparedCompletion{}, err
		}
	}
	ragResults, ragContext, err := s.retrieveRAG(ctx, p, owner, bot, userInput)
	if err != nil {
		return PreparedCompletion{}, err
	}
	rendered, err := s.renderPrompt(ctx, p, bot, userInput, ragContext, req.Metadata)
	if err != nil {
		return PreparedCompletion{}, err
	}
	providerMessages := applyRenderedPrompt(messages, userIndex, rendered)
	payload["model"] = bot.Model
	payload["messages"] = providerMessages
	payload["stream"] = stream
	delete(payload, "conversation_id")
	delete(payload, "metadata")
	delete(payload, "variables")
	upstreamRequest, err := json.Marshal(payload)
	if err != nil {
		return PreparedCompletion{}, fmt.Errorf("marshal upstream chat request: %w", err)
	}
	if err := s.repo.AddMessage(ctx, Message{
		TenantID: p.TenantID, ID: newID("chatmsg"), ConversationID: conversation.ID,
		Role: "user", Content: userMessage["content"].(string), OwnerActorID: p.ActorID,
	}); err != nil {
		return PreparedCompletion{}, err
	}
	inputRefs := mergeObjectRefs(historyRefs, ragInputs(ragResults))
	evidenceIDs := mergeStrings(historyEvidenceIDs, evidenceIDsForResults(ragResults))
	return PreparedCompletion{
		Request: inference.Request{
			Capability: inference.CapabilityChat, Model: bot.Model,
			EndpointID: strings.TrimSpace(bot.ProviderID), Payload: upstreamRequest,
			Stream: stream, Inputs: inputRefs, Estimate: completionEstimate(upstreamRequest),
		},
		Conversation: conversation, Chatbot: bot, Model: bot.Model,
		RAGResultCount: len(ragResults), EvidenceIDs: evidenceIDs, InputRefs: inputRefs,
	}, nil
}

// ExecutePrepared dispatches one prepared completion through the shared
// executor — policy, reservation, dispatch and settlement all live inside
// Execute. The returned stream/Body stays open until the caller closes it.
func (s *Service) ExecutePrepared(ctx context.Context, p auth.Principal, prepared PreparedCompletion) (*inference.Result, error) {
	return s.executePrepared(ctx, p, prepared)
}

func (s *Service) executePrepared(ctx context.Context, p auth.Principal, prepared PreparedCompletion) (*inference.Result, error) {
	if s.executor == nil {
		return nil, errors.New("chatbot: inference executor is not configured")
	}
	return s.executor.Execute(ctx, p, prepared.Request)
}

// PersistAssistantMessage retains the completed assistant turn. Persistence
// failures return the real repository error — a lost assistant message is
// never reported as a successful completion.
func (s *Service) PersistAssistantMessage(ctx context.Context, p auth.Principal, prepared PreparedCompletion, assistantContent string) error {
	if strings.TrimSpace(assistantContent) == "" {
		return nil
	}
	return s.repo.AddMessage(ctx, Message{
		TenantID: p.TenantID, ID: newID("chatmsg"), ConversationID: prepared.Conversation.ID,
		Role: "assistant", Content: assistantContent, OwnerActorID: p.ActorID,
		EvidenceIDs: prepared.EvidenceIDs, InputRefs: prepared.InputRefs,
	})
}

// ListConversation filters stored source-derived text against current evidence access.
func (s *Service) ListConversation(ctx context.Context, p auth.Principal, conversationID string) (ConversationView, error) {
	if err := requireTenant(p); err != nil {
		return ConversationView{}, err
	}
	conversation, err := s.repo.GetConversation(ctx, p.TenantID, strings.TrimSpace(conversationID))
	if err != nil {
		return ConversationView{}, err
	}
	if !auth.HasScope(p, "platform.admin") && conversation.OwnerActorID != actorKey(p) {
		return ConversationView{}, sql.ErrNoRows
	}
	bot, err := s.repo.GetByID(ctx, p.TenantID, conversation.ChatbotID)
	if err != nil || !chatbotAllowed(p, bot) {
		return ConversationView{}, sql.ErrNoRows
	}
	messages, _, _, err := s.authorizedHistory(ctx, p, conversation.ID)
	if err != nil {
		return ConversationView{}, err
	}
	messages, err = s.attachRunTurns(ctx, p, conversation, messages)
	if err != nil {
		return ConversationView{}, err
	}
	return ConversationView{Conversation: conversation, Messages: messages}, nil
}

// attachRunTurns binds turns to their durable runs (OA-14): assistant
// messages gain the ordered steps of the run named by run_id — through the
// ONE shared authorizedSteps loader, never a raw attempt fetch — and runs
// that ended without a persisted assistant reply project a DISPLAY-ONLY
// terminal turn at their own user message's position. Projection runs at
// READ time from durable run/job state because a run cancelled while queued
// or failed before the worker's fail closure never held a lease that
// AddRunMessage's fencing would accept; the projected turn is never
// persisted, so it structurally cannot enter inference history.
func (s *Service) attachRunTurns(ctx context.Context, p auth.Principal, conversation Conversation, messages []Message) ([]Message, error) {
	if s.runRepo == nil || len(messages) == 0 {
		return messages, nil
	}
	runs := map[string]AssistantRun{}
	answered := map[string]bool{}
	for _, msg := range messages {
		if msg.RunID == "" {
			continue
		}
		if _, seen := runs[msg.RunID]; !seen {
			run, err := s.runRepo.GetRun(ctx, p.TenantID, msg.RunID)
			if err != nil {
				continue // the run row is gone — the turn keeps its plain text
			}
			// Same live-status overlay as GetRun: a run row still reading
			// queued takes the bound job's terminal status.
			if s.jobSvc != nil && run.JobID != "" {
				if job, jobErr := s.jobSvc.Get(ctx, p, run.JobID); jobErr == nil {
					run.Status = job.Status
					if job.Status == jobs.StatusUnknown {
						run.Error = "worker outcome unknown; the attempt will not be repeated"
					}
				}
			}
			runs[msg.RunID] = run
		}
		if msg.Role == "assistant" {
			answered[msg.RunID] = true
		}
	}
	if len(runs) == 0 {
		return messages, nil
	}
	out := make([]Message, 0, len(messages)+len(runs))
	for _, msg := range messages {
		if msg.RunID != "" {
			if run, ok := runs[msg.RunID]; ok {
				if msg.Role == "assistant" {
					steps, _, err := s.authorizedSteps(ctx, p, run)
					if err != nil {
						return nil, err
					}
					msg.Steps = steps
				}
				out = append(out, msg)
				// The terminal projection slots in where the assistant turn
				// would have been: directly after the run's own user turn.
				if msg.Role != "assistant" {
					if turn, ok := s.terminalProjection(ctx, p, conversation, run, answered[msg.RunID]); ok {
						out = append(out, turn)
					}
				}
				continue
			}
		}
		out = append(out, msg)
	}
	return out, nil
}

// terminalProjection renders the display-only assistant turn for a run that
// ended without a persisted reply; successful (or still in-flight) runs are
// skipped — an unanswered successful run keeps today's empty-content shape.
func (s *Service) terminalProjection(ctx context.Context, p auth.Principal, conversation Conversation, run AssistantRun, answered bool) (Message, bool) {
	if answered {
		return Message{}, false
	}
	if run.Status != jobs.StatusFailed && run.Status != jobs.StatusCancelled && run.Status != jobs.StatusUnknown {
		return Message{}, false
	}
	steps, _, err := s.authorizedSteps(ctx, p, run)
	if err != nil {
		return Message{}, false
	}
	return Message{
		TenantID: p.TenantID, ConversationID: conversation.ID, Role: "assistant",
		Content: "", OwnerActorID: conversation.OwnerActorID,
		RunID: run.ID, Steps: steps, CreatedAt: run.CreatedAt,
	}, true
}

// ListAssistantToolBindings answers "which assistants use this tool"
// (OA-26): each requested tool ID maps to the names of assistants whose
// stored tool configuration references it, restricted to assistants the
// caller may already see — a visibility-filtered read, never a bypass.
func (s *Service) ListAssistantToolBindings(ctx context.Context, p auth.Principal, toolIDs []string) (map[string][]string, error) {
	if err := requireTenant(p); err != nil {
		return nil, err
	}
	want := make(map[string]struct{}, len(toolIDs))
	for _, id := range toolIDs {
		if id = strings.TrimSpace(id); id != "" {
			want[id] = struct{}{}
		}
	}
	bindings := make(map[string][]string, len(want))
	if len(want) == 0 {
		return bindings, nil
	}
	bots, err := s.List(ctx, p)
	if err != nil {
		return nil, err
	}
	for _, bot := range bots {
		for _, toolID := range configuredToolIDs(bot) {
			if _, ok := want[toolID]; ok {
				bindings[toolID] = append(bindings[toolID], bot.Name)
			}
		}
	}
	return bindings, nil
}

// maxTitleRunes bounds the server-derived conversation preview title.
const maxTitleRunes = 80

// ListConversations returns the caller's OWN conversations newest-first —
// bound to actorKey(p) unconditionally, admins included; there is no owner
// query parameter to widen it. Rows whose assistant the caller may no
// longer see (grant or audience) are dropped after the read.
func (s *Service) ListConversations(ctx context.Context, p auth.Principal, limit int, cursor string) ([]ConversationSummary, string, error) {
	if err := requireTenant(p); err != nil {
		return nil, "", err
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	bots := map[string]Instance{}
	out := make([]ConversationSummary, 0, limit)
	// A hidden row must never consume a page slot or end the listing: scan
	// forward (bounded batches) until the page holds `limit` visible rows or
	// the keyset is exhausted. `next` always resumes after the last row the
	// scan actually consumed, visible or not.
	next := ""
	batches := 0
	for len(out) < limit && batches < 10 {
		batches++
		batch, batchNext, err := s.repo.ListConversations(ctx, p.TenantID, actorKey(p), limit-len(out), cursor)
		if err != nil {
			if errors.Is(err, ErrInvalidCursor) {
				return nil, "", invalidInput(err)
			}
			return nil, "", err
		}
		for _, summary := range batch {
			bot, ok := bots[summary.ChatbotID]
			if !ok {
				bot, err = s.repo.GetByID(ctx, p.TenantID, summary.ChatbotID)
				if err != nil {
					continue // the assistant row is gone — so is its history
				}
				bots[summary.ChatbotID] = bot
			}
			if !chatbotAllowed(p, bot) {
				continue
			}
			summary.Title = s.conversationTitle(ctx, p, summary.ID)
			out = append(out, summary)
		}
		if batchNext == "" {
			next = ""
			break
		}
		cursor, next = batchNext, batchNext
	}
	return out, next, nil
}

// ListFailedRuns is the own-scope failed-run read for the member attention
// feed (OA-19): the owner binds to the caller unconditionally — admins
// included, no owner parameter, no tenant-wide variant. The repo query
// derives terminal status from the bound job so pre-dispatch failures and
// queued cancels surface without a worker.
func (s *Service) ListFailedRuns(ctx context.Context, p auth.Principal, limit int) ([]AssistantRun, error) {
	if err := requireTenant(p); err != nil {
		return nil, err
	}
	if s.runRepo == nil {
		return nil, errors.New("chatbot: durable runs are not configured")
	}
	return s.runRepo.ListFailedRuns(ctx, p.TenantID, actorKey(p), limit)
}

// CountConversations counts the caller's OWN conversations for the Home
// KPI: the owner binds to actorKey(p) unconditionally — a caller-supplied
// owner cannot widen the count; a tenant-wide variant would be a separate,
// explicitly gated read.
func (s *Service) CountConversations(ctx context.Context, p auth.Principal) (int64, error) {
	if err := requireTenant(p); err != nil {
		return 0, err
	}
	return s.repo.CountConversations(ctx, p.TenantID, actorKey(p))
}

// conversationTitle derives the preview from the first VISIBLE user message
// under the authorizedHistory suppression rule: messages carrying input
// refs but no evidence IDs, and messages whose evidence is revoked, never
// contribute preview text.
func (s *Service) conversationTitle(ctx context.Context, p auth.Principal, conversationID string) string {
	messages, _, _, err := s.authorizedHistory(ctx, p, conversationID)
	if err != nil {
		return ""
	}
	for _, message := range messages {
		if message.Role != "user" {
			continue
		}
		content := strings.TrimSpace(message.Content)
		// Redacted turns (revoked evidence) never contribute preview text.
		if content == "" {
			continue
		}
		if runes := []rune(content); len(runes) > maxTitleRunes {
			return string(runes[:maxTitleRunes])
		}
		return content
	}
	return ""
}

func (s *Service) authorizedHistory(ctx context.Context, p auth.Principal, conversationID string) ([]Message, []source.ObjectRef, []string, error) {
	stored, err := s.repo.ListMessages(ctx, p.TenantID, conversationID)
	if err != nil {
		return nil, nil, nil, err
	}
	var history []Message
	var refs []source.ObjectRef
	var evidenceIDs []string
	for _, msg := range stored {
		if msg.Role != "user" && msg.Role != "assistant" {
			continue
		}
		if len(msg.InputRefs) > 0 && len(msg.EvidenceIDs) == 0 {
			continue
		}
		if len(msg.EvidenceIDs) > 0 {
			denied, err := s.evidenceDenied(ctx, p, msg.EvidenceIDs)
			if err != nil {
				return nil, nil, nil, err
			}
			if denied {
				// Revoked evidence: the published turn survives replay as a
				// redacted skeleton (steps still attach via run_id) but its
				// text and refs never feed future inference.
				msg.Content = ""
				history = append(history, msg)
				continue
			}
		}
		history = append(history, msg)
		refs = mergeObjectRefs(refs, msg.InputRefs)
		evidenceIDs = mergeStrings(evidenceIDs, msg.EvidenceIDs)
	}
	return history, refs, evidenceIDs, nil
}

// evidenceDenied replays evidence IDs under CURRENT access: denied,
// revoked or unverifiable bindings (no resolver configured) are denied;
// infrastructure failures propagate.
func (s *Service) evidenceDenied(ctx context.Context, p auth.Principal, ids []string) (bool, error) {
	if len(ids) == 0 {
		return false, nil
	}
	resolver, ok := s.rag.(EvidenceResolver)
	if !ok {
		return true, nil
	}
	if _, err := resolver.ResolveEvidence(ctx, p, ids); err != nil {
		if errors.Is(err, source.ErrDenied) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

func evidenceIDsForResults(results []rag.SearchResult) []string {
	var ids []string
	for _, result := range results {
		if id := strings.TrimSpace(result.EvidenceID); id != "" {
			ids = append(ids, id)
		}
	}
	return mergeStrings(nil, ids)
}

func mergeStrings(groups ...[]string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, group := range groups {
		for _, value := range group {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			out = append(out, value)
		}
	}
	return out
}

func mergeObjectRefs(groups ...[]source.ObjectRef) []source.ObjectRef {
	seen := make(map[string]struct{})
	var out []source.ObjectRef
	for _, group := range groups {
		for _, ref := range group {
			key := ref.TenantID + "\x00" + ref.ObjectID + "\x00" + fmt.Sprint(ref.ObservedSeq)
			if ref.ObjectID == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, ref)
		}
	}
	return out
}

// ragInputs converts retrieved evidence into the immutable object refs the
// executor re-authorizes and merges into effective restrictions before
// routing — a restricted source can never reach an incompatible endpoint.
func ragInputs(results []rag.SearchResult) []source.ObjectRef {
	seen := map[string]struct{}{}
	var refs []source.ObjectRef
	for _, r := range results {
		id := strings.TrimSpace(r.DocumentID)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		ref := source.ObjectRef{ObjectID: id}
		for _, span := range r.Spans {
			if span.Ref.ObjectID == id && span.Ref.ObservedSeq > 0 {
				ref = span.Ref
				break
			}
		}
		refs = append(refs, ref)
	}
	return refs
}

// completionEstimate declares the per-kind unit bound: input is sized from
// the marshaled payload, output at the operation's configured maximum.
func completionEstimate(payload []byte) billing.Usage {
	return billing.Usage{
		InputUnits:  int64((utf8.RuneCount(payload) + 3) / 4),
		OutputUnits: maxOutputEstimate,
	}
}

func (s *Service) getByRef(ctx context.Context, p auth.Principal, ref string) (Instance, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Instance{}, sql.ErrNoRows
	}
	bot, err := s.repo.GetByID(ctx, p.TenantID, ref)
	if errors.Is(err, sql.ErrNoRows) {
		bot, err = s.repo.GetByExternalID(ctx, p.TenantID, ref)
	}
	if err != nil {
		return Instance{}, err
	}
	return bot, nil
}

func (s *Service) renderPrompt(ctx context.Context, p auth.Principal, bot Instance, userInput, ragContext string, metadata map[string]any) (prompts.RenderedPrompt, error) {
	tmpl, err := s.promptTemplate(ctx, p, bot)
	if err != nil {
		return prompts.RenderedPrompt{}, err
	}
	vars := mapFromAny(bot.Settings["variables"])
	if requestVars := mapFromAny(metadata["variables"]); len(requestVars) > 0 {
		for key, value := range requestVars {
			vars[key] = value
		}
	}
	return prompts.Render(tmpl, prompts.RenderInput{
		UserInput:  userInput,
		RAGContext: ragContext,
		Variables:  vars,
	})
}

func (s *Service) promptTemplate(ctx context.Context, p auth.Principal, bot Instance) (prompts.Template, error) {
	if strings.TrimSpace(bot.PromptTemplateID) != "" {
		tmpl, err := s.prompts.GetByID(ctx, p.TenantID, bot.PromptTemplateID)
		if err != nil {
			return prompts.Template{}, err
		}
		if !tmpl.IsActive {
			return prompts.Template{}, invalidInput(errors.New("prompt template is inactive"))
		}
		return tmpl, nil
	}
	tmpl, err := s.prompts.GetDefault(ctx, p.TenantID)
	if err == nil {
		return tmpl, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return prompts.Template{}, err
	}
	defaults := prompts.DefaultTemplates()
	return defaults[0], nil
}

func (s *Service) retrieveRAG(ctx context.Context, p, owner auth.Principal, bot Instance, query string) ([]rag.SearchResult, string, error) {
	if s.rag == nil || len(bot.RAGCollectionIDs) == 0 {
		return nil, "", nil
	}
	limit := settingInt(bot.Settings, "rag_limit", 4, 1, 20)
	maxChars := settingInt(bot.Settings, "rag_context_max_chars", 6000, 500, 50000)
	allowed := make(map[string]bool)
	lister, ok := s.rag.(ragCollectionLister)
	if !ok {
		return nil, "", source.ErrDenied
	}
	for index, principal := range []auth.Principal{p, owner} {
		collections, err := lister.ListCollections(ctx, principal)
		if err != nil {
			return nil, "", err
		}
		readable := make(map[string]bool, len(collections))
		for _, collection := range collections {
			readable[collection.ID] = true
		}
		if index == 0 {
			allowed = readable
		} else {
			for id := range allowed {
				if !readable[id] {
					delete(allowed, id)
				}
			}
		}
	}
	results := []rag.SearchResult{}
	for _, collectionID := range bot.RAGCollectionIDs {
		collectionID = strings.TrimSpace(collectionID)
		if collectionID == "" || !allowed[collectionID] {
			continue
		}
		found, err := s.rag.Search(ctx, p, rag.SearchInput{
			CollectionID: collectionID,
			Query:        query,
			Limit:        limit,
		})
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", ErrCollectionNotFound
		}
		if err != nil {
			return nil, "", err
		}
		results = append(results, found...)
	}
	context, err := s.rag.BuildContext(ctx, p, results, maxChars)
	if err != nil {
		return nil, "", err
	}
	return results, context.Text, nil
}

func (s *Service) ensureConversation(ctx context.Context, p auth.Principal, bot Instance, req chatCompletionRequest) (Conversation, error) {
	if strings.TrimSpace(req.ConversationID) != "" {
		conversation, err := s.repo.GetConversation(ctx, p.TenantID, strings.TrimSpace(req.ConversationID))
		if err != nil {
			return Conversation{}, err
		}
		if conversation.ChatbotID != bot.ID {
			return Conversation{}, sql.ErrNoRows
		}
		// Resume is owner-or-admin only: the principal's actor key must match
		// the stored owner unless it carries the platform.admin scope.
		if !auth.HasScope(p, "platform.admin") && conversation.OwnerActorID != actorKey(p) {
			return Conversation{}, sql.ErrNoRows
		}
		return conversation, nil
	}
	c := Conversation{
		TenantID:       p.TenantID,
		ID:             newID("chatconv"),
		ChatbotID:      bot.ID,
		OwnerActorID:   actorKey(p),
		ExternalUserID: strings.TrimSpace(req.User),
	}
	if err := s.repo.CreateConversation(ctx, c); err != nil {
		return Conversation{}, err
	}
	return s.repo.GetConversation(ctx, p.TenantID, c.ID)
}

func (s *Service) recordRequestMessages(ctx context.Context, p auth.Principal, conversationID string, messages []map[string]any) error {
	for _, msg := range messages {
		role := strings.TrimSpace(stringFromAny(msg["role"]))
		if role == "" {
			continue
		}
		if err := s.repo.AddMessage(ctx, Message{
			TenantID:       p.TenantID,
			ID:             newID("chatmsg"),
			ConversationID: conversationID,
			Role:           role,
			Content:        contentToString(msg["content"]),
			OwnerActorID:   p.ActorID,
		}); err != nil {
			return err
		}
	}
	return nil
}

type chatCompletionRequest struct {
	Model          string           `json:"model"`
	Messages       []map[string]any `json:"messages"`
	Stream         bool             `json:"stream,omitempty"`
	ConversationID string           `json:"conversation_id,omitempty"`
	User           string           `json:"user,omitempty"`
	Metadata       map[string]any   `json:"metadata,omitempty"`
}

func extractAssistantContent(raw []byte) string {
	var env map[string]any
	if json.Unmarshal(raw, &env) != nil {
		return ""
	}
	choices, ok := env["choices"].([]any)
	if !ok || len(choices) == 0 {
		return ""
	}
	first, ok := choices[0].(map[string]any)
	if !ok {
		return ""
	}
	msg, ok := first["message"].(map[string]any)
	if !ok {
		return ""
	}
	return contentToString(msg["content"])
}

func appendCompletionMetadata(raw []byte, chatbotID, conversationID string) []byte {
	var env map[string]any
	if json.Unmarshal(raw, &env) != nil {
		return raw
	}
	env["chatbot_id"] = chatbotID
	env["conversation_id"] = conversationID
	out, err := json.Marshal(env)
	if err != nil {
		return raw
	}
	return out
}

func applyRenderedPrompt(messages []map[string]any, userIndex int, rendered prompts.RenderedPrompt) []map[string]any {
	out := cloneMessages(messages)
	if rendered.UserPrompt != "" && userIndex >= 0 && userIndex < len(out) {
		out[userIndex]["content"] = rendered.UserPrompt
	}
	if rendered.SystemPrompt == "" {
		return out
	}
	system := map[string]any{
		"role":    "system",
		"content": rendered.SystemPrompt,
	}
	return append([]map[string]any{system}, out...)
}

func latestUserInput(messages []map[string]any) (string, int) {
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(stringFromAny(messages[i]["role"])), "user") {
			return contentToString(messages[i]["content"]), i
		}
	}
	return "", -1
}

func cloneMessages(messages []map[string]any) []map[string]any {
	out := make([]map[string]any, len(messages))
	for i, msg := range messages {
		copied := make(map[string]any, len(msg))
		for key, value := range msg {
			copied[key] = value
		}
		out[i] = copied
	}
	return out
}

func normalizeExternalID(raw, fallback string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = fallback
	}
	if len(raw) > 128 {
		return "", errors.New("external_id must be 128 characters or fewer")
	}
	for _, r := range raw {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return "", errors.New("external_id may contain only letters, numbers, hyphen, and underscore")
	}
	return raw, nil
}

// normalizeAudience trims and dedupes stored audience entries; empty
// entries are rejected on the invalid-input path. A nil input stays nil so
// Create can apply its owner-only default and Update can treat nil as
// "unchanged".
func normalizeAudience(entries []string) ([]string, error) {
	if entries == nil {
		return nil, nil
	}
	out := make([]string, 0, len(entries))
	seen := map[string]struct{}{}
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil, invalidInput(errors.New("audience entries must be non-empty actor IDs or \"*\""))
		}
		if _, ok := seen[entry]; ok {
			continue
		}
		seen[entry] = struct{}{}
		out = append(out, entry)
	}
	return out, nil
}

func normalizeStringList(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func nonNilMap(v map[string]any) map[string]any {
	if v == nil {
		return map[string]any{}
	}
	return v
}

func mapFromAny(v any) map[string]any {
	src, ok := v.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	out := make(map[string]any, len(src))
	for key, value := range src {
		out[key] = value
	}
	return out
}

func settingInt(settings map[string]any, key string, fallback, minValue, maxValue int) int {
	n := int(floatFromAny(settings[key]))
	if n == 0 {
		n = fallback
	}
	if n < minValue {
		return minValue
	}
	if n > maxValue {
		return maxValue
	}
	return n
}

type invalidInputError struct {
	err error
}

func (e invalidInputError) Error() string { return e.err.Error() }

func (e invalidInputError) Unwrap() error { return e.err }

func (e invalidInputError) Is(target error) bool { return target == ErrInvalidInput }

func invalidInput(err error) error {
	if err == nil {
		return nil
	}
	return invalidInputError{err: err}
}

func stringFromAny(v any) string {
	s, _ := v.(string)
	return s
}

func contentToString(v any) string {
	switch content := v.(type) {
	case string:
		return content
	case nil:
		return ""
	default:
		b, err := json.Marshal(content)
		if err != nil {
			return fmt.Sprint(content)
		}
		return string(b)
	}
}

func floatFromAny(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	default:
		return 0
	}
}
