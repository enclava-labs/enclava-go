package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

const maxSchemaBytes = 256 << 10
const maxInstanceBytes = 1 << 20

type rejectLoader struct{}

func (rejectLoader) Load(string) (any, error) {
	return nil, fmt.Errorf("remote JSON Schema references are disabled")
}

// CompileSchema accepts bounded JSON Schema 2020-12 and resolves only local
// references. No URL or filesystem schema fetch is possible.
func CompileSchema(raw json.RawMessage) (func(json.RawMessage) error, error) {
	if len(raw) == 0 || len(raw) > maxSchemaBytes || !json.Valid(raw) {
		return nil, fmt.Errorf("%w: invalid or oversized schema", ErrDenied)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: invalid schema", ErrDenied)
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: schema must be an object", ErrDenied)
	}
	if err := schemaPolicy(root, 0, new(int)); err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(rejectLoader{})
	const resource = "https://enclava.invalid/mcp/tool-schema.json"
	if err = compiler.AddResource(resource, doc); err != nil {
		return nil, fmt.Errorf("%w: invalid schema", ErrDenied)
	}
	compiled, err := compiler.Compile(resource)
	if err != nil {
		return nil, fmt.Errorf("%w: unsupported schema", ErrDenied)
	}
	return func(value json.RawMessage) error {
		if len(value) == 0 || len(value) > maxInstanceBytes || !json.Valid(value) {
			return ErrDenied
		}
		var parsed any
		dec := json.NewDecoder(bytes.NewReader(value))
		dec.UseNumber()
		if err := dec.Decode(&parsed); err != nil {
			return err
		}
		if dec.More() {
			return ErrDenied
		}
		if err := instancePolicy(parsed, 0, new(int)); err != nil {
			return err
		}
		return compiled.Validate(parsed)
	}, nil
}
func schemaPolicy(v any, depth int, nodes *int) error {
	*nodes++
	if depth > 128 || *nodes > 4096 {
		return ErrDenied
	}
	switch value := v.(type) {
	case map[string]any:
		for key, child := range value {
			switch key {
			case "$id":
				return fmt.Errorf("%w: schema identifiers are not accepted", ErrDenied)
			case "$schema":
				if child != "https://json-schema.org/draft/2020-12/schema" {
					return fmt.Errorf("%w: unsupported schema dialect", ErrDenied)
				}
			case "$ref":
				ref, ok := child.(string)
				if !ok || !strings.HasPrefix(ref, "#") {
					return fmt.Errorf("%w: only local schema references are allowed", ErrDenied)
				}
			default:
				if strings.HasSuffix(key, "Ref") {
					return fmt.Errorf("%w: unsupported schema reference", ErrDenied)
				}
			}
			if err := schemaPolicy(child, depth+1, nodes); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := schemaPolicy(child, depth+1, nodes); err != nil {
				return err
			}
		}
	}
	return nil
}

func instancePolicy(v any, depth int, nodes *int) error {
	*nodes++
	if depth > 128 || *nodes > 10000 {
		return ErrDenied
	}
	switch value := v.(type) {
	case map[string]any:
		for _, child := range value {
			if err := instancePolicy(child, depth+1, nodes); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := instancePolicy(child, depth+1, nodes); err != nil {
				return err
			}
		}
	}
	return nil
}
