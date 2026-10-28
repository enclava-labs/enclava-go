// activity.go — the Home "Running now" feed (OA-18): members see their own
// queued/running jobs keyed on the frozen principal's actor ID; managers see
// the tenant's jobs restricted to the operations their dashboard capabilities
// manage. Every row is a bounded, status-filtered read — predicates apply in
// SQL before LIMIT, and the DTO never serializes principals, policies,
// leases, fences or artifact refs.
package httpx

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/chatbot"
	"enclava-go/internal/connectors"
	"enclava-go/internal/extract"
	"enclava-go/internal/jobs"
	"enclava-go/internal/rag"
)

// ActivityItem is the safe projection of one durable job for Home.
type ActivityItem struct {
	ID        string          `json:"id"`
	Operation string          `json:"operation"`
	Status    string          `json:"status"`
	Label     string          `json:"label"`
	Detail    string          `json:"detail,omitempty"`
	Href      string          `json:"href,omitempty"`
	Progress  json.RawMessage `json:"progress,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// activityStatuses are the live states Home shows; failures surface through
// the attention aggregator instead.
var activityStatuses = []string{jobs.StatusQueued, jobs.StatusRunning}

// activityLimit caps the feed; the repo returns the NEWEST matching rows.
const activityLimit = 50

// managedActivityOps is the operation union of the manager capability groups
// (08-CONTEXT): knowledge, sources, extract, assistants.
func managedActivityOps(caps dashboardCaps) []string {
	var ops []string
	if caps.KnowledgeManage {
		ops = append(ops, rag.OperationEmbed, rag.OperationIngest, rag.OperationAccess)
	}
	if caps.SourcesManage {
		ops = append(ops, connectors.OperationSync)
	}
	if caps.ExtractManage {
		ops = append(ops, extract.OperationProcess, extract.OperationVision, extract.OperationPreview)
	}
	if caps.AssistantsManage {
		ops = append(ops, chatbot.RunOperation)
	}
	return ops
}

// memberActivityOps narrows a member's own feed to operations their feature
// capabilities cover (§3.4: a member with only extract sees own extract jobs).
func memberActivityOps(caps dashboardCaps) []string {
	var ops []string
	if caps.Search {
		ops = append(ops, rag.OperationEmbed, rag.OperationIngest, rag.OperationAccess)
	}
	if caps.SourcesManage {
		ops = append(ops, connectors.OperationSync)
	}
	if caps.Extract {
		ops = append(ops, extract.OperationProcess, extract.OperationVision, extract.OperationPreview)
	}
	if caps.Chat {
		ops = append(ops, chatbot.RunOperation)
	}
	return ops
}

// dashboardActivity composes the caller's activity feed. A manager (any
// managed op group) gets tenant jobs in their managed groups; a member falls
// back to own-actor jobs in the operations they hold.
func (a *App) dashboardActivity(ctx context.Context, p auth.Principal, caps dashboardCaps) ([]ActivityItem, error) {
	if a.jobsSvc == nil {
		log.Printf("dashboard activity: jobs service not configured")
		return []ActivityItem{}, nil
	}
	var list []jobs.Job
	var err error
	if ops := managedActivityOps(caps); len(ops) > 0 {
		list, err = a.jobsSvc.ListTenantActivity(ctx, p, ops, activityStatuses, activityLimit)
	} else {
		list, err = a.jobsSvc.ListActorActivity(ctx, p, activityStatuses, memberActivityOps(caps), activityLimit)
	}
	if err != nil {
		return nil, err
	}
	items := make([]ActivityItem, 0, len(list))
	for _, job := range list {
		items = append(items, activityItem(job))
	}
	return items, nil
}

// activityItem projects one job onto the safe DTO: operation-derived label,
// the owning dashboard section, and stored progress verbatim only when the
// job reports real counts (the repo scan already drops empty docs).
func activityItem(job jobs.Job) ActivityItem {
	return ActivityItem{
		ID:        job.ID,
		Operation: job.Operation,
		Status:    job.Status,
		Label:     dashboardHumanize(job.Operation),
		Href:      activityHref(job.Operation),
		Progress:  job.Progress,
		CreatedAt: job.CreatedAt,
		UpdatedAt: job.UpdatedAt,
	}
}

// activityHref points at the dashboard section that owns the operation.
func activityHref(operation string) string {
	switch {
	case strings.HasPrefix(operation, "rag."):
		return "/dashboard/knowledge"
	case operation == connectors.OperationSync:
		return "/dashboard/sources"
	case operation == extract.OperationProcess || operation == extract.OperationVision || operation == extract.OperationPreview:
		return "/dashboard/extract"
	case operation == chatbot.RunOperation:
		return "/dashboard/chat"
	default:
		return ""
	}
}
