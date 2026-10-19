package prompts

import "time"

// Template is the tenant-scoped prompt_templates domain row. TenantID and
// OwnerActorID are server-owned authorization fields — never serialized to
// clients.
type Template struct {
	TenantID        string         `json:"-"`
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	Description     string         `json:"description,omitempty"`
	SystemPrompt    string         `json:"system_prompt"`
	UserPrompt      string         `json:"user_prompt"`
	VariablesSchema map[string]any `json:"variables_schema,omitempty"`
	IsDefault       bool           `json:"is_default"`
	IsActive        bool           `json:"is_active"`
	OwnerActorID    string         `json:"-"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

type CreateTemplateInput struct {
	ID              string         `json:"id,omitempty"`
	Name            string         `json:"name"`
	Description     string         `json:"description,omitempty"`
	SystemPrompt    string         `json:"system_prompt"`
	UserPrompt      string         `json:"user_prompt"`
	VariablesSchema map[string]any `json:"variables_schema,omitempty"`
	IsDefault       bool           `json:"is_default,omitempty"`
	IsActive        *bool          `json:"is_active,omitempty"`
}

type UpdateTemplateInput struct {
	Name            *string        `json:"name,omitempty"`
	Description     *string        `json:"description,omitempty"`
	SystemPrompt    *string        `json:"system_prompt,omitempty"`
	UserPrompt      *string        `json:"user_prompt,omitempty"`
	VariablesSchema map[string]any `json:"variables_schema,omitempty"`
	IsDefault       *bool          `json:"is_default,omitempty"`
	IsActive        *bool          `json:"is_active,omitempty"`
}

type RenderInput struct {
	UserInput  string
	RAGContext string
	Variables  map[string]any
}

type RenderedPrompt struct {
	SystemPrompt string
	UserPrompt   string
}
