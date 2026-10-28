// dashboard_nav_test.go — OA-09 nav model: per-role membership, empty-group
// disappearance and active marking (T-07-03-02).
package httpx

import (
	"reflect"
	"testing"
)

func navIDs(groups []dashboardNavGroup) [][]string {
	out := make([][]string, len(groups))
	for i, g := range groups {
		ids := make([]string, len(g.Items))
		for j, it := range g.Items {
			ids[j] = it.ID
		}
		out[i] = ids
	}
	return out
}

func TestDashboardNavMember(t *testing.T) {
	caps := dashboardCaps{Chat: true, Search: true, Extract: true}
	groups := dashboardNav(caps, "")
	want := [][]string{{"home", "chat", "search", "extract"}, {"knowledge"}}
	if !reflect.DeepEqual(navIDs(groups), want) {
		t.Fatalf("member nav = %v, want %v", navIDs(groups), want)
	}
	if groups[0].Title != "WORK" || groups[1].Title != "BUILD" {
		t.Errorf("group titles = %q, %q; want WORK, BUILD", groups[0].Title, groups[1].Title)
	}
}

func TestDashboardNavAdmin(t *testing.T) {
	caps := dashboardCaps{Chat: true, Search: true, KnowledgeManage: true, SourcesManage: true,
		Extract: true, ExtractManage: true, AssistantsManage: true, ToolsManage: true,
		ModelsManage: true, AccessManage: true, BillingView: true}
	groups := dashboardNav(caps, "")
	if len(groups) != 3 {
		t.Fatalf("admin nav groups = %d, want 3", len(groups))
	}
	if !reflect.DeepEqual(navIDs(groups)[2], []string{"tools", "models", "access", "usage"}) {
		t.Errorf("ADMIN items = %v, want tools/models/access/usage", navIDs(groups)[2])
	}
	for _, it := range groups[2].Items {
		if !it.Admin {
			t.Errorf("ADMIN item %q must carry Admin for the lockhint", it.ID)
		}
	}
}

func TestDashboardNavCapless(t *testing.T) {
	groups := dashboardNav(dashboardCaps{}, "")
	// Home has no cap; Account is not a nav item — a cap-less principal sees
	// only WORK[Home].
	want := [][]string{{"home"}}
	if !reflect.DeepEqual(navIDs(groups), want) {
		t.Fatalf("cap-less nav = %v, want %v", navIDs(groups), want)
	}
}

func TestDashboardNavActive(t *testing.T) {
	caps := dashboardCaps{Chat: true, Search: true}
	groups := dashboardNav(caps, "search")
	var active []string
	for _, g := range groups {
		for _, it := range g.Items {
			if it.Active {
				active = append(active, it.ID)
			}
		}
	}
	if len(active) != 1 || active[0] != "search" {
		t.Fatalf("active items = %v, want exactly [search]", active)
	}
}
