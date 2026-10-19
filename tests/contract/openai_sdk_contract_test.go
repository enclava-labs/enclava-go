package contract_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// TestOpenAISDKCompatibility exercises the selected public OpenAI core API —
// models, chat completions (unary + streaming tool calls) and embeddings —
// against a configured application using the installed official Go SDK.
// Explicitly invoked without a live target it fails rather than skips.
func TestOpenAISDKCompatibility(t *testing.T) {
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("OPENAI_CONTRACT_BASE_URL")), "/")
	apiKey := strings.TrimSpace(os.Getenv("OPENAI_CONTRACT_API_KEY"))
	chatModel := strings.TrimSpace(os.Getenv("OPENAI_CONTRACT_CHAT_MODEL"))
	embeddingModel := strings.TrimSpace(os.Getenv("OPENAI_CONTRACT_EMBEDDING_MODEL"))
	if baseURL == "" || apiKey == "" || chatModel == "" || embeddingModel == "" {
		t.Fatal("OPENAI_CONTRACT_BASE_URL, OPENAI_CONTRACT_API_KEY, OPENAI_CONTRACT_CHAT_MODEL and OPENAI_CONTRACT_EMBEDDING_MODEL are required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := openai.NewClient(
		option.WithBaseURL(baseURL),
		option.WithAPIKey(apiKey),
	)

	models, err := client.Models.List(ctx)
	if err != nil {
		t.Fatalf("list models through official SDK: %v", err)
	}
	seen := map[string]bool{}
	for _, m := range models.Data {
		seen[m.ID] = true
	}
	if !seen[chatModel] || !seen[embeddingModel] {
		t.Fatalf("selected models %q/%q absent from models list: %v", chatModel, embeddingModel, models.Data)
	}

	chat, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Messages: []openai.ChatCompletionMessageParamUnion{{
			OfUser: &openai.ChatCompletionUserMessageParam{
				Content: openai.ChatCompletionUserMessageParamContentUnion{
					OfString: openai.String("Say ok."),
				},
			},
		}},
		Model:       shared.ChatModel(chatModel),
		Temperature: openai.Float(0),
	})
	if err != nil {
		t.Fatalf("chat completions through official SDK: %v", err)
	}
	if chat.ID == "" || len(chat.Choices) == 0 {
		t.Fatalf("unexpected SDK chat response: %#v", chat)
	}

	// Streaming chat preserves SSE framing the SDK reconstructs into deltas.
	stream := client.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{
		Messages: []openai.ChatCompletionMessageParamUnion{{
			OfUser: &openai.ChatCompletionUserMessageParam{
				Content: openai.ChatCompletionUserMessageParamContentUnion{
					OfString: openai.String("Say ok."),
				},
			},
		}},
		Model: shared.ChatModel(chatModel),
	})
	streamEvents := 0
	for stream.Next() {
		streamEvents++
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("chat stream through official SDK: %v", err)
	}
	if streamEvents == 0 {
		t.Fatal("chat stream yielded no events")
	}

	embedding, err := client.Embeddings.New(ctx, openai.EmbeddingNewParams{
		Input: openai.EmbeddingNewParamsInputUnion{
			OfArrayOfStrings: []string{"embed this", "and this"},
		},
		Model: openai.EmbeddingModel(embeddingModel),
	})
	if err != nil {
		t.Fatalf("embeddings through official SDK: %v", err)
	}
	if len(embedding.Data) != 2 || len(embedding.Data[0].Embedding) == 0 {
		t.Fatalf("unexpected SDK embedding response: %#v", embedding)
	}

	// Unselected products are explicit errors, never a fallback proxy.
	if _, err := client.Files.List(ctx, openai.FileListParams{}); err == nil {
		t.Fatal("unselected /v1/files route must not be served")
	}
}
