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
	// ErrInvalidCursor marks a malformed conversation-list keyset cursor; the
	// service maps it onto the 400-class invalid-input path, never ignores it.
	ErrInvalidCursor = errors.New("invalid conversation cursor")
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
	// Audience is stored authorization (D-07): actor IDs or "*" tenant-wide.
	// It intersects with resource grants in chatbotAllowed — owner and
	// platform.admin bypass the audience term, never the grant check.
	Audience     []string  `json:"audience,omitempty"`
	ExternalID   string    `json:"external_id"`
	IsEnabled    bool      `json:"is_enabled"`
	OwnerActorID string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Conversation is the tenant-scoped conversations row. OwnerActorID carries
// the initiating actor's identity — resume is owner-only. Title is the
// member-set name (PATCH); empty falls back to the derived preview.
type Conversation struct {
	TenantID       string    `json:"-"`
	ID             string    `json:"id"`
	ChatbotID      string    `json:"chatbot_id"`
	OwnerActorID   string    `json:"-"`
	ExternalUserID string    `json:"external_user_id,omitempty"`
	Title          string    `json:"title,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// Message is a tenant-scoped messages row. Token accounting lives on the
// usage record, not the message row. RunID links a turn to its durable
// agent run (OA-14); Steps is the ordered tool-step projection, populated
// only on assistant turns at read time — never persisted.
type Message struct {
	TenantID       string             `json:"-"`
	ID             string             `json:"id"`
	ConversationID string             `json:"conversation_id"`
	Role           string             `json:"role"`
	Content        string             `json:"content"`
	Status         string             `json:"status"`
	OwnerActorID   string             `json:"-"`
	RunID          string             `json:"run_id,omitempty"`
	Steps          []AssistantRunStep `json:"steps,omitempty"`
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

// ConversationSummary is one own-conversation list row (OA-13). Title is the
// member-set name when one exists, else the derived preview of the first
// VISIBLE user message; UpdatedAt is the last message timestamp.
type ConversationSummary struct {
	ID            string    `json:"id"`
	ChatbotID     string    `json:"assistant_id"`
	AssistantName string    `json:"assistant_name"`
	Title         string    `json:"title"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type AssistantRunInput struct {
	ConversationID string `json:"conversation_id,omitempty"`
	Input          string `json:"input"`
	// EvidenceIDs hand off server-issued evidence IDs ("ask about this"):
	// resolved and authorized server-side before the run starts — client
	// passage text or source refs are never accepted.
	EvidenceIDs []string `json:"evidence_ids,omitempty"`
}

type AssistantRun struct {
	ID             string `json:"id"`
	TenantID       string `json:"-"`
	JobID          string `json:"job_id,omitempty"`
	OwnerActorID   string `json:"-"`
	ChatbotID      string `json:"chatbot_id"`
	ConversationID string `json:"conversation_id"`
	Status         string `json:"status"`
	Input          string `json:"input"`
	Output         string `json:"output,omitempty"`
	Error          string `json:"error,omitempty"`
	// EndpointID/Model are the route actually dispatched, persisted from
	// inference.Result — never the configured provider label.
	EndpointID            string                     `json:"endpoint_id,omitempty"`
	Model                 string                     `json:"model,omitempty"`
	Steps                 []AssistantRunStep         `json:"steps,omitempty"`
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

// AssistantRunStep is one ordered tool-step projection on a run and on the
// assistant turn it belongs to (OA-14). Input/Output carry bounded
// tool-level payloads only — model request bodies and system prompts never
// reach a step row; when the run's evidence is no longer readable the
// source-bearing fields are cleared and metadata alone survives.
type AssistantRunStep struct {
	Number    int        `json:"number"`
	Kind      string     `json:"kind"`
	Name      string     `json:"name"`
	Status    string     `json:"status"`
	Input     string     `json:"input,omitempty"`
	Output    string     `json:"output,omitempty"`
	Error     string     `json:"error,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

type CreateChatbotInput struct {
	Name             string         `json:"name"`
	Description      string         `json:"description,omitempty"`
	PromptTemplateID string         `json:"prompt_template_id,omitempty"`
	ProviderID       string         `json:"provider_id,omitempty"`
	Model            string         `json:"model"`
	RAGCollectionIDs []string       `json:"rag_collection_ids,omitempty"`
	Settings         map[string]any `json:"settings,omitempty"`
	// Audience: nil omits (create defaults to owner-only), explicit []
	// locks the assistant to owner+platform.admin, "*" is tenant-wide.
	Audience   []string `json:"audience,omitempty"`
	ExternalID string   `json:"external_id,omitempty"`
	IsEnabled  *bool    `json:"is_enabled,omitempty"`
}

type UpdateChatbotInput struct {
	Name             *string        `json:"name,omitempty"`
	Description      *string        `json:"description,omitempty"`
	PromptTemplateID *string        `json:"prompt_template_id,omitempty"`
	ProviderID       *string        `json:"provider_id,omitempty"`
	Model            *string        `json:"model,omitempty"`
	RAGCollectionIDs []string       `json:"rag_collection_ids,omitempty"`
	Settings         map[string]any `json:"settings,omitempty"`
	// Audience nil = unchanged; non-nil replaces the stored audience after
	// trim/dedupe normalization (empty entries are rejected).
	Audience   []string `json:"audience,omitempty"`
	ExternalID *string  `json:"external_id,omitempty"`
	IsEnabled  *bool    `json:"is_enabled,omitempty"`
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
	Request            inference.Request
	Conversation       Conversation
	AssistantMessageID string
	Chatbot            Instance
	Model              string
	RAGResultCount     int
	EvidenceIDs        []string
	InputRefs          []source.ObjectRef
}
