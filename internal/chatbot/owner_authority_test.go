package chatbot

import (
	"context"
	"errors"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
)

type ownerAuthorityRAG struct {
	deniedActor string
	searches    int
}

func (f *ownerAuthorityRAG) Search(context.Context, auth.Principal, rag.SearchInput) ([]rag.SearchResult, error) {
	f.searches++
	return []rag.SearchResult{{DocumentID: "doc", Content: "content"}}, nil
}

func (f *ownerAuthorityRAG) ListCollections(_ context.Context, p auth.Principal) ([]rag.Collection, error) {
	if p.ActorID == f.deniedActor {
		return nil, nil
	}
	return []rag.Collection{{ID: "collection"}}, nil
}

func (*ownerAuthorityRAG) BuildContext(_ context.Context, _ auth.Principal, results []rag.SearchResult, _ int) (rag.ContextResult, error) {
	return rag.ContextResult{}, nil
}

func TestRetrieveRAGIntersectsCallerAndOwnerCollectionGrants(t *testing.T) {
	ctx := context.Background()
	caller := auth.Principal{TenantID: "tenant", ActorID: "caller"}
	owner := auth.Principal{TenantID: "tenant", ActorID: "owner"}
	bot := Instance{RAGCollectionIDs: []string{"collection"}}
	ragSvc := &ownerAuthorityRAG{deniedActor: owner.ActorID}
	svc := &Service{rag: ragSvc}
	results, _, err := svc.retrieveRAG(ctx, caller, owner, bot, "query")
	if err != nil || len(results) != 0 || ragSvc.searches != 0 {
		t.Fatalf("revoked owner grant allowed retrieval: results=%v searches=%d err=%v", results, ragSvc.searches, err)
	}

	ragSvc.deniedActor = ""
	results, _, err = svc.retrieveRAG(ctx, owner, owner, bot, "query")
	if err != nil || len(results) != 1 || ragSvc.searches != 1 {
		t.Fatalf("owner path changed: results=%v searches=%d err=%v", results, ragSvc.searches, err)
	}
}

func TestOwnerFallbackDoesNotBypassRestrictedAPIKey(t *testing.T) {
	bot := Instance{ID: "bot-a", OwnerActorID: "owner"}
	p := auth.Principal{
		ActorID: "owner", CredentialKind: auth.CredentialKindAPIKey,
		AllowedChatbots: map[string]struct{}{"other": {}},
	}
	if chatbotAllowed(p, bot) {
		t.Fatal("owner equality bypassed restricted API-key chatbot allowlist")
	}
	p.AllowedChatbots["bot-a"] = struct{}{}
	if !chatbotAllowed(p, bot) {
		t.Fatal("explicit chatbot grant was rejected")
	}
}

type ownerAuthorityResolver struct{ owner auth.Principal }

func (r ownerAuthorityResolver) PrincipalForActor(context.Context, string, string) (auth.Principal, error) {
	return r.owner, nil
}

func TestKnowledgeSearchIntersectsCallerAndOwnerCollectionGrants(t *testing.T) {
	caller := auth.Principal{TenantID: "tenant", ActorID: "caller"}
	owner := auth.Principal{TenantID: "tenant", ActorID: "owner"}
	bot := Instance{OwnerActorID: owner.ActorID, RAGCollectionIDs: []string{"collection"}, Settings: map[string]any{"tools": []any{"knowledge.search"}}}
	searcher := &ownerAuthorityRAG{deniedActor: owner.ActorID}
	builtins := Builtins{RAG: searcher, Owners: ownerAuthorityResolver{owner: owner}}
	_, err := builtins.Invoke(context.Background(), caller, bot, "knowledge.search", []byte(`{"collection_id":"collection","query":"q"}`), nil)
	if !errors.Is(err, source.ErrDenied) || searcher.searches != 0 {
		t.Fatalf("revoked owner grant allowed built-in retrieval: searches=%d err=%v", searcher.searches, err)
	}
}

type unresolvedOwner struct{}

func (unresolvedOwner) PrincipalForActor(context.Context, string, string) (auth.Principal, error) {
	return auth.Principal{}, errors.New("owner actor not a user")
}
func (unresolvedOwner) CurrentPrincipal(context.Context, auth.Principal) (auth.Principal, error) {
	return auth.Principal{}, errors.New("no current principal")
}

func TestSharedAssistantDeniesUnresolvableServiceAccountOwner(t *testing.T) {
	svc := &Service{principalResolver: unresolvedOwner{}}
	caller := auth.Principal{TenantID: "tenant", ActorID: "caller"}
	_, err := svc.ownerPrincipal(context.Background(), caller, Instance{OwnerActorID: "service-account"})
	if !errors.Is(err, source.ErrDenied) {
		t.Fatalf("unresolvable owner error = %v", err)
	}
}
