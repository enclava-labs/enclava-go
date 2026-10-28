// dashboard_caps.go — the one place dashboard scope strings become
// capabilities (D-18, source §3.1): templates and route guards read
// capability booleans only, and platform.admin grants the admin pages but
// never a feature capability.
package httpx

import (
	"context"

	"enclava-go/internal/auth"
)

// dashboardCap names one dashboard capability; the string form matches
// source §3.1 so logs and future nav wiring stay readable.
type dashboardCap string

const (
	capChat             dashboardCap = "chat"
	capSearch           dashboardCap = "search"
	capKnowledgeManage  dashboardCap = "knowledge.manage"
	capSourcesManage    dashboardCap = "sources.manage"
	capExtract          dashboardCap = "extract"
	capExtractManage    dashboardCap = "extract.manage"
	capAssistantsManage dashboardCap = "assistants.manage"
	capToolsManage      dashboardCap = "tools.manage"
	capModelsManage     dashboardCap = "models.manage"
	capAccessManage     dashboardCap = "access.manage"
	capBillingView      dashboardCap = "billing.view"
)

// dashboardCaps is the per-request capability set the nav model, page
// sections and route guards all read.
type dashboardCaps struct {
	Chat             bool
	Search           bool
	KnowledgeManage  bool
	SourcesManage    bool
	Extract          bool
	ExtractManage    bool
	AssistantsManage bool
	ToolsManage      bool
	ModelsManage     bool
	AccessManage     bool
	BillingView      bool
}

// grants resolves a capability name to its flag — the single cap→bool table
// the guard predicate goes through. Unknown names deny (fail closed).
func (c dashboardCaps) grants(cap dashboardCap) bool {
	switch cap {
	case capChat:
		return c.Chat
	case capSearch:
		return c.Search
	case capKnowledgeManage:
		return c.KnowledgeManage
	case capSourcesManage:
		return c.SourcesManage
	case capExtract:
		return c.Extract
	case capExtractManage:
		return c.ExtractManage
	case capAssistantsManage:
		return c.AssistantsManage
	case capToolsManage:
		return c.ToolsManage
	case capModelsManage:
		return c.ModelsManage
	case capAccessManage:
		return c.AccessManage
	case capBillingView:
		return c.BillingView
	}
	return false
}

// dashboardCapabilities maps a verified principal's scopes to dashboard
// capabilities exactly per source §3.1; platform.admin maps only to
// ToolsManage, AccessManage and BillingView — no implicit feature caps.
func dashboardCapabilities(p auth.Principal) dashboardCaps {
	return dashboardCaps{
		Chat:             auth.HasScope(p, "chatbot"),
		Search:           auth.HasScope(p, "rag"),
		KnowledgeManage:  auth.HasScope(p, "rag.manage"),
		SourcesManage:    auth.HasScope(p, "connectors.manage"),
		Extract:          auth.HasScope(p, "extract"),
		ExtractManage:    auth.HasScope(p, "extract.manage"),
		AssistantsManage: auth.HasScope(p, "chatbot.manage"),
		ToolsManage:      auth.HasScope(p, auth.ScopePlatformAdmin),
		ModelsManage:     auth.HasScope(p, "pricing.manage"),
		AccessManage:     auth.HasScope(p, auth.ScopePlatformAdmin),
		BillingView:      auth.HasScope(p, auth.ScopePlatformAdmin),
	}
}

type capsContextKey struct{}

// withCaps stashes the request's computed capabilities on the context.
func withCaps(ctx context.Context, caps dashboardCaps) context.Context {
	return context.WithValue(ctx, capsContextKey{}, caps)
}

// capsFromContext reads the capabilities capSession/requireCap stashed;
// ok is false on contexts built without them.
func capsFromContext(ctx context.Context) (dashboardCaps, bool) {
	caps, ok := ctx.Value(capsContextKey{}).(dashboardCaps)
	return caps, ok
}
