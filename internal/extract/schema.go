package extract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	outputSchemaVersion = "https://json-schema.org/draft/2020-12/schema"
	maxSchemaBytes      = 256 << 10
	maxInstanceBytes    = 1 << 20
	maxSchemaDepth      = 128
	maxSchemaNodes      = 4096
	maxExpandedWork     = 50_000 // Ref-expanded schema node budget, checked before compilation.
	maxInstanceNodes    = 10_000
	maxValidationWork   = 5_000_000
)

var ErrSchemaPolicy = errors.New("extract: schema exceeds local validation policy")

// SchemaValidationError preserves validator locations for consumer-visible failures.
type SchemaValidationError struct {
	SchemaLocation   string
	InstanceLocation string
	Message          string
}

func (e *SchemaValidationError) Error() string {
	return fmt.Sprintf("schema validation failed at %s (schema %s): %s", e.InstanceLocation, e.SchemaLocation, e.Message)
}

type compiledOutputSchema struct {
	schema *jsonschema.Schema
	work   int
}

// compileOutputSchema accepts only the declared 2020-12 dialect and local JSON Pointer references.
func compileOutputSchema(schema map[string]any) (*compiledOutputSchema, error) {
	if len(schema) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("encode output schema: %w", err)
	}
	if len(raw) > maxSchemaBytes {
		return nil, fmt.Errorf("%w: schema exceeds %d bytes", ErrSchemaPolicy, maxSchemaBytes)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decode output schema: %w", err)
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return nil, errors.New("output schema must be a JSON object")
	}
	if err := checkSchemaDialect(root); err != nil {
		return nil, err
	}
	work, err := schemaWork(root)
	if err != nil {
		return nil, err
	}

	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(rejectSchemaLoader{})
	compiler.AssertFormat()
	compiler.AssertContent()
	const resource = "https://enclava.invalid/extract/output-schema.json"
	if err := compiler.AddResource(resource, doc); err != nil {
		return nil, fmt.Errorf("register output schema: %w", err)
	}
	compiled, err := compiler.Compile(resource)
	if err != nil {
		return nil, fmt.Errorf("compile output schema: %w", err)
	}
	return &compiledOutputSchema{schema: compiled, work: work}, nil
}

func (s *compiledOutputSchema) validate(value any) error {
	if s == nil {
		return nil
	}
	var encoded bytes.Buffer
	if err := json.NewEncoder(&encoded).Encode(value); err != nil {
		return fmt.Errorf("encode extraction output: %w", err)
	}
	if encoded.Len() > maxInstanceBytes {
		return fmt.Errorf("%w: instance exceeds %d bytes", ErrSchemaPolicy, maxInstanceBytes)
	}
	nodes, err := checkJSONDepth(encoded.Bytes())
	if err != nil {
		return err
	}
	if nodes > 0 && s.work > maxValidationWork/nodes {
		return fmt.Errorf("%w: validation work exceeds %d", ErrSchemaPolicy, maxValidationWork)
	}
	if err := s.schema.Validate(value); err != nil {
		var validation *jsonschema.ValidationError
		if errors.As(err, &validation) {
			validation = mostSpecificValidation(validation)
			instanceLocation := strings.Join(validation.InstanceLocation, "/")
			if instanceLocation != "" {
				instanceLocation = "/" + instanceLocation
			}
			return &SchemaValidationError{SchemaLocation: validation.SchemaURL, InstanceLocation: instanceLocation, Message: validation.Error()}
		}
		return fmt.Errorf("validate extraction output: %w", err)
	}
	return nil
}
func mostSpecificValidation(root *jsonschema.ValidationError) *jsonschema.ValidationError {
	best := root
	for _, cause := range root.Causes {
		candidate := mostSpecificValidation(cause)
		if len(candidate.InstanceLocation) > len(best.InstanceLocation) {
			best = candidate
		}
	}
	return best
}

type rejectSchemaLoader struct{}

func (rejectSchemaLoader) Load(raw string) (any, error) {
	return nil, fmt.Errorf("external schema resource %q is forbidden", raw)
}

var acceptedVocabularies = map[string]bool{
	"https://json-schema.org/draft/2020-12/vocab/core":              true,
	"https://json-schema.org/draft/2020-12/vocab/applicator":        true,
	"https://json-schema.org/draft/2020-12/vocab/unevaluated":       true,
	"https://json-schema.org/draft/2020-12/vocab/validation":        true,
	"https://json-schema.org/draft/2020-12/vocab/meta-data":         true,
	"https://json-schema.org/draft/2020-12/vocab/format-annotation": true,
	"https://json-schema.org/draft/2020-12/vocab/format-assertion":  true,
	"https://json-schema.org/draft/2020-12/vocab/content":           true,
}

var supportedSchemaKeywords = map[string]bool{
	"$schema": true, "$id": true, "$vocabulary": true, "$anchor": true, "$dynamicAnchor": true,
	"$ref": true, "$dynamicRef": true, "$defs": true, "$comment": true,
	"title": true, "description": true, "default": true, "deprecated": true, "readOnly": true, "writeOnly": true, "examples": true,
	"type": true, "enum": true, "const": true, "multipleOf": true, "maximum": true, "exclusiveMaximum": true, "minimum": true, "exclusiveMinimum": true,
	"maxLength": true, "minLength": true, "pattern": true,
	"maxItems": true, "minItems": true, "uniqueItems": true, "maxContains": true, "minContains": true,
	"items": true, "prefixItems": true, "contains": true,
	"maxProperties": true, "minProperties": true, "required": true,
	"properties": true, "patternProperties": true, "additionalProperties": true,
	"dependentRequired": true, "dependentSchemas": true, "propertyNames": true,
	"if": true, "then": true, "else": true, "allOf": true, "anyOf": true, "oneOf": true, "not": true,
	"unevaluatedItems": true, "unevaluatedProperties": true, "format": true,
	"contentEncoding": true, "contentMediaType": true, "contentSchema": true,
}

func checkSchemaDialect(root map[string]any) error {
	var visitSchema func(any) error
	visitSchema = func(value any) error {
		schema, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		for key := range schema {
			if !supportedSchemaKeywords[key] {
				return fmt.Errorf("unsupported JSON Schema keyword %q", key)
			}
		}
		if dialect, ok := schema["$schema"]; ok && dialect != outputSchemaVersion {
			return fmt.Errorf("unsupported JSON Schema dialect %v; require %s", dialect, outputSchemaVersion)
		}
		if id, ok := schema["$id"]; ok && id != "" {
			return fmt.Errorf("%w: schema $id must be omitted or empty", ErrSchemaPolicy)
		}
		if vocab, ok := schema["$vocabulary"].(map[string]any); ok {
			for name, required := range vocab {
				if !acceptedVocabularies[name] {
					return fmt.Errorf("unsupported JSON Schema vocabulary %q", name)
				}
				if _, ok := required.(bool); !ok {
					return fmt.Errorf("invalid $vocabulary declaration for %q", name)
				}
			}
		}
		for _, keyword := range []string{"$ref", "$dynamicRef"} {
			if ref, ok := schema[keyword].(string); ok {
				if err := checkLocalPointerRef(ref); err != nil {
					return err
				}
			}
		}
		for keyword, child := range schema {
			switch keyword {
			case "$defs", "properties", "patternProperties", "dependentSchemas":
				if schemas, ok := child.(map[string]any); ok {
					for _, nested := range schemas {
						if err := visitSchema(nested); err != nil {
							return err
						}
					}
				}
			case "allOf", "anyOf", "oneOf", "prefixItems":
				if schemas, ok := child.([]any); ok {
					for _, nested := range schemas {
						if err := visitSchema(nested); err != nil {
							return err
						}
					}
				}
			case "items", "contains", "additionalProperties", "unevaluatedProperties", "unevaluatedItems",
				"propertyNames", "if", "then", "else", "not", "contentSchema":
				if err := visitSchema(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return visitSchema(root)
}

func checkLocalPointerRef(ref string) error {
	parsed, err := url.Parse(ref)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.RawQuery != "" || parsed.Path != "" {
		return fmt.Errorf("%w: only local fragment references are supported: %q", ErrSchemaPolicy, ref)
	}
	fragment, err := url.PathUnescape(parsed.EscapedFragment())
	if err != nil || (fragment != "" && !strings.HasPrefix(fragment, "/")) {
		return fmt.Errorf("%w: only JSON Pointer fragment references are supported: %q", ErrSchemaPolicy, ref)
	}
	return nil
}

// schemaWork bounds schema-tree size and recursively expands references before compilation.
func schemaWork(root map[string]any) (int, error) {
	byPointer := map[string]any{}
	nodes := 0
	var index func(any, string, int) error
	index = func(value any, pointer string, depth int) error {
		if depth > maxSchemaDepth {
			return fmt.Errorf("%w: schema depth exceeds %d", ErrSchemaPolicy, maxSchemaDepth)
		}
		switch v := value.(type) {
		case map[string]any:
			nodes++
			byPointer[pointer] = v
			for key, child := range v {
				if err := index(child, pointer+"/"+strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1"), depth+1); err != nil {
					return err
				}
			}
		case []any:
			for i, child := range v {
				if err := index(child, fmt.Sprintf("%s/%d", pointer, i), depth+1); err != nil {
					return err
				}
			}
		}
		if nodes > maxSchemaNodes {
			return fmt.Errorf("%w: schema node count exceeds %d", ErrSchemaPolicy, maxSchemaNodes)
		}
		return nil
	}
	if err := index(root, "", 0); err != nil {
		return 0, err
	}
	active := map[string]bool{}
	var cost func(any, string, int) (int, error)
	cost = func(value any, pointer string, depth int) (int, error) {
		if depth > maxSchemaDepth {
			return 0, fmt.Errorf("%w: schema depth exceeds %d", ErrSchemaPolicy, maxSchemaDepth)
		}
		switch v := value.(type) {
		case map[string]any:
			total := 1
			for keyword, child := range v {
				switch keyword {
				case "$defs", "properties", "patternProperties", "dependentSchemas":
					if schemas, ok := child.(map[string]any); ok {
						for name, nested := range schemas {
							escaped := strings.ReplaceAll(strings.ReplaceAll(name, "~", "~0"), "/", "~1")
							childCost, err := cost(nested, pointer+"/"+keyword+"/"+escaped, depth+1)
							if err != nil {
								return 0, err
							}
							total += childCost
						}
					}
				case "allOf", "anyOf", "oneOf", "prefixItems":
					if schemas, ok := child.([]any); ok {
						for i, nested := range schemas {
							childCost, err := cost(nested, fmt.Sprintf("%s/%s/%d", pointer, keyword, i), depth+1)
							if err != nil {
								return 0, err
							}
							total += childCost
						}
					}
				case "items", "contains", "additionalProperties", "unevaluatedProperties", "unevaluatedItems",
					"propertyNames", "if", "then", "else", "not", "contentSchema":
					childCost, err := cost(child, pointer+"/"+keyword, depth+1)
					if err != nil {
						return 0, err
					}
					total += childCost
				}
				if total > maxExpandedWork {
					return 0, fmt.Errorf("%w: expanded schema work exceeds %d", ErrSchemaPolicy, maxExpandedWork)
				}
			}
			for key, rawRef := range v {
				if key != "$ref" && key != "$dynamicRef" {
					continue
				}
				ref, ok := rawRef.(string)
				if !ok {
					continue
				}
				fragment, err := url.PathUnescape(strings.TrimPrefix(ref, "#"))
				if err != nil {
					return 0, fmt.Errorf("%w: invalid reference %q", ErrSchemaPolicy, ref)
				}
				target := fragment
				if target != "" {
					if !strings.HasPrefix(target, "/") {
						return 0, fmt.Errorf("%w: reference %q is not a JSON Pointer", ErrSchemaPolicy, ref)
					}
					for _, token := range strings.Split(target[1:], "/") {
						if strings.Contains(strings.ReplaceAll(token, "~0", ""), "~") {
							return 0, fmt.Errorf("%w: invalid JSON Pointer %q", ErrSchemaPolicy, ref)
						}
					}
				}
				targetValue, exists := byPointer[target]
				if !exists {
					continue // The compiler reports unresolved local pointers with schema location.
				}
				if active[target] {
					return 0, fmt.Errorf("%w: cyclic local reference expansion at %q", ErrSchemaPolicy, ref)
				}
				active[target] = true
				targetCost, err := cost(targetValue, target, depth+1)
				delete(active, target)
				if err != nil {
					return 0, err
				}
				total += targetCost
				if total > maxExpandedWork {
					return 0, fmt.Errorf("%w: expanded schema work exceeds %d", ErrSchemaPolicy, maxExpandedWork)
				}
			}
			return total, nil
		default:
			return 0, nil
		}
	}
	active[""] = true
	work, err := cost(root, "", 0)
	if err != nil {
		return 0, err
	}
	if work > maxExpandedWork {
		return 0, fmt.Errorf("%w: expanded schema work %d exceeds %d", ErrSchemaPolicy, work, maxExpandedWork)
	}
	return work, nil
}

func checkJSONDepth(data []byte) (int, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	depth, nodes := 0, 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nodes, nil
		}
		if err != nil {
			return 0, fmt.Errorf("decode output instance: %w", err)
		}
		nodes++
		if nodes > maxInstanceNodes {
			return 0, fmt.Errorf("%w: instance node count exceeds %d", ErrSchemaPolicy, maxInstanceNodes)
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{', '[':
				depth++
				if depth > maxSchemaDepth {
					return 0, fmt.Errorf("%w: instance depth exceeds %d", ErrSchemaPolicy, maxSchemaDepth)
				}
			case '}', ']':
				depth--
			}
		}
	}
}
