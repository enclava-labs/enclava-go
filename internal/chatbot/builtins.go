package chatbot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"enclava-go/internal/auth"
	"enclava-go/internal/extract"
	"enclava-go/internal/mcp"
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
)

const maxBuiltinOutputBytes = 64 << 10

var builtinSchemas = map[string]json.RawMessage{
	"knowledge.search": json.RawMessage(`{"type":"object","properties":{"collection_id":{"type":"string","minLength":1},"query":{"type":"string","minLength":1,"maxLength":8192}},"required":["collection_id","query"],"additionalProperties":false}`),
	"evidence.read":    json.RawMessage(`{"type":"object","properties":{"evidence_id":{"type":"string","minLength":1}},"required":["evidence_id"],"additionalProperties":false}`),
	"extract.submit":   json.RawMessage(`{"type":"object","properties":{"template_id":{"type":"string","minLength":1},"file_name":{"type":"string","minLength":1},"mime_type":{"type":"string","minLength":1},"content_base64":{"type":"string","minLength":1,"maxLength":2097152},"context":{"type":"object"}},"required":["template_id","file_name","mime_type","content_base64"],"additionalProperties":false}`),
	"extract.status":   json.RawMessage(`{"type":"object","properties":{"job_id":{"type":"string","minLength":1}},"required":["job_id"],"additionalProperties":false}`),
}

type BuiltinResult struct {
	Value       json.RawMessage
	InputRefs   []source.ObjectRef
	EvidenceIDs []string
}

type EvidenceReader interface {
	ReadEvidence(ctx context.Context, p auth.Principal, evidenceID string) (rag.EvidenceText, error)
}

type Builtins struct {
	RAG       RAGSearcher
	Evidence  EvidenceReader
	Extractor Extractor
	Owners    interface {
		PrincipalForActor(context.Context, string, string) (auth.Principal, error)
	}
}

func BuiltinToolDefinitions(bot Instance) ([]map[string]any, error) {
	tools, err := builtinToolDefinitions(bot, nil)
	if err == nil {
		modelToolNames(tools)
	}
	return tools, err
}

func builtinToolDefinitions(bot Instance, allowedMCP map[string]struct{}) ([]map[string]any, error) {
	ids := configuredToolIDs(bot)
	tools := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		if _, ok := allowedMCP[id]; ok {
			continue
		}
		schema, ok := builtinSchemas[id]
		if !ok {
			return nil, fmt.Errorf("chatbot: unknown built-in tool %q", id)
		}
		if _, err := mcp.CompileSchema(schema); err != nil {
			return nil, fmt.Errorf("chatbot: invalid built-in tool schema %s: %w", id, err)
		}
		var parameters map[string]any
		if err := json.Unmarshal(schema, &parameters); err != nil {
			return nil, err
		}
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
			"name": id, "description": builtinDescription(id), "parameters": parameters,
		}})
	}
	return tools, nil
}

func (b Builtins) Invoke(ctx context.Context, p auth.Principal, bot Instance, toolID string, args json.RawMessage, inheritedRefs []source.ObjectRef) (BuiltinResult, error) {
	if !toolEnabled(bot, toolID) || len(args) == 0 || !json.Valid(args) {
		return BuiltinResult{}, ErrInvalidInput
	}
	rawSchema, ok := builtinSchemas[toolID]
	if !ok {
		return BuiltinResult{}, ErrInvalidInput
	}
	validate, err := mcp.CompileSchema(rawSchema)
	if err != nil || mcp.ValidateJSON(validate, args) != nil {
		return BuiltinResult{}, ErrInvalidInput
	}
	switch toolID {
	case "knowledge.search":
		if b.RAG == nil {
			return BuiltinResult{}, errors.New("chatbot: knowledge service is unavailable")
		}
		var in struct {
			CollectionID string `json:"collection_id"`
			Query        string `json:"query"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return BuiltinResult{}, ErrInvalidInput
		}
		if !contains(bot.RAGCollectionIDs, in.CollectionID) {
			return BuiltinResult{}, source.ErrDenied
		}
		lister, ok := b.RAG.(interface {
			ListCollections(context.Context, auth.Principal) ([]rag.Collection, error)
		})
		if !ok {
			return BuiltinResult{}, source.ErrDenied
		}
		owner := p
		if bot.OwnerActorID != "" && bot.OwnerActorID != strings.TrimSpace(p.ActorID) {
			if b.Owners == nil {
				return BuiltinResult{}, source.ErrDenied
			}
			owner, err = b.Owners.PrincipalForActor(ctx, p.TenantID, bot.OwnerActorID)
			if err != nil {
				return BuiltinResult{}, fmt.Errorf("resolve assistant owner authority: %w", source.ErrDenied)
			}
		}
		for _, actor := range []auth.Principal{p, owner} {
			collections, err := lister.ListCollections(ctx, actor)
			if err != nil {
				return BuiltinResult{}, err
			}
			readable := false
			for _, collection := range collections {
				if collection.ID == in.CollectionID {
					readable = true
					break
				}
			}
			if !readable {
				return BuiltinResult{}, source.ErrDenied
			}
		}
		results, err := b.RAG.Search(ctx, p, rag.SearchInput{CollectionID: in.CollectionID, Query: in.Query, Limit: 5})
		if err != nil {
			return BuiltinResult{}, err
		}
		contextResult, err := b.RAG.BuildContext(ctx, p, results, 6000)
		if err != nil {
			return BuiltinResult{}, err
		}
		value, err := json.Marshal(map[string]any{"evidence_ids": evidenceIDsForResults(results), "content": contextResult.Text})
		if err != nil {
			return BuiltinResult{}, err
		}
		return BuiltinResult{Value: value, InputRefs: ragInputs(results), EvidenceIDs: evidenceIDsForResults(results)}, nil
	case "evidence.read":
		if b.Evidence == nil {
			return BuiltinResult{}, errors.New("chatbot: evidence reader is unavailable")
		}
		var in struct {
			EvidenceID string `json:"evidence_id"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return BuiltinResult{}, ErrInvalidInput
		}
		text, err := b.Evidence.ReadEvidence(ctx, p, in.EvidenceID)
		if err != nil {
			return BuiltinResult{}, err
		}
		value, err := json.Marshal(text)
		if err != nil {
			return BuiltinResult{}, err
		}
		return BuiltinResult{Value: value, InputRefs: ragInputs([]rag.SearchResult{{DocumentID: text.DocumentID, Spans: text.Spans}}), EvidenceIDs: []string{text.EvidenceID}}, nil
	case "extract.submit":
		if b.Extractor == nil {
			return BuiltinResult{}, errors.New("chatbot: extraction service is unavailable")
		}
		if len(inheritedRefs) > 0 {
			return BuiltinResult{}, errors.New("chatbot: extraction cannot consume source-derived run context")
		}
		var in struct {
			TemplateID    string         `json:"template_id"`
			FileName      string         `json:"file_name"`
			MIMEType      string         `json:"mime_type"`
			ContentBase64 string         `json:"content_base64"`
			Context       map[string]any `json:"context"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return BuiltinResult{}, ErrInvalidInput
		}
		data, err := base64.StdEncoding.DecodeString(in.ContentBase64)
		if err != nil {
			return BuiltinResult{}, ErrInvalidInput
		}
		result, err := b.Extractor.Process(ctx, p, extract.ProcessInput{
			TemplateID: in.TemplateID, FileName: in.FileName, MimeType: in.MIMEType,
			Bytes: data, Context: in.Context,
		})
		if err != nil {
			return BuiltinResult{}, err
		}
		value, err := json.Marshal(map[string]any{"job_id": result.JobID, "status": result.Status})
		if err != nil {
			return BuiltinResult{}, err
		}
		return BuiltinResult{Value: value}, nil
	case "extract.status":
		if b.Extractor == nil {
			return BuiltinResult{}, errors.New("chatbot: extraction service is unavailable")
		}
		var in struct {
			JobID string `json:"job_id"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return BuiltinResult{}, ErrInvalidInput
		}
		result, err := b.Extractor.GetJob(ctx, p, in.JobID)
		if err != nil {
			return BuiltinResult{}, err
		}
		value, err := json.Marshal(result)
		if err != nil {
			return BuiltinResult{}, err
		}
		return BuiltinResult{Value: value}, nil
	default:
		return BuiltinResult{}, ErrInvalidInput
	}
}

func configuredToolIDs(bot Instance) []string {
	values, ok := bot.Settings["tools"].([]any)
	if !ok {
		return nil
	}
	ids := make([]string, 0, len(values))
	for _, value := range values {
		if id, ok := value.(string); ok && strings.TrimSpace(id) != "" {
			ids = append(ids, strings.TrimSpace(id))
		}
	}
	return mergeStrings(nil, ids)
}

func toolEnabled(bot Instance, id string) bool { return contains(configuredToolIDs(bot), id) }

func contains(values []string, match string) bool {
	match = strings.TrimSpace(match)
	for _, value := range values {
		if value == match {
			return true
		}
	}
	return false
}

func builtinDescription(id string) string {
	switch id {
	case "knowledge.search":
		return "Search company knowledge in an assistant-approved collection."
	case "evidence.read":
		return "Read a currently authorized evidence chunk by its server-issued evidence ID."
	case "extract.submit":
		return "Submit an authorized asynchronous extraction job and return its durable job ID."
	case "extract.status":
		return "Read the current caller-owned extraction job status."
	default:
		return ""
	}
}
