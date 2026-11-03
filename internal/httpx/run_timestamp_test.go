package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enclava-go/internal/chatbot"
	"enclava-go/internal/jobs"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

func TestAgentRunAcceptedIncludesPersistedTimestamps(t *testing.T) {
	db := testdb.New(t)
	tenant := testdb.SeedTenant(t, db, "run-timestamp")
	p := openChatPrincipal(tenant)
	p.AllowedAgentsAll = true
	repo := postgres.NewChatbotRepo(db)
	svc := chatbot.NewService(repo, postgres.NewPromptsRepo(db), nil, nil)
	svc.ConfigureRuns(postgres.NewChatbotRunsRepo(db), jobs.NewService(postgres.NewJobsRepo(db), 0), nil, nil)
	bot, err := svc.Create(context.Background(), p, chatbot.CreateChatbotInput{Name: "Agent", ExternalID: "agent", Model: "chat-test-model"})
	if err != nil {
		t.Fatal(err)
	}
	app := &App{chatbotSvc: svc}
	rec := httptest.NewRecorder()
	app.handleAgentRuns(rec, httptest.NewRequest(http.MethodPost, "/api/v1/agent-runs", strings.NewReader(`{"assistant_id":"`+bot.ID+`","input":"hello"}`)), p)
	var run chatbot.AssistantRun
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusAccepted || run.CreatedAt.IsZero() || run.UpdatedAt.IsZero() {
		t.Fatalf("status=%d run=%+v body=%s", rec.Code, run, rec.Body.String())
	}
}
