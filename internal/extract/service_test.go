package extract

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestJobOperationsResolvesMemberFacingSet pins the closed ?operation= set:
// empty means the member-facing extraction pair, a member isolates it, and
// anything else — including shared operations like document.preview — is
// rejected rather than silently widened.
func TestJobOperationsResolvesMemberFacingSet(t *testing.T) {
	ops, err := jobOperations("")
	if err != nil || len(ops) != 2 {
		t.Fatalf("default ops=%v err=%v, want process+vision", ops, err)
	}
	ops, err = jobOperations(" " + OperationProcess + " ")
	if err != nil || len(ops) != 1 || ops[0] != OperationProcess {
		t.Fatalf("explicit ops=%v err=%v, want [extract.process]", ops, err)
	}
	for _, bogus := range []string{"bogus", "document.preview", "extract.preview"} {
		if _, err := jobOperations(bogus); !errors.Is(err, ErrInvalidOperation) {
			t.Fatalf("jobOperations(%q) = %v, want ErrInvalidOperation", bogus, err)
		}
	}
}

func TestParseJSONBodyCodeFencePreservesNumber(t *testing.T) {
	got, err := parseJSONBody("```json\n{\"a\":9007199254740993.0001}\n```")
	if err != nil {
		t.Fatalf("parse fenced json: %v", err)
	}
	if got["a"] != json.Number("9007199254740993.0001") {
		t.Fatalf("number was not preserved exactly: %#v", got["a"])
	}
}
func TestParseProviderExtractionKeepsNumericPrecision(t *testing.T) {
	data, _, usage, err := parseProviderExtraction([]byte(`{"usage":{"prompt_tokens":9007199254740993},"choices":[{"message":{"content":"{\"amount\":9007199254740993.0001}"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if usage.Prompt != 9007199254740993 {
		t.Fatalf("provider usage was rounded: %d", usage.Prompt)
	}
	if data["amount"] != json.Number("9007199254740993.0001") {
		t.Fatalf("provider output number was rounded: %#v", data["amount"])
	}
}

func TestValidateContextValuesNestedRejected(t *testing.T) {
	if err := validateContextValues(map[string]any{"ok": "v", "nested": map[string]any{"x": 1}}); err == nil {
		t.Fatal("expected nested value rejection")
	}
}

func TestExtractionOutputUsesLocalSchemaAndReportsAccuracyWarning(t *testing.T) {
	service := &Service{}
	template := Template{
		ID: "custom",
		OutputSchema: map[string]any{
			"$schema": outputSchemaVersion,
			"$defs": map[string]any{
				"amount": map[string]any{"type": "integer"},
			},
			"type": "object",
			"properties": map[string]any{
				"amount": map[string]any{"$ref": "#/$defs/amount"},
			},
			"required":             []any{"amount"},
			"additionalProperties": false,
		},
	}
	valid, warnings := service.validateOutput(map[string]any{"amount": json.Number("9007199254740993")}, template)
	if len(valid) != 0 {
		t.Fatalf("valid generated result rejected: %v", valid)
	}
	if len(warnings) != 1 || warnings[0] != "schema-valid output does not establish factual accuracy" {
		t.Fatalf("expected explicit factual-accuracy warning, got %v", warnings)
	}
	invalid, _ := service.validateOutput(map[string]any{"amount": "9007199254740993"}, template)
	if len(invalid) == 0 || !strings.Contains(invalid[0], "/amount") {
		t.Fatalf("schema-invalid generated result did not report the instance location: %v", invalid)
	}
}
