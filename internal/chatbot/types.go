package chatbot

import (
	"errors"
	"time"

	"enclava-go/internal/billing"
	"enclava-go/internal/inference"
)

var (
	ErrDisabled           = errors.New("chatbot is disabled")
	ErrModelNotAllowed    = errors.New("model is not allowed for this API key")
	ErrChatbotNotAllowed  = errors.New("chatbot is not allowed for this API key")
	ErrCollectionNotFound = errors.New("knowledge collection was not found")
	ErrInvalidInput       = errors.New("invalid chatbot input")
)

// Instance is the tenant-scoped chatbots domain row. TenantID and
// OwnerActorID are server-owned authorization fields — never serialized.
type Instance struct {
	TenantID         string         `json:"-"`
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	Description      string         `json:"description,omitempty"`
	PromptTemplateID string         `json:"prompt_template_id,omitempty"`
	ProviderID       string         `json:"provider_id,omitempty"`
	Model            string         `json:"model"`
	RAGCollectionIDs []string       `json:"rag_collection_ids,omitempty"`
	Settings         map[string]any `json:"settings,omitempty"`
	ExternalID       string         `json:"external_id"`
	IsEnabled        bool           `json:"is_enabled"`
	OwnerActorID     string         `json:"-"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
}

// Conversation is the tenant-scoped conversations row. OwnerActorID carries
// the initiating actor's identity — resume is owner-or-admin only.
type Conversation struct {
	TenantID       string    `json:"-"`
	ID             string    `json:"id"`
	ChatbotID      string    `json:"chatbot_id"`
	OwnerActorID   string    `json:"-"`
	ExternalUserID string    `json:"external_user_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// Message is a tenant-scoped messages row. Token accounting lives on the
// usage record, not the message row.
type Message struct {
	TenantID       string    `json:"-"`
	ID             string    `json:"id"`
	ConversationID string    `json:"conversation_id"`
	Role           string    `json:"role"`
	Content        string    `json:"content"`
	OwnerActorID   string    `json:"-"`
	CreatedAt      time.Time `json:"created_at"`
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
	Conversation   Conversation
	Chatbot        Instance
	Usage          *billing.UsageRecord
	RAGResultCount int
}

type PreparedCompletion struct {
	// Request is the executor input; dispatch goes only through
	// Service.ExecutePrepared — never a direct provider call.
	Request        inference.Request
	Conversation   Conversation
	Chatbot        Instance
	Model          string
	RAGResultCount int
}
