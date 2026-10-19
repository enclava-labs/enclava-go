package postgres_test

import (
	"context"
	"database/sql"
	"errors"
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
		TenantID:  tenantA,
		ID:        "tmpl_pc",
		Name:      "PC Template",
		UserPrompt: "{{.user_input}}",
		IsActive:  true,
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
		TenantID:   tenantA,
		ID:         "bot_pc",
		ExternalID: "pc_ext",
		Name:       "PC Bot",
		Model:      "m",
		PromptTemplateID: tmpl.ID,
		IsEnabled:  true,
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
