package chatbot

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

func TestRunModelResponsePreservesMalformedArguments(t *testing.T) {
	got, err := parseRunModelResponse([]byte(`{"choices":[{"message":{"content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"knowledge.search","arguments":"{\"collection_id\":\"c\",\"query\":\"q\"}"}}]}}]}`))
	if err != nil || len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != "knowledge.search" {
		t.Fatalf("parsed calls = %#v err=%v", got, err)
	}
	malformed, err := parseRunModelResponse([]byte(`{"choices":[{"message":{"tool_calls":[{"id":"call-1","type":"function","function":{"name":"extract.submit","arguments":"not-json"}}]}}]}`))
	if err != nil || string(malformed.ToolCalls[0].Arguments) != "not-json" {
		t.Fatalf("malformed tool arguments lost: %#v %v", malformed, err)
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
	summary := modelAttemptSummary("chat-test-model", "job-1", "attempt-1", 3, 2)
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
	if summary != `{"model":"chat-test-model","job_id":"job-1","attempt_id":"attempt-1","messages":3,"tools":2}` {
		t.Fatalf("summary carries more than the route shape: %q", summary)
	}
}

func TestModelToolNamesAreSafeAndReversible(t *testing.T) {
	ids := []string{"knowledge.search", "knowledge_search", strings.Repeat("x", 70), strings.Repeat("x", 69) + "y"}
	definitions := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		definitions = append(definitions, map[string]any{"function": map[string]any{"name": id}})
	}
	mapping := modelToolNames(definitions)
	valid := regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	for i, definition := range definitions {
		name := definition["function"].(map[string]any)["name"].(string)
		if !valid.MatchString(name) || mapping[name] != ids[i] {
			t.Fatalf("name %q maps to %q, want %q", name, mapping[name], ids[i])
		}
	}
	if len(mapping) != len(ids) {
		t.Fatalf("colliding names: %#v", mapping)
	}
}

func TestFitRunContextTrimsHistoryThenTools(t *testing.T) {
	messages := []map[string]any{
		{"role": "system", "content": "system"},
		{"role": "user", "content": strings.Repeat("old", 100)},
		{"role": "assistant", "content": "newer history"},
		{"role": "user", "content": "current"},
		{"role": "tool", "tool_call_id": "1", "content": strings.Repeat("result", 200)},
	}
	payload := map[string]any{"model": "test"}
	history := 2
	body, fitted, err := fitRunContext(payload, messages, 1, &history, 300)
	if err != nil || len(body) > 300 || history != 0 || len(fitted) != 3 {
		t.Fatalf("body=%d history=%d messages=%#v err=%v", len(body), history, fitted, err)
	}
	if fitted[1]["content"] != "current" || !strings.Contains(fitted[2]["content"].(string), "[truncated to fit context]") {
		t.Fatalf("current message or truncation missing: %#v", fitted)
	}
	history = 0
	_, _, err = fitRunContext(map[string]any{}, []map[string]any{{"role": "system", "content": strings.Repeat("x", 500)}}, 1, &history, 100)
	if err == nil || !strings.Contains(err.Error(), "run context exceeds the model's context budget") {
		t.Fatalf("fixed context error = %v", err)
	}
}

type marshalCounter struct {
	calls *int
	n     int
}

func (c marshalCounter) MarshalJSON() ([]byte, error) {
	*c.calls++
	return json.Marshal(map[string]int{"n": c.n})
}

func TestFitRunContextTrimsHistoryLinearly(t *testing.T) {
	const historyLen = 500
	messages := make([]map[string]any, 0, 1+historyLen+1)
	messages = append(messages, map[string]any{"role": "system", "content": "system"})
	calls := new(int)
	for i := range historyLen {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		messages = append(messages, map[string]any{"role": role, "content": marshalCounter{calls, i}})
	}
	messages = append(messages, map[string]any{"role": "user", "content": "current"})
	payload := map[string]any{"model": "test"}
	payload["messages"] = append(append([]map[string]any{}, messages[:1]...), messages[historyLen/2+1:]...)
	kept, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	budget := len(kept)
	history := historyLen
	*calls = 0
	body, fitted, err := fitRunContext(payload, messages, 1, &history, budget)
	if err != nil {
		t.Fatal(err)
	}
	if history != historyLen/2 || len(fitted) != len(messages)-historyLen/2 || len(body) > budget {
		t.Fatalf("body=%d history=%d messages=%d", len(body), history, len(fitted))
	}
	if string(body) != string(kept) {
		t.Fatal("fitted payload differs from expected history suffix")
	}
	if fitted[1]["content"].(marshalCounter).n != historyLen/2 {
		t.Fatalf("oldest retained history = %#v, want index %d", fitted[1]["content"], historyLen/2)
	}
	if *calls > 2*len(messages) {
		t.Fatalf("message marshals %d is not linear for %d messages", *calls, len(messages))
	}
}

func TestTruncateToolOutputKeepsMarkerAndUTF8(t *testing.T) {
	text := strings.Repeat("é", maxBuiltinOutputBytes)
	got := truncateRunText(text, maxBuiltinOutputBytes, "[truncated]")
	if len(got) > maxBuiltinOutputBytes || !strings.HasSuffix(got, "[truncated]") || !json.Valid([]byte(`"`+got+`"`)) {
		t.Fatalf("invalid truncation length=%d", len(got))
	}
}
