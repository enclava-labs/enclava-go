package rag

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"testing"

	"enclava-go/internal/jobs"
	"enclava-go/internal/llm"
)

func TestRetryEmbedErrorClassifiesTransientFailures(t *testing.T) {
	provider := func(err error) error {
		return fmt.Errorf("provider embeddings: %w", err)
	}
	upstream := func(err error) error {
		return fmt.Errorf("upstream request failed: %w", err)
	}

	cases := []struct {
		name string
		err  error
		want jobs.FailureClass
	}{
		{"url EOF", provider(upstream(&url.Error{Op: "Post", URL: "http://vibe:8000/v1/embeddings", Err: io.EOF})), jobs.FailureRetryable},
		{"upstream 503", provider(upstream(&llm.UpstreamError{StatusCode: 503})), jobs.FailureRetryable},
		{"upstream 429", provider(upstream(&llm.UpstreamError{StatusCode: 429})), jobs.FailureRetryable},
		{"upstream 400", provider(&llm.UpstreamError{StatusCode: 400}), jobs.FailureFatal},
		{"config error", errors.New("endpoint not approved"), jobs.FailureFatal},
		{"deadline exceeded", provider(context.DeadlineExceeded), jobs.FailureFatal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := jobs.FailureClassFor(retryEmbedError(tc.err)); got != tc.want {
				t.Fatalf("FailureClassFor(retryEmbedError(%v)) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
