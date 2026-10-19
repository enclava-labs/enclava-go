package prompts

const DefaultTemplateID = "default-chatbot-assistant"

// DefaultTemplates seeds the built-in per-tenant prompt template. Callers
// must set TenantID before inserting.
func DefaultTemplates() []Template {
	return []Template{
		{
			ID:          DefaultTemplateID,
			Name:        "Default Chatbot Assistant",
			Description: "General-purpose assistant prompt with optional RAG context.",
			SystemPrompt: "You are a concise, accurate assistant. Use the provided context when it is relevant. " +
				"If the context is insufficient, answer from general knowledge and say when you are uncertain.\n\n{{.rag_context}}",
			UserPrompt: "{{.user_input}}",
			VariablesSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			IsDefault: true,
			IsActive:  true,
		},
	}
}
