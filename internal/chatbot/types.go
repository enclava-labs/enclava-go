package chatbot

import (
	"errors"

	"enclava-go/internal/store/sqlite"
)

var (
	ErrDisabled           = errors.New("chatbot is disabled")
	ErrModelNotAllowed    = errors.New("model is not allowed for this API key")
	ErrChatbotNotAllowed  = errors.New("chatbot is not allowed for this API key")
	ErrCollectionNotFound = errors.New("knowledge collection was not found")
	ErrInvalidInput       = errors.New("invalid chatbot input")
)

type Access struct {
	APIKeyID           int64
	IsAdmin            bool
	AllowedModels      map[string]struct{}
	AllowedChatbots    map[string]struct{}
	AllowedAgents      map[string]struct{}
	AllowedModelsAll   bool
	AllowedChatbotsAll bool
	AllowedAgentsAll   bool
}

type CreateChatbotInput struct {
	Name             string         `json:"name"`
	Description      string         `json:"description,omitempty"`
	PromptTemplateID string         `json:"prompt_template_id,omitempty"`
	ProviderID       string         `json:"provider_id,omitempty"`
	Model            string         `json:"model"`
	RAGCollectionIDs []string       `json:"rag_collection_ids,omitempty"`
	Settings         map[string]any `json:"settings,omitempty"`
	ExternalID       string         `json:"external_id,omitempty"`
	IsEnabled        *bool          `json:"is_enabled,omitempty"`
}

type UpdateChatbotInput struct {
	Name             *string        `json:"name,omitempty"`
	Description      *string        `json:"description,omitempty"`
	PromptTemplateID *string        `json:"prompt_template_id,omitempty"`
	ProviderID       *string        `json:"provider_id,omitempty"`
	Model            *string        `json:"model,omitempty"`
	RAGCollectionIDs []string       `json:"rag_collection_ids,omitempty"`
	Settings         map[string]any `json:"settings,omitempty"`
	ExternalID       *string        `json:"external_id,omitempty"`
	IsEnabled        *bool          `json:"is_enabled,omitempty"`
}

type CompletionOutput struct {
	Response       []byte
	Conversation   sqlite.ChatbotConversation
	Chatbot        sqlite.ChatbotInstance
	Usage          sqlite.UsageRecord
	RAGResultCount int
}

type PreparedCompletion struct {
	UpstreamRequest []byte
	Conversation    sqlite.ChatbotConversation
	Chatbot         sqlite.ChatbotInstance
	APIKeyID        int64
	Model           string
	RAGResultCount  int
}
