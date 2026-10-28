// attention.go — the Home "Needs attention" aggregator (OA-19). Every item
// comes from a real producer read, capability-intersected per kind (H3):
// six manager kinds (failed documents, failed/stalled syncs, unverified
// confidential endpoints, unpriced required units, budgets ≥80%,
// schema-changed tools) plus the member's own failed extraction jobs and
// failed runs, each gated by its own feature capability. An empty producer
// adds nothing — nothing is fabricated.
package httpx

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/connectors"
	"enclava-go/internal/extract"
	"enclava-go/internal/inference"
	"enclava-go/internal/jobs"
)

// AttentionItem is one "Needs attention" row; ActionHref always points at
// the surface where the problem is fixed.
type AttentionItem struct {
	Kind        string `json:"kind"`
	Severity    string `json:"severity"`
	Text        string `json:"text"`
	ActionLabel string `json:"action_label"`
	ActionHref  string `json:"action_href"`
	// Cause is the raw failure text the shared error→next-step map reads —
	// never rendered verbatim (member-visible output stays mapped).
	Cause string `json:"-"`
	// Fix* carry a capability-appropriate next step when the producer has
	// one; the template renders FixText as the subline and FixHref/FixLabel
	// as the fix link (empty Href → text only, the administrator fallback).
	FixText, FixLabel, FixHref string `json:"-"`
	// Live marks a row whose underlying work is still non-terminal (a
	// syncing source); the attention fragment polls while any hold.
	Live bool `json:"-"`
}

const (
	attentionLimit      = 10 // per-kind item cap
	syncSourceFanOut    = 25 // sources scanned per attention render
	syncStalledAfter    = 24 * time.Hour
	unpricedProbeBudget = 64 // PriceCoverage calls per render
)

// dashboardAttention assembles the feed. One broken producer is logged and
// skipped — it never blanks the other kinds or invents a filler item.
func (a *App) dashboardAttention(ctx context.Context, p auth.Principal, caps dashboardCaps) ([]AttentionItem, error) {
	items := []AttentionItem{}
	a.attentionFailedDocuments(ctx, p, caps, &items)
	a.attentionSyncs(ctx, p, caps, &items)
	a.attentionEndpoints(ctx, p, caps, &items)
	a.attentionUnpricedUnits(ctx, p, caps, &items)
	a.attentionBudgets(ctx, p, caps, &items)
	a.attentionToolSchemas(ctx, p, caps, &items)
	a.attentionOwnFailedExtractions(ctx, p, caps, &items)
	a.attentionOwnFailedRuns(ctx, p, caps, &items)
	return items, nil
}

// attentionFailedDocuments: knowledge.manage → the source-authorized service
// read (never the raw repo count): restricted failures preceding an
// accessible one never crowd it out of the page.
func (a *App) attentionFailedDocuments(ctx context.Context, p auth.Principal, caps dashboardCaps, items *[]AttentionItem) {
	if !caps.KnowledgeManage || a.ragSvc == nil {
		return
	}
	docs, err := a.ragSvc.ListFailedDocuments(ctx, p, attentionLimit)
	if err != nil {
		log.Printf("dashboard attention: failed documents: %v", err)
		return
	}
	for _, d := range docs {
		*items = append(*items, AttentionItem{
			Kind:        "failed_document",
			Severity:    "bad",
			Text:        fmt.Sprintf("Document %q failed processing", d.FileName),
			ActionLabel: "Review document",
			ActionHref:  "/dashboard/knowledge/" + d.CollectionID,
		})
	}
}

// attentionSyncs: sources.manage → per managed source's newest sync jobs —
// bounded fan-out over the tenant's sources. Failed (with authentication
// failures called out) and stalled (running for over a day — a real
// timestamp observation, never invented progress).
func (a *App) attentionSyncs(ctx context.Context, p auth.Principal, caps dashboardCaps, items *[]AttentionItem) {
	if !caps.SourcesManage || a.connectorSvc == nil {
		return
	}
	sources, err := a.connectorSvc.ListSources(ctx, p, "", syncSourceFanOut, 0)
	if err != nil {
		log.Printf("dashboard attention: sources: %v", err)
		return
	}
	for _, src := range sources {
		syncs, err := a.connectorSvc.ListSyncJobs(ctx, p, src.ID, "", 5, 0)
		if err != nil {
			log.Printf("dashboard attention: sync jobs for source %s: %v", src.ID, err)
			continue
		}
		if item, ok := syncAttention(src, syncs); ok {
			*items = append(*items, item)
		}
	}
}

// syncAttention classifies one source's newest syncs; one item per source.
func syncAttention(src connectors.Source, syncs []connectors.SyncJob) (AttentionItem, bool) {
	href := "/dashboard/sources/" + src.ID
	for _, j := range syncs {
		switch {
		case j.Status == jobs.StatusFailed:
			why := "failed"
			if syncAuthFailure(j.ErrorMessage) {
				why = "failed authentication"
			}
			text := fmt.Sprintf("Source %q sync %s", src.Name, why)
			if j.ErrorMessage != "" {
				text += ": " + truncateAttention(j.ErrorMessage, 120)
			}
			item := AttentionItem{Kind: "sync_failed", Severity: "bad", Text: text,
				ActionLabel: "Open source", ActionHref: href, Cause: j.ErrorMessage}
			if syncAuthFailure(j.ErrorMessage) {
				// The fix-link row deep-links the edit drawer's credential
				// step; ActionHref keeps the existing open-source target.
				item.FixText, item.FixLabel, item.FixHref =
					"Authentication failed — reconnect the source's credentials.",
					"Reconnect", "/dashboard/sources?edit="+src.ID
			}
			return item, true
		case j.Status == jobs.StatusRunning:
			kind, label := "sync_running", "is syncing"
			if time.Since(j.CreatedAt) > syncStalledAfter {
				kind, label = "sync_stalled", "has been syncing for over a day"
			}
			return AttentionItem{Kind: kind, Severity: "warn",
				Text:        fmt.Sprintf("Source %q %s", src.Name, label),
				ActionLabel: "Open source", ActionHref: href, Live: true}, true
		}
	}
	return AttentionItem{}, false
}

// syncAuthFailure triages the recorded sync error text — display routing
// only, never an authority decision.
func syncAuthFailure(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "401") || strings.Contains(m, "403") ||
		strings.Contains(m, "unauthorized") || strings.Contains(m, "forbidden") ||
		strings.Contains(m, "authentication") || strings.Contains(m, "credential")
}

func truncateAttention(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// attentionEndpoints: models.manage → phase-7's attestation projection. The
// snapshot is already tenant-scoped and configuration-bound (an edited
// endpoint reports unobserved until its next dispatch genuinely re-verifies);
// any observation State != "verified" counts unverified — a failed
// attestation AND an evidence-less confidential_sdk (unobserved) alike.
func (a *App) attentionEndpoints(ctx context.Context, p auth.Principal, caps dashboardCaps, items *[]AttentionItem) {
	if !caps.ModelsManage || a.endpoints == nil {
		return
	}
	eps, err := a.endpoints.List(ctx, p.TenantID)
	if err != nil {
		log.Printf("dashboard attention: endpoints: %v", err)
		return
	}
	var obs []inference.AttestationObservation
	if a.executor != nil {
		obs = a.executor.AttestationSnapshot(p.TenantID, eps)
	}
	byID := make(map[string]inference.AttestationObservation, len(obs))
	for _, o := range obs {
		byID[o.EndpointID] = o
	}
	total, unverified := 0, 0
	for _, e := range eps {
		if !confidentialEndpoint(e) {
			continue
		}
		total++
		state := inference.AttestationUnobserved
		if o, ok := byID[e.ID]; ok {
			state = o.State
		}
		if state != inference.AttestationVerified {
			unverified++
		}
	}
	if unverified == 0 {
		return
	}
	*items = append(*items, AttentionItem{
		Kind:        "unverified_endpoint",
		Severity:    "warn",
		Text:        fmt.Sprintf("%d of %d confidential endpoints are not verified (failed or unobserved)", unverified, total),
		ActionLabel: "Review models",
		ActionHref:  "/dashboard/models",
	})
}

// attentionUnpricedUnits: models.manage → 08-07's capability-aware
// PriceCoverage over each enabled endpoint's advertised capabilities, probed
// for every model alias the endpoint binds (the selector's alias parse). A
// capability PriceCoverage does not know is simply skipped.
func (a *App) attentionUnpricedUnits(ctx context.Context, p auth.Principal, caps dashboardCaps, items *[]AttentionItem) {
	if !caps.ModelsManage || a.executor == nil || a.endpoints == nil {
		return
	}
	eps, err := a.endpoints.List(ctx, p.TenantID)
	if err != nil {
		log.Printf("dashboard attention: endpoints: %v", err)
		return
	}
	now := time.Now().UTC()
	probes := 0
	for _, e := range eps {
		if !e.Enabled {
			continue
		}
		aliases := boundModelAliases(e)
		if len(aliases) == 0 {
			continue // nothing routable — a binding gap, not a pricing gap
		}
		for _, capability := range advertisedPriceableCapabilities(e) {
			for _, alias := range aliases {
				if probes >= unpricedProbeBudget {
					return
				}
				probes++
				missing, err := a.executor.PriceCoverage(ctx, p.TenantID, e.ID, alias, capability, now)
				if err != nil || len(missing) == 0 {
					continue
				}
				kinds := make([]string, 0, len(missing))
				for _, k := range missing {
					kinds = append(kinds, string(k))
				}
				fix := dashboardErrorAction("required price missing: "+strings.Join(kinds, ", "), caps)
				*items = append(*items, AttentionItem{
					Kind:     "unpriced_units",
					Severity: "warn",
					Text: fmt.Sprintf("Endpoint %s cannot price %s for model %q (missing: %s)",
						endpointCardLabel(e), capability, alias, strings.Join(kinds, ", ")),
					ActionLabel: "Set prices",
					ActionHref:  "/dashboard/models/pricing",
					FixText:     fix.Text, FixLabel: fix.Label, FixHref: fix.Href,
				})
				break // one item per endpoint+capability
			}
		}
	}
}

// boundModelAliases extracts the public aliases an endpoint binds — the
// selector-facing twin of inference.upstreamModelFor/modelAliasOf.
func boundModelAliases(e inference.Endpoint) []string {
	var aliases []string
	seen := map[string]bool{}
	for _, c := range e.Capabilities {
		binding, ok := strings.CutPrefix(c, inference.ModelCapabilityPrefix)
		if !ok {
			continue
		}
		alias, _, _ := strings.Cut(binding, "=")
		alias = strings.TrimSpace(alias)
		if alias != "" && !seen[alias] {
			seen[alias] = true
			aliases = append(aliases, alias)
		}
	}
	return aliases
}

// advertisedPriceableCapabilities lists the endpoint's non-binding
// capabilities; PriceCoverage rejects the unpriced-unit-free ones itself.
func advertisedPriceableCapabilities(e inference.Endpoint) []string {
	var caps []string
	for _, c := range e.Capabilities {
		if !strings.HasPrefix(c, inference.ModelCapabilityPrefix) {
			caps = append(caps, c)
		}
	}
	return caps
}

// attentionBudgets: billing.view → the admission-matching progress scalar
// (never recomputed from liability sums), flagged at ≥80% in exact integer
// math.
func (a *App) attentionBudgets(ctx context.Context, p auth.Principal, caps dashboardCaps, items *[]AttentionItem) {
	if !caps.BillingView || a.billingRepo == nil {
		return
	}
	now := time.Now().UTC()
	budgets, err := a.billingRepo.ListAllActiveBudgets(ctx, p.TenantID, now)
	if err != nil {
		log.Printf("dashboard attention: budgets: %v", err)
		return
	}
	for _, b := range budgets {
		progress, _, err := a.budgetProgress(ctx, p.TenantID, b, now)
		if err != nil {
			log.Printf("dashboard attention: budget progress %s: %v", b.ID, err)
			continue
		}
		if !atLeast80Percent(progress.Counted, progress.LimitValue) {
			continue
		}
		*items = append(*items, AttentionItem{
			Kind:        "budget",
			Severity:    "warn",
			Text:        fmt.Sprintf("Budget %q is at %d%% of its %s limit", b.Name, attentionPercent(progress.Counted, progress.LimitValue), b.Period),
			ActionLabel: "Review budgets",
			ActionHref:  "/dashboard/usage/budgets",
		})
	}
}

// atLeast80Percent is counted/limit ≥ 4/5 in exact integer math without
// overflow: counts ≥ limit are trivially over; the multiply path only runs
// under a limit small enough to keep the products in range.
func atLeast80Percent(counted, limit int64) bool {
	if limit <= 0 {
		return false
	}
	if counted >= limit {
		return true
	}
	if limit > math.MaxInt64/5 {
		return counted > (limit/5)*4
	}
	return counted*5 >= limit*4
}

func attentionPercent(counted, limit int64) int64 {
	if limit <= 0 {
		return 0
	}
	return counted * 100 / limit
}

// attentionToolSchemas: tools.manage → durable discovery states. Changed
// rows mean an approved tool's schema moved (review before use); pending
// rows are brand-new tools awaiting first approval.
func (a *App) attentionToolSchemas(ctx context.Context, p auth.Principal, caps dashboardCaps, items *[]AttentionItem) {
	if !caps.ToolsManage || a.mcpRepo == nil {
		return
	}
	rows, err := a.mcpRepo.ListToolsByState(ctx, p.TenantID, "pending", "changed")
	if err != nil {
		log.Printf("dashboard attention: tool discoveries: %v", err)
		return
	}
	pending, changed := 0, 0
	for _, r := range rows {
		switch r.State {
		case "pending":
			pending++
		case "changed":
			changed++
		}
	}
	if changed > 0 {
		*items = append(*items, AttentionItem{
			Kind:        "tool_schema_changed",
			Severity:    "warn",
			Text:        fmt.Sprintf("%d approved tool(s) changed on their server — review before further use", changed),
			ActionLabel: "Review tools",
			ActionHref:  "/dashboard/tools",
		})
	}
	if pending > 0 {
		*items = append(*items, AttentionItem{
			Kind:        "tool_schema_pending",
			Severity:    "warn",
			Text:        fmt.Sprintf("%d newly discovered tool(s) awaiting review", pending),
			ActionLabel: "Review tools",
			ActionHref:  "/dashboard/tools",
		})
	}
}

// attentionOwnFailedExtractions: caps.Extract → the caller's OWN failed
// extraction jobs, actor-scoped with the status filter in SQL before LIMIT —
// an old failure behind a page of newer successes still surfaces.
func (a *App) attentionOwnFailedExtractions(ctx context.Context, p auth.Principal, caps dashboardCaps, items *[]AttentionItem) {
	if !caps.Extract || a.jobsSvc == nil {
		return
	}
	failed, err := a.jobsSvc.ListActorActivity(ctx, p, []string{jobs.StatusFailed},
		[]string{extract.OperationProcess, extract.OperationVision}, attentionLimit)
	if err != nil {
		log.Printf("dashboard attention: failed extraction jobs: %v", err)
		return
	}
	for _, job := range failed {
		text := "Your extraction failed"
		if job.Error != "" {
			text += ": " + truncateAttention(job.Error, 120)
		}
		fix := dashboardErrorAction(job.Error, caps)
		*items = append(*items, AttentionItem{
			Kind:        "failed_extraction",
			Severity:    "bad",
			Text:        text,
			ActionLabel: "Open Extract",
			ActionHref:  "/dashboard/extract",
			Cause:       job.Error,
			FixText:     fix.Text, FixLabel: fix.Label, FixHref: fix.Href,
		})
	}
}

// attentionOwnFailedRuns: caps.Chat → the owner-scoped failed-run read
// (08-06 service wrapper; admins included, no tenant-wide variant).
func (a *App) attentionOwnFailedRuns(ctx context.Context, p auth.Principal, caps dashboardCaps, items *[]AttentionItem) {
	if !caps.Chat || a.chatbotSvc == nil {
		return
	}
	runs, err := a.chatbotSvc.ListFailedRuns(ctx, p, attentionLimit)
	if err != nil {
		log.Printf("dashboard attention: failed runs: %v", err)
		return
	}
	for _, run := range runs {
		cause := run.Error
		if cause == "" {
			cause = run.Input
		}
		text := "Your assistant run failed"
		if cause != "" {
			text += ": " + truncateAttention(cause, 120)
		}
		fix := dashboardErrorAction(run.Error, caps)
		*items = append(*items, AttentionItem{
			Kind:        "failed_run",
			Severity:    "bad",
			Text:        text,
			ActionLabel: "Open chat",
			ActionHref:  "/dashboard/chat/" + run.ConversationID,
			Cause:       run.Error,
			FixText:     fix.Text, FixLabel: fix.Label, FixHref: fix.Href,
		})
	}
}
