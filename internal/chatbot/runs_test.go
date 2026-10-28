package chatbot

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestRunModelResponseParsesFunctionCallsAndRejectsMalformedArguments(t *testing.T) {
	got, err := parseRunModelResponse([]byte(`{"choices":[{"message":{"content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"knowledge.search","arguments":"{\"collection_id\":\"c\",\"query\":\"q\"}"}}]}}]}`))
	if err != nil || len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != "knowledge.search" {
		t.Fatalf("parsed calls = %#v err=%v", got, err)
	}
	if _, err := parseRunModelResponse([]byte(`{"choices":[{"message":{"tool_calls":[{"id":"call-1","type":"function","function":{"name":"extract.submit","arguments":"not-json"}}]}}]}`)); err == nil {
		t.Fatal("malformed tool arguments were accepted")
	}
	if _, err := parseRunModelResponse(json.RawMessage(`{}`)); err == nil {
		t.Fatal("response without choices was accepted")
	}
}

func TestRunBoundsAreConcrete(t *testing.T) {
	if maxRunTurns <= 0 || maxRunToolCalls <= 0 || maxRunInputBytes <= 0 || maxRunContextBytes <= 0 || maxRunOutputBytes <= 0 || maxRunWallTime <= 0 {
		t.Fatal("run bounds must be positive")
	}
}

func TestNormalizeRunEvidenceIDsBoundsAndDedupes(t *testing.T) {
	if got, err := normalizeRunEvidenceIDs(nil); err != nil || got != nil {
		t.Fatalf("empty input = %#v err=%v", got, err)
	}
	got, err := normalizeRunEvidenceIDs([]string{" ev-2 ", "ev-1", "ev-2", ""})
	if err == nil {
		t.Fatalf("empty entry accepted: %#v", got)
	}
	got, err = normalizeRunEvidenceIDs([]string{" ev-2 ", "ev-1", "ev-2"})
	if err != nil || fmt.Sprint(got) != fmt.Sprint([]string{"ev-2", "ev-1"}) {
		t.Fatalf("normalized = %#v err=%v, want trimmed and deduped", got, err)
	}
	oversize := make([]string, maxRunEvidenceIDs+1)
	for i := range oversize {
		oversize[i] = fmt.Sprintf("ev-%d", i)
	}
	if _, err := normalizeRunEvidenceIDs(oversize); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversize evidence list = %v, want ErrInvalidInput", err)
	}
}

// TestModelAttemptSummaryIsToolSafe: the persisted model-attempt input is a
// route summary only — the assembled request body (system prompt, history,
// attached passages) never reaches a step row.
func TestModelAttemptSummaryIsToolSafe(t *testing.T) {
	summary := modelAttemptSummary("chat-test-model", 3, 2)
	var decoded struct {
		Model    string `json:"model"`
		Messages int    `json:"messages"`
		Tools    int    `json:"tools"`
	}
	if err := json.Unmarshal([]byte(summary), &decoded); err != nil {
		t.Fatalf("summary is not the bounded JSON shape: %v", err)
	}
	if decoded.Model != "chat-test-model" || decoded.Messages != 3 || decoded.Tools != 2 {
		t.Fatalf("summary = %q", summary)
	}
	if summary != `{"model":"chat-test-model","messages":3,"tools":2}` {
		t.Fatalf("summary carries more than the route shape: %q", summary)
	}
}
