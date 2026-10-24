package chatbot_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/extract"
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
)

type evidenceReader struct {
	text rag.EvidenceText
	err  error
}

func (r evidenceReader) ReadEvidence(context.Context, auth.Principal, string) (rag.EvidenceText, error) {
	return r.text, r.err
}

type extractionStub struct {
	input         extract.ProcessInput
	processOutput extract.ProcessOutput
	detail        extract.JobDetail
	err           error
}

func (s *extractionStub) Process(_ context.Context, _ auth.Principal, in extract.ProcessInput) (extract.ProcessOutput, error) {
	s.input = in
	return s.processOutput, s.err
}
func (s *extractionStub) GetJob(context.Context, auth.Principal, string) (extract.JobDetail, error) {
	return s.detail, s.err
}

func TestBuiltinsRequireConfiguredToolsAndValidateArguments(t *testing.T) {
	ctx := context.Background()
	p := openPrincipal("tenant-a")
	searcher := &fakeRAGSearcher{}
	bot := chatbot.Instance{RAGCollectionIDs: []string{"collection-a"}, Settings: map[string]any{"tools": []any{"knowledge.search"}}}
	builtins := chatbot.Builtins{RAG: searcher}
	if _, err := builtins.Invoke(ctx, p, bot, "knowledge.search", json.RawMessage(`{"collection_id":"collection-a","query":"hello","extra":true}`), nil); !errors.Is(err, chatbot.ErrInvalidInput) {
		t.Fatalf("unknown argument accepted: %v", err)
	}
	if _, err := builtins.Invoke(ctx, p, bot, "knowledge.search", json.RawMessage(`{"collection_id":"other","query":"hello"}`), nil); !errors.Is(err, source.ErrDenied) {
		t.Fatalf("unattached collection accepted: %v", err)
	}
	result, err := builtins.Invoke(ctx, p, bot, "knowledge.search", json.RawMessage(`{"collection_id":"collection-a","query":"hello"}`), nil)
	if err != nil {
		t.Fatalf("search tool: %v", err)
	}
	if len(result.InputRefs) != 1 {
		t.Fatalf("search input refs = %#v", result.InputRefs)
	}
	if len(searcher.principals) != 1 || searcher.principals[0].ActorID != p.ActorID {
		t.Fatalf("search did not use caller principal: %#v", searcher.principals)
	}
}

func TestEvidenceReadAndExtractionToolsReturnRealReferences(t *testing.T) {
	ctx := context.Background()
	p := openPrincipal("tenant-a")
	refs := []source.ObjectRef{{TenantID: p.TenantID, ObjectID: "doc-1", ObservedSeq: 3}}
	bot := chatbot.Instance{Settings: map[string]any{"tools": []any{"evidence.read", "extract.submit", "extract.status"}}}
	extractor := &extractionStub{processOutput: extract.ProcessOutput{JobID: "job-real", Status: "queued"}, detail: extract.JobDetail{Job: extract.JobView{ID: "job-real", Status: "running"}}}
	builtins := chatbot.Builtins{Evidence: evidenceReader{text: rag.EvidenceText{EvidenceID: "ev-real", DocumentID: "doc-1", Content: "authorized clause", Spans: []source.Span{{Ref: refs[0]}}}}, Extractor: extractor}
	read, err := builtins.Invoke(ctx, p, bot, "evidence.read", json.RawMessage(`{"evidence_id":"ev-real"}`), nil)
	if err != nil || string(read.Value) == "" || string(read.Value) == "null" || len(read.InputRefs) != 1 || read.InputRefs[0].ObservedSeq != 3 {
		t.Fatalf("evidence read: %#v err=%v", read, err)
	}
	submitted, err := builtins.Invoke(ctx, p, bot, "extract.submit", json.RawMessage(`{"template_id":"owned-template","file_name":"notes.txt","mime_type":"text/plain","content_base64":"aGVsbG8="}`), nil)
	if err != nil || !strings.Contains(string(submitted.Value), "job-real") {
		t.Fatalf("extract submit did not return durable job: %#v err=%v", submitted, err)
	}
	if string(extractor.input.Bytes) != "hello" {
		t.Fatalf("extraction bytes = %q", extractor.input.Bytes)
	}
	if _, err := builtins.Invoke(ctx, p, bot, "extract.submit", json.RawMessage(`{"template_id":"owned-template","file_name":"notes.txt","mime_type":"text/plain","content_base64":"aGVsbG8="}`), refs); err == nil {
		t.Fatal("source-derived context was allowed into extraction")
	}
	if _, err := builtins.Invoke(ctx, p, bot, "extract.status", json.RawMessage(`{"job_id":"job-real"}`), nil); err != nil {
		t.Fatalf("extract status: %v", err)
	}
}

func containsString(values []string, match string) bool {
	for _, value := range values {
		if value == match {
			return true
		}
	}
	return false
}
