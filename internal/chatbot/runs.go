package chatbot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"enclava-go/internal/auth"
	"enclava-go/internal/extract"
	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
	"enclava-go/internal/mcp"
	"enclava-go/internal/prompts"
	"enclava-go/internal/rag"
	"enclava-go/internal/source"
)

const (
	RunOperation       = "chatbot.run"
	maxRunTurns        = 4
	maxRunToolCalls    = 8
	maxRunInputBytes   = 32 << 10
	maxRunContextBytes = 64 << 10
	maxRunOutputBytes  = 16 << 10
	// defaultRunMaxTokens bounds every agent-run model call: slow local
	// models rambling past the output byte cap otherwise burn the whole
	// wall clock and fail with "model output exceeds byte limit".
	defaultRunMaxTokens = 1024
	maxRunWallTime      = 2 * time.Minute
)

var ErrRunNotFound = errors.New("chatbot: run not found")

type runConfigSnapshot struct {
	Assistant Instance         `json:"assistant"`
	Prompt    prompts.Template `json:"prompt"`
}

// maxRunEvidenceIDs bounds the client-supplied evidence handoff.
const maxRunEvidenceIDs = 8

// normalizeRunEvidenceIDs trims, validates and dedupes the bounded
// evidence_ids handoff; empty entries or an oversize list are 400-class.
func normalizeRunEvidenceIDs(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > maxRunEvidenceIDs {
		return nil, invalidInput(fmt.Errorf("evidence_ids accepts at most %d entries", maxRunEvidenceIDs))
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, id := range raw {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, invalidInput(errors.New("evidence_ids entries must be non-empty evidence IDs"))
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

// resolveRunEvidence authorizes the client-supplied IDs server-side before
// anything persists: denied/forbidden/unknown IDs fail the run start under
// the existing not-found/denied mapping — never silently stripped. The
// resolutions' span refs become the run's canonical persisted input refs.
func (s *Service) resolveRunEvidence(ctx context.Context, p auth.Principal, ids []string) ([]source.ObjectRef, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	resolver, ok := s.rag.(EvidenceResolver)
	if !ok {
		return nil, source.ErrDenied
	}
	resolutions, err := resolver.ResolveEvidence(ctx, p, ids)
	if err != nil {
		return nil, err
	}
	var refs []source.ObjectRef
	for _, resolution := range resolutions {
		for _, span := range resolution.Spans {
			if span.Ref.ObjectID != "" {
				refs = append(refs, span.Ref)
			}
		}
	}
	return refs, nil
}

func (s *Service) StartRun(ctx context.Context, p auth.Principal, chatbotRef string, input AssistantRunInput) (AssistantRun, error) {
	if err := requireTenant(p); err != nil {
		return AssistantRun{}, err
	}
	if s.runRepo == nil || s.jobSvc == nil {
		return AssistantRun{}, errors.New("chatbot: durable runs are not configured")
	}
	input.Input = strings.TrimSpace(input.Input)
	if input.Input == "" || len(input.Input) > maxRunInputBytes {
		return AssistantRun{}, invalidInput(errors.New("run input is empty or exceeds the byte limit"))
	}
	evidenceIDs, err := normalizeRunEvidenceIDs(input.EvidenceIDs)
	if err != nil {
		return AssistantRun{}, err
	}
	evidenceRefs, err := s.resolveRunEvidence(ctx, p, evidenceIDs)
	if err != nil {
		return AssistantRun{}, err
	}
	bot, err := s.getByRef(ctx, p, chatbotRef)
	if err != nil {
		return AssistantRun{}, err
	}
	if !bot.IsEnabled {
		return AssistantRun{}, ErrDisabled
	}
	if !chatbotAllowed(p, bot) {
		return AssistantRun{}, ErrChatbotNotAllowed
	}
	if !auth.ModelAllowed(p, bot.Model) {
		return AssistantRun{}, ErrModelNotAllowed
	}
	conversation, err := s.ensureConversation(ctx, p, bot, chatCompletionRequest{ConversationID: input.ConversationID})
	if err != nil {
		return AssistantRun{}, err
	}
	template, err := s.promptTemplate(ctx, p, bot)
	if err != nil {
		return AssistantRun{}, err
	}
	snapshot, err := json.Marshal(runConfigSnapshot{Assistant: bot, Prompt: template})
	if err != nil {
		return AssistantRun{}, fmt.Errorf("snapshot assistant configuration: %w", err)
	}
	run := AssistantRun{
		ID: newID("chatrun"), TenantID: p.TenantID, OwnerActorID: actorKey(p), ChatbotID: bot.ID,
		ConversationID: conversation.ID, Status: jobs.StatusQueued, Input: input.Input,
		ConfigSnapshot: snapshot, PromptTemplateID: template.ID,
		EvidenceIDs: evidenceIDs, InputRefs: evidenceRefs,
	}
	if err := s.runRepo.CreateRun(ctx, run); err != nil {
		return AssistantRun{}, err
	}
	if err := s.repo.AddMessage(ctx, Message{
		TenantID: p.TenantID, ID: newID("chatmsg"), ConversationID: conversation.ID,
		Role: "user", Content: input.Input, OwnerActorID: p.ActorID,
		RunID: run.ID, EvidenceIDs: evidenceIDs, InputRefs: evidenceRefs,
	}); err != nil {
		run.Status, run.Error = jobs.StatusFailed, err.Error()
		_ = s.runRepo.UpdateRun(ctx, run)
		return AssistantRun{}, err
	}
	hash := sha256.Sum256([]byte(input.Input))
	job, _, err := s.jobSvc.Enqueue(ctx, p, jobs.EnqueueRequest{
		TenantID: p.TenantID, Operation: RunOperation, IdempotencyKey: run.ID,
		Subject: jobs.Subject{ObjectID: run.ID}, InputRef: run.ID, InputHash: hex.EncodeToString(hash[:]),
		Policy: map[string]any{"run_id": run.ID, "chatbot_id": bot.ID}, MaxAttempts: 1,
	})
	if err != nil {
		run.Status, run.Error = jobs.StatusFailed, err.Error()
		_ = s.runRepo.UpdateRun(ctx, run)
		return AssistantRun{}, err
	}
	run.JobID = job.ID
	if err := s.runRepo.UpdateRun(ctx, run); err != nil {
		return AssistantRun{}, err
	}
	return run, nil
}

func (s *Service) GetRun(ctx context.Context, p auth.Principal, id string) (AssistantRun, error) {
	if err := requireTenant(p); err != nil {
		return AssistantRun{}, err
	}
	run, err := s.runRepo.GetRun(ctx, p.TenantID, strings.TrimSpace(id))
	if err != nil || (!auth.HasScope(p, "platform.admin") && run.OwnerActorID != actorKey(p)) {
		return AssistantRun{}, ErrRunNotFound
	}
	// Current-visibility gate: even the run's owner reads retained runs only
	// while the CURRENT assistant row still admits them (grant AND audience;
	// owner/platform.admin bypass the audience term only). Unknown and
	// forbidden share ErrRunNotFound.
	if bot, botErr := s.repo.GetByID(ctx, p.TenantID, run.ChatbotID); botErr != nil || !chatbotAllowed(p, bot) {
		return AssistantRun{}, ErrRunNotFound
	}
	if s.jobSvc != nil && run.JobID != "" {
		if job, jobErr := s.jobSvc.Get(ctx, p, run.JobID); jobErr == nil {
			run.Status = job.Status
			if job.Status == jobs.StatusUnknown {
				run.Error = "worker outcome unknown; the attempt will not be repeated"
			}
		}
	}
	// One shared authorized projection: the current-evidence gate both
	// suppresses the retained output and reduces the steps to metadata —
	// the exact projection conversation replay attaches to the turn.
	steps, evidenceDenied, err := s.authorizedSteps(ctx, p, run)
	if err != nil {
		return AssistantRun{}, err
	}
	if evidenceDenied {
		run.Output = ""
	}
	run.Steps = steps
	return run, nil
}

// authorizedSteps projects a run's persisted attempts into ordered steps
// under the CURRENT evidence gate. It is the single attempt read path:
// GetRun and conversation assembly both consume this projection, so replay
// never fetches raw attempts through a second, unauthorized route. When the
// run's evidence is denied the projection keeps step METADATA ONLY —
// Number/Kind/Name/Status/timestamps survive, the source-bearing
// Input/Output/Error are cleared (tool errors can quote source content).
func (s *Service) authorizedSteps(ctx context.Context, p auth.Principal, run AssistantRun) ([]AssistantRunStep, bool, error) {
	denied, err := s.evidenceDenied(ctx, p, run.EvidenceIDs)
	if err != nil {
		return nil, false, err
	}
	attempts, err := s.runRepo.ListRunAttempts(ctx, run.TenantID, run.ID)
	if err != nil {
		return nil, false, err
	}
	if run.Status == jobs.StatusUnknown {
		for i := range attempts {
			if attempts[i].Status == "running" {
				attempts[i].Status = jobs.StatusUnknown
			}
		}
	}
	steps := make([]AssistantRunStep, 0, len(attempts))
	for _, attempt := range attempts {
		step := AssistantRunStep{
			Number: attempt.Number, Kind: attempt.Kind, Name: attempt.Name, Status: attempt.Status,
			Input: attempt.Input, Output: attempt.Output, Error: attempt.Error,
			StartedAt: attempt.StartedAt,
		}
		if !attempt.EndedAt.IsZero() {
			ended := attempt.EndedAt
			step.EndedAt = &ended
		}
		if denied {
			step.Input, step.Output, step.Error = "", "", ""
		}
		steps = append(steps, step)
	}
	return steps, denied, nil
}

func (s *Service) RunAttempts(ctx context.Context, p auth.Principal, runID string) ([]AssistantRunAttempt, error) {
	run, err := s.GetRun(ctx, p, runID)
	if err != nil {
		return nil, err
	}
	attempts := make([]AssistantRunAttempt, 0, len(run.Steps))
	for _, step := range run.Steps {
		attempt := AssistantRunAttempt{
			TenantID: run.TenantID, RunID: run.ID,
			Number: step.Number, Kind: step.Kind, Name: step.Name, Status: step.Status,
			Input: step.Input, Output: step.Output, Error: step.Error,
			StartedAt: step.StartedAt,
		}
		if step.EndedAt != nil {
			attempt.EndedAt = *step.EndedAt
		}
		attempts = append(attempts, attempt)
	}
	return attempts, nil
}

func (s *Service) CancelRun(ctx context.Context, p auth.Principal, runID string) error {
	run, err := s.runRepo.GetRun(ctx, p.TenantID, strings.TrimSpace(runID))
	if err != nil || (!auth.HasScope(p, "platform.admin") && run.OwnerActorID != actorKey(p)) {
		return ErrRunNotFound
	}
	if s.jobSvc == nil || run.JobID == "" {
		return ErrRunNotFound
	}
	if err := s.jobSvc.Cancel(ctx, p, run.JobID); err != nil {
		return err
	}
	job, err := s.jobSvc.Get(ctx, p, run.JobID)
	if err == nil && job.Status == jobs.StatusCancelled {
		run.Status = jobs.StatusCancelled
		return s.runRepo.UpdateRun(ctx, run)
	}
	return nil
}

// JobHandler is registered under RunOperation in the shared jobs.Runner.
func (s *Service) JobHandler() jobs.HandlerFunc {
	return func(ctx context.Context, claim *jobs.Claim, _ jobs.InputFunc) (string, error) {
		if claim == nil || claim.Job().Operation != RunOperation {
			return "", jobs.ErrInvalidRequest
		}
		if s.runRepo == nil || s.principalResolver == nil {
			return "", errors.New("chatbot: run worker dependencies are not configured")
		}
		job := claim.Job()
		run, err := s.runRepo.GetRun(ctx, job.TenantID, job.InputRef)
		if err != nil {
			return "", err
		}
		// Terminal-exit synchronization: every exit after the run row loads
		// marks the run under the same fencing rules executeRun's fail
		// closure uses — exits before that closure (admission denial,
		// decode errors) would otherwise leave the run row queued while the
		// durable job fails. A stale lease no-ops the fenced write.
		failRun := func(cause error) (string, error) {
			status := jobs.StatusFailed
			if errors.Is(cause, context.Canceled) {
				status = jobs.StatusCancelled
			}
			run.Status, run.Error = status, cause.Error()
			_ = s.runRepo.UpdateRunFenced(context.Background(), run, claim.Owner, claim.FenceToken)
			return "", cause
		}
		var frozen auth.Principal
		if err := json.Unmarshal(job.PrincipalSnapshot, &frozen); err != nil {
			return failRun(fmt.Errorf("decode run principal: %w", err))
		}
		if frozen.TenantID != job.TenantID || actorKey(frozen) != run.OwnerActorID || s.principalResolver == nil {
			return failRun(source.ErrDenied)
		}
		principal, err := s.principalResolver.CurrentPrincipal(ctx, frozen)
		if err != nil {
			return failRun(err)
		}
		if principal.TenantID != job.TenantID || actorKey(principal) != run.OwnerActorID {
			return failRun(source.ErrDenied)
		}
		runID, err := s.executeRun(ctx, principal, claim, run)
		if err != nil {
			return failRun(err)
		}
		return runID, nil
	}
}

func (s *Service) executeRun(parent context.Context, p auth.Principal, claim *jobs.Claim, run AssistantRun) (string, error) {
	if s.runRepo == nil || s.executor == nil || s.principalResolver == nil {
		return "", errors.New("chatbot: run worker dependencies are not configured")
	}
	ctx, cancel := context.WithTimeout(parent, maxRunWallTime)
	defer cancel()
	if len(run.Input) > maxRunInputBytes {
		return "", invalidInput(errors.New("stored run input exceeds limit"))
	}
	var frozen runConfigSnapshot
	if err := json.Unmarshal(run.ConfigSnapshot, &frozen); err != nil {
		return "", fmt.Errorf("decode run configuration: %w", err)
	}
	bot := frozen.Assistant
	currentBot, err := s.repo.GetByID(ctx, p.TenantID, run.ChatbotID)
	if err != nil {
		return "", ErrChatbotNotAllowed
	}
	bot.TenantID, bot.OwnerActorID = p.TenantID, currentBot.OwnerActorID
	// Admission authorizes against the CURRENT bot row and the CURRENT
	// principal — never frozen.Assistant, which supplies configuration only.
	// A run whose audience or grants narrowed while queued fails here.
	if !bot.IsEnabled || !chatbotAllowed(p, currentBot) {
		return "", ErrChatbotNotAllowed
	}
	if !auth.ModelAllowed(p, bot.Model) {
		return "", ErrModelNotAllowed
	}
	if frozen.Prompt.ID != run.PromptTemplateID || !frozen.Prompt.IsActive {
		return "", ErrInvalidInput
	}
	refreshPrincipal := func() error {
		current, err := s.principalResolver.CurrentPrincipal(ctx, p)
		if err != nil {
			return err
		}
		if current.TenantID != run.TenantID || actorKey(current) != run.OwnerActorID {
			return source.ErrDenied
		}
		p = current
		// Every privileged refresh re-loads the CURRENT assistant and
		// re-checks grants AND audience before the pending model/tool
		// dispatch — a mid-run audience or grant removal aborts the run
		// here, before any further dispatch executes.
		liveBot, err := s.repo.GetByID(ctx, p.TenantID, run.ChatbotID)
		if err != nil || !chatbotAllowed(p, liveBot) {
			return ErrChatbotNotAllowed
		}
		if !auth.ModelAllowed(p, bot.Model) {
			return ErrModelNotAllowed
		}
		return nil
	}
	if err := s.runRepo.UpdateRunFenced(ctx, run, claim.Owner, claim.FenceToken); err != nil {
		return "", err
	}
	if err := refreshPrincipal(); err != nil {
		return "", err
	}
	run.Status = jobs.StatusRunning
	if err := s.runRepo.UpdateRunFenced(ctx, run, claim.Owner, claim.FenceToken); err != nil {
		return "", err
	}
	storedHistory, historyRefs, historyEvidence, err := s.authorizedHistory(ctx, p, run.ConversationID)
	if err != nil {
		return "", err
	}
	messages := make([]map[string]any, 0, len(storedHistory)+1)
	for _, message := range storedHistory {
		// Redacted assistant turns (revoked evidence) contribute no text to
		// inference. Redacted USER turns stay: their content is replaced by
		// the rendered prompt below, and the run's own turn must remain the
		// last user message even when its attached evidence was revoked.
		if message.Role != "user" && strings.TrimSpace(message.Content) == "" {
			continue
		}
		messages = append(messages, map[string]any{"role": message.Role, "content": message.Content})
	}
	userIndex := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i]["role"] == "user" {
			userIndex = i
			break
		}
	}
	if userIndex < 0 {
		return "", source.ErrDenied
	}
	owner, err := s.ownerPrincipal(ctx, p, bot)
	if err != nil {
		return "", err
	}
	ragResults, ragContext, err := s.retrieveRAG(ctx, p, owner, bot, run.Input)
	if err != nil {
		return "", err
	}
	// Evidence-attached runs: re-read every persisted binding through the
	// CURRENT-passage reader under the CURRENT principal. Denied, revoked or
	// superseded IDs at execution time contribute NO passage and NO refs —
	// revoked source text is never reintroduced into inference context.
	attachedResults, attachedContext, err := s.attachedEvidence(ctx, p, run.EvidenceIDs, bot)
	if err != nil {
		return "", err
	}
	if len(attachedResults) > 0 {
		ragResults = append(ragResults, attachedResults...)
		ragContext = strings.TrimSpace(strings.Join(nonEmptyLines(ragContext, attachedContext), "\n\n"))
	}
	vars := mapFromAny(bot.Settings["variables"])
	rendered, err := prompts.Render(frozen.Prompt, prompts.RenderInput{UserInput: run.Input, RAGContext: ragContext, Variables: vars})
	if err != nil {
		return "", err
	}
	messages = applyRenderedPrompt(messages, userIndex, rendered)
	inputRefs := mergeObjectRefs(historyRefs, ragInputs(ragResults))
	evidenceIDs := mergeStrings(historyEvidence, evidenceIDsForResults(ragResults))
	payload := map[string]any{"model": bot.Model, "messages": messages, "stream": false, "max_tokens": defaultRunMaxTokens}
	var definitions []map[string]any
	var mcpDefinitions []map[string]any
	if s.mcpTools != nil {
		mcpDefinitions, err = s.mcpTools.Definitions(ctx, p, owner, bot)
	}
	if err != nil {
		return "", err
	}
	if len(mcpDefinitions) > 0 {
		approved := make(map[string]struct{}, len(mcpDefinitions))
		for _, tool := range mcpDefinitions {
			function, _ := tool["function"].(map[string]any)
			name, _ := function["name"].(string)
			approved[name] = struct{}{}
		}
		for _, id := range configuredToolIDs(bot) {
			if _, builtin := builtinSchemas[id]; builtin {
				continue
			}
			if _, ok := approved[id]; !ok {
				return "", ErrInvalidInput
			}
		}
		definitions, err = builtinToolDefinitions(bot, approved)
		if err != nil {
			return "", err
		}
		definitions = append(definitions, mcpDefinitions...)
	}
	if len(mcpDefinitions) == 0 {
		definitions, err = builtinToolDefinitions(bot, nil)
		if err != nil {
			return "", err
		}
	}
	toolNames := modelToolNames(definitions)
	toolSchemas := make(map[string]any, len(definitions))
	for _, definition := range definitions {
		function, _ := definition["function"].(map[string]any)
		name, _ := function["name"].(string)
		toolSchemas[name] = function["parameters"]
	}
	historyStart := 0
	if rendered.SystemPrompt != "" {
		historyStart = 1
	}
	historyCount := userIndex
	contextBudget := maxRunContextBytes
	if provider, ok := s.executor.(interface {
		ModelContextLength(context.Context, auth.Principal, string, string) (int64, error)
	}); ok {
		window, metadataErr := provider.ModelContextLength(ctx, p, bot.Model, strings.TrimSpace(bot.ProviderID))
		if metadataErr != nil && (errors.Is(metadataErr, context.Canceled) || errors.Is(metadataErr, context.DeadlineExceeded)) {
			return "", metadataErr
		}
		if metadataErr == nil && window > 0 && window < int64(maxRunContextBytes/3+defaultRunMaxTokens) {
			available := (window - defaultRunMaxTokens) * 3
			if available < 0 {
				available = 0
			}
			contextBudget = int(available)
		}
	}
	if len(definitions) > 0 {
		payload["tools"] = definitions
	}
	builtins := Builtins{RAG: s.rag, Extractor: s.extractor, Owners: s.principalResolver}
	if reader, ok := s.rag.(EvidenceReader); ok {
		builtins.Evidence = reader
	}
	run.InputRefs, run.EvidenceIDs = inputRefs, evidenceIDs
	if err := s.runRepo.UpdateRunFenced(ctx, run, claim.Owner, claim.FenceToken); err != nil {
		return "", err
	}
	toolCalls := 0
	attemptNumber, err := countRunAttempts(ctx, s.runRepo, p.TenantID, run.ID)
	if err != nil {
		return "", err
	}
	dispatched := false
	fail := func(cause error) (string, error) {
		if errors.Is(cause, context.Canceled) {
			run.Status = jobs.StatusCancelled
		} else {
			run.Status = jobs.StatusFailed
		}
		run.Error = cause.Error()
		_ = s.runRepo.UpdateRunFenced(context.Background(), run, claim.Owner, claim.FenceToken)
		return "", cause
	}
	for turn := 0; turn < maxRunTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		cancelled, err := claim.CancelRequested(ctx)
		if err != nil {
			return fail(err)
		}
		if cancelled {
			return fail(context.Canceled)
		}
		noTools := turn == maxRunTurns-1 || toolCalls >= maxRunToolCalls
		if noTools {
			delete(payload, "tools")
		}
		body, fitted, err := fitRunContext(payload, messages, historyStart, &historyCount, contextBudget)
		if err != nil {
			return fail(err)
		}
		messages = fitted
		attemptNumber++
		if err := refreshPrincipal(); err != nil {
			return fail(err)
		}
		// Attempt-write hygiene: model attempts persist a bounded route
		// summary only — the assembled request body (system prompt, history,
		// attached passages) never reaches a step row. Tool attempts keep
		// their tool-level arguments.
		attempt := AssistantRunAttempt{TenantID: p.TenantID, RunID: run.ID, Number: attemptNumber, Kind: "model", Name: bot.Model, Status: "running", Input: modelAttemptSummary(bot.Model, len(messages), len(definitions)), StartedAt: time.Now().UTC()}
		if err := s.runRepo.AddRunAttempt(ctx, attempt, claim.Owner, claim.FenceToken); err != nil {
			return fail(err)
		}
		if !dispatched {
			if err := claim.MarkDispatched(ctx); err != nil {
				return fail(err)
			}
			dispatched = true
		}
		result, err := s.executor.Execute(ctx, p, inference.Request{
			Capability: inference.CapabilityChat, Model: bot.Model, EndpointID: strings.TrimSpace(bot.ProviderID),
			Payload: body, Inputs: inputRefs, Estimate: completionEstimate(body),
		})
		if err != nil {
			attempt.Status = "failed"
			attempt.Error = err.Error()
			attempt.EndedAt = time.Now().UTC()
			_ = s.runRepo.UpdateRunAttempt(ctx, attempt, claim.Owner, claim.FenceToken)
			return fail(err)
		}
		// Observed route: persist the endpoint/model actually dispatched —
		// never the configured provider label.
		if result.EndpointID != "" {
			run.EndpointID = result.EndpointID
		}
		if result.Model != "" {
			run.Model = result.Model
		}
		response, readErr := io.ReadAll(io.LimitReader(result.Body, maxRunOutputBytes+1))
		closeErr := result.Body.Close()
		if readErr != nil {
			err = readErr
		} else if closeErr != nil {
			err = closeErr
		} else if result.SettlementError != nil {
			err = result.SettlementError
		} else if len(response) > maxRunOutputBytes {
			err = errors.New("model output exceeds byte limit")
		}
		if err != nil {
			attempt.Status = "failed"
			attempt.Error = err.Error()
			attempt.EndedAt = time.Now().UTC()
			_ = s.runRepo.UpdateRunAttempt(ctx, attempt, claim.Owner, claim.FenceToken)
			return fail(err)
		}
		attempt.Status = "succeeded"
		attempt.Output = string(response)
		attempt.EndedAt = time.Now().UTC()
		if err := s.runRepo.UpdateRunAttempt(ctx, attempt, claim.Owner, claim.FenceToken); err != nil {
			return fail(err)
		}
		turnResult, err := parseRunModelResponse(response)
		if err != nil {
			return fail(err)
		}
		if len(turnResult.ToolCalls) == 0 {
			if len(turnResult.Content) > maxRunOutputBytes {
				return fail(errors.New("assistant output exceeds byte limit"))
			}
			if strings.TrimSpace(turnResult.Content) == "" {
				return fail(errors.New("assistant returned an empty answer"))
			}
			run.Output, run.Status, run.EvidenceIDs, run.InputRefs = turnResult.Content, jobs.StatusSucceeded, evidenceIDs, inputRefs
			if strings.TrimSpace(turnResult.Content) != "" {
				if err := s.runRepo.AddRunMessage(ctx, Message{TenantID: p.TenantID, ID: newID("chatmsg"), ConversationID: run.ConversationID, Role: "assistant", Content: turnResult.Content, OwnerActorID: p.ActorID, RunID: run.ID, EvidenceIDs: evidenceIDs, InputRefs: inputRefs}, run.JobID, claim.Owner, claim.FenceToken); err != nil {
					return fail(err)
				}
			}
			if err := s.runRepo.UpdateRunFenced(ctx, run, claim.Owner, claim.FenceToken); err != nil {
				return "", err
			}
			return run.ID, nil
		}
		if noTools {
			return fail(errors.New("assistant returned tool calls when tools were unavailable"))
		}
		messages = append(messages, map[string]any{"role": "assistant", "content": turnResult.Content, "tool_calls": turnResult.RawToolCalls})
		for _, call := range turnResult.ToolCalls {
			overBudget := toolCalls >= maxRunToolCalls
			toolCalls++
			if err := ctx.Err(); err != nil {
				return fail(err)
			}
			cancelled, err := claim.CancelRequested(ctx)
			if err != nil {
				return fail(err)
			}
			if cancelled {
				return fail(context.Canceled)
			}
			attemptNumber++
			if err := refreshPrincipal(); err != nil {
				return fail(err)
			}
			internalName, known := toolNames[call.Name]
			attemptName := internalName
			if !known {
				attemptName = call.Name
			}
			toolAttempt := AssistantRunAttempt{TenantID: p.TenantID, RunID: run.ID, Number: attemptNumber, Kind: "tool", Name: attemptName, Status: "running", Input: string(call.Arguments), StartedAt: time.Now().UTC()}
			if err := s.runRepo.AddRunAttempt(ctx, toolAttempt, claim.Owner, claim.FenceToken); err != nil {
				return fail(err)
			}
			if !dispatched {
				if err := claim.MarkDispatched(ctx); err != nil {
					return fail(err)
				}
				dispatched = true
			}
			var toolResult BuiltinResult
			var invokeErr error
			ownerFailed := false
			var argsObject map[string]json.RawMessage
			if overBudget {
				invokeErr = errors.New("tool call budget exhausted")
			} else if !known {
				invokeErr = errors.New("unknown tool")
			} else if !json.Valid(call.Arguments) || json.Unmarshal(call.Arguments, &argsObject) != nil || argsObject == nil || !validToolArguments(toolSchemas[call.Name], call.Arguments) {
				invokeErr = errors.New("malformed tool arguments")
			} else if _, isBuiltin := builtinSchemas[internalName]; isBuiltin {
				toolResult, invokeErr = builtins.Invoke(ctx, p, bot, internalName, call.Arguments, inputRefs)
			} else if s.mcpTools != nil {
				ownerNow, ownerErr := s.ownerPrincipal(ctx, p, bot)
				if ownerErr != nil {
					invokeErr = ownerErr
					ownerFailed = true
				} else {
					toolResult, invokeErr = s.mcpTools.Invoke(ctx, p, ownerNow, bot, internalName, call.Arguments, inputRefs)
				}
			} else {
				invokeErr = errors.New("unknown tool")
			}
			oversized := invokeErr == nil && len(toolResult.Value) > maxBuiltinOutputBytes
			if oversized {
				toolResult.Value = json.RawMessage(truncateRunText(string(toolResult.Value), maxBuiltinOutputBytes, "[truncated]"))
				invokeErr = errors.New("tool output exceeds byte limit")
			}
			if invokeErr != nil {
				toolAttempt.Status = "failed"
				toolAttempt.Error = invokeErr.Error()
				toolAttempt.EndedAt = time.Now().UTC()
				if err := s.runRepo.UpdateRunAttempt(ctx, toolAttempt, claim.Owner, claim.FenceToken); err != nil {
					return fail(err)
				}
				if ctx.Err() != nil {
					return fail(ctx.Err())
				}
				if ownerFailed || errors.Is(invokeErr, source.ErrDenied) || errors.Is(invokeErr, inference.ErrDenied) || errors.Is(invokeErr, mcp.ErrDenied) || errors.Is(invokeErr, extract.ErrForbidden) || errors.Is(invokeErr, ErrChatbotNotAllowed) || errors.Is(invokeErr, ErrModelNotAllowed) || errors.Is(invokeErr, context.Canceled) || errors.Is(invokeErr, context.DeadlineExceeded) {
					return fail(invokeErr)
				}
				if !oversized {
					message := "tool invocation failed"
					if !known {
						message = "unknown tool"
					} else if overBudget {
						message = "tool call budget exhausted"
					} else if argsObject == nil || errors.Is(invokeErr, ErrInvalidInput) {
						message = "invalid tool arguments"
					}
					value, _ := json.Marshal(map[string]string{"error": message})
					toolResult.Value = value
				}
				messages = append(messages, map[string]any{"role": "tool", "tool_call_id": call.ID, "content": string(toolResult.Value)})
				if !oversized {
					continue
				}
			}
			inputRefs = mergeObjectRefs(inputRefs, toolResult.InputRefs)
			evidenceIDs = mergeStrings(evidenceIDs, toolResult.EvidenceIDs)
			run.InputRefs, run.EvidenceIDs = inputRefs, evidenceIDs
			if err := s.runRepo.UpdateRunFenced(ctx, run, claim.Owner, claim.FenceToken); err != nil {
				return fail(err)
			}
			if !oversized {
				toolAttempt.Status = "succeeded"
				toolAttempt.Output = string(toolResult.Value)
				toolAttempt.EndedAt = time.Now().UTC()
				if err := s.runRepo.UpdateRunAttempt(ctx, toolAttempt, claim.Owner, claim.FenceToken); err != nil {
					return fail(err)
				}
				messages = append(messages, map[string]any{"role": "tool", "tool_call_id": call.ID, "content": string(toolResult.Value)})
			}
		}
		payload["messages"] = messages
		if len(inputRefs) > 0 {
			run.InputRefs = inputRefs
			run.EvidenceIDs = evidenceIDs
			if err := s.runRepo.UpdateRunFenced(ctx, run, claim.Owner, claim.FenceToken); err != nil {
				return "", err
			}
		}
	}
	return fail(errors.New("assistant run exceeded turn limit"))
}

func countRunAttempts(ctx context.Context, repo RunRepository, tenantID, runID string) (int, error) {
	attempts, err := repo.ListRunAttempts(ctx, tenantID, runID)
	if err != nil {
		return 0, err
	}
	return len(attempts), nil
}

// modelAttemptSummary is the tool-safe model-attempt input summary persisted
// on step rows: route shape only, never the assembled request body.
func modelAttemptSummary(model string, messages, tools int) string {
	return fmt.Sprintf(`{"model":%q,"messages":%d,"tools":%d}`, model, messages, tools)
}

// attachedEvidence re-reads every attached evidence ID through the narrow
// CURRENT-passage reader (ReadEvidence authorizes the exact published
// version; ResolveEvidence is metadata-only and would happily return
// superseded records). Still-authorized passages become search-result-shaped
// inputs feeding the same context/ref plumbing as search-derived evidence.
func (s *Service) attachedEvidence(ctx context.Context, p auth.Principal, ids []string, bot Instance) ([]rag.SearchResult, string, error) {
	reader, ok := s.rag.(EvidenceReader)
	if !ok || len(ids) == 0 {
		return nil, "", nil
	}
	results := make([]rag.SearchResult, 0, len(ids))
	for _, id := range ids {
		text, err := reader.ReadEvidence(ctx, p, id)
		if err != nil {
			continue // denied/revoked/superseded: no passage, no refs
		}
		results = append(results, rag.SearchResult{
			DocumentID: text.DocumentID, CollectionID: text.CollectionID,
			Spans: text.Spans, EvidenceID: text.EvidenceID, Content: text.Content,
		})
	}
	if len(results) == 0 {
		return nil, "", nil
	}
	context, err := s.rag.BuildContext(ctx, p, results, settingInt(bot.Settings, "rag_context_max_chars", 6000, 500, 50000))
	if err != nil {
		return nil, "", err
	}
	return results, context.Text, nil
}

func nonEmptyLines(parts ...string) []string {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			out = append(out, part)
		}
	}
	return out
}

type runToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}
type runModelResponse struct {
	Content      string
	ToolCalls    []runToolCall
	RawToolCalls []any
}

func parseRunModelResponse(raw []byte) (runModelResponse, error) {
	var envelope struct {
		Choices []struct {
			Message struct {
				Content   any `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return runModelResponse{}, fmt.Errorf("decode assistant response: %w", err)
	}
	if len(envelope.Choices) == 0 {
		return runModelResponse{}, errors.New("assistant response has no choices")
	}
	message := envelope.Choices[0].Message
	result := runModelResponse{Content: contentToString(message.Content)}
	for _, call := range message.ToolCalls {
		args := call.Function.Arguments
		var encoded string
		if json.Unmarshal(args, &encoded) == nil {
			args = json.RawMessage(encoded)
		}
		if call.Type != "function" || call.ID == "" || call.Function.Name == "" {
			return runModelResponse{}, ErrInvalidInput
		}
		result.ToolCalls = append(result.ToolCalls, runToolCall{ID: call.ID, Name: call.Function.Name, Arguments: args})
		result.RawToolCalls = append(result.RawToolCalls, map[string]any{"id": call.ID, "type": "function", "function": map[string]any{"name": call.Function.Name, "arguments": string(args)}})
	}
	return result, nil
}

func validToolArguments(schema any, args json.RawMessage) bool {
	raw, err := json.Marshal(schema)
	if err != nil {
		return false
	}
	validate, err := mcp.CompileSchema(raw)
	return err == nil && mcp.ValidateJSON(validate, args) == nil
}

func modelToolNames(definitions []map[string]any) map[string]string {
	ids := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		function, _ := definition["function"].(map[string]any)
		id, _ := function["name"].(string)
		ids = append(ids, id)
	}
	sort.Strings(ids)
	names := make(map[string]string, len(ids))
	reverse := make(map[string]string, len(ids))
	for _, id := range ids {
		var b strings.Builder
		for _, r := range id {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
				b.WriteRune(r)
			} else {
				b.WriteByte('_')
			}
		}
		base := b.String()
		if base == "" {
			base = "_"
		}
		if len(base) > 64 {
			base = base[:64]
		}
		name := base
		for n := 2; ; n++ {
			if _, exists := reverse[name]; !exists {
				break
			}
			suffix := fmt.Sprintf("_%d", n)
			prefix := base
			if len(prefix)+len(suffix) > 64 {
				prefix = prefix[:64-len(suffix)]
			}
			name = prefix + suffix
		}
		names[id], reverse[name] = name, id
	}
	for _, definition := range definitions {
		function, _ := definition["function"].(map[string]any)
		id, _ := function["name"].(string)
		function["name"] = names[id]
	}
	return reverse
}

func truncateRunText(text string, limit int, marker string) string {
	if len(text) <= limit {
		return text
	}
	if limit <= len(marker) {
		return marker
	}
	end := limit - len(marker)
	for end > 0 && !utf8.ValidString(text[:end]) {
		end--
	}
	return text[:end] + marker
}

func fitRunContext(payload map[string]any, messages []map[string]any, historyStart int, historyCount *int, budget int) ([]byte, []map[string]any, error) {
	const marker = "[truncated to fit context]"
	for {
		payload["messages"] = messages
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, messages, err
		}
		if len(body) <= budget {
			return body, messages, nil
		}
		if *historyCount > 0 {
			messages = append(messages[:historyStart], messages[historyStart+1:]...)
			*historyCount--
			continue
		}
		largest, size := -1, len(marker)
		for i, message := range messages {
			if message["role"] != "tool" {
				continue
			}
			text, _ := message["content"].(string)
			if len(text) > size {
				largest, size = i, len(text)
			}
		}
		if largest < 0 {
			return nil, messages, invalidInput(errors.New("run context exceeds the model's context budget"))
		}
		text := messages[largest]["content"].(string)
		text = strings.TrimSuffix(text, marker)
		limit := size / 2
		if limit < len(marker) {
			limit = len(marker)
		}
		if len(text)+len(marker) <= limit {
			messages[largest]["content"] = marker
		} else {
			messages[largest]["content"] = truncateRunText(text+marker, limit, marker)
		}
	}
}
