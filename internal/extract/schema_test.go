package extract

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCompileOutputSchemaLocalCompositionAndPrecision(t *testing.T) {
	schema := map[string]any{
		"$schema": outputSchemaVersion,
		"$defs": map[string]any{
			"amount": map[string]any{"type": "number", "minimum": 9007199254740993},
		},
		"type": "object",
		"properties": map[string]any{
			"amount": map[string]any{"$ref": "#/$defs/amount"},
			"kind":   map[string]any{"enum": []any{"1", 1}},
			"choice": map[string]any{"oneOf": []any{map[string]any{"const": "selected"}, map[string]any{"const": 1}}},
		},
		"required":             []any{"amount", "kind"},
		"allOf":                []any{map[string]any{"not": map[string]any{"properties": map[string]any{"kind": map[string]any{"const": "bad"}}}}},
		"additionalProperties": false,
	}
	compiled, err := compileOutputSchema(schema)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, value := range []map[string]any{
		{"amount": jsonNum("9007199254740993"), "kind": "1", "choice": "selected"},
		{"amount": jsonNum("9007199254740993.0001"), "kind": jsonNum("1"), "choice": jsonNum("1")},
	} {
		if err := compiled.validate(value); err != nil {
			t.Errorf("expected valid value %#v: %v", value, err)
		}
	}
	for _, value := range []map[string]any{
		{"amount": jsonNum("9007199254740992"), "kind": "1"},
		{"amount": jsonNum("9007199254740993"), "kind": jsonNum("2")},
		{"amount": jsonNum("9007199254740993"), "kind": "bad"},
		{"amount": jsonNum("9007199254740993"), "kind": "1", "choice": "other"},
	} {
		var validation *SchemaValidationError
		if err := compiled.validate(value); !errors.As(err, &validation) {
			t.Errorf("expected typed schema failure for %#v, got %v", value, err)
		} else if !strings.Contains(validation.InstanceLocation, "/") {
			t.Errorf("expected actual instance location, got %q", validation.InstanceLocation)
		}
	}
}

func TestSchemaEnumPreservesStringAndNumberTypes(t *testing.T) {
	compiled, err := compileOutputSchema(map[string]any{"enum": []any{"1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := compiled.validate("1"); err != nil {
		t.Fatalf("string enum value rejected: %v", err)
	}
	var validation *SchemaValidationError
	if err := compiled.validate(json.Number("1")); !errors.As(err, &validation) {
		t.Fatalf("numeric value was coerced to string enum: %v", err)
	}
}

func TestCompileOutputSchemaRejectsUnsupportedAndExternalFeatures(t *testing.T) {
	for name, schema := range map[string]map[string]any{
		"old dialect":        {"$schema": "http://json-schema.org/draft-07/schema#", "type": "string"},
		"external ref":       {"$ref": "https://example.invalid/schema.json"},
		"file ref":           {"$ref": "file:///etc/passwd"},
		"unknown vocabulary": {"$vocabulary": map[string]any{"https://example.invalid/vocab": true}},
		"unknown keyword":    {"x-provider-schema": true},
		"unsupported id":     {"$id": "https://example.invalid/schema", "type": "string"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := compileOutputSchema(schema); err == nil {
				t.Fatal("expected schema rejection")
			}
		})
	}
	requests := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- struct{}{}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"string"}`))
	}))
	defer server.Close()
	if _, err := compileOutputSchema(map[string]any{"$ref": server.URL + "/schema.json"}); err == nil {
		t.Fatal("external HTTP reference was accepted")
	}
	select {
	case <-requests:
		t.Fatal("validator fetched an external schema")
	case <-time.After(25 * time.Millisecond):
	}
}

func TestCompileOutputSchemaRejectsReferenceCyclesAndWorkExpansion(t *testing.T) {
	cycle := map[string]any{
		"$defs": map[string]any{"node": map[string]any{"$ref": "#/$defs/node"}},
		"$ref":  "#/$defs/node",
	}
	if _, err := compileOutputSchema(cycle); !errors.Is(err, ErrSchemaPolicy) {
		t.Fatalf("expected explicit cycle policy error, got %v", err)
	}

	defs := map[string]any{"leaf": map[string]any{"type": "string"}}
	for i := range 8 {
		branches := make([]any, 16)
		for j := range branches {
			branches[j] = map[string]any{"$ref": fmt.Sprintf("#/$defs/layer%d", i-1)}
		}
		defs[fmt.Sprintf("layer%d", i)] = map[string]any{"anyOf": branches}
	}
	schema := map[string]any{"$defs": defs, "$ref": "#/$defs/layer7"}
	if _, err := compileOutputSchema(schema); !errors.Is(err, ErrSchemaPolicy) {
		t.Fatalf("expected expansion budget rejection, got %v", err)
	}
}

func TestCompileOutputSchemaSupportsArrayAndUnevaluatedProperties(t *testing.T) {
	compiled, err := compileOutputSchema(map[string]any{
		"type":                  "object",
		"allOf":                 []any{map[string]any{"properties": map[string]any{"values": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}}}},
		"unevaluatedProperties": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := compiled.validate(map[string]any{"values": []any{jsonNum("1"), jsonNum("2")}}); err != nil {
		t.Fatalf("valid array rejected: %v", err)
	}
	var validation *SchemaValidationError
	if err := compiled.validate(map[string]any{"values": []any{jsonNum("1"), "bad"}}); !errors.As(err, &validation) {
		t.Fatalf("expected typed array validation error, got %v", err)
	}
}

func TestCompileOutputSchemaRejectsInstanceResourceLimits(t *testing.T) {
	compiled, err := compileOutputSchema(map[string]any{"type": "string"})
	if err != nil {
		t.Fatal(err)
	}
	if err := compiled.validate(strings.Repeat("x", maxInstanceBytes)); !errors.Is(err, ErrSchemaPolicy) {
		t.Fatalf("expected instance size policy error, got %v", err)
	}
}

func jsonNum(s string) json.Number { return json.Number(s) }
