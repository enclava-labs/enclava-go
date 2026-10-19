package prompts_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/prompts"
	"enclava-go/internal/store/postgres"
	"enclava-go/internal/testdb"
)

func TestRenderInjectsPromptVariables(t *testing.T) {
	rendered, err := prompts.Render(prompts.Template{
		ID:           "test",
		SystemPrompt: "Answer for {{.audience}} using {{.rag_context}}",
		UserPrompt:   "Question: {{.user_input}}",
	}, prompts.RenderInput{
		UserInput:  "Where is the citadel?",
		RAGContext: "maple-vector-citadel",
		Variables: map[string]any{
			"audience": "operators",
		},
	})
	if err != nil {
		t.Fatalf("render prompt: %v", err)
	}
	if !strings.Contains(rendered.SystemPrompt, "operators") || !strings.Contains(rendered.SystemPrompt, "maple-vector-citadel") {
		t.Fatalf("system prompt did not include variables: %q", rendered.SystemPrompt)
	}
	if rendered.UserPrompt != "Question: Where is the citadel?" {
		t.Fatalf("unexpected user prompt: %q", rendered.UserPrompt)
	}
}

type stubVisibleIDs struct {
	ids map[string]struct{}
	err error
}

func (s stubVisibleIDs) VisiblePromptTemplateIDs(context.Context, auth.Principal) (map[string]struct{}, error) {
	return s.ids, s.err
}

func openPrincipal(tenantID string) auth.Principal {
	return auth.Principal{
		TenantID:           tenantID,
		ActorID:            "user_admin",
		ActorKind:          auth.ActorKindUser,
		AllowedChatbotsAll: true,
	}
}

func TestServiceCRUDTenantScoped(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	tenantA := testdb.SeedTenant(t, db, "prompt-a")
	tenantB := testdb.SeedTenant(t, db, "prompt-b")
	repo := postgres.NewPromptsRepo(db)
	svc := prompts.NewService(repo, nil)
	a := openPrincipal(tenantA)
	b := openPrincipal(tenantB)

	created, err := svc.Create(ctx, a, prompts.CreateTemplateInput{
		ID:           "tmpl_a",
		Name:         "A Template",
		SystemPrompt: "sys",
		UserPrompt:   "{{.user_input}}",
	})
	if err != nil {
		t.Fatalf("create template: %v", err)
	}
	if created.TenantID != tenantA || created.OwnerActorID != "user_admin" {
		t.Fatalf("tenant/owner not stamped: %#v", created)
	}

	list, err := svc.List(ctx, a)
	if err != nil || len(list) != 1 {
		t.Fatalf("list tenant a: %v %#v", err, list)
	}
	listB, err := svc.List(ctx, b)
	if err != nil {
		t.Fatalf("list tenant b: %v", err)
	}
	if len(listB) != 0 {
		t.Fatalf("tenant b sees tenant a templates: %#v", listB)
	}
	if _, err := svc.Get(ctx, b, created.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant get = %v, want ErrNoRows", err)
	}
	if _, err := svc.Update(ctx, b, created.ID, prompts.UpdateTemplateInput{}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant update = %v, want ErrNoRows", err)
	}
	if err := svc.Delete(ctx, b, created.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant delete = %v, want ErrNoRows", err)
	}
	if _, err := svc.Get(ctx, a, created.ID); err != nil {
		t.Fatalf("owner get: %v", err)
	}
	name := "Renamed"
	updated, err := svc.Update(ctx, a, created.ID, prompts.UpdateTemplateInput{Name: &name})
	if err != nil || updated.Name != "Renamed" {
		t.Fatalf("update: %v %#v", err, updated)
	}
	if err := svc.Delete(ctx, a, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.Get(ctx, a, created.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted get = %v, want ErrNoRows", err)
	}
}

func TestServiceListFollowsChatbotVisibility(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	tenant := testdb.SeedTenant(t, db, "prompt-vis")
	repo := postgres.NewPromptsRepo(db)

	open := openPrincipal(tenant)
	svc := prompts.NewService(repo, stubVisibleIDs{ids: map[string]struct{}{"tmpl_allowed": {}}})
	for _, id := range []string{"tmpl_allowed", "tmpl_hidden"} {
		if _, err := svc.Create(ctx, open, prompts.CreateTemplateInput{ID: id, Name: id}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	restricted := auth.Principal{TenantID: tenant, ActorID: "key1",
		AllowedChatbots: map[string]struct{}{"bot1": {}}}

	got, err := svc.List(ctx, restricted)
	if err != nil {
		t.Fatalf("restricted list: %v", err)
	}
	if len(got) != 1 || got[0].ID != "tmpl_allowed" {
		t.Fatalf("restricted list = %#v", got)
	}
	if _, err := svc.Get(ctx, restricted, "tmpl_hidden"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("hidden get = %v, want ErrNoRows", err)
	}
	if _, err := svc.Get(ctx, restricted, "tmpl_allowed"); err != nil {
		t.Fatalf("allowed get: %v", err)
	}
	got, err = svc.List(ctx, open)
	if err != nil || len(got) != 2 {
		t.Fatalf("open list = %v %#v", err, got)
	}

	// No visibility source: a restricted principal sees nothing.
	bareSvc := prompts.NewService(repo, nil)
	got, err = bareSvc.List(ctx, restricted)
	if err != nil || len(got) != 0 {
		t.Fatalf("bare restricted list = %v %#v", err, got)
	}
	if _, err := bareSvc.Get(ctx, restricted, "tmpl_allowed"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("bare hidden get = %v, want ErrNoRows", err)
	}
}
