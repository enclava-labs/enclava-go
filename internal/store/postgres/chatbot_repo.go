package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"enclava-go/internal/chatbot"
	"enclava-go/internal/inference"
)

// ChatbotRepo implements chatbot.Repository over the tenant-scoped chatbots,
// conversations and messages domain tables. Every statement is pinned to
// tenant_id; missing, malformed-tenant and cross-tenant lookups all read as
// sql.ErrNoRows so unknown and forbidden share one outcome.
type ChatbotRepo struct {
	db *DB
}

func NewChatbotRepo(db *DB) *ChatbotRepo {
	return &ChatbotRepo{db: db}
}

var _ chatbot.Repository = (*ChatbotRepo)(nil)

const chatbotSelect = `
	SELECT tenant_id::text, id, external_id, name, description,
	       COALESCE(prompt_template_id, ''), COALESCE(endpoint_id, ''), model,
	       collection_ids, settings, is_enabled, COALESCE(owner_actor_id, ''), created_at
	FROM chatbots`

type chatbotScanner interface {
	Scan(dest ...any) error
}

func scanChatbot(s chatbotScanner) (chatbot.Instance, error) {
	var b chatbot.Instance
	var collections, settings []byte
	err := s.Scan(&b.TenantID, &b.ID, &b.ExternalID, &b.Name, &b.Description,
		&b.PromptTemplateID, &b.ProviderID, &b.Model,
		&collections, &settings, &b.IsEnabled, &b.OwnerActorID, &b.CreatedAt)
	if err != nil {
		return chatbot.Instance{}, err
	}
	b.RAGCollectionIDs = scanJSONList(collections)
	b.Settings = map[string]any{}
	_ = json.Unmarshal(settings, &b.Settings)
	b.UpdatedAt = b.CreatedAt
	return b, nil
}

func (r *ChatbotRepo) Create(ctx context.Context, b chatbot.Instance) error {
	collections, err := jsonList(b.RAGCollectionIDs)
	if err != nil {
		return fmt.Errorf("marshal chatbot collection ids: %w", err)
	}
	settings, err := json.Marshal(map[string]any(b.Settings))
	if err != nil {
		return fmt.Errorf("marshal chatbot settings: %w", err)
	}
	_, err = r.db.SQL.ExecContext(ctx, `
		INSERT INTO chatbots
			(tenant_id, id, external_id, name, description, prompt_template_id,
			 endpoint_id, model, collection_ids, settings, is_enabled, owner_actor_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		b.TenantID, b.ID, b.ExternalID, b.Name, b.Description,
		nullableText(b.PromptTemplateID), nullableText(b.ProviderID), b.Model,
		collections, settings, b.IsEnabled, nullableText(b.OwnerActorID))
	if err != nil {
		return fmt.Errorf("create chatbot: %w", err)
	}
	return nil
}

func (r *ChatbotRepo) List(ctx context.Context, tenantID string) ([]chatbot.Instance, error) {
	rows, err := r.db.SQL.QueryContext(ctx,
		chatbotSelect+` WHERE tenant_id = $1 ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list chatbots: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []chatbot.Instance{}
	for rows.Next() {
		b, err := scanChatbot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate chatbots: %w", err)
	}
	return out, nil
}

func (r *ChatbotRepo) GetByID(ctx context.Context, tenantID, id string) (chatbot.Instance, error) {
	b, err := scanChatbot(r.db.SQL.QueryRowContext(ctx,
		chatbotSelect+` WHERE tenant_id = $1 AND id = $2`, tenantID, id))
	return chatbotGetResult(b, err)
}

func (r *ChatbotRepo) GetByExternalID(ctx context.Context, tenantID, externalID string) (chatbot.Instance, error) {
	b, err := scanChatbot(r.db.SQL.QueryRowContext(ctx,
		chatbotSelect+` WHERE tenant_id = $1 AND external_id = $2`, tenantID, externalID))
	return chatbotGetResult(b, err)
}

func chatbotGetResult(b chatbot.Instance, err error) (chatbot.Instance, error) {
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return chatbot.Instance{}, sql.ErrNoRows
	}
	if err != nil {
		return chatbot.Instance{}, fmt.Errorf("get chatbot: %w", err)
	}
	return b, nil
}

func (r *ChatbotRepo) Update(ctx context.Context, b chatbot.Instance) error {
	collections, err := jsonList(b.RAGCollectionIDs)
	if err != nil {
		return fmt.Errorf("marshal chatbot collection ids: %w", err)
	}
	settings, err := json.Marshal(map[string]any(b.Settings))
	if err != nil {
		return fmt.Errorf("marshal chatbot settings: %w", err)
	}
	res, err := r.db.SQL.ExecContext(ctx, `
		UPDATE chatbots
		SET external_id = $3, name = $4, description = $5, prompt_template_id = $6,
		    endpoint_id = $7, model = $8, collection_ids = $9, settings = $10, is_enabled = $11
		WHERE tenant_id = $1 AND id = $2`,
		b.TenantID, b.ID, b.ExternalID, b.Name, b.Description,
		nullableText(b.PromptTemplateID), nullableText(b.ProviderID), b.Model,
		collections, settings, b.IsEnabled)
	if err != nil {
		return fmt.Errorf("update chatbot: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (r *ChatbotRepo) Delete(ctx context.Context, tenantID, id string) error {
	res, err := r.db.SQL.ExecContext(ctx,
		`DELETE FROM chatbots WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("delete chatbot: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ---------------------------------------------------------------------
// Conversations + messages

const conversationSelect = `
	SELECT tenant_id::text, id, chatbot_id, owner_actor_id, COALESCE(external_id, ''), created_at
	FROM conversations`

func scanConversation(s chatbotScanner) (chatbot.Conversation, error) {
	var c chatbot.Conversation
	if err := s.Scan(&c.TenantID, &c.ID, &c.ChatbotID, &c.OwnerActorID, &c.ExternalUserID, &c.CreatedAt); err != nil {
		return chatbot.Conversation{}, err
	}
	return c, nil
}

func (r *ChatbotRepo) CreateConversation(ctx context.Context, c chatbot.Conversation) error {
	_, err := r.db.SQL.ExecContext(ctx, `
		INSERT INTO conversations (tenant_id, id, chatbot_id, owner_actor_id, external_id)
		VALUES ($1, $2, $3, $4, $5)`,
		c.TenantID, c.ID, c.ChatbotID, c.OwnerActorID, nullableText(c.ExternalUserID))
	if err != nil {
		return fmt.Errorf("create conversation: %w", err)
	}
	return nil
}

func (r *ChatbotRepo) GetConversation(ctx context.Context, tenantID, id string) (chatbot.Conversation, error) {
	c, err := scanConversation(r.db.SQL.QueryRowContext(ctx,
		conversationSelect+` WHERE tenant_id = $1 AND id = $2`, tenantID, id))
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return chatbot.Conversation{}, sql.ErrNoRows
	}
	if err != nil {
		return chatbot.Conversation{}, fmt.Errorf("get conversation: %w", err)
	}
	return c, nil
}

func (r *ChatbotRepo) AddMessage(ctx context.Context, m chatbot.Message) error {
	_, err := r.db.SQL.ExecContext(ctx, `
		INSERT INTO messages (tenant_id, id, conversation_id, role, content, owner_actor_id)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		m.TenantID, m.ID, m.ConversationID, m.Role, m.Content, nullableText(m.OwnerActorID))
	if err != nil {
		return fmt.Errorf("add message: %w", err)
	}
	return nil
}

// ListMessages is a repo-level read for tests and replay surfaces; the
// service authorizes via the owning conversation.
func (r *ChatbotRepo) ListMessages(ctx context.Context, tenantID, conversationID string) ([]chatbot.Message, error) {
	rows, err := r.db.SQL.QueryContext(ctx, `
		SELECT tenant_id::text, id, conversation_id, role, content,
		       COALESCE(owner_actor_id, ''), created_at
		FROM messages
		WHERE tenant_id = $1 AND conversation_id = $2
		ORDER BY created_at ASC`, tenantID, conversationID)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []chatbot.Message{}
	for rows.Next() {
		var m chatbot.Message
		if err := rows.Scan(&m.TenantID, &m.ID, &m.ConversationID, &m.Role, &m.Content, &m.OwnerActorID, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}
	return out, nil
}

// ChatbotConversationStore adapts ChatbotRepo to inference.ConversationStore
// so retained conversations are first-class authorized inputs at the shared
// executor boundary.
type ChatbotConversationStore struct {
	repo *ChatbotRepo
}

func NewChatbotConversationStore(repo *ChatbotRepo) *ChatbotConversationStore {
	return &ChatbotConversationStore{repo: repo}
}

func (s *ChatbotConversationStore) GetConversation(ctx context.Context, tenantID, id string) (inference.Conversation, error) {
	c, err := s.repo.GetConversation(ctx, tenantID, id)
	if err != nil {
		return inference.Conversation{}, err
	}
	return inference.Conversation{
		TenantID:     c.TenantID,
		ID:           c.ID,
		OwnerActorID: c.OwnerActorID,
	}, nil
}

var _ inference.ConversationStore = (*ChatbotConversationStore)(nil)
