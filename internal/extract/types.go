package extract

import (
	"errors"
	"time"
)

// OperationProcess is the durable-jobs operation extract enqueues.
const OperationProcess = "extract.process"

var (
	// ErrNotFound is the single public outcome for unknown AND forbidden
	// tenant-scoped rows — a cross-tenant id is indistinguishable from absent.
	ErrNotFound = errors.New("extract: not found")
	// ErrForbidden denies a principal that exists but lacks the resource grant.
	ErrForbidden = errors.New("extract: forbidden")
)

// ProcessInput is one extraction request. Bytes are streamed into a durable
// input artifact before any job work; IdempotencyKey, when set, is the
// caller's replay identity (defaults to the input content hash).
type ProcessInput struct {
	TemplateID     string
	Context        map[string]any
	FileName       string
	MimeType       string
	Bytes          []byte
	IdempotencyKey string
}

// ProcessOutput acknowledges the durable job — extraction is asynchronous;
// data/results are read back through GetJob once the job completes.
type ProcessOutput struct {
	Success              bool           `json:"success"`
	JobID                string         `json:"job_id"`
	Status               string         `json:"status"`
	ModelUsed            string         `json:"model_used,omitempty"`
	Data                 map[string]any `json:"data,omitempty"`
	ValidationErrors     []string       `json:"validation_errors,omitempty"`
	ValidationWarnings   []string       `json:"validation_warnings,omitempty"`
	ProcessingTimeMS     int64          `json:"processing_time_ms,omitempty"`
	PromptTokensUsed     int            `json:"prompt_tokens_used,omitempty"`
	CompletionTokensUsed int            `json:"completion_tokens_used,omitempty"`
	TokensUsed           int            `json:"tokens_used,omitempty"`
	CostMicros           int64          `json:"cost_micros,omitempty"`
}

// Template is the tenant-scoped extraction template (extract_templates).
type Template struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Description   string         `json:"description,omitempty"`
	SystemPrompt  string         `json:"system_prompt"`
	UserPrompt    string         `json:"user_prompt"`
	ContextSchema map[string]any `json:"context_schema,omitempty"`
	OutputSchema  map[string]any `json:"output_schema,omitempty"`
	DefaultModel  string         `json:"model,omitempty"`
	Enabled       bool           `json:"is_active"`
	IsDefault     bool           `json:"is_default"`
	OwnerActorID  string         `json:"owner_actor_id,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
}

// Settings is the tenant's extract_settings row (one 'default' row).
type Settings struct {
	ID            string    `json:"id"`
	DefaultModel  string    `json:"default_model,omitempty"`
	MaxFileSizeMB int64     `json:"max_file_size_mb"`
	CreatedAt     time.Time `json:"created_at"`
}

// JobView is the consumer projection over jobs.Job — JSON field names match
// the legacy extract_jobs surface consumers already render.
type JobView struct {
	ID            string         `json:"id"`
	JobID         string         `json:"job_id"`
	TemplateID    string         `json:"template_id"`
	Status        string         `json:"status"`
	FileName      string         `json:"file_name"`
	FileMimeType  string         `json:"file_mime_type"`
	FileSizeBytes int64          `json:"file_size_bytes"`
	ModelUsed     string         `json:"model_used,omitempty"`
	ErrorMessage  string         `json:"error_message,omitempty"`
	InputRef      string         `json:"input_ref,omitempty"`
	ResultRef     string         `json:"result_ref,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
	CompletedAt   *time.Time     `json:"completed_at"`
	Policy        map[string]any `json:"-"`
}

// ResultView is the decoded result artifact payload, projected with the JSON
// field names consumers expect (data, raw_response, validation_*, tokens).
type ResultView struct {
	ID                   string         `json:"id"`
	JobID                string         `json:"job_id"`
	Data                 map[string]any `json:"data"`
	RawResponse          string         `json:"raw_response"`
	ValidationErrors     []string       `json:"validation_errors"`
	ValidationWarnings   []string       `json:"validation_warnings"`
	PromptTokensUsed     int            `json:"prompt_tokens_used"`
	CompletionTokensUsed int            `json:"completion_tokens_used"`
	TokensUsed           int            `json:"tokens_used"`
	CostMicros           int64          `json:"cost_micros"`
	CostCents            int64          `json:"cost_cents"`
	CreatedAt            time.Time      `json:"created_at"`
}

// JobDetail bundles the job projection with its decoded result when present.
type JobDetail struct {
	Job    JobView     `json:"job"`
	Result *ResultView `json:"result,omitempty"`
}

const (
	StatusPending             = "pending"
	StatusProcessing          = "processing"
	StatusCompleted           = "completed"
	StatusCompletedWithErrors = "completed_with_errors"
	StatusFailed              = "failed"
)
