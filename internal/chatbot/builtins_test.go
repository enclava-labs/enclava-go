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
	"enclava-go/internal/inference"
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

func TestRunToolFailuresAreReturnedToModel(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool string
		args string
	}{
		{"unknown", "missing", `{}`},
		{"malformed", "extract_status", `not-json`},
		{"invoke", "extract_status", `{"job_id":"job"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"tool_calls": []any{map[string]any{"id": "call", "type": "function", "function": map[string]any{"name": tc.tool, "arguments": tc.args}}}}}}})
			f := newRunWorkerFixture(t, []string{string(call), runWorkerDone}, nil)
			member, _, _ := runWorkerPrincipals(f.tenant)
			bot := seedAudienceBot(t, f.chatRepo, f.tenant, []string{runWorkerMember})
			bot.Settings = map[string]any{"tools": []any{"extract.status"}}
			if err := f.chatRepo.Update(context.Background(), bot); err != nil {
				t.Fatal(err)
			}
			run, err := startAndRun(t, f, member, bot.ID, chatbot.AssistantRunInput{Input: "help"})
			if err != nil {
				t.Fatal(err)
			}
			attempts, err := f.runsRepo.ListRunAttempts(context.Background(), f.tenant, run.ID)
			if err != nil || len(attempts) != 3 || attempts[1].Status != "failed" {
				t.Fatalf("attempts=%#v err=%v", attempts, err)
			}
			var request struct {
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
				Tools []struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tools"`
			}
			if err := json.Unmarshal(f.executor.requests[1].Payload, &request); err != nil {
				t.Fatal(err)
			}
			if len(request.Tools) != 1 || request.Tools[0].Function.Name != "extract_status" {
				t.Fatalf("tool names=%#v", request.Tools)
			}
			last := request.Messages[len(request.Messages)-1]
			if last.Role != "tool" || !strings.Contains(last.Content, `"error"`) {
				t.Fatalf("tool result=%#v", last)
			}
		})
	}
}

func TestRunFinalTurnHasNoToolsAndEmptyAnswerFails(t *testing.T) {
	const call = `{"choices":[{"message":{"tool_calls":[{"id":"call","type":"function","function":{"name":"unknown","arguments":"{}"}}]}}]}`
	for _, tc := range []struct {
		name      string
		answer    string
		wantError bool
	}{
		{"answer", runWorkerDone, false},
		{"empty", `{"choices":[{"message":{"content":"  "}}]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunWorkerFixture(t, []string{call, call, call, tc.answer}, nil)
			member, _, _ := runWorkerPrincipals(f.tenant)
			bot := seedAudienceBot(t, f.chatRepo, f.tenant, []string{runWorkerMember})
			bot.Settings = map[string]any{"tools": []any{"extract.status"}}
			if err := f.chatRepo.Update(context.Background(), bot); err != nil {
				t.Fatal(err)
			}
			_, err := startAndRun(t, f, member, bot.ID, chatbot.AssistantRunInput{Input: "help"})
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "assistant returned an empty answer") {
					t.Fatalf("error=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			var request map[string]any
			if err := json.Unmarshal(f.executor.requests[3].Payload, &request); err != nil {
				t.Fatal(err)
			}
			if _, ok := request["tools"]; ok {
				t.Fatal("final turn included tools")
			}
		})
	}
}

func TestRunToolDenialAborts(t *testing.T) {
	const call = `{"choices":[{"message":{"tool_calls":[{"id":"call","type":"function","function":{"name":"evidence_read","arguments":"{\"evidence_id\":\"denied\"}"}}]}}]}`
	f := newRunWorkerFixtureWithRAG(t, &fakeRAGSearcher{evidenceReadDenied: true}, []string{call, runWorkerDone}, nil)
	member, _, _ := runWorkerPrincipals(f.tenant)
	bot := seedAudienceBot(t, f.chatRepo, f.tenant, []string{runWorkerMember})
	bot.Settings = map[string]any{"tools": []any{"evidence.read"}}
	if err := f.chatRepo.Update(context.Background(), bot); err != nil {
		t.Fatal(err)
	}
	run, err := startAndRun(t, f, member, bot.ID, chatbot.AssistantRunInput{Input: "help"})
	if !errors.Is(err, source.ErrDenied) || len(f.executor.requests) != 1 {
		t.Fatalf("denial=%v dispatches=%d", err, len(f.executor.requests))
	}
	attempts, err := f.runsRepo.ListRunAttempts(context.Background(), f.tenant, run.ID)
	if err != nil || len(attempts) != 2 || attempts[1].Status != "failed" || attempts[1].Name != "evidence.read" {
		t.Fatalf("attempts=%#v err=%v", attempts, err)
	}
}

type namedMCPDispatcher struct {
	invoked string
	err     error
}

func (d *namedMCPDispatcher) Definitions(context.Context, auth.Principal, auth.Principal, chatbot.Instance) ([]map[string]any, error) {
	return []map[string]any{{"type": "function", "function": map[string]any{
		"name": "approved.lookup", "parameters": map[string]any{"type": "object"},
	}}}, nil
}

func (d *namedMCPDispatcher) Invoke(_ context.Context, _, _ auth.Principal, _ chatbot.Instance, name string, _ json.RawMessage, _ []source.ObjectRef) (chatbot.BuiltinResult, error) {
	d.invoked = name
	if d.err != nil {
		return chatbot.BuiltinResult{}, d.err
	}
	return chatbot.BuiltinResult{Value: json.RawMessage(`{"ok":true}`)}, nil
}

func TestRunMapsMCPModelNameToApprovedID(t *testing.T) {
	const call = `{"choices":[{"message":{"tool_calls":[{"id":"call","type":"function","function":{"name":"approved_lookup","arguments":"{}"}}]}}]}`
	f := newRunWorkerFixture(t, []string{call, runWorkerDone}, nil)
	dispatcher := &namedMCPDispatcher{}
	f.svc.ConfigureMCP(dispatcher)
	member, _, _ := runWorkerPrincipals(f.tenant)
	bot := seedAudienceBot(t, f.chatRepo, f.tenant, []string{runWorkerMember})
	bot.Settings = map[string]any{"tools": []any{"approved.lookup"}}
	if err := f.chatRepo.Update(context.Background(), bot); err != nil {
		t.Fatal(err)
	}
	_, err := startAndRun(t, f, member, bot.ID, chatbot.AssistantRunInput{Input: "help"})
	if err != nil || dispatcher.invoked != "approved.lookup" {
		t.Fatalf("invoked=%q err=%v", dispatcher.invoked, err)
	}
	var request struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(f.executor.requests[0].Payload, &request); err != nil {
		t.Fatal(err)
	}
	if len(request.Tools) != 1 || request.Tools[0].Function.Name != "approved_lookup" {
		t.Fatalf("definitions=%#v", request.Tools)
	}
}

func TestRunTrimsOldHistoryBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	f := newRunWorkerFixture(t, []string{runWorkerDone}, nil)
	member, _, _ := runWorkerPrincipals(f.tenant)
	bot := seedAudienceBot(t, f.chatRepo, f.tenant, []string{runWorkerMember})
	conversation := chatbot.Conversation{TenantID: f.tenant, ID: newRunTestID("conv"), ChatbotID: bot.ID, OwnerActorID: member.ActorID}
	if err := f.chatRepo.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{strings.Repeat("oldest", 12000), "recent history"} {
		if err := f.chatRepo.AddMessage(ctx, chatbot.Message{TenantID: f.tenant, ID: newRunTestID("msg"), ConversationID: conversation.ID, Role: "user", Content: content, OwnerActorID: member.ActorID}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := startAndRun(t, f, member, bot.ID, chatbot.AssistantRunInput{Input: "current question", ConversationID: conversation.ID})
	if err != nil {
		t.Fatal(err)
	}
	body := string(f.executor.requests[0].Payload)
	if strings.Contains(body, "oldest") || !strings.Contains(body, "recent history") || !strings.Contains(body, "current question") {
		t.Fatalf("unexpected fitted context: %s", body)
	}
}

func TestRunToolPolicyDenialsAbort(t *testing.T) {
	t.Run("mcp inference denial", func(t *testing.T) {
		const call = `{"choices":[{"message":{"tool_calls":[{"id":"call","type":"function","function":{"name":"approved_lookup","arguments":"{}"}}]}}]}`
		f := newRunWorkerFixture(t, []string{call, runWorkerDone}, nil)
		f.svc.ConfigureMCP(&namedMCPDispatcher{err: inference.ErrDenied})
		member, _, _ := runWorkerPrincipals(f.tenant)
		bot := seedAudienceBot(t, f.chatRepo, f.tenant, []string{runWorkerMember})
		bot.Settings = map[string]any{"tools": []any{"approved.lookup"}}
		if err := f.chatRepo.Update(context.Background(), bot); err != nil {
			t.Fatal(err)
		}
		run, err := startAndRun(t, f, member, bot.ID, chatbot.AssistantRunInput{Input: "help"})
		if !errors.Is(err, inference.ErrDenied) || len(f.executor.requests) != 1 {
			t.Fatalf("denial=%v dispatches=%d", err, len(f.executor.requests))
		}
		attempts, err := f.runsRepo.ListRunAttempts(context.Background(), f.tenant, run.ID)
		if err != nil || len(attempts) != 2 || attempts[1].Status != "failed" {
			t.Fatalf("attempts=%#v err=%v", attempts, err)
		}
	})
	t.Run("extract forbidden", func(t *testing.T) {
		args, _ := json.Marshal(map[string]any{"template_id": "t", "file_name": "f.txt", "mime_type": "text/plain", "content_base64": "eA=="})
		call, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"tool_calls": []any{map[string]any{"id": "call", "type": "function", "function": map[string]any{"name": "extract_submit", "arguments": string(args)}}}}}}})
		f := newRunWorkerFixture(t, []string{string(call), runWorkerDone}, nil)
		f.svc.ConfigureRuns(f.runsRepo, f.jobSvc, &extractionStub{err: extract.ErrForbidden}, runPrincipalResolver{})
		member, _, _ := runWorkerPrincipals(f.tenant)
		bot := seedAudienceBot(t, f.chatRepo, f.tenant, []string{runWorkerMember})
		bot.Settings = map[string]any{"tools": []any{"extract.submit"}}
		if err := f.chatRepo.Update(context.Background(), bot); err != nil {
			t.Fatal(err)
		}
		_, err := startAndRun(t, f, member, bot.ID, chatbot.AssistantRunInput{Input: "help"})
		if !errors.Is(err, extract.ErrForbidden) || len(f.executor.requests) != 1 {
			t.Fatalf("forbidden=%v dispatches=%d", err, len(f.executor.requests))
		}
	})
}
