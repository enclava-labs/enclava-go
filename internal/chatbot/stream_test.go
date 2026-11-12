package chatbot

import (
	"encoding/json"
	"strings"
	"testing"
)

func sse(events ...string) string {
	body := ""
	for _, e := range events {
		body += "data: " + e + "\n\n"
	}
	return body + "data: [DONE]\n\n"
}

func TestAssembleChatStreamTextOnly(t *testing.T) {
	var deltas []string
	out, err := assembleChatStream(strings.NewReader(sse(
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"Hello "},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":"world"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
	)), func(s string) { deltas = append(deltas, s) })
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if strings.Join(deltas, "|") != "Hello |world" {
		t.Fatalf("deltas = %v", deltas)
	}
	parsed, err := parseRunModelResponse(out)
	if err != nil {
		t.Fatalf("assembled JSON not runnable: %v — %s", err, out)
	}
	if parsed.Content != "Hello world" || len(parsed.ToolCalls) != 0 {
		t.Fatalf("parsed = %+v", parsed)
	}
	var env struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Total int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if len(env.Choices) != 1 || env.Choices[0].FinishReason != "stop" || env.Usage.Total != 5 {
		t.Fatalf("envelope = %s", out)
	}
}

func TestAssembleChatStreamToolCallsMerged(t *testing.T) {
	out, err := assembleChatStream(strings.NewReader(sse(
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"echo","arguments":"{\"a\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"ping","arguments":"{}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)), nil)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	parsed, err := parseRunModelResponse(out)
	if err != nil {
		t.Fatalf("parse: %v — %s", err, out)
	}
	if parsed.Content != "" || len(parsed.ToolCalls) != 2 {
		t.Fatalf("parsed = %+v", parsed)
	}
	first, second := parsed.ToolCalls[0], parsed.ToolCalls[1]
	if first.ID != "call_1" || first.Name != "echo" || string(first.Arguments) != `{"a":1}` {
		t.Fatalf("first = %+v", first)
	}
	if second.ID != "call_2" || second.Name != "ping" || string(second.Arguments) != "{}" {
		t.Fatalf("second = %+v", second)
	}
}

func TestAssembleChatStreamMalformedLine(t *testing.T) {
	_, err := assembleChatStream(strings.NewReader("data: {not json}\n\n"), nil)
	if err == nil {
		t.Fatal("expected error for malformed event")
	}
}

func TestAssembleChatStreamOversizeLine(t *testing.T) {
	huge := "data: " + strings.Repeat("x", maxStreamLineBytes+1) + "\n\n"
	_, err := assembleChatStream(strings.NewReader(huge), nil)
	if err == nil {
		t.Fatalf("expected oversize line error, got %v", err)
	}
}

func TestAssembleChatStreamPlainJSONPassthrough(t *testing.T) {
	// An upstream that ignores the stream flag answers with the ordinary
	// completion envelope; it must flow through byte-identical.
	body := `{"choices":[{"message":{"role":"assistant","content":"all done"},"finish_reason":"stop"}]}`
	var deltas int
	out, err := assembleChatStream(strings.NewReader(body), func(string) { deltas++ })
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if string(out) != body {
		t.Fatalf("passthrough = %s", out)
	}
	if deltas != 0 {
		t.Fatalf("plain body must not emit deltas (%d)", deltas)
	}
}
func TestAssembleChatStreamDrainsOnError(t *testing.T) {
	// A malformed event mid-stream must still drain the remaining body so
	// stream settlement sees EOF.
	var read int
	tracking := trackingReader{r: strings.NewReader("data: nope\n\ndata: {\"choices\":[]}\n\n"), n: &read}
	if _, err := assembleChatStream(&tracking, nil); err == nil {
		t.Fatal("expected malformed JSON error")
	}
	if read != len("data: nope\n\ndata: {\"choices\":[]}\n\n") {
		t.Fatalf("drained %d of %d bytes", read, len("data: nope\n\ndata: {\"choices\":[]}\n\n"))
	}
}

type trackingReader struct {
	r *strings.Reader
	n *int
}

func (t *trackingReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	*t.n += n
	return n, err
}

func TestAssembleChatStreamRejectsTruncatedStream(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"
	if _, err := assembleChatStream(strings.NewReader(body), nil); err == nil {
		t.Fatal("truncated stream (no finish_reason, no [DONE]) must fail")
	}
}
