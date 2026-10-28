package extract

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
	"enclava-go/internal/llm"
	"enclava-go/internal/source"
)

// TemplatesRepo is the tenant-scoped extract_templates persistence boundary.
type TemplatesRepo interface {
	ListTemplates(ctx context.Context, tenantID string) ([]Template, error)
	GetTemplate(ctx context.Context, tenantID, id string) (Template, error)
	UpsertTemplate(ctx context.Context, tenantID string, t Template) error
	DeleteTemplate(ctx context.Context, tenantID, id string) error
	ReplaceDefaultTemplates(ctx context.Context, tenantID string, templates []Template) error
}

// SettingsRepo is the tenant-scoped extract_settings persistence boundary.
type SettingsRepo interface {
	GetSettings(ctx context.Context, tenantID string) (Settings, error)
	UpsertSettings(ctx context.Context, tenantID string, s Settings) (Settings, error)
}

// JobReader is the owner-scoped read view over durable job rows. Every
// method binds the caller's submitter identity — unknown and forbidden ids
// resolve to the same jobs.ErrJobNotFound outcome.
type JobReader interface {
	GetOwnedJob(ctx context.Context, tenantID, id, submitterKey string) (jobs.JobRow, error)
	GetIdempotency(ctx context.Context, tenantID, operation, key string) (jobs.JobRow, error)
	ListOwnedJobsFiltered(ctx context.Context, tenantID, submitterKey string, ops, statuses []string, limit int) ([]jobs.JobRow, error)
	CountOwnedJobsFiltered(ctx context.Context, tenantID, submitterKey string, ops []string) (int, error)
	CountOwnedJobsSinceFiltered(ctx context.Context, tenantID, submitterKey string, ops []string, since time.Time) (int, error)
	CountLiveArtifactReferences(ctx context.Context, tenantID, artifactRef, excludingJobID string) (int, error)
	ListDeletedExtractJobs(ctx context.Context) ([]jobs.JobRow, error)
	CountActiveExtractions(ctx context.Context, tenantID string) (int, error)
}

// Service owns the extract pipeline: Process persists the upload as a durable
// input artifact and enqueues an "extract.process" job — it never calls the
// provider synchronously. JobHandler is the worker-side fenced attempt: the
// shared inference executor alone resolves policy, reserves, dispatches and
// settles — this service never reserves or records usage itself.
type Service struct {
	templatesRepo TemplatesRepo
	settingsRepo  SettingsRepo
	jobRows       JobReader
	jobs          *jobs.Service
	artifacts     *artifacts.Store
	executor      Executor
	endpoints     EndpointsLister
	sources       source.SourceRepository
	authorizer    *source.Authorizer
	maxUploadMB   int64
}

// SetSourceRepository enables current source-grant checks for jobs tied to an
// existing source version. Uploaded evidence remains owner-bound.
func (s *Service) SetSourceRepository(repo source.SourceRepository) {
	s.sources = repo
	if repo != nil {
		s.authorizer = source.NewAuthorizer(repo, 0)
	}
}

// Executor is the shared authorized inference boundary the attempt dispatches
// through; *inference.Service satisfies it.
type Executor interface {
	Execute(ctx context.Context, p auth.Principal, req inference.Request) (*inference.Result, error)
}

// EndpointsLister exposes the tenant's configured endpoint model bindings for
// model selection; *postgres.EndpointsRepo satisfies it.
type EndpointsLister interface {
	ListCapabilities(ctx context.Context, tenantID, capability string) ([]inference.Endpoint, error)
}

func NewService(
	templatesRepo TemplatesRepo,
	settingsRepo SettingsRepo,
	jobRows JobReader,
	jobSvc *jobs.Service,
	artifactStore *artifacts.Store,
	executor Executor,
	endpoints EndpointsLister,
	maxUploadMB int64,
) *Service {
	return &Service{
		templatesRepo: templatesRepo,
		settingsRepo:  settingsRepo,
		jobRows:       jobRows,
		jobs:          jobSvc,
		artifacts:     artifactStore,
		executor:      executor,
		endpoints:     endpoints,
		maxUploadMB:   maxUploadMB,
	}
}

// maxExtractOutputTokens bounds the reserved output-side estimate; settle
// records the truthful actual usage without clipping.
const maxExtractOutputTokens = 8192

// writeLeaseTTL covers the input upload stream; the attempt's own writes
// get a fresh lease.
const writeLeaseTTL = 10 * time.Minute

// ExtractAdmissionLimit bounds a tenant's queued+running extraction jobs
// (best-effort, like the preview cap): durable storage cannot be exhausted
// by repeated submissions from valid credentials.
const ExtractAdmissionLimit = 25

var ErrExtractionAdmissionLimited = errors.New("extract: extraction admission limit reached")

// uploadMIMEs maps accepted extensions to their declared media types.
var uploadMIMEs = map[string]string{
	".pdf": "application/pdf", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".png": "image/png", ".txt": "text/plain", ".md": "text/markdown",
	".markdown": "text/markdown", ".html": "text/html", ".htm": "text/html",
	".csv": "text/csv", ".json": "application/json",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
}

// SeedDefaults seeds the tenant's default templates (idempotent upserts).

func (s *Service) SeedDefaults(ctx context.Context, p auth.Principal) error {
	for _, t := range DefaultTemplates() {
		if err := s.templatesRepo.UpsertTemplate(ctx, p.TenantID, t); err != nil {
			return err
		}
	}
	return nil
}

// Process authorizes the request, persists the input bytes as a durable
// artifact BEFORE any job work, then enqueues the extract job and returns
// its durable id. No provider dispatch happens here — the worker runs the
// attempt through JobHandler under the fenced lease.
func (s *Service) Process(ctx context.Context, p auth.Principal, input ProcessInput) (ProcessOutput, error) {
	return s.ProcessReader(ctx, p, input)
}

func (s *Service) ProcessReader(ctx context.Context, p auth.Principal, input ProcessInput) (ProcessOutput, error) {
	if input.Operation == "" {
		input.Operation = OperationProcess
	}
	if input.Operation != OperationProcess && input.Operation != OperationVision {
		return ProcessOutput{}, fmt.Errorf("unsupported extraction operation %q", input.Operation)
	}
	input.MimeType = normalizedUploadMIME(input.FileName, input.MimeType)
	start := time.Now()
	if p.TenantID == "" {
		return ProcessOutput{}, fmt.Errorf("%w: tenant is required", ErrForbidden)
	}
	if input.Reader != nil {
		if strings.TrimSpace(input.TemplateID) == "" || strings.TrimSpace(input.FileName) == "" {
			return ProcessOutput{}, errors.New("template_id and file are required")
		}
		if err := validateContextValues(input.Context); err != nil {
			return ProcessOutput{}, err
		}
		if err := validateUploadMetadata(input); err != nil {
			return ProcessOutput{}, err
		}
	} else if err := s.validateInput(input); err != nil {
		return ProcessOutput{}, err
	}
	// Fail fast on admission before any artifact bytes are persisted.
	if s.jobRows != nil {
		if active, err := s.jobRows.CountActiveExtractions(ctx, p.TenantID); err != nil {
			return ProcessOutput{}, fmt.Errorf("count active extractions: %w", err)
		} else if active >= ExtractAdmissionLimit {
			return ProcessOutput{}, ErrExtractionAdmissionLimited
		}
	}
	if !auth.ExtractTemplateAllowed(p, input.TemplateID) {
		return ProcessOutput{}, ErrForbidden
	}
	if s.artifacts == nil || s.jobs == nil {
		return ProcessOutput{}, errors.New("extract: durable pipeline is not configured")
	}

	template, err := s.templatesRepo.GetTemplate(ctx, p.TenantID, input.TemplateID)
	if err != nil {
		return ProcessOutput{}, mapNotFound(err)
	}
	if !template.Enabled {
		return ProcessOutput{}, errors.New("template is inactive")
	}
	if template.OutputSchema == nil {
		template.OutputSchema = map[string]any{}
	}
	if _, err := compileOutputSchema(template.OutputSchema); err != nil {
		return ProcessOutput{}, fmt.Errorf("invalid output_schema: %w", err)
	}
	outputSchemaJSON, err := json.Marshal(template.OutputSchema)
	if err != nil {
		return ProcessOutput{}, fmt.Errorf("encode output_schema snapshot: %w", err)
	}

	capability := inference.CapabilityChat
	if strings.HasPrefix(strings.ToLower(input.MimeType), "image/") {
		capability = inference.CapabilityVision
	}
	model, err := s.resolveModel(ctx, p.TenantID, template)
	if err != nil {
		return ProcessOutput{}, err
	}
	endpointID, err := s.resolveEndpoint(ctx, p.TenantID, model, capability)
	if err != nil {
		return ProcessOutput{}, err
	}
	// Persist the input BEFORE the job row exists — the job's input_ref binds
	// the finalized artifact version.
	w, _, err := s.artifacts.Begin(ctx, artifacts.Meta{
		Ref:      artifacts.Ref{TenantID: p.TenantID, Kind: artifacts.KindOriginal},
		MimeType: input.MimeType,
	}, writeLeaseTTL, s.maxUploadMB*1024*1024)
	if err != nil {
		return ProcessOutput{}, fmt.Errorf("begin input artifact: %w", err)
	}
	reader := input.Reader
	if reader == nil {
		reader = bytes.NewReader(input.Bytes)
	}
	if err := copyUpload(ctx, w, reader, input.FileName); err != nil {
		_ = w.Abort(ctx)
		return ProcessOutput{}, fmt.Errorf("write input artifact: %w", err)
	}
	if err := w.Finalize(ctx); err != nil {
		return ProcessOutput{}, fmt.Errorf("finalize input artifact: %w", err)
	}
	ref := w.Ref()
	if ref.ByteSize == 0 {
		_ = s.artifacts.Delete(ctx, ref)
		return ProcessOutput{}, errors.New("uploaded file is empty")
	}

	idem := strings.TrimSpace(input.IdempotencyKey)
	if idem == "" {
		idem = "extract:" + input.TemplateID + ":" + ref.SHA256
	}

	policy := map[string]any{
		"task_version": 1, "task_operation": input.Operation,
		"template_id": template.ID, "model": model,
		"capability": capability, "endpoint_id": endpointID,
		"system_prompt": template.SystemPrompt, "user_prompt": template.UserPrompt,
		"file_name": filepath.Base(input.FileName), "mime_type": input.MimeType,
		"file_size": ref.ByteSize, "input_version": int64(ref.Version),
		"output_schema_json": string(outputSchemaJSON), "schema_version": outputSchemaVersion,
		"context": input.Context,
	}
	existing, replay, err := s.existingSubmission(ctx, p, input.Operation, idem, ref.SHA256, policy)
	if err != nil {
		_ = s.artifacts.Delete(ctx, ref)
		return ProcessOutput{}, err
	}
	if replay {
		_ = s.artifacts.Delete(ctx, ref)
		return ProcessOutput{Success: true, JobID: existing.ID, Status: existing.Status, ModelUsed: model}, nil
	}
	job, _, err := s.jobs.Enqueue(ctx, p, jobs.EnqueueRequest{
		TenantID: p.TenantID, Operation: input.Operation, IdempotencyKey: idem,
		Subject:  jobs.Subject{ObjectID: ref.ID, ObservedRevision: int64(ref.Version)},
		InputRef: ref.ID, InputHash: ref.SHA256, Policy: policy,
	})
	if err != nil {
		if errors.Is(err, jobs.ErrIdempotencyConflict) {
			_ = s.artifacts.Delete(ctx, ref)
			existing, replay, lookupErr := s.existingSubmission(ctx, p, input.Operation, idem, ref.SHA256, policy)
			if lookupErr == nil && replay {
				return ProcessOutput{Success: true, JobID: existing.ID, Status: existing.Status, ModelUsed: model}, nil
			}
		}
		// No job row exists, so nothing references the input artifact —
		// delete it rather than orphaning durable bytes.
		_ = s.artifacts.Delete(ctx, ref)
		return ProcessOutput{}, fmt.Errorf("enqueue extract job: %w", err)
	}
	return ProcessOutput{
		Success:          true,
		JobID:            job.ID,
		Status:           job.Status,
		ModelUsed:        model,
		ProcessingTimeMS: time.Since(start).Milliseconds(),
		// data/tokens are populated by the attempt; replayed jobs surface the
		// stored identity, not a new execution.
		Data:                 map[string]any{},
		ValidationWarnings:   []string{},
		ValidationErrors:     []string{},
		CompletionTokensUsed: 0,
		PromptTokensUsed:     0,
		TokensUsed:           0,
		CostMicros:           0,
	}, nil
}
func (s *Service) existingSubmission(ctx context.Context, p auth.Principal, operation, key, inputHash string, policy map[string]any) (jobs.Job, bool, error) {
	row, err := s.jobRows.GetIdempotency(ctx, p.TenantID, operation, key)
	if errors.Is(err, jobs.ErrJobNotFound) {
		return jobs.Job{}, false, nil
	}
	if err != nil {
		return jobs.Job{}, false, fmt.Errorf("check extract idempotency: %w", err)
	}
	if row.SubmitterKey != jobs.SubmitterKey(p) || row.Status == jobs.StatusDeleted ||
		row.InputHash != inputHash || !sameJobPolicy(row.Policy, policy) {
		return jobs.Job{}, false, jobs.ErrIdempotencyConflict
	}
	return row.Job, true, nil
}

func sameJobPolicy(a, b map[string]any) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	return err == nil && bytes.Equal(left, right)
}

// attempt executes one claimed extract job under the fenced lease. The
// initiating principal is read back from the frozen job snapshot, template
// and model authorization are re-checked, and the shared executor alone
// resolves policy, reserves, dispatches and settles — one reservation, one
// settlement, no module-side billing.
func (s *Service) attempt(ctx context.Context, claim *jobs.Claim, input jobs.InputFunc) (string, error) {
	job := claim.Job()

	var p auth.Principal
	if err := json.Unmarshal(job.PrincipalSnapshot, &p); err != nil {
		return "", fmt.Errorf("decode frozen principal: %w", err)
	}

	templateID, _ := job.Policy["template_id"].(string)
	template, err := s.templatesRepo.GetTemplate(ctx, job.TenantID, templateID)
	if err != nil {
		return "", fmt.Errorf("load template: %w", mapNotFound(err))
	}
	if !template.Enabled {
		return "", errors.New("template is inactive")
	}
	if stringValue(job.Policy["task_operation"]) != job.Operation ||
		(job.Operation != OperationProcess && job.Operation != OperationVision) {
		return "", errors.New("unsupported extraction task operation")
	}
	model, _ := job.Policy["model"].(string)
	if strings.TrimSpace(model) == "" {
		return "", errors.New("job is missing its selected model")
	}
	taskVersion, ok := job.Policy["task_version"].(float64)
	if !ok || taskVersion != 1 {
		return "", fmt.Errorf("unsupported extraction task version %v", job.Policy["task_version"])
	}
	capability := stringValue(job.Policy["capability"])
	if capability != inference.CapabilityChat && capability != inference.CapabilityVision {
		return "", errors.New("job is missing its selected inference capability")
	}
	endpointID := stringValue(job.Policy["endpoint_id"])
	if endpointID == "" {
		return "", errors.New("job is missing its selected endpoint")
	}
	// Template allowlist is re-checked here; model/endpoint/restriction
	// authorization is the executor's job alone.
	if !auth.ExtractTemplateAllowed(p, template.ID) {
		return "", ErrForbidden
	}
	if stringValue(job.Policy["schema_version"]) != outputSchemaVersion {
		return "", fmt.Errorf("unsupported extraction schema version %q", stringValue(job.Policy["schema_version"]))
	}
	// The job snapshot keeps template edits from changing the validation contract.
	schemaSnapshot, ok := job.Policy["output_schema_json"].(string)
	if !ok || schemaSnapshot == "" {
		return "", errors.New("job is missing its output schema snapshot")
	}
	schemaDecoder := json.NewDecoder(strings.NewReader(schemaSnapshot))
	schemaDecoder.UseNumber()
	if err := schemaDecoder.Decode(&template.OutputSchema); err != nil {
		return "", fmt.Errorf("decode job output schema snapshot: %w", err)
	}
	if _, err := compileOutputSchema(template.OutputSchema); err != nil {
		return "", fmt.Errorf("invalid job output schema: %w", err)
	}

	in, err := input(ctx)
	if err != nil {
		return "", fmt.Errorf("read input artifact: %w", err)
	}
	defer func() { _ = in.Close() }()
	inBytes, err := io.ReadAll(contextReader{ctx: ctx, reader: in})
	if err != nil {
		return "", fmt.Errorf("read input bytes: %w", err)
	}

	systemPrompt, systemOK := job.Policy["system_prompt"].(string)
	userPrompt, userOK := job.Policy["user_prompt"].(string)
	if !systemOK || !userOK || systemPrompt == "" || userPrompt == "" {
		return "", errors.New("job is missing its immutable prompt snapshot")
	}
	promptSystem, promptUser := interpolate(systemPrompt, userPrompt, contextMap(job.Policy["context"]))
	inInput := ProcessInput{
		TemplateID: template.ID,
		Context:    contextMap(job.Policy["context"]),
		FileName:   stringValue(job.Policy["file_name"]),
		MimeType:   stringValue(job.Policy["mime_type"]),
		Bytes:      inBytes,
	}
	mimeType := strings.ToLower(inInput.MimeType)
	if !strings.HasPrefix(mimeType, "image/") {
		inputVersion, _ := job.Policy["input_version"].(float64)
		doc, parseErr := NewParser(s.artifacts).Parse(ctx, artifacts.Ref{
			TenantID: job.TenantID, ID: job.InputRef, Kind: artifacts.KindOriginal,
			Version: int(inputVersion),
		}, ParseOptions{
			Filename: inInput.FileName, MIMEType: mimeType,
			ObjectRef: source.ObjectRef{TenantID: job.TenantID, ObjectID: job.InputRef, ObservedSeq: 1},
		})
		if parseErr != nil {
			return "", fmt.Errorf("parse extraction input: %w", parseErr)
		}
		var text strings.Builder
		for _, block := range doc.Blocks {
			if text.Len() > 0 {
				text.WriteByte('\n')
			}
			text.WriteString(block.Text)
		}
		inInput.DocumentText = text.String()
	}
	if err := claim.MarkDispatched(ctx); err != nil {
		return "", err
	}
	payload, err := json.Marshal(s.buildProviderRequest(model, template, promptSystem, promptUser, inInput))
	if err != nil {
		return "", fmt.Errorf("marshal provider request: %w", err)
	}

	inputVersion, _ := job.Policy["input_version"].(float64)
	res, err := s.executor.Execute(ctx, p, inference.Request{
		Capability: capability,
		Model:      model,
		EndpointID: endpointID,
		Payload:    payload,
		Artifacts: []artifacts.Ref{{
			TenantID: job.TenantID,
			ID:       job.InputRef,
			Version:  int(inputVersion),
		}},
		Estimate: billing.Usage{
			InputUnits:  estimateInputTokens(int64(len(inBytes)), promptSystem, promptUser),
			OutputUnits: maxExtractOutputTokens,
		},
		JobID:     job.ID,
		AttemptID: claim.AttemptID(),
	})
	if err != nil {
		return "", fmt.Errorf("provider chat completion: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.SettlementError != nil {
		return "", fmt.Errorf("settle reservation: %w", res.SettlementError)
	}
	chatRespRaw, err := io.ReadAll(res.Body)
	if err != nil {
		return "", fmt.Errorf("read provider response: %w", err)
	}

	parsed, rawContent, usage, parseErr := parseProviderExtraction(chatRespRaw)
	if parseErr != nil {
		if parsed == nil {
			parsed = map[string]any{}
		}
		parsed["_parse_error"] = parseErr.Error()
	}
	validationErrors, validationWarnings := s.validateOutput(parsed, template)

	promptUnits, completionUnits := usage.Prompt, usage.Completion
	if res.Usage != nil {
		// Settled units are the accounting truth; the parsed provider report
		// stays in RawResponse for display only.
		promptUnits = int(res.Usage.Units[billing.UnitInput])
		completionUnits = int(res.Usage.Units[billing.UnitOutput])
	}

	costMicros := int64(0)
	if res.Usage != nil {
		costMicros = res.Usage.AmountMicros
	}
	resultDoc := ResultView{
		ID:                   newID("res"),
		JobID:                job.ID,
		Data:                 parsed,
		RawResponse:          rawContent,
		ValidationErrors:     validationErrors,
		ValidationWarnings:   validationWarnings,
		SchemaVersion:        stringValue(job.Policy["schema_version"]),
		OutputSchema:         template.OutputSchema,
		Generated:            job.Operation == OperationVision,
		PromptTokensUsed:     promptUnits,
		CompletionTokensUsed: completionUnits,
		TokensUsed:           promptUnits + completionUnits,
		CostMicros:           costMicros,
		CostCents:            costMicros / 10_000,
		CreatedAt:            time.Now().UTC(),
	}
	docBytes, err := json.Marshal(resultDoc)
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}
	rw, rref, err := s.artifacts.Begin(ctx, artifacts.Meta{
		Ref:      artifacts.Ref{TenantID: job.TenantID, Kind: artifacts.KindResult},
		MimeType: "application/json",
	}, writeLeaseTTL, int64(len(docBytes))+4096)
	if err != nil {
		return "", fmt.Errorf("begin result artifact: %w", err)
	}
	if _, err := rw.Write(docBytes); err != nil {
		_ = rw.Abort(ctx)
		return "", fmt.Errorf("write result artifact: %w", err)
	}
	if err := rw.Finalize(ctx); err != nil {
		return "", fmt.Errorf("finalize result artifact: %w", err)
	}
	rref = rw.Ref()

	return rref.ID, nil
}

// memberJobOperations is the member-facing extraction operation set — shared
// jobs (previews, embeds, syncs) never surface in extraction lists or stats.
var memberJobOperations = []string{OperationProcess, OperationVision}

// jobOperations resolves the ?operation= filter: empty selects the full
// member-facing extraction set; anything else must name one of its members.
func jobOperations(operation string) ([]string, error) {
	operation = strings.TrimSpace(operation)
	if operation == "" {
		return memberJobOperations, nil
	}
	for _, op := range memberJobOperations {
		if op == operation {
			return []string{op}, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrInvalidOperation, operation)
}

// ListJobs returns the caller's newest extraction jobs plus the total row
// count for paging — owner-bound through the jobs submitter key and scoped to
// the requested extraction operation (empty = extract.process+extract.vision).
func (s *Service) ListJobs(ctx context.Context, p auth.Principal, operation string, limit, offset int) ([]JobView, int, error) {
	ops, err := jobOperations(operation)
	if err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	key := jobs.SubmitterKey(p)
	total, err := s.jobRows.CountOwnedJobsFiltered(ctx, p.TenantID, key, ops)
	if err != nil {
		return nil, 0, err
	}
	rows, err := s.jobRows.ListOwnedJobsFiltered(ctx, p.TenantID, key, ops, nil, limit+offset)
	if err != nil {
		return nil, 0, err
	}
	if offset > len(rows) {
		offset = len(rows)
	}
	rows = rows[offset:]
	out := make([]JobView, 0, len(rows))
	for _, row := range rows {
		out = append(out, jobView(row))
	}
	return out, total, nil
}

// JobStats returns the caller's recent (submitted at or after `since`) and
// total non-deleted extraction job counts — the owner-bound pair the
// dashboard renders. Shared operations never move these counters.
func (s *Service) JobStats(ctx context.Context, p auth.Principal, since time.Time) (recent int64, total int64, err error) {
	key := jobs.SubmitterKey(p)
	t, err := s.jobRows.CountOwnedJobsFiltered(ctx, p.TenantID, key, memberJobOperations)
	if err != nil {
		return 0, 0, err
	}
	rec, err := s.jobRows.CountOwnedJobsSinceFiltered(ctx, p.TenantID, key, memberJobOperations, since)
	if err != nil {
		return 0, 0, err
	}
	return int64(rec), int64(t), nil
}

// GetJob returns the owner-bound job detail; the decoded result artifact is
// attached when the job completed. Unknown and foreign ids both resolve to
// ErrNotFound.
func (s *Service) GetJob(ctx context.Context, p auth.Principal, jobID string) (JobDetail, error) {
	row, err := s.jobRows.GetOwnedJob(ctx, p.TenantID, jobID, jobs.SubmitterKey(p))
	if err != nil {
		return JobDetail{}, mapNotFound(err)
	}
	if row.Operation != OperationProcess && row.Operation != OperationVision {
		return JobDetail{}, ErrNotFound
	}
	if err := s.authorizeSource(ctx, p, row); err != nil {
		return JobDetail{}, ErrNotFound
	}
	view := jobView(row)
	detail := JobDetail{Job: view}
	if row.ResultRef == "" {
		return detail, nil
	}
	if s.artifacts == nil {
		return JobDetail{}, errors.New("extract result artifact store unavailable")
	}
	rc, err := s.artifacts.Read(ctx, artifacts.Ref{TenantID: p.TenantID, ID: row.ResultRef})
	if err != nil {
		return JobDetail{}, fmt.Errorf("read extract result artifact: %w", err)
	}
	defer func() { _ = rc.Close() }()
	var res ResultView
	decoder := json.NewDecoder(io.LimitReader(rc, 8<<20))
	decoder.UseNumber()
	if err := decoder.Decode(&res); err != nil {
		return JobDetail{}, fmt.Errorf("decode extract result artifact: %w", err)
	}
	detail.Result = &res
	return detail, nil
}

// ListTemplates is the authoring/management list: the full tenant set for
// extract.manage callers, otherwise the caller's execution-grant
// intersection. An empty grant set yields an empty list — never a fabricated
// default (D-25 deny-default).
func (s *Service) ListTemplates(ctx context.Context, p auth.Principal) ([]Template, error) {
	all, err := s.templatesRepo.ListTemplates(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	if auth.HasScope(p, "extract.manage") {
		return all, nil
	}
	return allowedTemplates(all, p), nil
}

// ListExecutableTemplates is the Process picker read: ALWAYS the caller's
// execution-grant intersection — extract.manage never bypasses it. Template
// administration and execution selection are separate authorities (D-25).
func (s *Service) ListExecutableTemplates(ctx context.Context, p auth.Principal) ([]Template, error) {
	all, err := s.templatesRepo.ListTemplates(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	return allowedTemplates(all, p), nil
}

// allowedTemplates projects the tenant set onto the principal's stored
// extract-template grants; the empty grant set stays empty.
func allowedTemplates(all []Template, p auth.Principal) []Template {
	out := make([]Template, 0, len(all))
	for _, t := range all {
		if auth.ExtractTemplateAllowed(p, t.ID) {
			out = append(out, t)
		}
	}
	return out
}

func (s *Service) GetTemplate(ctx context.Context, p auth.Principal, id string) (Template, error) {
	t, err := s.templatesRepo.GetTemplate(ctx, p.TenantID, id)
	if err != nil {
		return Template{}, mapNotFound(err)
	}
	return t, nil
}

func (s *Service) UpsertTemplate(ctx context.Context, p auth.Principal, t Template) error {
	if strings.TrimSpace(t.ID) == "" {
		return errors.New("template id is required")
	}
	if strings.TrimSpace(t.Name) == "" {
		return errors.New("template name is required")
	}
	if strings.TrimSpace(t.SystemPrompt) == "" || strings.TrimSpace(t.UserPrompt) == "" {
		return errors.New("system_prompt and user_prompt are required")
	}
	if t.ContextSchema == nil {
		t.ContextSchema = map[string]any{}
	}
	if !isPrimitiveSchema(t.ContextSchema) {
		return errors.New("context_schema must be a flat object with primitive type definitions")
	}
	if t.OutputSchema == nil {
		t.OutputSchema = map[string]any{}
	}
	if len(t.OutputSchema) > 0 {
		if _, err := compileOutputSchema(t.OutputSchema); err != nil {
			return fmt.Errorf("invalid output_schema: %w", err)
		}
		t.OutputSchema["$schema"] = outputSchemaVersion
	}
	if t.OwnerActorID == "" {
		t.OwnerActorID = p.ActorID
	}
	return s.templatesRepo.UpsertTemplate(ctx, p.TenantID, t)
}

func (s *Service) DeleteTemplate(ctx context.Context, p auth.Principal, id string) error {
	return s.templatesRepo.DeleteTemplate(ctx, p.TenantID, id)
}

func (s *Service) ResetDefaultTemplates(ctx context.Context, p auth.Principal) error {
	return s.templatesRepo.ReplaceDefaultTemplates(ctx, p.TenantID, DefaultTemplates())
}

func (s *Service) GetSettings(ctx context.Context, p auth.Principal) (Settings, error) {
	set, err := s.settingsRepo.GetSettings(ctx, p.TenantID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// First-touch: persist the tenant's default row.
			return s.settingsRepo.UpsertSettings(ctx, p.TenantID, Settings{
				ID:            "default",
				MaxFileSizeMB: s.maxUploadMB,
			})
		}
		return Settings{}, err
	}
	return set, nil
}

func (s *Service) UpdateSettings(ctx context.Context, p auth.Principal, in Settings) (Settings, error) {
	if in.MaxFileSizeMB <= 0 {
		in.MaxFileSizeMB = s.maxUploadMB
	}
	in.ID = "default"
	return s.settingsRepo.UpsertSettings(ctx, p.TenantID, in)
}

// ModelsForExtract lists the model aliases the tenant's enabled
// vision-capable endpoints actually expose — configuration truth, not an
// upstream probe. Unknown to the executor means not selectable.
func (s *Service) ModelsForExtract(ctx context.Context, tenantID string) ([]llm.Model, error) {
	endpoints, err := s.endpoints.ListCapabilities(ctx, tenantID, inference.CapabilityVision)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	var models []llm.Model
	for _, e := range endpoints {
		for _, cap := range e.Capabilities {
			binding, ok := strings.CutPrefix(cap, inference.ModelCapabilityPrefix)
			if !ok {
				continue
			}
			alias, _, _ := strings.Cut(binding, "=")
			if alias == "" {
				continue
			}
			if _, dup := seen[alias]; !dup {
				seen[alias] = struct{}{}
				models = append(models, llm.Model{ID: alias})
			}
		}
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}

func (s *Service) validateInput(input ProcessInput) error {
	if strings.TrimSpace(input.TemplateID) == "" {
		return errors.New("template_id is required")
	}
	if strings.TrimSpace(input.FileName) == "" {
		return errors.New("file is required")
	}
	if len(input.Bytes) == 0 {
		return errors.New("uploaded file is empty")
	}
	if int64(len(input.Bytes)) > s.maxUploadMB*1024*1024 {
		return fmt.Errorf("file exceeds max size of %dMB", s.maxUploadMB)
	}
	if err := validateUploadMetadata(input); err != nil {
		return err
	}
	if err := validateContextValues(input.Context); err != nil {
		return err
	}
	return nil
}
func normalizedUploadMIME(filename, media string) string {
	if media != "" && media != "application/octet-stream" {
		return media
	}
	if inferred := uploadMIMEs[strings.ToLower(filepath.Ext(filename))]; inferred != "" {
		return inferred
	}
	return media
}

func validateUploadMetadata(input ProcessInput) error {
	ext := strings.ToLower(filepath.Ext(input.FileName))
	media := strings.ToLower(strings.TrimSpace(strings.SplitN(input.MimeType, ";", 2)[0]))
	want, ok := uploadMIMEs[ext]
	if !ok {
		return fmt.Errorf("unsupported file type: %s", ext)
	}
	if media != "" && media != "application/octet-stream" && media != want {
		return fmt.Errorf("file MIME type %q does not match %s", media, ext)
	}
	if input.Operation == OperationVision && !strings.HasPrefix(want, "image/") {
		return errors.New("image recognition requires an image file")
	}
	return nil
}

func (s *Service) resolveModel(ctx context.Context, tenantID string, t Template) (string, error) {
	if strings.TrimSpace(t.DefaultModel) != "" {
		return t.DefaultModel, nil
	}
	settings, err := s.settingsRepo.GetSettings(ctx, tenantID)
	if err == nil && strings.TrimSpace(settings.DefaultModel) != "" {
		return settings.DefaultModel, nil
	}
	models, err := s.ModelsForExtract(ctx, tenantID)
	if err != nil {
		return "", fmt.Errorf("list models for fallback: %w", err)
	}
	if len(models) > 0 {
		return models[0].ID, nil
	}
	return "", errors.New("no models available")
}
func (s *Service) resolveEndpoint(ctx context.Context, tenantID, model, capability string) (string, error) {
	endpoints, err := s.endpoints.ListCapabilities(ctx, tenantID, capability)
	if err != nil {
		return "", fmt.Errorf("list endpoints for extraction: %w", err)
	}
	sort.Slice(endpoints, func(i, j int) bool { return endpoints[i].ID < endpoints[j].ID })
	for _, endpoint := range endpoints {
		if !endpoint.Enabled {
			continue
		}
		for _, binding := range endpoint.Capabilities {
			alias, ok := strings.CutPrefix(binding, inference.ModelCapabilityPrefix)
			if !ok {
				continue
			}
			alias, _, _ = strings.Cut(alias, "=")
			if alias == model {
				return endpoint.ID, nil
			}
		}
	}
	return "", fmt.Errorf("selected model %q has no enabled %s endpoint", model, capability)
}

func (s *Service) buildProviderRequest(model string, template Template, systemPrompt, userPrompt string, input ProcessInput) map[string]any {
	content := []map[string]any{}
	mime := strings.ToLower(input.MimeType)
	if strings.HasPrefix(mime, "image/") {
		b64 := base64.StdEncoding.EncodeToString(input.Bytes)
		content = append(content, map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url":    fmt.Sprintf("data:%s;base64,%s", input.MimeType, b64),
				"detail": "high",
			},
		})
	}
	text := userPrompt
	if input.DocumentText != "" {
		text += "\n\nDocument text:\n" + input.DocumentText
	}
	content = append(content, map[string]any{"type": "text", "text": text})

	responseFormat := map[string]any{"type": "json_object"}
	if len(template.OutputSchema) > 0 {
		responseFormat = map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   schemaName(template.ID),
				"schema": template.OutputSchema,
				"strict": false,
			},
		}
	}

	return map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": content},
		},
		"response_format": responseFormat,
		"temperature":     0,
	}
}

func interpolate(systemPrompt, userPrompt string, context map[string]any) (string, string) {
	if context == nil {
		return systemPrompt, userPrompt
	}
	for k, v := range context {
		placeholder := "{" + k + "}"
		strVal := fmt.Sprint(v)
		systemPrompt = strings.ReplaceAll(systemPrompt, placeholder, strVal)
		userPrompt = strings.ReplaceAll(userPrompt, placeholder, strVal)
	}
	return systemPrompt, userPrompt
}

func validateContextValues(ctx map[string]any) error {
	for key, value := range ctx {
		if strings.TrimSpace(key) == "" {
			return errors.New("context keys must be non-empty strings")
		}
		switch value.(type) {
		case nil, string, float64, bool, int, int64, uint64:
			continue
		default:
			return fmt.Errorf("context key %q has non-primitive value", key)
		}
	}
	return nil
}

func isPrimitiveSchema(schema map[string]any) bool {
	for _, v := range schema {
		if _, ok := v.(map[string]any); !ok {
			return false
		}
	}
	return true
}

type usageInfo struct {
	Prompt     int
	Completion int
	Total      int
}

func parseProviderExtraction(raw []byte) (map[string]any, string, usageInfo, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var envelope map[string]any
	if err := decoder.Decode(&envelope); err != nil {
		return nil, "", usageInfo{}, fmt.Errorf("decode provider response: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, "", usageInfo{}, errors.New("invalid trailing provider response data")
	}

	usage := usageInfo{}
	if usageRaw, ok := envelope["usage"].(map[string]any); ok {
		usage.Prompt = providerUsageCount(usageRaw["prompt_tokens"])
		usage.Completion = providerUsageCount(usageRaw["completion_tokens"])
		usage.Total = providerUsageCount(usageRaw["total_tokens"])
	}

	choicesRaw, ok := envelope["choices"].([]any)
	if !ok || len(choicesRaw) == 0 {
		return map[string]any{}, "", usage, errors.New("provider returned no choices")
	}
	choice, _ := choicesRaw[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	contentValue := message["content"]
	rawContent := extractContentText(contentValue)
	parsed, err := parseJSONBody(rawContent)
	if err != nil {
		return map[string]any{}, rawContent, usage, err
	}
	return parsed, rawContent, usage, nil
}

func extractContentText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := m["text"].(string); ok {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func parseJSONBody(content string) (map[string]any, error) {
	t := strings.TrimSpace(content)
	if strings.HasPrefix(t, "```json") {
		t = strings.TrimPrefix(t, "```json")
	}
	if strings.HasPrefix(t, "```") {
		t = strings.TrimPrefix(t, "```")
	}
	if strings.HasSuffix(t, "```") {
		t = strings.TrimSuffix(t, "```")
	}
	t = strings.TrimSpace(t)
	if t == "" {
		return map[string]any{}, errors.New("empty model response")
	}
	decoder := json.NewDecoder(strings.NewReader(t))
	decoder.UseNumber()
	var out map[string]any
	if err := decoder.Decode(&out); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid trailing JSON data")
	}
	return out, nil
}

func (s *Service) validateOutput(data map[string]any, template Template) ([]string, []string) {
	errorsList := []string{}
	warnings := []string{}
	if data == nil || len(data) == 0 {
		errorsList = append(errorsList, "empty extraction output")
		return errorsList, warnings
	}
	if _, hasParseErr := data["_parse_error"]; hasParseErr {
		errorsList = append(errorsList, "model output is not valid JSON")
	}
	compiled, err := compileOutputSchema(template.OutputSchema)
	if err != nil {
		errorsList = append(errorsList, err.Error())
	} else if err := compiled.validate(data); err != nil {
		errorsList = append(errorsList, err.Error())
	}
	if template.ID == "detailed_invoice" {
		if _, ok := data["invoice_number"]; !ok {
			warnings = append(warnings, "missing invoice_number")
		}
		if _, ok := data["total_amount"]; !ok {
			warnings = append(warnings, "missing total_amount")
		}
	}
	if len(errorsList) == 0 {
		warnings = append(warnings, "schema-valid output does not establish factual accuracy")
	}
	return errorsList, warnings
}

func schemaName(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "extract_output"
	}
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	name := strings.Trim(b.String(), "_-")
	if name == "" {
		name = "extract"
	}
	return name + "_output"
}

func providerUsageCount(value any) int {
	number, ok := value.(json.Number)
	if !ok {
		return 0
	}
	count, err := number.Int64()
	if err != nil {
		return 0
	}
	return int(count)
}

// jobView projects a durable job row onto the consumer shape; request fields
// come from the immutable policy document.
func jobView(row jobs.JobRow) JobView {
	v := JobView{
		ID:           row.ID,
		JobID:        row.ID,
		Status:       row.Status,
		InputRef:     row.InputRef,
		ResultRef:    row.ResultRef,
		ErrorMessage: jobError(row.Job),
		CreatedAt:    row.CreatedAt,
		UpdatedAt:    row.UpdatedAt,
		CompletedAt:  row.CompletedAt,
	}
	v.TemplateID = stringValue(row.Policy["template_id"])
	v.ModelUsed = stringValue(row.Policy["model"])
	v.FileName = stringValue(row.Policy["file_name"])
	v.FileMimeType = stringValue(row.Policy["mime_type"])
	if n, ok := row.Policy["file_size"].(float64); ok {
		v.FileSizeBytes = int64(n)
	}
	return v
}

func jobError(j jobs.Job) string {
	return j.Error
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func contextMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

// estimateInputTokens bounds input-side units: bytes are a conservative
// proxy for tokens (≥1 token / ~2 bytes incl. base64 transport inflation),
// plus prompt text and a fixed envelope allowance.
func estimateInputTokens(fileBytes int64, systemPrompt, userPrompt string) int64 {
	return fileBytes/2 + int64(len(systemPrompt)+len(userPrompt))/2 + 512
}

// mapNotFound collapses storage not-found errors onto the package sentinel so
// cross-tenant and absent ids produce one public outcome.
func mapNotFound(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, jobs.ErrJobNotFound) || errors.Is(err, artifacts.ErrNotFound) ||
		errors.Is(err, ErrNotFound) || strings.Contains(err.Error(), "no rows") {
		return ErrNotFound
	}
	return err
}
