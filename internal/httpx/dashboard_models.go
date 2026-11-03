// dashboard_models.go — Models + Pricing (OA-27, source §6.10): the admin
// endpoints table (assurance/health rendered from observed state only) and
// the pricing matrix with a Set-price flow that actually submits — the old
// pricing.js was never served. Handlers render; every write goes through
// Enclava.api → /api/v1 (D-17).
//
// 09-10 handoff (old-file deletion map): this page replaces
// handleDashboardPageProviders, handleDashboardPagePricing,
// handleDashboardProviders, handleDashboardPricing and their routes
// (/dashboard/providers, /dashboard/pricing, /dashboard/partials/providers,
// /dashboard/partials/pricing) plus partial_providers.tmpl,
// partial_pricing.tmpl, providers.js and pricing.js — all deleted by 09-10;
// dashboard_resources.go is NOT edited here.
package httpx

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/inference"
)

func init() { registerDashboardPage((*App).registerDashboardModelsRoutes) }

// modelsUnitKinds is the matrix column order (Input / Cached input /
// Output); which of them a row requires is inference.RequiredUnits of the
// row's capability.
func modelsUnitKinds() []billing.UnitKind {
	return []billing.UnitKind{billing.UnitInput, billing.UnitCachedInput, billing.UnitOutput}
}

// modelsData is the page view model for both tabs; it implements
// dashboardPageMeta so the layout serves static/pages/models.js — the old
// page's pricing.js was never emitted, which is the bug class D-22 fixes.
type modelsData struct {
	Tab       string // "endpoints" | "pricing"
	Endpoints []modelsEndpointRow
	Rows      []modelsPriceRow
	Empty     dashboardEmpty // zero-row state for the active tab (OA-37)
	Error     string
}

func (m modelsData) DashboardTitle() string  { return "Models" }
func (m modelsData) DashboardModule() string { return "models" }

// modelsEndpointRow is one endpoints-table row. Assurance and health are
// observed projections only — never inferred from protocol or provider type.
type modelsEndpointRow struct {
	ID               string
	Protocol         string
	Origin           string
	Enabled          bool
	Caps             []string // plain capabilities (model bindings render as Models)
	Models           []string // "alias" or "alias → upstream"
	AssuranceState   string   // verified | none | unverified
	AssuranceChecked *time.Time
	AssuranceNote    string
	Runtime          string // ready | degraded | unreachable | unobserved | disabled
	Latency          string // "42 ms" only when observed
	RuntimeNote      string // observed LastError, never a guess
	Share            string // "34%" only when endpoint usage data exists
}

// modelsPriceRow is one matrix row: alias @ endpoint priced for one
// capability the endpoint advertises, through the shared coverage rule
// (endpoint set when complete, else the whole model set — never a per-unit
// merge).
type modelsPriceRow struct {
	EndpointID      string
	Model           string // public alias
	Upstream        string // resolved upstream model id
	Capability      string
	CapabilityLabel string
	Basis           string // endpoint | model — which price set every cell shows
	SubjectID       string // the Basis set's subject; every cell write lands there
	Unpriced        bool   // a required unit is missing — reserve would refuse
	Cells           []modelsPriceCell
}

// modelsPriceCell is one Input/Cached/Output cell. The rate data-* pairs
// prefill the shared Set-price form; the write target is the row's Basis
// price set (the coverage rule is whole-set, never a per-unit merge).
type modelsPriceCell struct {
	Unit         string
	UnitLabel    string
	Required     bool
	Priced       bool
	RateMicros   int64
	UnitQuantity int64
	Currency     string
}

type modelsDrawerData struct {
	Editing          bool
	Endpoint         inference.Endpoint
	Checks           []modelsCapabilityCheck
	Aliases          string
	AssuranceState   string
	AssuranceChecked *time.Time
	AssuranceNote    string
	Missing          bool // lookup failed — the drawer renders its empty state, not a bare 404
	Empty            dashboardEmpty
}

type modelsCapabilityCheck struct {
	Value   string
	Label   string
	Checked bool
}

// modelsCapabilityChoices is the fixed checklist; model aliases are edited
// through their own labeled field (the "model:<alias>[=<upstream>]" wire
// syntax stays out of the form).
var modelsCapabilityChoices = []struct{ Value, Label string }{
	{inference.CapabilityChat, "Chat"},
	{inference.CapabilityEmbeddings, "Embeddings"},
	{inference.CapabilityVision, "Vision"},
	{inference.CapabilityResponses, "Responses"},
	{inference.CapabilityCompact, "Responses (compact)"},
	{inference.CapabilityMCP, "MCP tools"},
}

type modelsHistoryData struct {
	EndpointID string
	Model      string
	Capability string
	Rows       []modelsHistoryRow
	Empty      dashboardEmpty
}

type modelsHistoryRow struct {
	Subject       string // endpoint | model
	SubjectID     string
	UnitLabel     string
	RateMicros    int64
	UnitQuantity  int64
	Currency      string
	EffectiveFrom time.Time
}

// registerDashboardModelsRoutes mounts the page, both tabs, the two tab
// fragments and the two drawers — every route under models.manage.
func (a *App) registerDashboardModelsRoutes() {
	a.mux.HandleFunc("/dashboard/models", a.sessionAuth.requireCap(capModelsManage, a.modelsGetOnly(a.handleDashboardModelsPage)))
	a.mux.HandleFunc("/dashboard/models/pricing", a.sessionAuth.requireCap(capModelsManage, a.modelsGetOnly(a.handleDashboardModelsPricingPage)))
	a.mux.HandleFunc("/dashboard/partials/models/endpoints", a.sessionAuth.requireCap(capModelsManage, a.modelsGetOnly(a.handleDashboardModelsEndpointsPartial)))
	a.mux.HandleFunc("/dashboard/partials/models/pricing", a.sessionAuth.requireCap(capModelsManage, a.modelsGetOnly(a.handleDashboardModelsPricingPartial)))
	a.mux.HandleFunc("/dashboard/partials/models/endpoint-drawer", a.sessionAuth.requireCap(capModelsManage, a.modelsGetOnly(a.handleDashboardModelsEndpointDrawer)))
	a.mux.HandleFunc("/dashboard/partials/models/price-history", a.sessionAuth.requireCap(capModelsManage, a.modelsGetOnly(a.handleDashboardModelsPriceHistory)))
}

// modelsGetOnly keeps every route read-only: dashboard handlers render (D-17)
// and all writes are API calls, so non-GET is a bare 405.
func (a *App) modelsGetOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			WriteMethodNotAllowed(w)
			return
		}
		next(w, r)
	}
}

func (a *App) handleDashboardModelsPage(w http.ResponseWriter, r *http.Request) {
	a.renderDashboard(w, r, "models", a.modelsEndpointsData(r))
}

func (a *App) handleDashboardModelsPricingPage(w http.ResponseWriter, r *http.Request) {
	a.renderDashboard(w, r, "models", a.modelsPricingData(r))
}

func (a *App) handleDashboardModelsEndpointsPartial(w http.ResponseWriter, r *http.Request) {
	a.renderDashboardFragment(w, r, "models", "models-endpoints", a.modelsEndpointsData(r))
}

func (a *App) handleDashboardModelsPricingPartial(w http.ResponseWriter, r *http.Request) {
	a.renderDashboardFragment(w, r, "models", "models-pricing", a.modelsPricingData(r))
}

func (a *App) handleDashboardModelsEndpointDrawer(w http.ResponseWriter, r *http.Request) {
	data, ok := a.modelsDrawerData(r)
	if !ok {
		// A stale ?id= link gets the drawer's own empty state — the same
		// sentence-only pattern a deleted row renders, never a bare 404.
		data = modelsDrawerData{
			Missing: true,
			Empty:   dashboardEmpty{Icon: "models", Text: "That endpoint is no longer available."},
		}
	}
	a.renderDashboardFragment(w, r, "models", "models-endpoint-drawer", data)
}

func (a *App) handleDashboardModelsPriceHistory(w http.ResponseWriter, r *http.Request) {
	data, ok := a.modelsPriceHistoryData(r)
	if !ok {
		WriteOpenAIError(w, http.StatusBadRequest, "endpoint is required", "invalid_request_error", "invalid_request")
		return
	}
	a.renderDashboardFragment(w, r, "models", "models-price-history", data)
}

// modelsAttestations projects the observed attestation snapshot onto the
// tenant's endpoint rows; nil executor (or any error) leaves every endpoint
// unobserved — the truthful default.
func (a *App) modelsAttestations(ctx context.Context, tenantID string, eps []inference.Endpoint) map[string]inference.AttestationObservation {
	if a.executor == nil {
		return nil
	}
	out := make(map[string]inference.AttestationObservation, len(eps))
	for _, o := range a.executor.AttestationSnapshot(tenantID, eps) {
		out[o.EndpointID] = o
	}
	return out
}

// modelsAssurance maps an endpoint's observed attestation + configured
// assurance mode to the cell's three truthful outcomes. Verification is
// never inferred: no observation → verified is impossible, and a missing
// assurance mode reads "Not confidential" regardless of protocol.
func modelsAssurance(e inference.Endpoint, o inference.AttestationObservation) (state string, checked *time.Time, note string) {
	if o.State == inference.AttestationVerified {
		return "verified", laterStamp(o.VerifiedAt, e.AssuranceVerifiedAt), ""
	}
	if e.AssuranceMode == "" {
		return "none", nil, ""
	}
	return "unverified", nil, o.LastError
}

// modelsEndpointRuntime joins the observed dispatch state to one row. An
// absent projection (or the runtime's no-observation unknown) reports
// "unobserved" — never healthy, and never the lost-contact label, which
// belongs to observed work whose worker disappeared.
func modelsEndpointRuntime(rt inference.EndpointRuntime, row *modelsEndpointRow) {
	row.Runtime = rt.State
	if rt.State == "" || rt.State == inference.RuntimeStateUnknown {
		row.Runtime = "unobserved"
	}
	if rt.LatencyP50Millis != nil {
		row.Latency = fmt.Sprintf("%d ms", *rt.LatencyP50Millis)
	}
	row.RuntimeNote = rt.LastError
}

func (a *App) modelsEndpointsData(r *http.Request) modelsData {
	data := modelsData{Tab: "endpoints"}
	p, _ := auth.PrincipalFromContext(r.Context())
	caps, _ := capsFromContext(r.Context())
	data.Empty = modelsEndpointsEmpty(caps.ModelsManage)
	if a.endpoints == nil {
		data.Error = "Endpoints are not configured on this deployment"
		return data
	}
	eps, err := a.endpoints.List(r.Context(), p.TenantID)
	if err != nil {
		log.Printf("models page: list endpoints: %v", err)
		data.Error = "Endpoint list failed"
		return data
	}
	runs := map[string]inference.EndpointRuntime{}
	if a.executor != nil {
		rts, err := a.executor.EndpointRuntime(r.Context(), p.TenantID)
		if err != nil {
			log.Printf("models page: endpoint runtime: %v", err)
		}
		for _, rt := range rts {
			runs[rt.EndpointID] = rt
		}
	}
	att := a.modelsAttestations(r.Context(), p.TenantID, eps)
	shares := a.modelsTrafficShares(r, p.TenantID)
	for _, e := range eps {
		row := modelsEndpointRow{
			ID:       e.ID,
			Protocol: e.Protocol,
			Origin:   e.BaseOrigin,
			Enabled:  e.Enabled,
			Share:    shares[e.ID],
		}
		for _, c := range e.Capabilities {
			if _, alias, upstream, isModel := modelsBindingParts(c); isModel {
				row.Models = append(row.Models, modelsModelDisplay(alias, upstream))
				continue
			}
			row.Caps = append(row.Caps, modelsCapabilityLabel(c))
		}
		row.AssuranceState, row.AssuranceChecked, row.AssuranceNote = modelsAssurance(e, att[e.ID])
		modelsEndpointRuntime(runs[e.ID], &row)
		data.Endpoints = append(data.Endpoints, row)
	}
	return data
}

// modelsTrafficShares renders the traffic column only where settled usage
// exists (30d window): a percentage of real counts, otherwise no cell value.
func (a *App) modelsTrafficShares(r *http.Request, tenantID string) map[string]string {
	shares := map[string]string{}
	if a.billingRepo == nil {
		return shares
	}
	sums, err := a.billingRepo.UsageByEndpoint(r.Context(), tenantID, time.Now().UTC().Add(-30*24*time.Hour))
	if err != nil {
		log.Printf("models page: usage by endpoint: %v", err)
		return shares
	}
	counts := map[string]int64{}
	var total int64
	for _, s := range sums {
		if s.Key == "" {
			continue
		}
		counts[s.Key] += s.RequestCount
		total += s.RequestCount
	}
	if total == 0 {
		return shares
	}
	for id, c := range counts {
		shares[id] = fmt.Sprintf("%d%%", (c*100+total/2)/total)
	}
	return shares
}

// modelsBindingParts splits a capability entry: ("", alias, upstream, true)
// for "model:<alias>[=<upstream>]" bindings, or (cap, "", "", false) for
// plain capabilities.
func modelsBindingParts(entry string) (capability, alias, upstream string, isModel bool) {
	binding, ok := strings.CutPrefix(entry, inference.ModelCapabilityPrefix)
	if !ok {
		return entry, "", "", false
	}
	alias, upstream, _ = strings.Cut(binding, "=")
	alias = strings.TrimSpace(alias)
	upstream = strings.TrimSpace(upstream)
	if alias == "" {
		return entry, "", "", false
	}
	if upstream == "" {
		upstream = alias
	}
	return "", alias, upstream, true
}

func modelsModelDisplay(alias, upstream string) string {
	if upstream == "" || upstream == alias {
		return alias
	}
	return alias + " → " + upstream
}

// modelsEndpointsEmpty / modelsPricingEmpty name each tab's zero-row state.
// The action is filled only when the caller can manage models — the
// label/url pair always travels with the capability check (T-11-03-03).
func modelsEndpointsEmpty(canManage bool) dashboardEmpty {
	e := dashboardEmpty{
		Icon: "models",
		Text: "No endpoints yet — add one to route models.",
	}
	if canManage {
		e.ActionDrawerLabel = "Add endpoint"
		e.ActionDrawerURL = "/dashboard/partials/models/endpoint-drawer"
	}
	return e
}

func modelsPricingEmpty(canManage bool) dashboardEmpty {
	e := dashboardEmpty{
		Icon: "billing",
		Text: "No model routes to price — bind a model alias on an endpoint first.",
	}
	if canManage {
		e.ActionDrawerLabel = "Add endpoint"
		e.ActionDrawerURL = "/dashboard/partials/models/endpoint-drawer"
	}
	return e
}

// modelsCapabilityLabel renders capability wire values in the drawer's own
// plain words; unknown entries humanize instead of leaking raw strings.
func modelsCapabilityLabel(value string) string {
	for _, c := range modelsCapabilityChoices {
		if c.Value == value {
			return c.Label
		}
	}
	return dashboardHumanize(value)
}

// unitLabel renders unit kinds in plain words; the shared vocabulary has no
// unit rows, so the matrix labels are fixed here once.
func unitLabel(k billing.UnitKind) string {
	switch k {
	case billing.UnitInput:
		return "Input"
	case billing.UnitCachedInput:
		return "Cached input"
	case billing.UnitOutput:
		return "Output"
	default:
		return dashboardHumanize(string(k))
	}
}

func (a *App) modelsPricingData(r *http.Request) modelsData {
	data := modelsData{Tab: "pricing"}
	p, _ := auth.PrincipalFromContext(r.Context())
	caps, _ := capsFromContext(r.Context())
	data.Empty = modelsPricingEmpty(caps.ModelsManage)
	switch {
	case a.endpoints == nil:
		data.Error = "Endpoints are not configured on this deployment"
		return data
	case a.billingRepo == nil:
		data.Error = "Pricing is not configured on this deployment"
		return data
	}
	eps, err := a.endpoints.List(r.Context(), p.TenantID)
	if err != nil {
		log.Printf("models page: list endpoints: %v", err)
		data.Error = "Endpoint list failed"
		return data
	}
	now := time.Now().UTC()
	for _, e := range eps {
		for _, c := range e.Capabilities {
			_, alias, upstream, isModel := modelsBindingParts(c)
			if !isModel {
				continue
			}
			for _, capability := range advertisedPriceableCapabilities(e) {
				if len(inference.RequiredUnits(capability)) == 0 {
					continue // never priced (MCP): nothing to set
				}
				data.Rows = append(data.Rows, a.modelsPriceRow(r.Context(), p.TenantID, e.ID, alias, upstream, capability, now))
			}
		}
	}
	return data
}

// modelsPriceRow prices one (model, endpoint) pair for one capability through
// the shared coverage rule: the endpoint price set when it covers every
// required unit, otherwise the whole model set (the fallback Reserve
// applies; the sets are never merged). Missing units — the reserve path
// would refuse the request — render "Not priced" with an inline Set price
// control.
func (a *App) modelsPriceRow(ctx context.Context, tenantID, endpointID, alias, upstream, capability string, now time.Time) modelsPriceRow {
	row := modelsPriceRow{
		EndpointID:      endpointID,
		Model:           alias,
		Upstream:        upstream,
		Capability:      capability,
		CapabilityLabel: modelsCapabilityLabel(capability),
	}
	endpointRates, err := a.billingRepo.LatestPrices(ctx, tenantID, billing.PriceSubjectEndpoint, endpointID, row.Capability, now)
	if err != nil {
		log.Printf("models page: latest endpoint prices: %v", err)
	}
	modelRates, err := a.billingRepo.LatestPrices(ctx, tenantID, billing.PriceSubjectModel, upstream, row.Capability, now)
	if err != nil {
		log.Printf("models page: latest model prices: %v", err)
	}
	kinds := inference.RequiredUnits(capability)
	useEndpoint := missingPriceKinds(endpointRates, kinds) == nil
	var missing []billing.UnitKind
	if a.executor != nil {
		// The shared coverage helper is the authority on what dispatch
		// would refuse; the display selection follows its whole-set rule.
		if m, err := a.executor.PriceCoverage(ctx, tenantID, endpointID, alias, row.Capability, now); err == nil {
			missing = m
			useEndpoint = len(m) == 0 && missingPriceKinds(endpointRates, kinds) == nil
		}
	}
	effective := modelRates
	basis, subjectID := billing.PriceSubjectModel, upstream
	if useEndpoint {
		effective, basis, subjectID = endpointRates, billing.PriceSubjectEndpoint, endpointID
		if missing == nil {
			missing = missingPriceKinds(endpointRates, kinds)
		}
	} else if a.executor == nil || missing == nil {
		missing = missingPriceKinds(modelRates, kinds)
	}
	rateFor := func(k billing.UnitKind) (billing.Price, bool) {
		for _, r := range effective {
			if r.Unit == k {
				return r, true
			}
		}
		return billing.Price{}, false
	}
	for _, k := range modelsUnitKinds() {
		cell := modelsPriceCell{Unit: string(k), UnitLabel: unitLabel(k), Required: slices.Contains(kinds, k)}
		if rate, ok := rateFor(k); ok {
			cell.Priced = true
			cell.RateMicros = rate.RateMicros
			cell.UnitQuantity = rate.UnitQuantity
			cell.Currency = rate.Currency
		}
		row.Cells = append(row.Cells, cell)
	}
	row.Basis, row.SubjectID = basis, subjectID
	row.Unpriced = len(missing) > 0
	return row
}

// missingPriceKinds reports the required kinds absent from one price set —
// the display twin of inference.missingPriceUnits.
func missingPriceKinds(prices []billing.Price, kinds []billing.UnitKind) []billing.UnitKind {
	var missing []billing.UnitKind
	for _, k := range kinds {
		found := false
		for _, p := range prices {
			if p.Unit == k {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, k)
		}
	}
	return missing
}

func (a *App) modelsDrawerData(r *http.Request) (modelsDrawerData, bool) {
	data := modelsDrawerData{}
	p, _ := auth.PrincipalFromContext(r.Context())
	if a.endpoints == nil {
		return data, false
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		for _, c := range modelsCapabilityChoices {
			data.Checks = append(data.Checks, modelsCapabilityCheck{Value: c.Value, Label: c.Label})
		}
		return data, true
	}
	e, err := a.endpoints.Get(r.Context(), p.TenantID, id)
	if err != nil {
		log.Printf("models drawer: get endpoint: %v", err)
		return data, false
	}
	data.Editing = true
	data.Endpoint = e
	checked := map[string]bool{}
	var aliases []string
	for _, c := range e.Capabilities {
		if _, alias, upstream, isModel := modelsBindingParts(c); isModel {
			if upstream != alias {
				alias = alias + "=" + upstream
			}
			aliases = append(aliases, alias)
			continue
		}
		checked[c] = true
	}
	data.Aliases = strings.Join(aliases, ", ")
	for _, c := range modelsCapabilityChoices {
		data.Checks = append(data.Checks, modelsCapabilityCheck{Value: c.Value, Label: c.Label, Checked: checked[c.Value]})
	}
	var obs inference.AttestationObservation
	if a.executor != nil {
		for _, o := range a.executor.AttestationSnapshot(p.TenantID, []inference.Endpoint{e}) {
			if o.EndpointID == e.ID {
				obs = o
			}
		}
	}
	data.AssuranceState, data.AssuranceChecked, data.AssuranceNote = modelsAssurance(e, obs)
	return data, true
}

func (a *App) modelsPriceHistoryData(r *http.Request) (modelsHistoryData, bool) {
	p, _ := auth.PrincipalFromContext(r.Context())
	if a.billingRepo == nil {
		return modelsHistoryData{}, false
	}
	data := modelsHistoryData{
		EndpointID: strings.TrimSpace(r.URL.Query().Get("endpoint")),
		Model:      strings.TrimSpace(r.URL.Query().Get("model")),
		Capability: strings.TrimSpace(r.URL.Query().Get("capability")),
		Empty:      dashboardEmpty{Icon: "billing", Text: "No price history for this model yet."},
	}
	if data.Capability == "" {
		data.Capability = inference.CapabilityChat
	}
	if data.EndpointID == "" {
		return modelsHistoryData{}, false
	}
	endpointRows, err := a.billingRepo.ListPrices(r.Context(), p.TenantID, billing.PriceSubjectEndpoint, data.EndpointID, data.Capability, 200, 0)
	if err != nil {
		log.Printf("models history: list endpoint prices: %v", err)
	}
	for _, row := range endpointRows {
		data.Rows = append(data.Rows, modelsHistoryRow{
			Subject:       billing.PriceSubjectEndpoint,
			SubjectID:     data.EndpointID,
			UnitLabel:     unitLabel(row.Unit),
			RateMicros:    row.RateMicros,
			UnitQuantity:  row.UnitQuantity,
			Currency:      row.Currency,
			EffectiveFrom: row.EffectiveFrom,
		})
	}
	// The model half of the pair needs the persisted upstream binding — the
	// alias alone is not a price subject.
	if data.Model != "" && a.endpoints != nil {
		if e, err := a.endpoints.Get(r.Context(), p.TenantID, data.EndpointID); err == nil {
			upstream := modelsUpstreamFor(e, data.Model)
			if upstream == "" {
				upstream = data.Model
			}
			modelRows, err := a.billingRepo.ListPrices(r.Context(), p.TenantID, billing.PriceSubjectModel, upstream, data.Capability, 200, 0)
			if err != nil {
				log.Printf("models history: list model prices: %v", err)
			}
			for _, row := range modelRows {
				data.Rows = append(data.Rows, modelsHistoryRow{
					Subject:       billing.PriceSubjectModel,
					SubjectID:     upstream,
					UnitLabel:     unitLabel(row.Unit),
					RateMicros:    row.RateMicros,
					UnitQuantity:  row.UnitQuantity,
					Currency:      row.Currency,
					EffectiveFrom: row.EffectiveFrom,
				})
			}
		}
	}
	sort.SliceStable(data.Rows, func(i, j int) bool {
		return data.Rows[i].EffectiveFrom.After(data.Rows[j].EffectiveFrom)
	})
	return data, true
}

// modelsUpstreamFor resolves an alias against an endpoint's bindings — the
// display-side read of the "model:<alias>[=<upstream>]" wire syntax.
func modelsUpstreamFor(e inference.Endpoint, alias string) string {
	for _, c := range e.Capabilities {
		if _, a, upstream, isModel := modelsBindingParts(c); isModel && a == alias {
			return upstream
		}
	}
	return ""
}
