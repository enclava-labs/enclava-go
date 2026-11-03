package httpx

import (
	"context"
	"strings"
	"testing"

	"enclava-go/internal/billing"
	"enclava-go/internal/store/postgres"
)

func TestDashboardRequestRefusalAuditFilter(t *testing.T) {
	f := newBudgetFixture(t)
	ctx := context.Background()
	if err := f.repo.RecordRequestRefused(ctx, f.tenant, billing.Subject{Type: billing.BudgetSubjectAPIKey, ID: "key-1"}, "user", "owner-1", "model-1", "chat", billing.RefusalPriceMissing); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.CreateBillingAudit(ctx, f.tenant, postgres.BillingAuditRow{
		ID: NewID("billaudit"), SubjectType: billing.BudgetSubjectTenant, SubjectID: f.tenant,
		Action: "budget.created", ActorType: "user", ActorID: "admin", Detail: map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}
	all := renderUsagePage(t, f, f.app.handleDashboardUsageAuditFragment, "/dashboard/usage/audit")
	if !strings.Contains(all, ">request.refused</a>") || !strings.Contains(all, "kind=request.refused") {
		t.Fatalf("missing refusal chip: %s", all)
	}
	filtered := renderUsagePage(t, f, f.app.handleDashboardUsageAuditFragment, "/dashboard/usage/audit?kind=request.refused")
	if !strings.Contains(filtered, "Request refused") || strings.Contains(filtered, "Budget created") || !strings.Contains(filtered, `aria-current="page"`) {
		t.Fatalf("refusal filter did not narrow the table: %s", filtered)
	}
}
