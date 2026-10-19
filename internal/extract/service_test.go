package extract

import (
	"testing"
)

func TestParseJSONBody_CodeFence(t *testing.T) {
	m, err := parseJSONBody("```json\n{\"a\":1}\n```")
	if err != nil {
		t.Fatalf("parse fenced json: %v", err)
	}
	if m["a"].(float64) != 1 {
		t.Fatalf("unexpected map: %v", m)
	}
}

func TestValidateContextValues_NestedRejected(t *testing.T) {
	if err := validateContextValues(map[string]any{"ok": "v", "nested": map[string]any{"x": 1}}); err == nil {
		t.Fatal("expected nested value rejection")
	}
}

func TestValidateAgainstSchemaNullableObjectAndArrayAcceptNull(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"obj": map[string]any{"type": []string{"object", "null"}, "properties": map[string]any{"x": map[string]any{"type": "string"}}},
			"arr": map[string]any{"type": []string{"array", "null"}, "items": map[string]any{"type": "string"}},
		},
	}
	errs := validateAgainstSchema(map[string]any{"obj": nil, "arr": nil}, schema, "$")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

func TestValidateAgainstSchemaNonNullObjectAndArray(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"obj": map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}},
			"arr": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
	}
	errs := validateAgainstSchema(map[string]any{"obj": map[string]any{"x": "v"}, "arr": []any{"a"}}, schema, "$")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

func TestValidateAgainstSchemaNullRejectedWhenNotNullable(t *testing.T) {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"obj": map[string]any{"type": "object"}},
	}
	errs := validateAgainstSchema(map[string]any{"obj": nil}, schema, "$")
	if len(errs) == 0 {
		t.Fatal("expected rejection for null object")
	}
}

func TestValidateAgainstSchemaRequiredAndNullableProperty(t *testing.T) {
	schema := map[string]any{
		"type":     "object",
		"required": []any{"req"},
		"properties": map[string]any{
			"req": map[string]any{"type": []string{"string", "null"}},
		},
	}
	errs := validateAgainstSchema(map[string]any{}, schema, "$")
	if len(errs) == 0 {
		t.Fatal("expected required-field error")
	}
	errs = validateAgainstSchema(map[string]any{"req": nil}, schema, "$")
	if len(errs) != 0 {
		t.Fatalf("nullable field should accept null: %v", errs)
	}
}
