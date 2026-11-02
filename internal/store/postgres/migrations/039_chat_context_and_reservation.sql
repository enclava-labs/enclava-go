ALTER TABLE messages ADD COLUMN status TEXT NOT NULL DEFAULT 'complete'
    CHECK (status IN ('reserved', 'complete', 'interrupted'));

UPDATE prompt_templates
SET system_prompt = 'You are a concise, accurate assistant. Use the provided context when it is relevant. If the context is insufficient, answer from general knowledge and say when you are uncertain.',
    user_prompt = E'{{if .rag_context}}Context:\n{{.rag_context}}\n\n{{end}}{{.user_input}}'
WHERE id = 'default-chatbot-assistant'
  AND system_prompt = E'You are a concise, accurate assistant. Use the provided context when it is relevant. If the context is insufficient, answer from general knowledge and say when you are uncertain.\n\n{{.rag_context}}'
  AND user_prompt = '{{.user_input}}';
