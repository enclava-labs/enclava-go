package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enclava-go/internal/chatbot"
	"enclava-go/internal/prompts"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

func TestDeleteInstructionsInUseReturnsAssistantNames(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "prompt-conflict")
	p := openChatPrincipal(tenant)
	p.Scopes = map[string]struct{}{"chatbot.manage": {}}
	repo := postgres.NewPromptsRepo(db)
	bots := chatbot.NewService(postgres.NewChatbotRepo(db), repo, nil, nil)
	service := prompts.NewService(repo, bots)
	template, err := service.Create(ctx, p, prompts.CreateTemplateInput{ID: "used", Name: "Used", UserPrompt: "{{.user_input}}"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = bots.Create(ctx, p, chatbot.CreateChatbotInput{Name: "Reference Assistant", ExternalID: "reference", Model: "chat-test-model", PromptTemplateID: template.ID, Audience: []string{"*"}})
	if err != nil {
		t.Fatal(err)
	}
	app := &App{chatbotSvc: bots, promptSvc: service}
	rec := httptest.NewRecorder()
	app.handlePromptTemplateByID(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/prompt-templates/used", nil), p)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Reference Assistant") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
