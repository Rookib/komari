package rpc

import "testing"

func TestAgentControlPolicyRejectsEveryPrincipal(t *testing.T) {
	principals := map[string]*Principal{
		"anonymous": NewAnonymousPrincipal(),
		"agent":     NewAgentPrincipal("node-1"),
		"user":      NewUserPrincipal("owner"),
		"api-key":   NewAPIKeyPrincipal(),
		"nil":       nil,
	}
	for _, method := range []string{"admin:getAgentStartupConfig", "admin:switchAgentVersion"} {
		for name, principal := range principals {
			t.Run(method+"/"+name, func(t *testing.T) {
				if CheckPrincipal(principal, method) {
					t.Fatal("unsafe upstream agent control was authorized")
				}
			})
		}
		Allow(method, RoleGuest)
		if CheckPrincipal(NewUserPrincipal("owner"), method) {
			t.Fatal("ACL override bypassed the TCP-safe policy")
		}
		Allow(method, RoleAdmin)
	}
}

func TestAgentControlPolicyPreservesExistingRoles(t *testing.T) {
	if !CheckPrincipal(NewAgentPrincipal("node-1"), "client:report") {
		t.Fatal("authenticated agent reports must remain permitted")
	}
	if CheckPrincipal(NewAPIKeyPrincipal(), "client:report") {
		t.Fatal("API key must not impersonate an agent")
	}
	if !CheckPrincipal(NewUserPrincipal("owner"), "admin:addClient") {
		t.Fatal("ordinary admin operations must remain permitted")
	}
}
