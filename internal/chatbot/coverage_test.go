package chatbot_test

import (
	"testing"

	"enclava-go/internal/auth"
	"enclava-go/internal/chatbot"
)

func TestRoleAccessCoverage(t *testing.T) {
	bot := chatbot.Instance{ID: "bot", ExternalID: "external", Model: "chat", ProviderID: "endpoint", Audience: []string{"*"}}
	full := auth.ResourceGrants{
		Assistants: auth.Allowlist{Items: []string{"external"}},
		Models:     auth.Allowlist{Items: []string{"chat", "embed"}},
		Endpoints:  auth.Allowlist{Items: []string{"endpoint"}},
	}
	cases := []struct {
		name    string
		bot     chatbot.Instance
		grants  auth.ResourceGrants
		members []string
		embed   []string
		want    string
	}{
		{"deny by default", bot, auth.ResourceGrants{}, nil, nil, "not_granted"},
		{"everyone granted external id", bot, full, nil, []string{"embed"}, "reachable"},
		{"missing embedding", bot, auth.ResourceGrants{Assistants: full.Assistants, Models: auth.Allowlist{Items: []string{"chat"}}, Endpoints: full.Endpoints}, nil, []string{"embed"}, "not_granted"},
		{"specific members none", chatbot.Instance{ID: "bot", Model: "chat", ProviderID: "endpoint", Audience: []string{}}, full, []string{"member"}, nil, "no_audience"},
		{"specific member selected", chatbot.Instance{ID: "bot", ExternalID: "external", Model: "chat", ProviderID: "endpoint", Audience: []string{"member"}}, full, []string{"member"}, nil, "reachable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checks := chatbot.RoleAccessCoverage(tc.bot, tc.grants, tc.members, []string{"endpoint"}, tc.embed)
			if got := chatbot.AccessCoverageStatus(checks); got != tc.want {
				t.Fatalf("status = %q, want %q; checks: %+v", got, tc.want, checks)
			}
		})
	}
}
