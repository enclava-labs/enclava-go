package extract

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/jobs"
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

// TestEffectiveMaxOutputPrecedence pins the cap ladder: the template's own
// max_output_tokens wins, the env-configured service default fills in, and
// neither set means no explicit cap is sent at all.
func TestEffectiveMaxOutputPrecedence(t *testing.T) {
	svc := &Service{}
	if got := svc.effectiveMaxOutput(Template{}); got != 0 {
		t.Fatalf("unset cap = %d, want 0 (no explicit cap)", got)
	}
	svc.SetDefaultMaxOutputTokens(4096)
	if got := svc.effectiveMaxOutput(Template{}); got != 4096 {
		t.Fatalf("env default = %d, want 4096", got)
	}
	if got := svc.effectiveMaxOutput(Template{MaxOutputTokens: 1024}); got != 1024 {
		t.Fatalf("template cap = %d, want 1024 to beat the env default", got)
	}
	svc.SetDefaultMaxOutputTokens(-5)
	if got := svc.effectiveMaxOutput(Template{}); got != 0 {
		t.Fatalf("negative default must clamp to no-cap, got %d", got)
	}
}

// TestBuildProviderRequestOmitsCapWhenUnset: an explicit template/env cap is
// the payload's max_tokens; no cap leaves the key absent so the executor's
// own default injection and model-limit clamp apply.
func TestBuildProviderRequestOmitsCapWhenUnset(t *testing.T) {
	svc := &Service{}
	req := svc.buildProviderRequest("m", Template{}, "s", "u", ProcessInput{MimeType: "text/plain"}, 0)
	if _, present := req["max_tokens"]; present {
		t.Fatalf("no-cap request declared max_tokens: %v", req["max_tokens"])
	}
	req = svc.buildProviderRequest("m", Template{}, "s", "u", ProcessInput{MimeType: "text/plain"}, 2048)
	if req["max_tokens"] != int64(2048) {
		t.Fatalf("cap request max_tokens=%v, want 2048", req["max_tokens"])
	}
}

// TestUpsertTemplateMaxOutputTokensValidation covers the accepted range:
// unset and 1..32768 pass, negatives and larger values are refused before
// the repo is touched.
func TestUpsertTemplateMaxOutputTokensValidation(t *testing.T) {
	svc := &Service{templatesRepo: nil}
	base := Template{ID: "t", Name: "t", SystemPrompt: "s", UserPrompt: "u"}
	p := auth.Principal{TenantID: "ten", ActorID: "a"}
	for _, bad := range []int{-1, -100, 32769, 1 << 20} {
		tpl := base
		tpl.MaxOutputTokens = bad
		err := svc.UpsertTemplate(context.Background(), p, tpl)
		if err == nil || !strings.Contains(err.Error(), "max_output_tokens") {
			t.Fatalf("cap %d = %v, want a max_output_tokens range error", bad, err)
		}
	}
}

// TestJobViewReadsValidationMarker: the fenced progress marker is the list
// row's validation verdict — a marker count >0 projects needs_review with
// the error count, zero projects valid, and rows without a marker stay
// silent (pre-marker jobs).
func TestJobViewReadsValidationMarker(t *testing.T) {
	row := jobs.JobRow{Job: jobs.Job{ID: "j1", Status: jobs.StatusSucceeded, Policy: map[string]any{}}}
	row.Progress = json.RawMessage(`{"percent":100,"message":"extract-validation:2"}`)
	v := jobView(row)
	if v.ValidationStatus != ValidationNeedsReview || v.ValidationErrorCount != 2 {
		t.Fatalf("marker row = %q/%d, want needs_review/2", v.ValidationStatus, v.ValidationErrorCount)
	}
	row.Progress = json.RawMessage(`{"percent":100,"message":"extract-validation:0"}`)
	v = jobView(row)
	if v.ValidationStatus != ValidationPassed || v.ValidationErrorCount != 0 {
		t.Fatalf("clean marker = %q/%d, want valid/0", v.ValidationStatus, v.ValidationErrorCount)
	}
	row.Progress = json.RawMessage(`{"percent":40,"message":"half done"}`)
	v = jobView(row)
	if v.ValidationStatus != "" {
		t.Fatalf("unmarked row surfaced %q, want no validation verdict", v.ValidationStatus)
	}
}
