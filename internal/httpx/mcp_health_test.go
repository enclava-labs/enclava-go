package httpx

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"enclava-go/internal/mcp"
)

func TestToolsServerDiscoveryHealthLabels(t *testing.T) {
	f := newIdentityFixture(t)
	seedToolsServer(t, f, "never", "", true)
	seedToolsServer(t, f, "ok", "", true)
	seedToolsServer(t, f, "failed", "", true)
	seedToolsServer(t, f, "disabled", "", false)
	ctx := context.Background()
	if err := f.app.mcpRepo.RecordDiscovery(ctx, f.tenant, "ok", []mcp.DiscoveredTool{}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.mcpRepo.RecordDiscoveryFailure(ctx, f.tenant, "failed"); err != nil {
		t.Fatal(err)
	}
	server, err := f.app.mcpRepo.GetServer(ctx, f.tenant, "failed")
	if err != nil || server.DiscoveryAt.IsZero() || server.DiscoveryError == "" {
		t.Fatalf("server=%+v err=%v", server, err)
	}
	sess, _ := toolsAdminSession(t, f)
	rec := sessionForm(t, f.app, http.MethodGet, "/dashboard/partials/tools/servers", nil, sess, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, label := range []string{"Enabled", "Connected", "Unreachable", "Disabled", "Discovery failed"} {
		if !strings.Contains(body, label) {
			t.Errorf("missing %q in %s", label, body)
		}
	}
}
