package rag

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/inference"
	"enclava-go/internal/llm"
)

type rateLimitedExecutor struct{ rejections, calls int }

func (e *rateLimitedExecutor) Execute(context.Context, auth.Principal, inference.Request) (*inference.Result, error) {
	e.calls++
	if e.calls <= e.rejections {
		return nil, &llm.UpstreamError{StatusCode: 429, Message: "rate limit"}
	}
	return &inference.Result{Body: io.NopCloser(bytes.NewBufferString(`{"data":[{"index":0,"embedding":[1,0]}]}`))}, nil
}

func TestEmbeddingJobsWaitOutRateLimits(t *testing.T) {
	old := embedRateLimitPause
	embedRateLimitPause = time.Millisecond
	t.Cleanup(func() { embedRateLimitPause = old })

	// A job-scoped call resends the rejected batch after the pause.
	job := &rateLimitedExecutor{rejections: 3}
	vectors, err := createEmbeddings(context.Background(), job, auth.Principal{}, "m", 0, []string{"text"}, embedScope{JobID: "job_1"})
	if err != nil || len(vectors) != 1 || job.calls != 4 {
		t.Fatalf("job embed: vectors=%v calls=%d err=%v", vectors, job.calls, err)
	}

	// An interactive call (search) fails fast instead of waiting.
	search := &rateLimitedExecutor{rejections: 3}
	if _, err := createEmbeddings(context.Background(), search, auth.Principal{}, "m", 0, []string{"text"}, embedScope{}); !embedRateLimited(err) || search.calls != 1 {
		t.Fatalf("interactive embed: calls=%d err=%v", search.calls, err)
	}

	// A provider that never recovers still ends the job attempt.
	stuck := &rateLimitedExecutor{rejections: 1 << 30}
	if _, err := createEmbeddings(context.Background(), stuck, auth.Principal{}, "m", 0, []string{"text"}, embedScope{JobID: "job_2"}); !embedRateLimited(err) || stuck.calls != embedRateLimitWaits+1 {
		t.Fatalf("stuck embed: calls=%d err=%v", stuck.calls, err)
	}
}
