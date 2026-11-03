package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"enclava-go/internal/chatbot"
	"enclava-go/internal/jobs"
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

// TestChatbotRunsRepoFailedRunsMessageRunIDAndObservedRoute covers the
// 08-06 repo contract: ListFailedRuns derives terminal status from the
// BOUND JOB (covering job-side failures while the run row reads queued,
// the input_ref fallback when job_id is unset, and the run row's own
// wrapped terminal status), stays owner-scoped and newest-first; messages
// round-trip run_id; runs round-trip the observed endpoint/model.
func TestChatbotRunsRepoFailedRunsMessageRunIDAndObservedRoute(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	tenant := testdb.SeedTenant(t, db, "runs-repo")
	chatRepo := postgres.NewChatbotRepo(db)
	runsRepo := postgres.NewChatbotRunsRepo(db)
	jobsRepo := postgres.NewJobsRepo(db)

	bot := chatbot.Instance{TenantID: tenant, ID: "bot_runs", ExternalID: "bot_runs_ext", Name: "Runs Bot", Model: "m", IsEnabled: true}
	if err := chatRepo.Create(ctx, bot); err != nil {
		t.Fatalf("create bot: %v", err)
	}
	for _, conv := range []chatbot.Conversation{
		{TenantID: tenant, ID: "conv_runs", ChatbotID: bot.ID, OwnerActorID: "actor_1"},
		{TenantID: tenant, ID: "conv_runs_2", ChatbotID: bot.ID, OwnerActorID: "actor_2"},
	} {
		if err := chatRepo.CreateConversation(ctx, conv); err != nil {
			t.Fatalf("create conversation: %v", err)
		}
	}

	insertRun := func(t *testing.T, id, owner, convID, status, jobID string) {
		t.Helper()
		if err := runsRepo.CreateRun(ctx, chatbot.AssistantRun{
			TenantID: tenant, ID: id, ChatbotID: bot.ID, ConversationID: convID,
			OwnerActorID: owner, Status: status, JobID: jobID, Input: "in",
		}); err != nil {
			t.Fatalf("create run %s: %v", id, err)
		}
	}
	insertJob := func(t *testing.T, id, runID, status, errDoc string) {
		t.Helper()
		if _, _, err := jobsRepo.InsertJob(ctx, jobs.InsertRecord{
			Job: jobs.Job{
				TenantID: tenant, ID: id, Operation: chatbot.RunOperation, Status: jobs.StatusQueued,
				IdempotencyKey: id, InputRef: runID, InputHash: "hash", MaxAttempts: 1,
			},
			SubmitterKey: "actor_1", Principal: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("insert job %s: %v", id, err)
		}
		if _, err := db.SQL.ExecContext(ctx,
			`UPDATE jobs SET status = $3, error = NULLIF($4, '')::jsonb WHERE tenant_id = $1::uuid AND id = $2`,
			tenant, id, status, errDoc); err != nil {
			t.Fatalf("terminal job %s: %v", id, err)
		}
	}

	// Bound-job failure while the run row still reads queued (pre-dispatch).
	insertRun(t, "run_job_failed", "actor_1", "conv_runs", jobs.StatusQueued, "job_f1")
	insertJob(t, "job_f1", "run_job_failed", jobs.StatusFailed, `{"message":"boom at admission","retryable":false}`)
	// Cancelled bound job.
	insertRun(t, "run_job_cancelled", "actor_1", "conv_runs", jobs.StatusQueued, "job_c1")
	insertJob(t, "job_c1", "run_job_cancelled", jobs.StatusCancelled, "")
	// Succeeded runs never list.
	insertRun(t, "run_ok", "actor_1", "conv_runs", jobs.StatusQueued, "job_ok")
	insertJob(t, "job_ok", "run_ok", jobs.StatusSucceeded, "")
	// Observed endpoint/model round-trip through UpdateRun/GetRun.
	if err := runsRepo.UpdateRun(ctx, chatbot.AssistantRun{
		TenantID: tenant, ID: "run_ok", Status: jobs.StatusSucceeded,
		EndpointID: "ep_observed", Model: "upstream-model-9",
	}); err != nil {
		t.Fatalf("update run route: %v", err)
	}
	got, err := runsRepo.GetRun(ctx, tenant, "run_ok")
	if err != nil || got.EndpointID != "ep_observed" || got.Model != "upstream-model-9" {
		t.Fatalf("observed route = %q/%q err=%v", got.EndpointID, got.Model, err)
	}
	// Another owner's failed run.
	insertRun(t, "run_foreign", "actor_2", "conv_runs_2", jobs.StatusFailed, "")
	// Run row failed under the terminal-exit wrap while the job kept running.
	insertRun(t, "run_wrapped", "actor_1", "conv_runs", jobs.StatusFailed, "job_w1")
	insertJob(t, "job_w1", "run_wrapped", jobs.StatusRunning, "")
	// job_id unset: the input_ref fallback binds the job.
	insertRun(t, "run_ref_fallback", "actor_1", "conv_runs", jobs.StatusQueued, "")
	insertJob(t, "job_rf", "run_ref_fallback", jobs.StatusFailed, `{"message":"lost lease"}`)

	failed, err := runsRepo.ListFailedRuns(ctx, tenant, "actor_1", 50)
	if err != nil {
		t.Fatalf("list failed runs: %v", err)
	}
	gotIDs := map[string]chatbot.AssistantRun{}
	order := make([]string, 0, len(failed))
	for _, run := range failed {
		gotIDs[run.ID] = run
		order = append(order, run.ID)
	}
	for _, want := range []string{"run_job_failed", "run_job_cancelled", "run_wrapped", "run_ref_fallback"} {
		if _, ok := gotIDs[want]; !ok {
			t.Fatalf("failed-run feed missing %s: %v", want, order)
		}
	}
	if _, ok := gotIDs["run_ok"]; ok {
		t.Fatal("succeeded run appeared in the failed-run feed")
	}
	if _, ok := gotIDs["run_foreign"]; ok {
		t.Fatal("another owner's run leaked into the failed-run feed")
	}
	if gotIDs["run_job_failed"].Status != jobs.StatusFailed || gotIDs["run_job_failed"].Error != "boom at admission" {
		t.Fatalf("job-derived status/error = %q/%q", gotIDs["run_job_failed"].Status, gotIDs["run_job_failed"].Error)
	}
	if gotIDs["run_job_cancelled"].Status != jobs.StatusCancelled {
		t.Fatalf("cancelled status = %q", gotIDs["run_job_cancelled"].Status)
	}
	if gotIDs["run_wrapped"].Status != jobs.StatusFailed {
		t.Fatalf("wrapped terminal status = %q", gotIDs["run_wrapped"].Status)
	}
	if gotIDs["run_ref_fallback"].Status != jobs.StatusFailed || gotIDs["run_ref_fallback"].Error != "lost lease" {
		t.Fatalf("input_ref fallback run = %q/%q", gotIDs["run_ref_fallback"].Status, gotIDs["run_ref_fallback"].Error)
	}
	// Owner scope is unconditional.
	if foreign, err := runsRepo.ListFailedRuns(ctx, tenant, "actor_2", 50); err != nil || len(foreign) != 1 || foreign[0].ID != "run_foreign" {
		t.Fatalf("actor_2 feed = %#v err=%v", foreign, err)
	}

	// Deterministic newest-first order and the bound limit: age all four of
	// actor_1's failed runs to fixed instants, newest = run_job_cancelled.
	if _, err := db.SQL.ExecContext(ctx, `
		UPDATE chatbot_runs SET created_at = CASE id
			WHEN 'run_wrapped' THEN '2020-01-01T00:00:00Z'::timestamptz
			WHEN 'run_ref_fallback' THEN '2020-01-02T00:00:00Z'::timestamptz
			WHEN 'run_job_failed' THEN '2020-01-03T00:00:00Z'::timestamptz
			WHEN 'run_job_cancelled' THEN '2020-01-04T00:00:00Z'::timestamptz
		END
		WHERE tenant_id = $1::uuid AND id IN ('run_wrapped','run_ref_fallback','run_job_failed','run_job_cancelled')`, tenant); err != nil {
		t.Fatalf("age runs: %v", err)
	}
	page, err := runsRepo.ListFailedRuns(ctx, tenant, "actor_1", 2)
	if err != nil || len(page) != 2 || page[0].ID != "run_job_cancelled" || page[1].ID != "run_job_failed" {
		t.Fatalf("first failed-run page = %v err=%v, want the newest two in order", page, err)
	}

	// Message run_id round-trip.
	if err := chatRepo.AddMessage(ctx, chatbot.Message{
		TenantID: tenant, ID: "msg_runlink", ConversationID: "conv_runs",
		Role: "user", Content: "hi", RunID: "run_job_failed",
	}); err != nil {
		t.Fatalf("add message: %v", err)
	}
	messages, err := chatRepo.ListMessages(ctx, tenant, "conv_runs")
	if err != nil || len(messages) == 0 || messages[0].RunID != "run_job_failed" {
		t.Fatalf("message run_id = %#v err=%v", messages, err)
	}

	// Service-scoped conversation counts stay owner-bound.
	counts := map[string]int64{}
	for _, owner := range []string{"actor_1", "actor_2", "actor_3"} {
		count, err := chatRepo.CountConversations(ctx, tenant, owner)
		if err != nil {
			t.Fatalf("count conversations: %v", err)
		}
		counts[owner] = count
	}
	if counts["actor_1"] != 1 || counts["actor_2"] != 1 || counts["actor_3"] != 0 {
		t.Fatalf("conversation counts = %v", counts)
	}
}
