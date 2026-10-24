package chatbot

import (
	"encoding/json"
	"errors"
	"time"

	"enclava-go/internal/billing"
	"enclava-go/internal/inference"
	"enclava-go/internal/source"
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
	TenantID       string             `json:"-"`
	ID             string             `json:"id"`
	ConversationID string             `json:"conversation_id"`
	Role           string             `json:"role"`
	Content        string             `json:"content"`
	OwnerActorID   string             `json:"-"`
	EvidenceIDs    []string           `json:"evidence_ids,omitempty"`
	InputRefs      []source.ObjectRef `json:"-"`
	CreatedAt      time.Time          `json:"created_at"`
}

// InputRef links generated conversation text to the evidence whose current
// authorization must still hold before that text can be reused.
type InputRef struct {
	EvidenceID string           `json:"evidence_id"`
	ObjectRef  source.ObjectRef `json:"object_ref"`
}

type ConversationView struct {
	Conversation Conversation `json:"conversation"`
	Messages     []Message    `json:"messages"`
}

type AssistantRunInput struct {
	ConversationID string `json:"conversation_id,omitempty"`
	Input          string `json:"input"`
}

type AssistantRun struct {
	ID                    string                     `json:"id"`
	TenantID              string                     `json:"-"`
	JobID                 string                     `json:"job_id,omitempty"`
	OwnerActorID          string                     `json:"-"`
	ChatbotID             string                     `json:"chatbot_id"`
	ConversationID        string                     `json:"conversation_id"`
	Status                string                     `json:"status"`
	Input                 string                     `json:"input"`
	Output                string                     `json:"output,omitempty"`
	Error                 string                     `json:"error,omitempty"`
	ConfigSnapshot        json.RawMessage            `json:"-"`
	PromptTemplateID      string                     `json:"prompt_template_id,omitempty"`
	PromptTemplateVersion int                        `json:"prompt_template_version,omitempty"`
	EvidenceIDs           []string                   `json:"evidence_ids,omitempty"`
	InputRefs             []source.ObjectRef         `json:"-"`
	Restrictions          []source.SourceRestriction `json:"-"`
	FenceToken            int64                      `json:"-"`
	CreatedAt             time.Time                  `json:"created_at"`
	UpdatedAt             time.Time                  `json:"updated_at"`
}

type AssistantRunAttempt struct {
	TenantID  string    `json:"-"`
	RunID     string    `json:"run_id"`
	Number    int       `json:"number"`
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	Input     string    `json:"input,omitempty"`
	Output    string    `json:"output,omitempty"`
	Error     string    `json:"error,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
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
	EvidenceIDs    []string
	InputRefs      []source.ObjectRef
}
