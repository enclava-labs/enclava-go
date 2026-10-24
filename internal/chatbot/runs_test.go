package chatbot

import (
	"encoding/json"
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
