// dashboard_caps_test.go — per-role capability assertions for the §3.1
// table: each default role yields exactly its capabilities and
// platform.admin alone grants no feature capability.
package httpx

import (
	"reflect"
	"testing"

	"enclava-go/internal/auth"
)

func scopePrincipal(scopes ...string) auth.Principal {
	set := make(map[string]struct{}, len(scopes))
	for _, s := range scopes {
		set[s] = struct{}{}
	}
	return auth.Principal{Scopes: set}
}

func TestDashboardCapabilitiesPerRole(t *testing.T) {
	memberScopes := []string{
		"models.list", "chat.completions", "embeddings.create",
		"responses", "chatbot", "rag", "extract",
	}
	knowledgeManagerScopes := append(append([]string{}, memberScopes...),
		"rag.manage", "connectors.manage", "chatbot.manage", "extract.manage")
	adminScopes := append(append([]string{}, knowledgeManagerScopes...),
		"platform.admin", "models.list", "openai.proxy", "pricing.manage")

	member := dashboardCaps{Chat: true, Search: true, Extract: true, API: true}
	knowledgeManager := dashboardCaps{
		Chat: true, Search: true, Extract: true,
		KnowledgeManage: true, SourcesManage: true,
		ExtractManage: true, AssistantsManage: true, API: true,
	}
	admin := dashboardCaps{
		Chat: true, Search: true, KnowledgeManage: true,
		SourcesManage: true, Extract: true, ExtractManage: true,
		AssistantsManage: true, ToolsManage: true, ModelsManage: true,
		AccessManage: true, BillingView: true, API: true,
	}
	platformAdminOnly := dashboardCaps{ToolsManage: true, AccessManage: true, BillingView: true}

	for name, tc := range map[string]struct {
		principal auth.Principal
		want      dashboardCaps
	}{
		"member":           {scopePrincipal(memberScopes...), member},
		"knowledgeManager": {scopePrincipal(knowledgeManagerScopes...), knowledgeManager},
		"admin":            {scopePrincipal(adminScopes...), admin},
		"platformAdminOnly": {
			scopePrincipal(auth.ScopePlatformAdmin), platformAdminOnly,
		},
	} {
		if got := dashboardCapabilities(tc.principal); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: capabilities = %+v, want %+v", name, got, tc.want)
		}
	}
}
