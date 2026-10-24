package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"enclava-go/internal/chatbot"
	"enclava-go/internal/jobs"
	"enclava-go/internal/source"
)

type ChatbotRunsRepo struct{ db *DB }

func NewChatbotRunsRepo(db *DB) *ChatbotRunsRepo { return &ChatbotRunsRepo{db: db} }

var _ chatbot.RunRepository = (*ChatbotRunsRepo)(nil)

func (r *ChatbotRunsRepo) CreateRun(ctx context.Context, run chatbot.AssistantRun) error {
	if len(run.ConfigSnapshot) == 0 {
		run.ConfigSnapshot = json.RawMessage(`{}`)
	}
	evidence, err := json.Marshal(nonNilStrings(run.EvidenceIDs))
	if err != nil {
		return fmt.Errorf("marshal run evidence ids: %w", err)
	}
	refs, err := json.Marshal(nonNilObjectRefs(run.InputRefs))
	if err != nil {
		return fmt.Errorf("marshal run input refs: %w", err)
	}
	restrictions, err := json.Marshal(nonNilRestrictions(run.Restrictions))
	if err != nil {
		return fmt.Errorf("marshal run restrictions: %w", err)
	}
	_, err = r.db.SQL.ExecContext(ctx, `
		INSERT INTO chatbot_runs
			(tenant_id,id,chatbot_id,conversation_id,job_id,owner_actor_id,status,input,output,error,
			 config_snapshot,prompt_template_id,prompt_template_version,evidence_ids,input_refs,restrictions)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		run.TenantID, run.ID, run.ChatbotID, run.ConversationID, run.JobID, run.OwnerActorID,
		run.Status, run.Input, run.Output, run.Error, run.ConfigSnapshot,
		run.PromptTemplateID, run.PromptTemplateVersion, evidence, refs, restrictions)
	if err != nil {
		return fmt.Errorf("create chatbot run: %w", err)
	}
	return nil
}

func (r *ChatbotRunsRepo) GetRun(ctx context.Context, tenantID, id string) (chatbot.AssistantRun, error) {
	var run chatbot.AssistantRun
	var evidence, refs, restrictions []byte
	err := r.db.SQL.QueryRowContext(ctx, `
		SELECT tenant_id::text,id,chatbot_id,conversation_id,job_id,owner_actor_id,status,input,output,error,
		       config_snapshot,prompt_template_id,prompt_template_version,evidence_ids,input_refs,restrictions,fence_token,created_at,updated_at
		FROM chatbot_runs WHERE tenant_id=$1 AND id=$2`, tenantID, id).Scan(
		&run.TenantID, &run.ID, &run.ChatbotID, &run.ConversationID, &run.JobID, &run.OwnerActorID, &run.Status,
		&run.Input, &run.Output, &run.Error, &run.ConfigSnapshot,
		&run.PromptTemplateID, &run.PromptTemplateVersion, &evidence, &refs, &restrictions,
		&run.FenceToken, &run.CreatedAt, &run.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) || isInvalidUUID(err) {
		return chatbot.AssistantRun{}, sql.ErrNoRows
	}
	if err != nil {
		return chatbot.AssistantRun{}, fmt.Errorf("get chatbot run: %w", err)
	}
	_ = json.Unmarshal(evidence, &run.EvidenceIDs)
	_ = json.Unmarshal(refs, &run.InputRefs)
	_ = json.Unmarshal(restrictions, &run.Restrictions)
	return run, nil
}

func (r *ChatbotRunsRepo) UpdateRun(ctx context.Context, run chatbot.AssistantRun) error {
	res, err := r.db.SQL.ExecContext(ctx, `
		UPDATE chatbot_runs SET job_id=$3,status=$4,output=$5,error=$6,evidence_ids=$7,input_refs=$8,restrictions=$9,updated_at=now()
		WHERE tenant_id=$1 AND id=$2 AND status IN ('queued','running')`,
		run.TenantID, run.ID, run.JobID, run.Status, run.Output, run.Error,
		jsonValue(nonNilStrings(run.EvidenceIDs)), jsonValue(nonNilObjectRefs(run.InputRefs)), jsonValue(nonNilRestrictions(run.Restrictions)))
	if err != nil {
		return fmt.Errorf("update chatbot run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (r *ChatbotRunsRepo) UpdateRunFenced(ctx context.Context, run chatbot.AssistantRun, owner string, fence int64) error {
	res, err := r.db.SQL.ExecContext(ctx, `
		UPDATE chatbot_runs SET status=$5,output=$6,error=$7,evidence_ids=$8,input_refs=$9,restrictions=$10,
		       fence_token=$4,updated_at=now()
		WHERE tenant_id=$1 AND id=$2 AND job_id=$3 AND status IN ('queued','running')
		  AND EXISTS (SELECT 1 FROM jobs j WHERE j.tenant_id=$1 AND j.id=$3 AND j.fence_token=$4 AND j.lease_owner=$11)`,
		run.TenantID, run.ID, run.JobID, fence, run.Status, run.Output, run.Error,
		jsonValue(nonNilStrings(run.EvidenceIDs)), jsonValue(nonNilObjectRefs(run.InputRefs)), jsonValue(nonNilRestrictions(run.Restrictions)), owner)
	if err != nil {
		return fmt.Errorf("fenced chatbot run update: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return jobs.ErrStaleFence
	}
	return nil
}

func (r *ChatbotRunsRepo) UpdateRunAttempt(ctx context.Context, a chatbot.AssistantRunAttempt, owner string, fence int64) error {
	res, err := r.db.SQL.ExecContext(ctx, `
		UPDATE chatbot_run_attempts SET status=$4,output=$5,error=$6,ended_at=$7
		WHERE tenant_id=$1 AND run_id=$2 AND number=$3
		  AND EXISTS (SELECT 1 FROM chatbot_runs r JOIN jobs j ON j.tenant_id=r.tenant_id AND j.id=r.job_id
		    WHERE r.tenant_id=$1 AND r.id=$2 AND j.fence_token=$8 AND j.lease_owner=$9)`,
		a.TenantID, a.RunID, a.Number, a.Status, a.Output, a.Error, nullableRunTime(a.EndedAt), fence, owner)
	if err != nil {
		return fmt.Errorf("fenced chatbot attempt update: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return jobs.ErrStaleFence
	}
	return nil
}

func (r *ChatbotRunsRepo) AddRunMessage(ctx context.Context, message chatbot.Message, jobID, owner string, fence int64) error {
	evidence, err := json.Marshal(nonNilStrings(message.EvidenceIDs))
	if err != nil {
		return err
	}
	refs, err := json.Marshal(nonNilObjectRefs(message.InputRefs))
	if err != nil {
		return err
	}
	res, err := r.db.SQL.ExecContext(ctx, `
		INSERT INTO messages (tenant_id,id,conversation_id,role,content,owner_actor_id,evidence_ids,input_refs)
		SELECT $1,$2,$3,$4,$5,$6,$7,$8 WHERE EXISTS (
		  SELECT 1 FROM jobs j WHERE j.tenant_id=$1 AND j.id=$9 AND j.fence_token=$10 AND j.lease_owner=$11)`,
		message.TenantID, message.ID, message.ConversationID, message.Role, message.Content,
		nullableText(message.OwnerActorID), evidence, refs, jobID, fence, owner)
	if err != nil {
		return fmt.Errorf("fenced chatbot message insert: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return jobs.ErrStaleFence
	}
	return nil
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func nonNilObjectRefs(values []source.ObjectRef) []source.ObjectRef {
	if values == nil {
		return []source.ObjectRef{}
	}
	return values
}
func nonNilRestrictions(values []source.SourceRestriction) []source.SourceRestriction {
	if values == nil {
		return []source.SourceRestriction{}
	}
	return values
}

func nullableRunTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

func (r *ChatbotRunsRepo) AddRunAttempt(ctx context.Context, a chatbot.AssistantRunAttempt, owner string, fence int64) error {
	var ended any
	if !a.EndedAt.IsZero() {
		ended = a.EndedAt
	}
	res, err := r.db.SQL.ExecContext(ctx, `
		INSERT INTO chatbot_run_attempts (tenant_id,run_id,number,kind,name,status,input,output,error,started_at,ended_at)
		SELECT r.tenant_id,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11
		FROM chatbot_runs r JOIN jobs j ON j.tenant_id=r.tenant_id AND j.id=r.job_id
		WHERE r.tenant_id=$1 AND r.id=$2 AND j.fence_token=$13 AND j.lease_owner=$12`,
		a.TenantID, a.RunID, a.Number, a.Kind, a.Name, a.Status, a.Input, a.Output, a.Error, a.StartedAt, ended, owner, fence)
	if err != nil {
		return fmt.Errorf("add chatbot run attempt: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return jobs.ErrStaleFence
	}
	return nil
}

func (r *ChatbotRunsRepo) ListRunAttempts(ctx context.Context, tenantID, runID string) ([]chatbot.AssistantRunAttempt, error) {
	rows, err := r.db.SQL.QueryContext(ctx, `SELECT tenant_id::text,run_id,number,kind,name,status,input,output,error,started_at,ended_at
		FROM chatbot_run_attempts WHERE tenant_id=$1 AND run_id=$2 ORDER BY number`, tenantID, runID)
	if err != nil {
		return nil, fmt.Errorf("list chatbot run attempts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []chatbot.AssistantRunAttempt{}
	for rows.Next() {
		var a chatbot.AssistantRunAttempt
		var ended sql.NullTime
		if err := rows.Scan(&a.TenantID, &a.RunID, &a.Number, &a.Kind, &a.Name, &a.Status, &a.Input, &a.Output, &a.Error, &a.StartedAt, &ended); err != nil {
			return nil, err
		}
		if ended.Valid {
			a.EndedAt = ended.Time
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate chatbot run attempts: %w", err)
	}
	return out, nil
}

func jsonValue(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`[]`)
	}
	return b
}
