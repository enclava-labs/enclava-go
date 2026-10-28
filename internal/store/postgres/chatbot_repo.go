package postgres

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"enclava-go/internal/chatbot"
	"enclava-go/internal/inference"
	"enclava-go/internal/source"
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
	       collection_ids, settings, audience, is_enabled, COALESCE(owner_actor_id, ''), created_at
	FROM chatbots`

type chatbotScanner interface {
	Scan(dest ...any) error
}

func scanChatbot(s chatbotScanner) (chatbot.Instance, error) {
	var b chatbot.Instance
	var collections, settings, audience []byte
	err := s.Scan(&b.TenantID, &b.ID, &b.ExternalID, &b.Name, &b.Description,
		&b.PromptTemplateID, &b.ProviderID, &b.Model,
		&collections, &settings, &audience, &b.IsEnabled, &b.OwnerActorID, &b.CreatedAt)
	if err != nil {
		return chatbot.Instance{}, err
	}
	b.RAGCollectionIDs = scanJSONList(collections)
	b.Audience = scanJSONList(audience)
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
	audience, err := jsonList(b.Audience)
	if err != nil {
		return fmt.Errorf("marshal chatbot audience: %w", err)
	}
	settings, err := json.Marshal(map[string]any(b.Settings))
	if err != nil {
		return fmt.Errorf("marshal chatbot settings: %w", err)
	}
	_, err = r.db.SQL.ExecContext(ctx, `
		INSERT INTO chatbots
			(tenant_id, id, external_id, name, description, prompt_template_id,
			 endpoint_id, model, collection_ids, settings, audience, is_enabled, owner_actor_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		b.TenantID, b.ID, b.ExternalID, b.Name, b.Description,
		nullableText(b.PromptTemplateID), nullableText(b.ProviderID), b.Model,
		collections, settings, audience, b.IsEnabled, nullableText(b.OwnerActorID))
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
	audience, err := jsonList(b.Audience)
	if err != nil {
		return fmt.Errorf("marshal chatbot audience: %w", err)
	}
	settings, err := json.Marshal(map[string]any(b.Settings))
	if err != nil {
		return fmt.Errorf("marshal chatbot settings: %w", err)
	}
	res, err := r.db.SQL.ExecContext(ctx, `
		UPDATE chatbots
		SET external_id = $3, name = $4, description = $5, prompt_template_id = $6,
		    endpoint_id = $7, model = $8, collection_ids = $9, settings = $10,
		    audience = $11, is_enabled = $12
		WHERE tenant_id = $1 AND id = $2`,
		b.TenantID, b.ID, b.ExternalID, b.Name, b.Description,
		nullableText(b.PromptTemplateID), nullableText(b.ProviderID), b.Model,
		collections, settings, audience, b.IsEnabled)
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
	if m.EvidenceIDs == nil {
		m.EvidenceIDs = []string{}
	}
	if m.InputRefs == nil {
		m.InputRefs = []source.ObjectRef{}
	}
	evidenceIDs, err := json.Marshal(m.EvidenceIDs)
	if err != nil {
		return fmt.Errorf("marshal message evidence ids: %w", err)
	}
	inputRefs, err := json.Marshal(m.InputRefs)
	if err != nil {
		return fmt.Errorf("marshal message input refs: %w", err)
	}
	_, err = r.db.SQL.ExecContext(ctx, `
		INSERT INTO messages (tenant_id, id, conversation_id, role, content, owner_actor_id, evidence_ids, input_refs, run_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		m.TenantID, m.ID, m.ConversationID, m.Role, m.Content,
		nullableText(m.OwnerActorID), evidenceIDs, inputRefs, nullableText(m.RunID))
	if err != nil {
		return fmt.Errorf("add message: %w", err)
	}
	return nil
}

// ListMessages returns tenant-owned conversation messages in creation order;
// the service reauthorizes evidence references before replay or display.
func (r *ChatbotRepo) ListMessages(ctx context.Context, tenantID, conversationID string) ([]chatbot.Message, error) {
	rows, err := r.db.SQL.QueryContext(ctx, `
		SELECT tenant_id::text, id, conversation_id, role, content,
		       COALESCE(owner_actor_id, ''), COALESCE(run_id, ''), evidence_ids, input_refs, created_at
		FROM messages
		WHERE tenant_id = $1 AND conversation_id = $2
		ORDER BY created_at ASC, id ASC`, tenantID, conversationID)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []chatbot.Message{}
	for rows.Next() {
		var m chatbot.Message
		var evidenceIDs, inputRefs []byte
		if err := rows.Scan(&m.TenantID, &m.ID, &m.ConversationID, &m.Role, &m.Content,
			&m.OwnerActorID, &m.RunID, &evidenceIDs, &inputRefs, &m.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(evidenceIDs, &m.EvidenceIDs)
		_ = json.Unmarshal(inputRefs, &m.InputRefs)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}
	return out, nil
}

// CountConversations counts one owner's conversations. The owner predicate
// is unconditional at this boundary; the service binds the owner to the
// caller so no read can widen the count past the principal's own rows.
func (r *ChatbotRepo) CountConversations(ctx context.Context, tenantID, ownerActorID string) (int64, error) {
	var count int64
	err := r.db.SQL.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM conversations WHERE tenant_id = $1 AND owner_actor_id = $2`,
		tenantID, ownerActorID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count conversations: %w", err)
	}
	return count, nil
}

// ListConversationCountsByAssistant batches the per-assistant conversation
// counts behind the Assistants table: one grouped read over the tenant's
// conversations for exactly the listed assistants. Assistants without
// conversations are absent from the map — callers treat missing rows as zero.
func (r *ChatbotRepo) ListConversationCountsByAssistant(ctx context.Context, tenantID string, chatbotIDs []string) (map[string]int64, error) {
	counts := make(map[string]int64, len(chatbotIDs))
	if len(chatbotIDs) == 0 {
		return counts, nil
	}
	rows, err := r.db.SQL.QueryContext(ctx,
		`SELECT chatbot_id, COUNT(*) FROM conversations WHERE tenant_id = $1 AND chatbot_id = ANY($2) GROUP BY chatbot_id`,
		tenantID, chatbotIDs)
	if err != nil {
		return nil, fmt.Errorf("count conversations by assistant: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("scan conversation count: %w", err)
		}
		counts[id] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate conversation counts: %w", err)
	}
	return counts, nil
}

// encodeConversationCursor makes the opaque keyset cursor: the exact
// (created_at, id) pair of the last returned row, base64-wrapped.
func encodeConversationCursor(createdAt time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(createdAt.UnixNano(), 10) + "\x00" + id))
}

func decodeConversationCursor(cursor string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: %v", chatbot.ErrInvalidCursor, err)
	}
	parts := strings.SplitN(string(raw), "\x00", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return time.Time{}, "", fmt.Errorf("%w: malformed payload", chatbot.ErrInvalidCursor)
	}
	nanos, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: %v", chatbot.ErrInvalidCursor, err)
	}
	return time.Unix(0, nanos).UTC(), parts[1], nil
}

// ListConversations returns one owner's conversations newest-first over the
// (created_at DESC, id DESC) keyset with the joined assistant name and the
// last-activity timestamp. The owner predicate is unconditional — admins
// listing get their own rows, never the tenants'. Titles are derived by the
// service under the evidence-visibility rule.
func (r *ChatbotRepo) ListConversations(ctx context.Context, tenantID, ownerActorID string, limit int, cursor string) ([]chatbot.ConversationSummary, string, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	query := `
		SELECT c.id, c.chatbot_id, COALESCE(cb.name, ''), c.created_at,
		       COALESCE(last_msg.last_at, c.created_at)
		FROM conversations c
		LEFT JOIN chatbots cb ON cb.tenant_id = c.tenant_id AND cb.id = c.chatbot_id
		LEFT JOIN LATERAL (
			SELECT max(m.created_at) AS last_at
			FROM messages m
			WHERE m.tenant_id = c.tenant_id AND m.conversation_id = c.id
		) last_msg ON true
		WHERE c.tenant_id = $1 AND c.owner_actor_id = $2`
	args := []any{tenantID, ownerActorID}
	if cursor != "" {
		afterCreatedAt, afterID, err := decodeConversationCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		query += ` AND (c.created_at, c.id) < ($3, $4)`
		args = append(args, afterCreatedAt, afterID)
	}
	query += fmt.Sprintf(` ORDER BY c.created_at DESC, c.id DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit+1)

	rows, err := r.db.SQL.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("list conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []chatbot.ConversationSummary{}
	for rows.Next() {
		var s chatbot.ConversationSummary
		if err := rows.Scan(&s.ID, &s.ChatbotID, &s.AssistantName, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, "", err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("iterate conversations: %w", err)
	}
	var next string
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = encodeConversationCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
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
