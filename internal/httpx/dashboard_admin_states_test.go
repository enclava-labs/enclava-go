package httpx

import (
	"strings"
	"testing"

	"enclava-go/internal/connectors"
	"enclava-go/internal/rag"
)

func TestAdminSourcesPollingAndReconnect(t *testing.T) {
	set, err := dashboardTemplateSet("sources")
	if err != nil {
		t.Fatal(err)
	}
	render := func(caps dashboardCaps, data dashboardSourcesData) string {
		t.Helper()
		var out strings.Builder
		if err := set.ExecuteTemplate(&out, "frag-sources-table", dashboardView{Can: caps, Data: data}); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	row := dashboardSourceRow{Source: connectors.Source{ID: "source-1", Name: "Example"}, State: "syncing"}
	live := render(dashboardCaps{}, dashboardSourcesData{Rows: []dashboardSourceRow{row}, AnySyncing: true})
	if !strings.Contains(live, "every 3s") || strings.Contains(live, `role="progressbar"`) {
		t.Fatal("uncounted live sync must poll without a progress bar")
	}
	row.HasProgress, row.ProgressPct = dashboardStatPercent(&connectors.SyncJob{Stats: map[string]any{"processed": 2.0, "total": 5.0}})
	counted := render(dashboardCaps{}, dashboardSourcesData{Rows: []dashboardSourceRow{row}, AnySyncing: true})
	if !strings.Contains(counted, `role="progressbar"`) || !strings.Contains(counted, `aria-valuenow="40"`) {
		t.Fatal("reported counts must render their real ratio")
	}
	row.State, row.LastError = "warning", "authorization failed"
	data := dashboardSourcesData{Rows: []dashboardSourceRow{row}}
	member := render(dashboardCaps{}, data)
	manager := render(dashboardCaps{SourcesManage: true}, data)
	if strings.Contains(member, "every 3s") || strings.Contains(member, "/dashboard/sources?edit=") {
		t.Fatal("terminal row must stop polling and withhold manager link")
	}
	if !strings.Contains(manager, "/dashboard/sources?edit=source-1") {
		t.Fatal("manager reconnect link missing")
	}
}

func TestAdminSyncProgressCounts(t *testing.T) {
	if ok, _ := dashboardStatPercent(&connectors.SyncJob{Stats: map[string]any{"percent": 42.0}}); ok {
		t.Fatal("percentage alone must not create count-backed progress")
	}
	if ok, pct := dashboardStatPercent(&connectors.SyncJob{Stats: map[string]any{"processed": 2.0, "total": 5.0}}); !ok || pct != 40 {
		t.Fatalf("count-backed progress = %v, %d", ok, pct)
	}
}

func TestAdminSourceEditDrawer(t *testing.T) {
	set, err := dashboardTemplateSet("sources")
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	data := dashboardSourcesDrawerData{EditSource: &connectors.Source{ID: "source-1", Type: "slack", Name: "Example"}}
	if err := set.ExecuteTemplate(&out, "frag-sources-drawer", dashboardView{Data: data}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `data-source-edit="source-1"`) || !strings.Contains(out.String(), `id="source-edit-credential"`) {
		t.Fatal("edit drawer must expose the source credential control")
	}
}

func TestAdminBudgetEmptyCapability(t *testing.T) {
	set, err := dashboardTemplateSet("usage")
	if err != nil {
		t.Fatal(err)
	}
	for _, capable := range []bool{false, true} {
		caps := dashboardCaps{BillingView: capable}
		var out strings.Builder
		if err := set.ExecuteTemplate(&out, "frag-usage-budgets", dashboardView{Can: caps, Data: usageBudgetsView("", nil, caps)}); err != nil {
			t.Fatal(err)
		}
		hasAction := strings.Contains(out.String(), `data-drawer-url="/dashboard/partials/usage/budget-drawer"`)
		if hasAction != capable {
			t.Fatalf("budget action present=%v for capability=%v", hasAction, capable)
		}
		if strings.Contains(out.String(), "every 3s") {
			t.Fatal("budget list must not poll")
		}
	}
}

func TestAdminBudgetDrawerEmptyPickers(t *testing.T) {
	set, err := dashboardTemplateSet("usage")
	if err != nil {
		t.Fatal(err)
	}
	marker := dashboardEmpty{Icon: "users", Text: "empty fixture"}
	view := usageView{Drawer: usageBudgetDrawer{KeysEmpty: marker, UsersEmpty: marker, ServicesEmpty: marker}}
	var out strings.Builder
	if err := set.ExecuteTemplate(&out, "frag-usage-budget-drawer", dashboardView{Data: view}); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "empty fixture") != 3 {
		t.Fatal("each empty subject picker must render its empty state")
	}
}

func TestAdminToolRegisterEmptyScopes(t *testing.T) {
	set, err := dashboardTemplateSet("tools")
	if err != nil {
		t.Fatal(err)
	}
	data := struct {
		Server *mcpServerResponse
		Scopes []toolsScopeChoice
		Empty  dashboardEmpty
	}{Empty: dashboardEmpty{Icon: "tools", Text: "empty fixture"}}
	var out strings.Builder
	if err := set.ExecuteTemplate(&out, "frag-tools-register", dashboardView{Data: data}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "empty fixture") {
		t.Fatal("empty scope picker must render its empty state")
	}
}

func TestAdminModelLeafEmptyStates(t *testing.T) {
	set, err := dashboardTemplateSet("models")
	if err != nil {
		t.Fatal(err)
	}
	marker := dashboardEmpty{Icon: "billing", Text: "empty fixture"}
	fixtures := []struct {
		fragment string
		data     any
	}{
		{"frag-models-price-history", modelsHistoryData{Empty: marker}},
		{"frag-models-endpoint-drawer", modelsDrawerData{Missing: true, Empty: marker}},
	}
	for _, fixture := range fixtures {
		var out strings.Builder
		if err := set.ExecuteTemplate(&out, fixture.fragment, dashboardView{Data: fixture.data}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "empty fixture") {
			t.Fatalf("%s did not render the empty state", fixture.fragment)
		}
	}
}

func TestAdminRoleEditorEmptyGrant(t *testing.T) {
	set, err := dashboardTemplateSet("access")
	if err != nil {
		t.Fatal(err)
	}
	data := accessView{RoleEditor: &accessRoleEditor{Grants: []accessGrantSection{{
		Kind: "models", Label: "Models", Empty: dashboardEmpty{Icon: "models", Text: "empty fixture"},
	}}}}
	var out strings.Builder
	if err := set.ExecuteTemplate(&out, "frag-access-role-editor", dashboardView{Data: data}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "empty fixture") || strings.Contains(out.String(), `data-grant-item`) || strings.Contains(out.String(), `data-grant-all`) {
		t.Fatal("empty grant section must not imply or offer ungrantable resources")
	}
}

func TestAdminDocumentsPollingAndPriceAction(t *testing.T) {
	set, err := dashboardTemplateSet("knowledge_collection")
	if err != nil {
		t.Fatal(err)
	}
	render := func(status, message string, caps dashboardCaps) string {
		t.Helper()
		docs := []rag.Document{{FileName: "Example", Status: status, ErrorMessage: message}}
		view := knowledgeCollectionData{Docs: docs, AnyProcessing: knowledgeAnyProcessing(docs)}
		var out strings.Builder
		if err := set.ExecuteTemplate(&out, "frag-knowledge-documents", dashboardView{Can: caps, Data: view}); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	if !strings.Contains(render("processing", "", dashboardCaps{}), "every 3s") {
		t.Fatal("processing documents must poll")
	}
	for _, status := range []string{"ready", "failed", "unknown"} {
		if strings.Contains(render(status, "", dashboardCaps{}), "every 3s") {
			t.Fatalf("%s must not poll", status)
		}
	}
	message := "required price missing: cached_input"
	if strings.Contains(render("failed", message, dashboardCaps{}), "/dashboard/models/pricing") {
		t.Fatal("price action must be withheld without capability")
	}
	if !strings.Contains(render("failed", message, dashboardCaps{ModelsManage: true}), "/dashboard/models/pricing") {
		t.Fatal("price action missing for manager")
	}
}

func TestAdminModelEmptyActions(t *testing.T) {
	set, err := dashboardTemplateSet("models")
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		fragment string
		empty    func(bool) dashboardEmpty
	}{
		{"frag-models-endpoints", modelsEndpointsEmpty},
		{"frag-models-pricing", modelsPricingEmpty},
	} {
		for _, capable := range []bool{false, true} {
			var out strings.Builder
			view := dashboardView{Can: dashboardCaps{ModelsManage: capable}, Data: modelsData{Empty: fixture.empty(capable)}}
			if err := set.ExecuteTemplate(&out, fixture.fragment, view); err != nil {
				t.Fatal(err)
			}
			hasAction := strings.Contains(out.String(), `data-drawer-url="/dashboard/partials/models/endpoint-drawer"`)
			if hasAction != capable {
				t.Fatalf("%s action present=%v for capability=%v", fixture.fragment, hasAction, capable)
			}
			if strings.Contains(out.String(), "every 3s") {
				t.Fatalf("%s must not poll", fixture.fragment)
			}
		}
	}
}
