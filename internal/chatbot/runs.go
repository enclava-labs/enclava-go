package chatbot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
	"enclava-go/internal/prompts"
	"enclava-go/internal/source"
)

const (
	RunOperation       = "chatbot.run"
	maxRunTurns        = 4
	maxRunToolCalls    = 8
	maxRunInputBytes   = 32 << 10
	maxRunContextBytes = 64 << 10
	maxRunOutputBytes  = 16 << 10
	maxRunWallTime     = 2 * time.Minute
)

var ErrRunNotFound = errors.New("chatbot: run not found")

type runConfigSnapshot struct {
	Assistant Instance         `json:"assistant"`
	Prompt    prompts.Template `json:"prompt"`
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
	}
	if err := s.runRepo.CreateRun(ctx, run); err != nil {
		return AssistantRun{}, err
	}
	if err := s.repo.AddMessage(ctx, Message{
		TenantID: p.TenantID, ID: newID("chatmsg"), ConversationID: conversation.ID,
		Role: "user", Content: input.Input, OwnerActorID: p.ActorID,
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
	if len(run.EvidenceIDs) > 0 {
		resolver, ok := s.rag.(EvidenceResolver)
		if !ok {
			run.Output = ""
		} else if _, err := resolver.ResolveEvidence(ctx, p, run.EvidenceIDs); err != nil {
			if errors.Is(err, source.ErrDenied) {
				run.Output = ""
			} else {
				return AssistantRun{}, err
			}
		}
	}
	return run, nil
}

func (s *Service) RunAttempts(ctx context.Context, p auth.Principal, runID string) ([]AssistantRunAttempt, error) {
	run, err := s.GetRun(ctx, p, runID)
	if err != nil {
		return nil, err
	}
	if len(run.EvidenceIDs) > 0 {
		resolver, ok := s.rag.(EvidenceResolver)
		if !ok {
			return nil, nil
		}
		if _, err := resolver.ResolveEvidence(ctx, p, run.EvidenceIDs); err != nil {
			if errors.Is(err, source.ErrDenied) {
				return nil, nil
			}
			return nil, err
		}
	}
	attempts, err := s.runRepo.ListRunAttempts(ctx, p.TenantID, run.ID)
	if err != nil {
		return nil, err
	}
	if run.Status == jobs.StatusUnknown {
		for i := range attempts {
			if attempts[i].Status == "running" {
				attempts[i].Status = jobs.StatusUnknown
			}
		}
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
		var frozen auth.Principal
		if err := json.Unmarshal(job.PrincipalSnapshot, &frozen); err != nil {
			return "", fmt.Errorf("decode run principal: %w", err)
		}
		if frozen.TenantID != job.TenantID || actorKey(frozen) != run.OwnerActorID || s.principalResolver == nil {
			return "", source.ErrDenied
		}
		principal, err := s.principalResolver.CurrentPrincipal(ctx, frozen)
		if err != nil {
			return "", err
		}
		if principal.TenantID != job.TenantID || actorKey(principal) != run.OwnerActorID {
			return "", source.ErrDenied
		}
		return s.executeRun(ctx, principal, claim, run)
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
	vars := mapFromAny(bot.Settings["variables"])
	rendered, err := prompts.Render(frozen.Prompt, prompts.RenderInput{UserInput: run.Input, RAGContext: ragContext, Variables: vars})
	if err != nil {
		return "", err
	}
	messages = applyRenderedPrompt(messages, userIndex, rendered)
	inputRefs := mergeObjectRefs(historyRefs, ragInputs(ragResults))
	evidenceIDs := mergeStrings(historyEvidence, evidenceIDsForResults(ragResults))
	payload := map[string]any{"model": bot.Model, "messages": messages, "stream": false}
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
		definitions, err = BuiltinToolDefinitions(bot)
		if err != nil {
			return "", err
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
		body, err := json.Marshal(payload)
		if err != nil {
			return fail(err)
		}
		if len(body) > maxRunContextBytes {
			return fail(invalidInput(errors.New("run context exceeds byte limit")))
		}
		attemptNumber++
		if err := refreshPrincipal(); err != nil {
			return fail(err)
		}
		attempt := AssistantRunAttempt{TenantID: p.TenantID, RunID: run.ID, Number: attemptNumber, Kind: "model", Name: bot.Model, Status: "running", Input: string(body), StartedAt: time.Now().UTC()}
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
			run.Output, run.Status, run.EvidenceIDs, run.InputRefs = turnResult.Content, jobs.StatusSucceeded, evidenceIDs, inputRefs
			if strings.TrimSpace(turnResult.Content) != "" {
				if err := s.runRepo.AddRunMessage(ctx, Message{TenantID: p.TenantID, ID: newID("chatmsg"), ConversationID: run.ConversationID, Role: "assistant", Content: turnResult.Content, OwnerActorID: p.ActorID, EvidenceIDs: evidenceIDs, InputRefs: inputRefs}, run.JobID, claim.Owner, claim.FenceToken); err != nil {
					return fail(err)
				}
			}
			if err := s.runRepo.UpdateRunFenced(ctx, run, claim.Owner, claim.FenceToken); err != nil {
				return "", err
			}
			return run.ID, nil
		}
		if turn+1 >= maxRunTurns || toolCalls+len(turnResult.ToolCalls) > maxRunToolCalls {
			return fail(errors.New("assistant run exceeded tool or turn limit"))
		}
		messages = append(messages, map[string]any{"role": "assistant", "content": turnResult.Content, "tool_calls": turnResult.RawToolCalls})
		for _, call := range turnResult.ToolCalls {
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
			toolAttempt := AssistantRunAttempt{TenantID: p.TenantID, RunID: run.ID, Number: attemptNumber, Kind: "tool", Name: call.Name, Status: "running", Input: string(call.Arguments), StartedAt: time.Now().UTC()}
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
			if _, isBuiltin := builtinSchemas[call.Name]; isBuiltin {
				toolResult, invokeErr = builtins.Invoke(ctx, p, bot, call.Name, call.Arguments, inputRefs)
			} else if s.mcpTools != nil {
				ownerNow, ownerErr := s.ownerPrincipal(ctx, p, bot)
				if ownerErr != nil {
					invokeErr = ownerErr
				} else {
					toolResult, invokeErr = s.mcpTools.Invoke(ctx, p, ownerNow, bot, call.Name, call.Arguments, inputRefs)
				}
			} else {
				invokeErr = ErrInvalidInput
			}
			if invokeErr != nil {
				toolAttempt.Status = "failed"
				toolAttempt.Error = invokeErr.Error()
				toolAttempt.EndedAt = time.Now().UTC()
				_ = s.runRepo.UpdateRunAttempt(ctx, toolAttempt, claim.Owner, claim.FenceToken)
				return fail(invokeErr)
			}
			if len(toolResult.Value) > maxBuiltinOutputBytes {
				return fail(errors.New("tool output exceeds byte limit"))
			}
			inputRefs = mergeObjectRefs(inputRefs, toolResult.InputRefs)
			evidenceIDs = mergeStrings(evidenceIDs, toolResult.EvidenceIDs)
			run.InputRefs, run.EvidenceIDs = inputRefs, evidenceIDs
			if err := s.runRepo.UpdateRunFenced(ctx, run, claim.Owner, claim.FenceToken); err != nil {
				return fail(err)
			}
			toolAttempt.Status = "succeeded"
			toolAttempt.Output = string(toolResult.Value)
			toolAttempt.EndedAt = time.Now().UTC()
			if err := s.runRepo.UpdateRunAttempt(ctx, toolAttempt, claim.Owner, claim.FenceToken); err != nil {
				return fail(err)
			}
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": call.ID, "content": string(toolResult.Value)})
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
		var argsObject map[string]json.RawMessage
		if call.Type != "function" || call.ID == "" || call.Function.Name == "" || len(args) == 0 || !json.Valid(args) || json.Unmarshal(args, &argsObject) != nil || argsObject == nil {
			return runModelResponse{}, ErrInvalidInput
		}
		result.ToolCalls = append(result.ToolCalls, runToolCall{ID: call.ID, Name: call.Function.Name, Arguments: args})
		result.RawToolCalls = append(result.RawToolCalls, map[string]any{"id": call.ID, "type": "function", "function": map[string]any{"name": call.Function.Name, "arguments": string(args)}})
	}
	return result, nil
}
