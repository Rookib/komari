package jsonrpc

import (
	"context"
	"testing"

	"github.com/komari-monitor/komari/pkg/rpc"
)

func TestDisabledAgentControlsNeverReachAgentRuntime(t *testing.T) {
	principals := map[string]*rpc.Principal{
		"anonymous": rpc.NewAnonymousPrincipal(),
		"agent":     rpc.NewAgentPrincipal("node-1"),
		"user":      rpc.NewUserPrincipal("owner"),
		"api-key":   rpc.NewAPIKeyPrincipal(),
	}
	for _, method := range []string{"admin:getAgentStartupConfig", "admin:switchAgentVersion"} {
		for name, principal := range principals {
			t.Run(method+"/"+name, func(t *testing.T) {
				// An unknown offline node would otherwise enter runtime lookup.
				req := &rpc.JsonRpcRequest{ID: 7, Method: method, Params: map[string]any{"uuid": "offline-node", "version": "latest"}}
				meta := &rpc.ContextMeta{Principal: principal, TempShareValid: true}
				resp := Dispatch(context.Background(), meta, req)
				if resp.Error == nil || resp.Error.Code != rpc.PermissionDenied || resp.Result != nil {
					t.Fatalf("expected a policy denial before agent lookup, got %#v", resp)
				}
			})
		}
		// Internal calls do not pass Dispatch: the handler still must deny.
		resp := rpc.Call(8, method, map[string]any{"uuid": "offline-node", "version": "latest"})
		if resp.Error == nil || resp.Error.Code != rpc.PermissionDenied || resp.Result != nil {
			t.Fatalf("internal call bypassed the policy: %#v", resp)
		}
	}
}
