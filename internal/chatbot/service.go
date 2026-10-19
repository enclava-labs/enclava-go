package chatbot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"enclava-go/internal/llm"
	"enclava-go/internal/prompts"
	"enclava-go/internal/rag"
	"enclava-go/internal/store/sqlite"
)

type Repository interface {
	Create(ctx context.Context, b sqlite.ChatbotInstance) error
	List(ctx context.Context) ([]sqlite.ChatbotInstance, error)
	GetByID(ctx context.Context, id string) (sqlite.ChatbotInstance, error)
	GetByExternalID(ctx context.Context, externalID string) (sqlite.ChatbotInstance, error)
	Update(ctx context.Context, b sqlite.ChatbotInstance) error
	Delete(ctx context.Context, id string) error
	CreateConversation(ctx context.Context, c sqlite.ChatbotConversation) error
	GetConversation(ctx context.Context, id string) (sqlite.ChatbotConversation, error)
	AddMessage(ctx context.Context, m sqlite.ChatbotMessage) error
}

type PromptRepository interface {
	GetByID(ctx context.Context, id string) (sqlite.PromptTemplate, error)
	GetDefault(ctx context.Context) (sqlite.PromptTemplate, error)
}

type RAGSearcher interface {
	Search(ctx context.Context, access rag.Access, input rag.SearchInput) ([]rag.SearchResult, error)
}

type UsageRecorder interface {
	Record(ctx context.Context, rec sqlite.UsageRecord) error
}

type Service struct {
	repo      Repository
	prompts   PromptRepository
	rag       RAGSearcher
	provider  llm.Provider
	usageRepo UsageRecorder
}

func NewService(repo Repository, promptRepo PromptRepository, ragSvc RAGSearcher, provider llm.Provider, usageRepo UsageRecorder) *Service {
	return &Service{
		repo:      repo,
		prompts:   promptRepo,
		rag:       ragSvc,
		provider:  provider,
		usageRepo: usageRepo,
	}
}

func (s *Service) Create(ctx context.Context, input CreateChatbotInput) (sqlite.ChatbotInstance, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return sqlite.ChatbotInstance{}, errors.New("name is required")
	}
	model := strings.TrimSpace(input.Model)
	if model == "" {
		return sqlite.ChatbotInstance{}, errors.New("model is required")
	}
	id := newID("chatbot")
	externalID, err := normalizeExternalID(input.ExternalID, id)
	if err != nil {
		return sqlite.ChatbotInstance{}, err
	}
	enabled := true
	if input.IsEnabled != nil {
		enabled = *input.IsEnabled
	}
	b := sqlite.ChatbotInstance{
		ID:               id,
		Name:             name,
		Description:      strings.TrimSpace(input.Description),
		PromptTemplateID: strings.TrimSpace(input.PromptTemplateID),
		ProviderID:       strings.TrimSpace(input.ProviderID),
		Model:            model,
		RAGCollectionIDs: normalizeStringList(input.RAGCollectionIDs),
		Settings:         nonNilMap(input.Settings),
		ExternalID:       externalID,
		IsEnabled:        enabled,
	}
	if err := s.repo.Create(ctx, b); err != nil {
		return sqlite.ChatbotInstance{}, err
	}
	return s.repo.GetByID(ctx, id)
}

func (s *Service) List(ctx context.Context) ([]sqlite.ChatbotInstance, error) {
	return s.repo.List(ctx)
}

func (s *Service) Get(ctx context.Context, id string) (sqlite.ChatbotInstance, error) {
	return s.repo.GetByID(ctx, strings.TrimSpace(id))
}

func (s *Service) Update(ctx context.Context, id string, input UpdateChatbotInput) (sqlite.ChatbotInstance, error) {
	b, err := s.repo.GetByID(ctx, strings.TrimSpace(id))
	if err != nil {
		return sqlite.ChatbotInstance{}, err
	}
	if input.Name != nil {
		b.Name = strings.TrimSpace(*input.Name)
	}
	if b.Name == "" {
		return sqlite.ChatbotInstance{}, errors.New("name is required")
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
		return sqlite.ChatbotInstance{}, errors.New("model is required")
	}
	if input.RAGCollectionIDs != nil {
		b.RAGCollectionIDs = normalizeStringList(input.RAGCollectionIDs)
	}
	if input.Settings != nil {
		b.Settings = nonNilMap(input.Settings)
	}
	if input.ExternalID != nil {
		externalID, err := normalizeExternalID(*input.ExternalID, b.ID)
		if err != nil {
			return sqlite.ChatbotInstance{}, err
		}
		b.ExternalID = externalID
	}
	if input.IsEnabled != nil {
		b.IsEnabled = *input.IsEnabled
	}
	if err := s.repo.Update(ctx, b); err != nil {
		return sqlite.ChatbotInstance{}, err
	}
	return s.repo.GetByID(ctx, b.ID)
}

func (s *Service) Delete(ctx context.Context, id string) error {
	return s.repo.Delete(ctx, strings.TrimSpace(id))
}

func (s *Service) ExternalModels(ctx context.Context, access Access, chatbotRef string) (llm.ModelsResponse, error) {
	bot, err := s.getByRef(ctx, chatbotRef)
	if err != nil {
		return llm.ModelsResponse{}, err
	}
	if !bot.IsEnabled {
		return llm.ModelsResponse{}, ErrDisabled
	}
	if !chatbotAllowed(access, bot) {
		return llm.ModelsResponse{}, ErrChatbotNotAllowed
	}
	if !modelAllowed(access, bot.Model) {
		return llm.ModelsResponse{}, ErrModelNotAllowed
	}
	models, err := s.provider.ListModels(ctx)
	if err != nil {
		return llm.ModelsResponse{}, err
	}
	model := llm.Model{ID: bot.Model, Object: "model"}
	for _, candidate := range models.Data {
		if candidate.ID == bot.Model {
			model = candidate
			break
		}
	}
	return llm.ModelsResponse{
		Object: "list",
		Data:   []llm.Model{model},
	}, nil
}

func (s *Service) CreateChatCompletion(ctx context.Context, access Access, chatbotRef string, rawRequest []byte) (CompletionOutput, error) {
	prepared, err := s.PrepareChatCompletion(ctx, access, chatbotRef, rawRequest, false)
	if err != nil {
		return CompletionOutput{}, err
	}
	resp, err := s.provider.CreateChatCompletion(ctx, prepared.UpstreamRequest)
	if err != nil {
		s.RecordFailure(ctx, prepared, providerErrorStatus(err))
		return CompletionOutput{}, err
	}
	usage := parseUsage(resp)
	assistantContent := extractAssistantContent(resp)
	record := s.RecordCompletion(ctx, prepared, assistantContent, usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, 200)
	resp = appendCompletionMetadata(resp, prepared.Chatbot.ID, prepared.Conversation.ID)
	return CompletionOutput{
		Response:       resp,
		Conversation:   prepared.Conversation,
		Chatbot:        prepared.Chatbot,
		Usage:          record,
		RAGResultCount: prepared.RAGResultCount,
	}, nil
}

func (s *Service) PrepareChatCompletion(ctx context.Context, access Access, chatbotRef string, rawRequest []byte, stream bool) (PreparedCompletion, error) {
	bot, err := s.getByRef(ctx, chatbotRef)
	if err != nil {
		return PreparedCompletion{}, err
	}
	if !bot.IsEnabled {
		return PreparedCompletion{}, ErrDisabled
	}
	if !chatbotAllowed(access, bot) {
		return PreparedCompletion{}, ErrChatbotNotAllowed
	}
	if !modelAllowed(access, bot.Model) {
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
	originalMessages := cloneMessages(req.Messages)
	userInput, userIndex := latestUserInput(req.Messages)
	if strings.TrimSpace(userInput) == "" || userIndex < 0 {
		return PreparedCompletion{}, invalidInput(errors.New("at least one user message is required"))
	}

	ragResults, ragContext, err := s.retrieveRAG(ctx, access, bot, userInput)
	if err != nil {
		return PreparedCompletion{}, err
	}
	rendered, err := s.renderPrompt(ctx, bot, userInput, ragContext, req.Metadata)
	if err != nil {
		return PreparedCompletion{}, err
	}
	providerMessages := applyRenderedPrompt(req.Messages, userIndex, rendered)
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

	conversation, err := s.ensureConversation(ctx, access, bot, req, userInput, len(ragResults))
	if err != nil {
		return PreparedCompletion{}, err
	}
	if err := s.recordRequestMessages(ctx, conversation.ID, originalMessages, bot.Model); err != nil {
		return PreparedCompletion{}, err
	}
	return PreparedCompletion{
		UpstreamRequest: upstreamRequest,
		Conversation:    conversation,
		Chatbot:         bot,
		APIKeyID:        access.APIKeyID,
		Model:           bot.Model,
		RAGResultCount:  len(ragResults),
	}, nil
}

func (s *Service) RecordCompletion(ctx context.Context, prepared PreparedCompletion, assistantContent string, promptTokens, completionTokens, totalTokens int64, statusCode int) sqlite.UsageRecord {
	if totalTokens == 0 {
		totalTokens = promptTokens + completionTokens
	}
	_ = s.repo.AddMessage(ctx, sqlite.ChatbotMessage{
		ID:               newID("chatmsg"),
		ConversationID:   prepared.Conversation.ID,
		Role:             "assistant",
		Content:          assistantContent,
		Model:            prepared.Model,
		PromptTokens:     int(promptTokens),
		CompletionTokens: int(completionTokens),
		TotalTokens:      int(totalTokens),
		Metadata: map[string]any{
			"source":           "provider",
			"rag_result_count": prepared.RAGResultCount,
		},
	})
	record := sqlite.UsageRecord{
		ID:               newID("usage"),
		APIKeyID:         0,
		Endpoint:         "/api/v1/chatbot/external/{chatbot_id}/chat/completions",
		Model:            prepared.Model,
		ChatbotID:        prepared.Chatbot.ID,
		ProviderID:       prepared.Chatbot.ProviderID,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      totalTokens,
		CostCents:        0,
		StatusCode:       statusCode,
	}
	if prepared.Conversation.APIKeyID != nil {
		record.APIKeyID = *prepared.Conversation.APIKeyID
	}
	if prepared.APIKeyID > 0 {
		record.APIKeyID = prepared.APIKeyID
	}
	if s.usageRepo != nil && record.APIKeyID > 0 {
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = s.usageRepo.Record(recordCtx, record)
	}
	return record
}

func (s *Service) RecordFailure(ctx context.Context, prepared PreparedCompletion, statusCode int) sqlite.UsageRecord {
	record := sqlite.UsageRecord{
		ID:         newID("usage"),
		APIKeyID:   prepared.APIKeyID,
		Endpoint:   "/api/v1/chatbot/external/{chatbot_id}/chat/completions",
		Model:      prepared.Model,
		ChatbotID:  prepared.Chatbot.ID,
		ProviderID: prepared.Chatbot.ProviderID,
		StatusCode: statusCode,
	}
	if prepared.Conversation.APIKeyID != nil {
		record.APIKeyID = *prepared.Conversation.APIKeyID
	}
	if prepared.APIKeyID > 0 {
		record.APIKeyID = prepared.APIKeyID
	}
	if s.usageRepo != nil && record.APIKeyID > 0 {
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = s.usageRepo.Record(recordCtx, record)
	}
	return record
}

func (s *Service) getByRef(ctx context.Context, ref string) (sqlite.ChatbotInstance, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return sqlite.ChatbotInstance{}, sql.ErrNoRows
	}
	bot, err := s.repo.GetByID(ctx, ref)
	if err == nil {
		return bot, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return sqlite.ChatbotInstance{}, err
	}
	return s.repo.GetByExternalID(ctx, ref)
}

func (s *Service) renderPrompt(ctx context.Context, bot sqlite.ChatbotInstance, userInput, ragContext string, metadata map[string]any) (prompts.RenderedPrompt, error) {
	tmpl, err := s.promptTemplate(ctx, bot)
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

func (s *Service) promptTemplate(ctx context.Context, bot sqlite.ChatbotInstance) (sqlite.PromptTemplate, error) {
	if strings.TrimSpace(bot.PromptTemplateID) != "" {
		tmpl, err := s.prompts.GetByID(ctx, bot.PromptTemplateID)
		if err != nil {
			return sqlite.PromptTemplate{}, err
		}
		if !tmpl.IsActive {
			return sqlite.PromptTemplate{}, invalidInput(errors.New("prompt template is inactive"))
		}
		return tmpl, nil
	}
	tmpl, err := s.prompts.GetDefault(ctx)
	if err == nil {
		return tmpl, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return sqlite.PromptTemplate{}, err
	}
	defaults := prompts.DefaultTemplates()
	return defaults[0], nil
}

func (s *Service) retrieveRAG(ctx context.Context, access Access, bot sqlite.ChatbotInstance, query string) ([]rag.SearchResult, string, error) {
	if s.rag == nil || len(bot.RAGCollectionIDs) == 0 {
		return nil, "", nil
	}
	limit := settingInt(bot.Settings, "rag_limit", 4, 1, 20)
	maxChars := settingInt(bot.Settings, "rag_context_max_chars", 6000, 500, 50000)
	results := []rag.SearchResult{}
	for _, collectionID := range bot.RAGCollectionIDs {
		collectionID = strings.TrimSpace(collectionID)
		if collectionID == "" {
			continue
		}
		found, err := s.rag.Search(ctx, rag.Access{APIKeyID: access.APIKeyID, IsAdmin: true}, rag.SearchInput{
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
	return results, buildRAGContext(results, maxChars), nil
}

func (s *Service) ensureConversation(ctx context.Context, access Access, bot sqlite.ChatbotInstance, req chatCompletionRequest, title string, ragResultCount int) (sqlite.ChatbotConversation, error) {
	if strings.TrimSpace(req.ConversationID) != "" {
		conversation, err := s.repo.GetConversation(ctx, strings.TrimSpace(req.ConversationID))
		if err != nil {
			return sqlite.ChatbotConversation{}, err
		}
		if conversation.ChatbotID != bot.ID {
			return sqlite.ChatbotConversation{}, sql.ErrNoRows
		}
		if !access.IsAdmin {
			if conversation.APIKeyID == nil || *conversation.APIKeyID != access.APIKeyID {
				return sqlite.ChatbotConversation{}, sql.ErrNoRows
			}
		}
		return conversation, nil
	}
	apiKeyID := access.APIKeyID
	c := sqlite.ChatbotConversation{
		ID:             newID("chatconv"),
		ChatbotID:      bot.ID,
		APIKeyID:       &apiKeyID,
		ExternalUserID: strings.TrimSpace(req.User),
		Title:          truncate(title, 120),
		Metadata: map[string]any{
			"rag_result_count": ragResultCount,
		},
	}
	if err := s.repo.CreateConversation(ctx, c); err != nil {
		return sqlite.ChatbotConversation{}, err
	}
	return s.repo.GetConversation(ctx, c.ID)
}

func (s *Service) recordRequestMessages(ctx context.Context, conversationID string, messages []map[string]any, model string) error {
	for _, msg := range messages {
		role := strings.TrimSpace(stringFromAny(msg["role"]))
		if role == "" {
			continue
		}
		if err := s.repo.AddMessage(ctx, sqlite.ChatbotMessage{
			ID:             newID("chatmsg"),
			ConversationID: conversationID,
			Role:           role,
			Content:        contentToString(msg["content"]),
			Model:          model,
			Metadata: map[string]any{
				"source": "request",
			},
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

type parsedUsage struct {
	Model            string
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
}

func parseUsage(raw []byte) parsedUsage {
	var env map[string]any
	if json.Unmarshal(raw, &env) != nil {
		return parsedUsage{}
	}
	out := parsedUsage{}
	if model, ok := env["model"].(string); ok {
		out.Model = model
	}
	usageRaw, ok := env["usage"].(map[string]any)
	if !ok {
		return out
	}
	out.PromptTokens = int64(floatFromAny(usageRaw["prompt_tokens"]))
	out.CompletionTokens = int64(floatFromAny(usageRaw["completion_tokens"]))
	out.TotalTokens = int64(floatFromAny(usageRaw["total_tokens"]))
	return out
}

func providerErrorStatus(err error) int {
	var upstream *llm.UpstreamError
	if errors.As(err, &upstream) && upstream.StatusCode > 0 {
		return upstream.StatusCode
	}
	return http.StatusBadGateway
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

func buildRAGContext(results []rag.SearchResult, maxChars int) string {
	if len(results) == 0 || maxChars <= 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Relevant context:\n")
	for i, result := range results {
		if b.Len() >= maxChars {
			break
		}
		content := strings.TrimSpace(result.Content)
		if content == "" {
			continue
		}
		source := strings.TrimSpace(result.FileName)
		if source == "" {
			source = result.DocumentID
		}
		line := fmt.Sprintf("[%d] %s\n%s\n\n", i+1, source, content)
		remaining := maxChars - b.Len()
		if len(line) > remaining {
			line = line[:remaining]
		}
		b.WriteString(line)
	}
	return strings.TrimSpace(b.String())
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

func modelAllowed(access Access, model string) bool {
	if access.AllowedModelsAll {
		return true
	}
	if _, ok := access.AllowedModels["*"]; ok {
		return true
	}
	_, ok := access.AllowedModels[model]
	return ok
}

func chatbotAllowed(access Access, bot sqlite.ChatbotInstance) bool {
	if access.AllowedChatbotsAll || access.AllowedAgentsAll {
		return true
	}
	if _, ok := access.AllowedChatbots["*"]; ok {
		return true
	}
	if _, ok := access.AllowedAgents["*"]; ok {
		return true
	}
	if _, ok := access.AllowedChatbots[bot.ID]; ok {
		return true
	}
	if _, ok := access.AllowedChatbots[bot.ExternalID]; ok {
		return true
	}
	if _, ok := access.AllowedAgents[bot.ID]; ok {
		return true
	}
	if _, ok := access.AllowedAgents[bot.ExternalID]; ok {
		return true
	}
	return false
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

func truncate(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) <= max {
		return value
	}
	return strings.TrimSpace(value[:max])
}
