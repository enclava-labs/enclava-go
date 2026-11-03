package chatbot_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

type outcomeExecutor struct {
	cancel   context.CancelFunc
	deadline bool
	request  inference.Request
}

func (e *outcomeExecutor) Execute(ctx context.Context, _ auth.Principal, req inference.Request) (*inference.Result, error) {
	e.request = req
	if e.deadline {
		return nil, context.DeadlineExceeded
	}
	e.cancel()
	return nil, ctx.Err()
}

func TestRunModelCancellationAndTimeoutFinalizeStep(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancelled", true: "deadline"}[deadline], func(t *testing.T) {
			db := testdb.New(t)
			tenant := testdb.SeedTenant(t, db, "run-outcomes")
			chatRepo := postgres.NewChatbotRepo(db)
			runsRepo := postgres.NewChatbotRunsRepo(db)
			jobSvc := jobs.NewService(postgres.NewJobsRepo(db), 0)
			member, _, _ := runWorkerPrincipals(tenant)
			bot := seedAudienceBot(t, chatRepo, tenant, []string{runWorkerMember})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			executor := &outcomeExecutor{cancel: cancel, deadline: deadline}
			svc := chatbot.NewService(chatRepo, postgres.NewPromptsRepo(db), nil, executor)
			svc.ConfigureRuns(runsRepo, jobSvc, nil, runPrincipalResolver{})
			run, err := svc.StartRun(ctx, member, bot.ID, chatbot.AssistantRunInput{Input: "hello"})
			if err != nil {
				t.Fatal(err)
			}
			if run.CreatedAt.IsZero() || run.UpdatedAt.IsZero() {
				t.Fatalf("missing persisted timestamps: %+v", run)
			}
			claim := leaseRunJob(t, jobSvc)
			_, err = svc.JobHandler()(ctx, claim, nil)
			if deadline && (err == nil || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "timed out")) {
				t.Fatalf("timeout error: %v", err)
			}
			if !deadline && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel error: %v", err)
			}
			attempts, err := runsRepo.ListRunAttempts(context.Background(), tenant, run.ID)
			if err != nil || len(attempts) != 1 || attempts[0].Status == "running" || attempts[0].EndedAt.IsZero() {
				t.Fatalf("steps=%+v err=%v", attempts, err)
			}
			if deadline && !strings.Contains(attempts[0].Error, "model call timed out") {
				t.Fatalf("step=%+v", attempts[0])
			}
			if executor.request.JobID != run.JobID || executor.request.AttemptID != claim.AttemptID() || !strings.Contains(attempts[0].Input, claim.AttemptID()) {
				t.Fatalf("request=%+v step=%+v", executor.request, attempts[0])
			}
			stored, err := runsRepo.GetRun(context.Background(), tenant, run.ID)
			if err != nil || (deadline && stored.Status != jobs.StatusFailed) || (!deadline && stored.Status != jobs.StatusCancelled) {
				t.Fatalf("run=%+v err=%v", stored, err)
			}
		})
	}
}
