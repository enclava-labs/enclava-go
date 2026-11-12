package chatbot

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
)

// SSE framing bounds mirror the shared gateway stream readers
// (httpx maxSSELineBytes): one bounded line buffer, no whole-body buffering.
const maxStreamLineBytes = 256 * 1024

// streamContentCap bounds the accumulated assistant text/tool arguments while
// assembling: past it the stream is still drained (settlement requires EOF)
// but nothing more is retained — the caller's output byte check then fails
// the attempt, so memory stays bounded under a runaway upstream.
const streamContentCap = maxRunOutputBytes

type streamToolCall struct {
	ID        string
	Name      strings.Builder
	Arguments strings.Builder
}

// assembleChatStream reads an OpenAI-compatible SSE chat stream and returns
// the equivalent NON-stream chat.completion JSON parseRunModelResponse
// accepts: concatenated delta content, tool_calls merged by index, the last
// finish_reason and the usage chunk. onDelta receives every content fragment
// of choices[0] in order. The reader is always drained to EOF before
// returning so stream settlement observes the terminal usage event.
func assembleChatStream(body io.Reader, onDelta func(string)) ([]byte, error) {
	var content strings.Builder
	var finish string
	var usage json.RawMessage
	calls := map[int]*streamToolCall{}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxStreamLineBytes)
	eventData := make([]byte, 0, 256)
	// Non-SSE bodies (an upstream that ignores the stream flag, and the
	// scripted fixtures in CI) pass through unchanged.
	var plain []byte
	sawData := false
	sawDone := false
	handle := func(data []byte) error {
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   any `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage json.RawMessage `json:"usage"`
		}
		if len(data) == 0 {
			return nil
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			return err
		}
		if len(chunk.Usage) > 0 {
			usage = chunk.Usage
		}
		for i, choice := range chunk.Choices {
			if choice.FinishReason != "" {
				finish = choice.FinishReason
			}
			if i > 0 {
				continue
			}
			text := contentToString(choice.Delta.Content)
			if text != "" {
				if content.Len() < streamContentCap {
					content.WriteString(text)
				}
				if onDelta != nil {
					onDelta(text)
				}
			}
			for _, call := range choice.Delta.ToolCalls {
				merged := calls[call.Index]
				if merged == nil {
					merged = &streamToolCall{}
					calls[call.Index] = merged
				}
				if call.ID != "" {
					merged.ID = call.ID
				}
				merged.Name.WriteString(call.Function.Name)
				if merged.Arguments.Len() < streamContentCap {
					merged.Arguments.WriteString(call.Function.Arguments)
				}
			}
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "data:"):
			sawData = true
			data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if data == "[DONE]" {
				sawDone = true
				eventData = eventData[:0]
				continue
			}
			if len(eventData) > 0 {
				eventData = append(eventData, '\n')
			}
			eventData = append(eventData, data...)
		case trimmed == "":
			if len(eventData) > 0 {
				if err := handle(eventData); err != nil {
					// shortcut: drain the rest of the body, then fail.
					_, _ = io.Copy(io.Discard, body)
					return nil, err
				}
				eventData = eventData[:0]
			}
		default:
			if !sawData && len(plain) <= streamContentCap {
				plain = append(plain, scanner.Bytes()...)
				plain = append(plain, '\n')
			}
		}
	}
	if err := scanner.Err(); err != nil {
		_, _ = io.Copy(io.Discard, body)
		return nil, err
	}
	if len(eventData) > 0 {
		if err := handle(eventData); err != nil {
			return nil, err
		}
	}
	if !sawData {
		// An upstream that answered with plain JSON despite the stream
		// request: the body already is the completion envelope.
		if !json.Valid(plain) {
			return nil, errors.New("chat stream carried no events")
		}
		return bytes.TrimRight(plain, "\n"), nil
	}
	// A stream that ends with neither a finish_reason nor [DONE] was cut
	// off: failing the attempt beats persisting a truncated answer as final.
	if finish == "" && !sawDone {
		return nil, errors.New("chat stream ended before completion")
	}
	return marshalAssembledCompletion(content.String(), calls, finish, usage)
}

func marshalAssembledCompletion(content string, calls map[int]*streamToolCall, finish string, usage json.RawMessage) ([]byte, error) {
	if finish == "" && content == "" && len(calls) == 0 {
		return nil, errors.New("chat stream carried no events")
	}
	indices := make([]int, 0, len(calls))
	for index := range calls {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	message := map[string]any{"role": "assistant", "content": content}
	if len(indices) > 0 {
		toolCalls := make([]any, 0, len(indices))
		for _, index := range indices {
			call := calls[index]
			toolCalls = append(toolCalls, map[string]any{
				"id": call.ID, "type": "function",
				"function": map[string]any{"name": call.Name.String(), "arguments": call.Arguments.String()},
			})
		}
		message["tool_calls"] = toolCalls
	}
	if finish == "" {
		finish = "stop"
	}
	envelope := map[string]any{
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
	}
	if len(usage) > 0 {
		envelope["usage"] = usage
	}
	return json.Marshal(envelope)
}
