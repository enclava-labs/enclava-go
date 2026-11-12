package httpx

import (
	"testing"

	"enclava-go/internal/chatbot"
)

// TestAssistantHasTools covers the streaming-vs-run routing read: only a
// settings["tools"] entry keeps an assistant on the polled agent-run path.
func TestAssistantHasTools(t *testing.T) {
	cases := []struct {
		name string
		bot  chatbot.Instance
		want bool
	}{
		{"no settings", chatbot.Instance{}, false},
		{"empty tools", chatbot.Instance{Settings: map[string]any{"tools": []any{}}}, false},
		{"tools wrong type", chatbot.Instance{Settings: map[string]any{"tools": "search"}}, false},
		{"one tool", chatbot.Instance{Settings: map[string]any{"tools": []any{"builtin_search"}}}, true},
	}
	for _, tc := range cases {
		if got := assistantHasTools(tc.bot); got != tc.want {
			t.Errorf("%s: assistantHasTools = %v, want %v", tc.name, got, tc.want)
		}
	}
}
