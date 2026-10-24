package chatbot

import (
	"context"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/rag"
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
