package chatbot_test

import (
	"context"
	"testing"

	"enclava-go/internal/chatbot"
)

// TestListConversationsKeysetStableAcrossCreatedAtTies is the B1 gap fill:
// rows sharing one created_at must still page deterministically — the
// (created_at DESC, id DESC) keyset never duplicates or drops a row, and a
// same-actor conversation in ANOTHER tenant never enters the page stream.
func TestListConversationsKeysetStableAcrossCreatedAtTies(t *testing.T) {
	ctx := context.Background()
	f := newChatFixture(t, &fakeRAGSearcher{}, nil)
	creator := openPrincipal(f.tenantA)
	member := openPrincipal(f.tenantA)
	member.ActorID = "tie_member"

	bot, err := f.svc.Create(ctx, creator, chatbot.CreateChatbotInput{
		Name: "Tie Bot", Model: "chat-test-model", Audience: []string{member.ActorID},
	})
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	member.AllowedChatbots = map[string]struct{}{bot.ID: {}}

	own := map[string]bool{}
	for range 3 {
		own[seedConversation(t, f, bot.ID, member.ActorID, "tie message").ID] = true
	}
	seedConversation(t, f, bot.ID, "someone_else", "not mine")
	// Identical created_at across every row forces the id tie-break.
	if _, err := f.db.SQL.ExecContext(ctx,
		`UPDATE conversations SET created_at = '2024-06-01T12:00:00Z'::timestamptz WHERE tenant_id = $1::uuid`,
		f.tenantA); err != nil {
		t.Fatalf("freeze created_at: %v", err)
	}

	// Same actor id, other tenant — must never join the page stream.
	if err := f.chatRepo.Create(ctx, chatbot.Instance{
		TenantID: f.tenantB, ID: "bot_b_tie", ExternalID: "bot_b_tie",
		Name: "B Bot", Model: "m", IsEnabled: true, OwnerActorID: "seed",
		Audience: []string{member.ActorID},
	}); err != nil {
		t.Fatalf("seed tenant-b bot: %v", err)
	}
	if err := f.chatRepo.CreateConversation(ctx, chatbot.Conversation{
		TenantID: f.tenantB, ID: "conv_b_tie", ChatbotID: "bot_b_tie", OwnerActorID: member.ActorID,
	}); err != nil {
		t.Fatalf("seed tenant-b conversation: %v", err)
	}

	// Page through with limit=1: three distinct own rows, then exhaustion.
	cursor := ""
	seen := map[string]bool{}
	for range 8 {
		summaries, next, err := f.svc.ListConversations(ctx, member, 1, cursor)
		if err != nil {
			t.Fatalf("page: %v", err)
		}
		for _, s := range summaries {
			if s.ID == "conv_b_tie" || !own[s.ID] {
				t.Fatalf("leaked non-own/foreign conversation %q", s.ID)
			}
			if seen[s.ID] {
				t.Fatalf("keyset tie duplicated %q", s.ID)
			}
			seen[s.ID] = true
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 3 {
		t.Fatalf("keyset pages covered %d rows, want exactly the 3 own ties: %#v", len(seen), seen)
	}

	// Determinism and ordering: a second walk yields the same id-descending
	// order within the tie.
	cursor = ""
	var order []string
	for {
		summaries, next, err := f.svc.ListConversations(ctx, member, 1, cursor)
		if err != nil {
			t.Fatalf("second pass: %v", err)
		}
		for _, s := range summaries {
			order = append(order, s.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(order) != 3 {
		t.Fatalf("second pass order = %v", order)
	}
	for i := 1; i < len(order); i++ {
		if order[i] > order[i-1] {
			t.Fatalf("tie order %v is not id-descending within equal created_at", order)
		}
	}
}
