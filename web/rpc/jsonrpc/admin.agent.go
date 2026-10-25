package jsonrpc

import (
	"context"

	"github.com/komari-monitor/komari/pkg/rpc"
)

func init() {
	RegisterWithGroupAndMeta("getAgentStartupConfig", rpc.RoleAdmin, adminGetAgentStartupConfig, &rpc.MethodMeta{
		Name:        "admin:getAgentStartupConfig",
		Summary:     "Agent startup credential export is disabled",
		Description: "This TCP-safe distribution does not request or expose agent startup credentials.",
	})
	RegisterWithGroupAndMeta("switchAgentVersion", rpc.RoleAdmin, adminSwitchAgentVersion, &rpc.MethodMeta{
		Name:        "admin:switchAgentVersion",
		Summary:     "Remote agent replacement is disabled",
		Description: "TCP-safe agent upgrades use the reviewed installer and release checksum verification.",
	})
	rpc.MarkSensitive("admin:getAgentStartupConfig")
	rpc.MarkSensitive("admin:switchAgentVersion")
}

// Deny at the handler as well as the permission boundary so internal callers
// cannot bypass the policy by invoking rpc.Call directly.
func adminGetAgentStartupConfig(_ context.Context, _ *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	return nil, rpc.MakeError(rpc.PermissionDenied, "Agent startup credential export is disabled by the TCP-safe policy", nil)
}

func adminSwitchAgentVersion(_ context.Context, _ *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	return nil, rpc.MakeError(rpc.PermissionDenied, "Remote agent replacement is disabled by the TCP-safe policy", nil)
}
