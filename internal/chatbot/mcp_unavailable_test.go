package chatbot

import (
	"context"
	"strings"
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/mcp"
)

type classifiedMCPRepo struct {
	*runMCPRepo
	state string
}

func (r *classifiedMCPRepo) ListAllApprovals(_ context.Context, _, _ string) ([]mcp.Approval, error) {
	return r.approvals, nil
}

func (r *classifiedMCPRepo) DiscoveryState(_ context.Context, _, _, _ string) (string, error) {
	return r.state, nil
}

func TestMCPUnavailableReasons(t *testing.T) {
	p := auth.Principal{TenantID: "tenant", Scopes: map[string]struct{}{"use": {}}}
	for _, tc := range []struct {
		name              string
		enabled, approved bool
		state             string
		scopes            []string
		want              string
	}{
		{"pending", true, false, "pending", []string{"use"}, "pending approval"},
		{"rejected", true, false, "rejected", []string{"use"}, "rejected"},
		{"disabled", false, true, "approved", []string{"use"}, "disabled server"},
		{"scope", true, true, "approved", []string{"missing"}, "caller scopes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &classifiedMCPRepo{runMCPRepo: &runMCPRepo{server: mcp.Server{TenantID: "tenant", ID: "server", Enabled: tc.enabled, AudienceScopes: []string{"use"}}, approvals: []mcp.Approval{{ModelName: "tool", ToolID: "tool", Enabled: tc.approved, CallerScopes: tc.scopes}}}, state: tc.state}
			d := MCPRunTools{Executor: &mcp.Executor{Repository: repo}}
			err := d.UnavailableReason(context.Background(), p, p, Instance{}, "tool")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("reason=%v want %s", err, tc.want)
			}
		})
	}
}
