package jsonrpc

import (
	"context"
	"testing"

	"github.com/komari-monitor/komari/pkg/rpc"
)

func TestRpcHelpUsesQualifiedMethodName(t *testing.T) {
	resp := rpc.Call(1, "rpc.help", map[string]any{"method": "common:getMe"})
	if resp.Error != nil {
		t.Fatalf("rpc.help returned error: %+v", resp.Error)
	}
	meta, ok := resp.Result.(*rpc.MethodMeta)
	if !ok || meta.Name != "common:getMe" {
		t.Fatalf("unexpected method metadata: %#v", resp.Result)
	}
}

// TestPrivateSiteLoginWhitelist 守卫 issue #567:私有站点模式下,登录页渲染所需的
// 元信息接口必须始终在白名单中,否则匿名用户无法看到登录框。
func TestPrivateSiteLoginWhitelist(t *testing.T) {
	required := []string{
		"public:getMe",
		"public:getPublicSettings",
		"public:getVersion",
	}
	for _, m := range required {
		if !privateSiteLoginWhitelist[m] {
			t.Errorf("method %q must be in privateSiteLoginWhitelist for login page to render under private site (issue #567)", m)
		}
	}

	// 节点列表等数据接口不应在白名单(应被私有站点拦截)。
	mustBlocked := []string{
		"public:getNodesInformation",
		"public:getRecordsByUUID",
		"public:getPingRecords",
	}
	for _, m := range mustBlocked {
		if privateSiteLoginWhitelist[m] {
			t.Errorf("data method %q must NOT be in privateSiteLoginWhitelist (would leak data under private site)", m)
		}
	}
}

func TestDispatchPrincipalOverridesLegacyPermission(t *testing.T) {
	method := "common:testPrincipalAuthority"
	if err := rpc.Register(method, func(ctx context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
		return rpc.MetaFromContext(ctx).Permission, nil
	}); err != nil {
		t.Fatal(err)
	}
	defer rpc.Unregister(method)
	meta := &rpc.ContextMeta{Principal: rpc.NewAgentPrincipal("node-1"), Permission: rpc.RoleAdmin}
	resp := Dispatch(context.Background(), meta, &rpc.JsonRpcRequest{ID: 1, Method: method})
	if resp.Error != nil || resp.Result != rpc.RoleClient {
		t.Fatalf("legacy Permission overrode the authenticated principal: %#v", resp)
	}
}
