package extract

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/jobs"
	"enclava-go/internal/llm"
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
	ListOwnedJobs(ctx context.Context, tenantID, submitterKey string, limit int) ([]jobs.JobRow, error)
	CountOwnedJobs(ctx context.Context, tenantID, submitterKey string) (int, error)
	CountOwnedJobsSince(ctx context.Context, tenantID, submitterKey string, since time.Time) (int, error)
}

// Service owns the extract pipeline: Process persists the upload as a durable
// input artifact and enqueues an "extract.process" job — it never calls the
// provider synchronously. JobHandler is the worker-side fenced attempt:
// reserve → dispatch → provider → result artifact → settle.
type Service struct {
	templatesRepo TemplatesRepo
	settingsRepo  SettingsRepo
	jobRows       JobReader
	jobs          *jobs.Service
	artifacts     *artifacts.Store
	billing       *billing.Service
	provider      llm.Provider
	maxUploadMB   int64
}

func NewService(
	templatesRepo TemplatesRepo,
	settingsRepo SettingsRepo,
	jobRows JobReader,
	jobSvc *jobs.Service,
	artifactStore *artifacts.Store,
	billingSvc *billing.Service,
	provider llm.Provider,
	maxUploadMB int64,
) *Service {
	return &Service{
		templatesRepo: templatesRepo,
		settingsRepo:  settingsRepo,
		jobRows:       jobRows,
		jobs:          jobSvc,
		artifacts:     artifactStore,
		billing:       billingSvc,
		provider:      provider,
		maxUploadMB:   maxUploadMB,
	}
}

// maxExtractOutputTokens bounds the reserved output-side estimate; settle
// records the truthful actual usage without clipping.
const maxExtractOutputTokens = 8192

// writeLeaseTTL covers the input upload stream; the attempt's own writes
// get a fresh lease.
const writeLeaseTTL = 10 * time.Minute

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
	start := time.Now()
	if p.TenantID == "" {
		return ProcessOutput{}, fmt.Errorf("%w: tenant is required", ErrForbidden)
	}
	if err := s.validateInput(input); err != nil {
		return ProcessOutput{}, err
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

	model, err := s.resolveModel(ctx, p.TenantID, template)
	if err != nil {
		return ProcessOutput{}, err
	}
	if !auth.ModelAllowed(p, model) {
		return ProcessOutput{}, fmt.Errorf("%w: model is not allowed for this credential", ErrForbidden)
	}

	// Persist the input BEFORE the job row exists — the job's input_ref binds
	// the finalized artifact version.
	w, ref, err := s.artifacts.Begin(ctx, artifacts.Meta{
		Ref:      artifacts.Ref{TenantID: p.TenantID, Kind: artifacts.KindOriginal},
		MimeType: input.MimeType,
	}, writeLeaseTTL, s.maxUploadMB*1024*1024)
	if err != nil {
		return ProcessOutput{}, fmt.Errorf("begin input artifact: %w", err)
	}
	if _, err := w.Write(input.Bytes); err != nil {
		_ = w.Abort(ctx)
		return ProcessOutput{}, fmt.Errorf("write input artifact: %w", err)
	}
	if err := w.Finalize(ctx); err != nil {
		return ProcessOutput{}, fmt.Errorf("finalize input artifact: %w", err)
	}
	ref = w.Ref()

	idem := strings.TrimSpace(input.IdempotencyKey)
	sum := sha256.Sum256(input.Bytes)
	inputHash := hex.EncodeToString(sum[:])
	if idem == "" {
		idem = "extract:" + input.TemplateID + ":" + inputHash
	}

	job, _, err := s.jobs.Enqueue(ctx, p, jobs.EnqueueRequest{
		TenantID:       p.TenantID,
		Operation:      OperationProcess,
		IdempotencyKey: idem,
		Subject: jobs.Subject{
			ObjectID:         ref.ID,
			ObservedRevision: int64(ref.Version),
		},
		InputRef:  ref.ID,
		InputHash: inputHash,
		Policy: map[string]any{
			"template_id": template.ID,
			"model":       model,
			"file_name":   input.FileName,
			"mime_type":   input.MimeType,
			"file_size":   int64(len(input.Bytes)),
			"context":     input.Context,
		},
	})
	if err != nil {
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

// JobHandler is the jobs.Runner handler for OperationProcess. It is
// code-complete but not dispatched by composition yet — the runner wiring
// for workers lands with the executor plan; Process only enqueues.
func (s *Service) JobHandler() jobs.HandlerFunc {
	return func(ctx context.Context, claim *jobs.Claim, input jobs.InputFunc) (string, error) {
		return s.attempt(ctx, claim, input)
	}
}

// attempt executes one claimed extract job under the fenced lease:
// Reserve → MarkDispatched → provider → result artifact → Settle. The
// initiating principal is read back from the frozen job snapshot and every
// authorization is re-checked before privileged work.
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
	model, _ := job.Policy["model"].(string)
	if strings.TrimSpace(model) == "" {
		if model, err = s.resolveModel(ctx, job.TenantID, template); err != nil {
			return "", err
		}
	}
	if !auth.ExtractTemplateAllowed(p, template.ID) || !auth.ModelAllowed(p, model) {
		return "", ErrForbidden
	}

	in, err := input(ctx)
	if err != nil {
		return "", fmt.Errorf("read input artifact: %w", err)
	}
	defer func() { _ = in.Close() }()
	inBytes, err := io.ReadAll(in)
	if err != nil {
		return "", fmt.Errorf("read input bytes: %w", err)
	}

	promptSystem, promptUser := interpolate(template.SystemPrompt, template.UserPrompt, contextMap(job.Policy["context"]))

	// Bounded per-kind estimate: reserve before any provider call. Missing
	// price refuses dispatch (ErrPriceMissing) — no silent free units.
	estimate := billing.Usage{
		InputUnits:  estimateInputTokens(int64(len(inBytes)), promptSystem, promptUser),
		OutputUnits: maxExtractOutputTokens,
	}
	res, err := s.billing.Reserve(ctx, p, billing.ReserveRequest{
		Capability: OperationProcess,
		Model:      model,
		JobID:      job.ID,
		AttemptID:  claim.AttemptID(),
		Estimate:   estimate,
		Price:      billing.PriceKey{SubjectType: billing.PriceSubjectModel, SubjectID: model},
	})
	if err != nil {
		return "", err
	}
	releaseHold := func() { _ = s.billing.ReleaseUndispatched(context.WithoutCancel(ctx), p, res.ID) }

	if err := claim.MarkDispatched(ctx); err != nil {
		releaseHold()
		return "", err
	}
	if err := s.billing.MarkDispatched(ctx, p, res.ID); err != nil {
		releaseHold()
		return "", fmt.Errorf("mark reservation dispatched: %w", err)
	}

	inInput := ProcessInput{
		TemplateID: template.ID,
		Context:    contextMap(job.Policy["context"]),
		FileName:   stringValue(job.Policy["file_name"]),
		MimeType:   stringValue(job.Policy["mime_type"]),
		Bytes:      inBytes,
	}
	requestBytes, err := json.Marshal(s.buildProviderRequest(model, template, promptSystem, promptUser, inInput))
	if err != nil {
		return "", fmt.Errorf("marshal provider request: %w", err)
	}

	chatRespRaw, err := s.provider.CreateChatCompletion(ctx, requestBytes)
	if err != nil {
		// The upstream call was dispatched; its outcome is unobserved —
		// retain liability, never silently retry.
		_ = s.billing.MarkUnknown(context.WithoutCancel(ctx), p, res.ID)
		return "", fmt.Errorf("provider chat completion: %w", err)
	}

	parsed, rawContent, usage, parseErr := parseProviderExtraction(chatRespRaw)
	if parseErr != nil {
		if parsed == nil {
			parsed = map[string]any{}
		}
		parsed["_parse_error"] = parseErr.Error()
	}
	validationErrors, validationWarnings := s.validateOutput(parsed, template)

	resultDoc := ResultView{
		ID:                   newID("res"),
		JobID:                job.ID,
		Data:                 parsed,
		RawResponse:          rawContent,
		ValidationErrors:     validationErrors,
		ValidationWarnings:   validationWarnings,
		PromptTokensUsed:     usage.Prompt,
		CompletionTokensUsed: usage.Completion,
		TokensUsed:           usage.Total,
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
		_ = s.billing.MarkUnknown(context.WithoutCancel(ctx), p, res.ID)
		return "", fmt.Errorf("begin result artifact: %w", err)
	}
	if _, err := rw.Write(docBytes); err != nil {
		_ = rw.Abort(ctx)
		_ = s.billing.MarkUnknown(context.WithoutCancel(ctx), p, res.ID)
		return "", fmt.Errorf("write result artifact: %w", err)
	}
	if err := rw.Finalize(ctx); err != nil {
		_ = s.billing.MarkUnknown(context.WithoutCancel(ctx), p, res.ID)
		return "", fmt.Errorf("finalize result artifact: %w", err)
	}
	rref = rw.Ref()

	usageRecord, err := s.billing.Settle(ctx, p, res.ID, billing.Usage{
		InputUnits:       int64(usage.Prompt),
		OutputUnits:      int64(usage.Completion),
		CachedInputUnits: 0,
	})
	if err != nil {
		// Result bytes exist but settlement failed: retain liability and fail —
		// a stale retry reserves a fresh hold under the same fenced attempt.
		_ = s.artifacts.Delete(context.WithoutCancel(ctx), rref)
		_ = s.billing.MarkUnknown(context.WithoutCancel(ctx), p, res.ID)
		return "", fmt.Errorf("settle reservation: %w", err)
	}
	_ = usageRecord
	return rref.ID, nil
}

// ListJobs returns the caller's newest jobs plus the total row count for
// paging — owner-bound through the jobs submitter key.
func (s *Service) ListJobs(ctx context.Context, p auth.Principal, limit, offset int) ([]JobView, int, error) {
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
	total, err := s.jobRows.CountOwnedJobs(ctx, p.TenantID, key)
	if err != nil {
		return nil, 0, err
	}
	rows, err := s.jobRows.ListOwnedJobs(ctx, p.TenantID, key, limit+offset)
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
// total non-deleted job counts — the owner-bound pair the dashboard renders.
func (s *Service) JobStats(ctx context.Context, p auth.Principal, since time.Time) (recent int64, total int64, err error) {
	key := jobs.SubmitterKey(p)
	t, err := s.jobRows.CountOwnedJobs(ctx, p.TenantID, key)
	if err != nil {
		return 0, 0, err
	}
	rec, err := s.jobRows.CountOwnedJobsSince(ctx, p.TenantID, key, since)
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
	view := jobView(row)
	detail := JobDetail{Job: view}
	if row.ResultRef == "" || s.artifacts == nil {
		return detail, nil
	}
	rc, err := s.artifacts.Read(ctx, artifacts.Ref{TenantID: p.TenantID, ID: row.ResultRef})
	if err != nil {
		// A missing/tombstoned result artifact degrades to "no result" —
		// never reveals storage detail.
		return detail, nil
	}
	defer func() { _ = rc.Close() }()
	var res ResultView
	if err := json.NewDecoder(io.LimitReader(rc, 8<<20)).Decode(&res); err != nil {
		return detail, nil
	}
	detail.Result = &res
	return detail, nil
}

func (s *Service) ListTemplates(ctx context.Context, p auth.Principal) ([]Template, error) {
	return s.templatesRepo.ListTemplates(ctx, p.TenantID)
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
	if len(t.OutputSchema) > 0 && !isObjectSchema(t.OutputSchema) {
		return errors.New("output_schema must be a JSON object schema")
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

func (s *Service) ModelsForExtract(ctx context.Context) ([]llm.Model, error) {
	modelsResp, err := s.provider.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	models := make([]llm.Model, 0, len(modelsResp.Data))
	for _, m := range modelsResp.Data {
		if llm.SupportsVisionOrDocument(m) {
			models = append(models, m)
		}
	}
	// Some providers don't populate capability metadata; fall back to all models.
	if len(models) == 0 {
		return modelsResp.Data, nil
	}
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
	ext := strings.ToLower(filepath.Ext(input.FileName))
	allowed := map[string]struct{}{".pdf": {}, ".jpg": {}, ".jpeg": {}, ".png": {}}
	if _, ok := allowed[ext]; !ok {
		return fmt.Errorf("unsupported file type: %s", ext)
	}
	if err := validateContextValues(input.Context); err != nil {
		return err
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

	modelsResp, err := s.provider.ListModels(ctx)
	if err != nil {
		return "", fmt.Errorf("list models for fallback: %w", err)
	}
	for _, m := range modelsResp.Data {
		if llm.SupportsVisionOrDocument(m) {
			return m.ID, nil
		}
	}
	if len(modelsResp.Data) > 0 {
		return modelsResp.Data[0].ID, nil
	}
	return "", errors.New("no models available")
}

func (s *Service) buildProviderRequest(model string, template Template, systemPrompt, userPrompt string, input ProcessInput) map[string]any {
	b64 := base64.StdEncoding.EncodeToString(input.Bytes)
	dataURL := fmt.Sprintf("data:%s;base64,%s", input.MimeType, b64)

	content := []map[string]any{}
	mime := strings.ToLower(input.MimeType)
	if strings.Contains(mime, "image/") {
		content = append(content, map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url":    dataURL,
				"detail": "high",
			},
		})
	} else {
		content = append(content, map[string]any{
			"type": "input_file",
			"input_file": map[string]any{
				"filename":  input.FileName,
				"file_data": dataURL,
			},
		})
	}
	content = append(content, map[string]any{
		"type": "text",
		"text": userPrompt,
	})

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

func isObjectSchema(schema map[string]any) bool {
	if len(schema) == 0 {
		return true
	}
	if typ, ok := schema["type"]; ok && !schemaTypeAllows(typ, "object") {
		return false
	}
	if props, ok := schema["properties"]; ok {
		if _, ok := props.(map[string]any); !ok {
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
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, "", usageInfo{}, fmt.Errorf("decode provider response: %w", err)
	}

	usage := usageInfo{}
	if usageRaw, ok := envelope["usage"].(map[string]any); ok {
		usage.Prompt = int(toFloat(usageRaw["prompt_tokens"]))
		usage.Completion = int(toFloat(usageRaw["completion_tokens"]))
		usage.Total = int(toFloat(usageRaw["total_tokens"]))
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
	var out map[string]any
	if err := json.Unmarshal([]byte(t), &out); err != nil {
		return nil, err
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
	if len(template.OutputSchema) > 0 {
		errorsList = append(errorsList, validateAgainstSchema(data, template.OutputSchema, "$")...)
	}
	if template.ID == "detailed_invoice" {
		if _, ok := data["invoice_number"]; !ok {
			warnings = append(warnings, "missing invoice_number")
		}
		if _, ok := data["total_amount"]; !ok {
			warnings = append(warnings, "missing total_amount")
		}
	}
	return errorsList, warnings
}

func validateAgainstSchema(value any, schema map[string]any, path string) []string {
	if len(schema) == 0 {
		return nil
	}
	errorsList := []string{}
	if enumRaw, ok := schema["enum"].([]any); ok && len(enumRaw) > 0 {
		matched := false
		for _, candidate := range enumRaw {
			if fmt.Sprint(candidate) == fmt.Sprint(value) {
				matched = true
				break
			}
		}
		if !matched {
			errorsList = append(errorsList, fmt.Sprintf("%s must be one of the declared enum values", path))
		}
	}
	if typ, ok := schema["type"]; ok && !valueMatchesSchemaType(value, typ) {
		errorsList = append(errorsList, fmt.Sprintf("%s must be %s", path, schemaTypeDescription(typ)))
		return errorsList
	}
	if value == nil && schemaTypeAllows(schema["type"], "null") {
		return errorsList
	}

	objectValue, isObj := value.(map[string]any)
	if isObj || schemaTypeAllows(schema["type"], "object") || schema["properties"] != nil || schema["required"] != nil {
		if !isObj {
			errorsList = append(errorsList, fmt.Sprintf("%s must be object", path))
			return errorsList
		}
		for _, field := range schemaStringSlice(schema["required"]) {
			if _, ok := objectValue[field]; !ok {
				errorsList = append(errorsList, fmt.Sprintf("%s.%s is required", path, field))
			}
		}
		properties := schemaObjectMap(schema["properties"])
		for field, fieldSchema := range properties {
			childValue, ok := objectValue[field]
			if !ok {
				continue
			}
			childSchema, ok := fieldSchema.(map[string]any)
			if !ok {
				continue
			}
			errorsList = append(errorsList, validateAgainstSchema(childValue, childSchema, path+"."+field)...)
		}
		if additional, ok := schema["additionalProperties"].(bool); ok && !additional && len(properties) > 0 {
			for field := range objectValue {
				if _, ok := properties[field]; !ok {
					errorsList = append(errorsList, fmt.Sprintf("%s.%s is not allowed by output_schema", path, field))
				}
			}
		}
		return errorsList
	}

	arrayValue, isArray := value.([]any)
	if isArray || schemaTypeAllows(schema["type"], "array") {
		if !isArray {
			errorsList = append(errorsList, fmt.Sprintf("%s must be array", path))
			return errorsList
		}
		if itemSchema, ok := schema["items"].(map[string]any); ok {
			for i, item := range arrayValue {
				errorsList = append(errorsList, validateAgainstSchema(item, itemSchema, fmt.Sprintf("%s[%d]", path, i))...)
			}
		}
	}
	return errorsList
}

func valueMatchesSchemaType(value any, typ any) bool {
	for _, t := range schemaTypeList(typ) {
		switch t {
		case "null":
			if value == nil {
				return true
			}
		case "string":
			if _, ok := value.(string); ok {
				return true
			}
		case "number":
			switch value.(type) {
			case float64, float32, int, int64, int32, uint64, uint32:
				return true
			}
		case "integer":
			switch v := value.(type) {
			case float64:
				return v == float64(int64(v))
			case int, int64, int32, uint64, uint32:
				return true
			}
		case "boolean":
			if _, ok := value.(bool); ok {
				return true
			}
		case "object":
			if _, ok := value.(map[string]any); ok {
				return true
			}
		case "array":
			if _, ok := value.([]any); ok {
				return true
			}
		}
	}
	return false
}

func schemaTypeAllows(typ any, want string) bool {
	for _, t := range schemaTypeList(typ) {
		if t == want {
			return true
		}
	}
	return false
}

func schemaTypeList(typ any) []string {
	switch v := typ.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []string{strings.TrimSpace(v)}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case []string:
		return v
	default:
		return nil
	}
}

func schemaTypeDescription(typ any) string {
	types := schemaTypeList(typ)
	if len(types) == 0 {
		return "declared JSON schema type"
	}
	return strings.Join(types, " or ")
}

func schemaStringSlice(raw any) []string {
	switch v := raw.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case []string:
		return v
	default:
		return nil
	}
}

func schemaObjectMap(raw any) map[string]any {
	if m, ok := raw.(map[string]any); ok {
		return m
	}
	return map[string]any{}
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

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	default:
		return 0
	}
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
