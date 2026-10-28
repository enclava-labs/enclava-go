package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"enclava-go/internal/chatbot"
	"enclava-go/internal/prompts"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

func TestPromptsChatbotReposTenantScoped(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	tenantA := testdb.SeedTenant(t, db, "pc-a")
	tenantB := testdb.SeedTenant(t, db, "pc-b")

	promptRepo := postgres.NewPromptsRepo(db)
	chatRepo := postgres.NewChatbotRepo(db)

	tmpl := prompts.Template{
		TenantID:   tenantA,
		ID:         "tmpl_pc",
		Name:       "PC Template",
		UserPrompt: "{{.user_input}}",
		IsActive:   true,
	}
	if err := promptRepo.Create(ctx, tmpl); err != nil {
		t.Fatalf("create template: %v", err)
	}
	if _, err := promptRepo.GetByID(ctx, tenantB, tmpl.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant template get = %v", err)
	}
	list, err := promptRepo.List(ctx, tenantA)
	if err != nil || len(list) != 1 {
		t.Fatalf("list templates: %v %#v", err, list)
	}
	if listB, err := promptRepo.List(ctx, tenantB); err != nil || len(listB) != 0 {
		t.Fatalf("tenant b template list = %v %#v", err, listB)
	}

	bot := chatbot.Instance{
		TenantID:         tenantA,
		ID:               "bot_pc",
		ExternalID:       "pc_ext",
		Name:             "PC Bot",
		Model:            "m",
		PromptTemplateID: tmpl.ID,
		IsEnabled:        true,
	}
	if err := chatRepo.Create(ctx, bot); err != nil {
		t.Fatalf("create chatbot: %v", err)
	}
	got, err := chatRepo.GetByExternalID(ctx, tenantA, "pc_ext")
	if err != nil || got.ID != bot.ID {
		t.Fatalf("get by external id: %v %#v", err, got)
	}
	if _, err := chatRepo.GetByID(ctx, tenantB, bot.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant chatbot get = %v", err)
	}

	conv := chatbot.Conversation{
		TenantID:     tenantA,
		ID:           "conv_pc",
		ChatbotID:    bot.ID,
		OwnerActorID: "actor_1",
	}
	if err := chatRepo.CreateConversation(ctx, conv); err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	if _, err := chatRepo.GetConversation(ctx, tenantB, conv.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant conversation = %v", err)
	}
	if err := chatRepo.AddMessage(ctx, chatbot.Message{
		TenantID: tenantA, ID: "msg_pc", ConversationID: conv.ID,
		Role: "user", Content: "hi",
	}); err != nil {
		t.Fatalf("add message: %v", err)
	}
	msgs, err := chatRepo.ListMessages(ctx, tenantA, conv.ID)
	if err != nil || len(msgs) != 1 || msgs[0].Content != "hi" {
		t.Fatalf("list messages: %v %#v", err, msgs)
	}
}

// TestChatbotRepoListConversationsKeyset covers the OA-13 repo contract:
// owner-bound rows only, newest-first (created_at DESC, id DESC) with
// deterministic tie-breaking, opaque cursors that reject malformed payloads,
// a hard limit cap and the joined assistant name.
func TestChatbotRepoListConversationsKeyset(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	tenantA := testdb.SeedTenant(t, db, "conv-list-a")
	tenantB := testdb.SeedTenant(t, db, "conv-list-b")
	repo := postgres.NewChatbotRepo(db)

	if err := repo.Create(ctx, chatbot.Instance{
		TenantID: tenantA, ID: "bot_list", ExternalID: "bot_list_ext",
		Name: "List Bot", Model: "m", Audience: []string{"*"},
	}); err != nil {
		t.Fatalf("create bot: %v", err)
	}
	// Tenant B gets its own row so the cross-tenant conversation satisfies
	// the conversations FK while never appearing in tenant A's lists.
	if err := repo.Create(ctx, chatbot.Instance{
		TenantID: tenantB, ID: "bot_list_b", ExternalID: "bot_list_ext_b",
		Name: "Other Tenant Bot", Model: "m",
	}); err != nil {
		t.Fatalf("create tenant b bot: %v", err)
	}

	seed := func(t *testing.T, tenant, id, owner string) chatbot.Conversation {
		t.Helper()
		conv := chatbot.Conversation{TenantID: tenant, ID: id, ChatbotID: "bot_list", OwnerActorID: owner}
		if tenant != tenantA {
			conv.ChatbotID = "bot_list_b"
		}
		if err := repo.CreateConversation(ctx, conv); err != nil {
			t.Fatalf("seed conversation %s: %v", id, err)
		}
		return conv
	}
	age := func(t *testing.T, id string, at string) {
		t.Helper()
		if _, err := db.SQL.ExecContext(ctx,
			`UPDATE conversations SET created_at = $1::timestamptz WHERE tenant_id = $2::uuid AND id = $3`,
			at, tenantA, id); err != nil {
			t.Fatalf("age %s: %v", id, err)
		}
	}

	seed(t, tenantA, "conv_t1_a", "actor_1")
	seed(t, tenantA, "conv_t1_b", "actor_1")
	seed(t, tenantA, "conv_t2_a", "actor_1")
	seed(t, tenantA, "conv_t2_b", "actor_1")
	seed(t, tenantA, "conv_other", "actor_2")
	seed(t, tenantB, "conv_cross", "actor_1")
	age(t, "conv_t1_a", "2020-01-01T00:00:00Z")
	age(t, "conv_t1_b", "2020-01-01T00:00:00Z")
	age(t, "conv_t2_a", "2021-06-15T12:00:00Z")
	age(t, "conv_t2_b", "2021-06-15T12:00:00Z")
	age(t, "conv_other", "2022-01-01T00:00:00Z")

	// The newest aged row carries a message so updated_at moves past
	// created_at.
	if err := repo.AddMessage(ctx, chatbot.Message{
		TenantID: tenantA, ID: "msg_last", ConversationID: "conv_t2_b",
		Role: "user", Content: "latest activity",
	}); err != nil {
		t.Fatalf("add message: %v", err)
	}

	// Page 1: the 2021 tie, ids DESC — conv_other belongs to another owner
	// and never appears.
	summaries, next, err := repo.ListConversations(ctx, tenantA, "actor_1", 2, "")
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if got := ids(summaries); fmt.Sprint(got) != fmt.Sprint([]string{"conv_t2_b", "conv_t2_a"}) || next == "" {
		t.Fatalf("page 1 = %v next=%q", got, next)
	}
	if summaries[0].AssistantName != "List Bot" {
		t.Fatalf("assistant name = %q", summaries[0].AssistantName)
	}
	if !summaries[0].UpdatedAt.After(summaries[0].CreatedAt) {
		t.Fatalf("updated_at %v should follow created_at %v", summaries[0].UpdatedAt, summaries[0].CreatedAt)
	}

	// Page 2 is terminal: actor_1 owns exactly the four aged rows.
	summaries, next, err = repo.ListConversations(ctx, tenantA, "actor_1", 2, next)
	if err != nil || next != "" {
		t.Fatalf("page 2 = %v next=%q err=%v", ids(summaries), next, err)
	}
	if got := ids(summaries); fmt.Sprint(got) != fmt.Sprint([]string{"conv_t1_b", "conv_t1_a"}) {
		t.Fatalf("page 2 = %v, want the 2020 tie in id DESC order", got)
	}

	// Malformed cursors are 400-class errors, never silently ignored.
	for _, bad := range []string{"not-base64!!", "aaaa", "AAAA-GOES-ON"} {
		if _, _, err := repo.ListConversations(ctx, tenantA, "actor_1", 2, bad); !errors.Is(err, chatbot.ErrInvalidCursor) {
			t.Fatalf("cursor %q = %v, want ErrInvalidCursor", bad, err)
		}
	}

	// Cross-tenant and cross-owner rows stay invisible on every page.
	full, _, err := repo.ListConversations(ctx, tenantA, "actor_1", 50, "")
	if err != nil {
		t.Fatalf("full list: %v", err)
	}
	for _, summary := range full {
		if summary.ID == "conv_other" || summary.ID == "conv_cross" {
			t.Fatalf("foreign row leaked: %s", summary.ID)
		}
	}

	// The limit is capped at 50 regardless of the request.
	for i := range 60 {
		id := fmt.Sprintf("conv_bulk_%02d", i)
		seed(t, tenantA, id, "bulk_actor")
		age(t, id, "2019-01-01T00:00:00Z")
	}
	bulk, _, err := repo.ListConversations(ctx, tenantA, "bulk_actor", 100, "")
	if err != nil || len(bulk) != 50 {
		t.Fatalf("bulk list = %d rows err=%v, want the 50-row cap", len(bulk), err)
	}
}

func ids(summaries []chatbot.ConversationSummary) []string {
	out := make([]string, 0, len(summaries))
	for _, summary := range summaries {
		out = append(out, summary.ID)
	}
	return out
}
