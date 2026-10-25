package client

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/api"
)

func TestSecurityEndpointsRequireAgentPrincipal(t *testing.T) {
	principals := map[string]*rpc.Principal{
		"missing":     nil,
		"anonymous":   rpc.NewAnonymousPrincipal(),
		"user":        rpc.NewUserPrincipal("owner"),
		"api-key":     rpc.NewAPIKeyPrincipal(),
		"empty-agent": rpc.NewAgentPrincipal(""),
	}
	for _, handler := range []struct {
		name string
		fn   gin.HandlerFunc
	}{
		{"list", ListSecurityActions},
		{"ack", AckSecurityAction},
		{"guard", UploadSSHAuthGuardAlert},
	} {
		for name, principal := range principals {
			t.Run(handler.name+"/"+name, func(t *testing.T) {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(http.MethodPost, "/?token=forged", strings.NewReader(`{"client":"node-2","id":"action","status":"success"}`))
				c.Set("client_uuid", "node-2")
				if principal != nil {
					api.SetPrincipal(c, principal)
				}
				handler.fn(c)
				if w.Code != http.StatusUnauthorized {
					t.Fatalf("expected unauthorized before database or payload processing, got %d", w.Code)
				}
			})
		}
	}
}

func TestSecurityQueueUsesAuthenticatedAgentUUID(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/?token=another-node", nil)
	c.Set("client_uuid", "node-2")
	api.SetPrincipal(c, rpc.NewAgentPrincipal("node-1"))
	if uuid, ok := securityClientUUIDFromContext(c); !ok || uuid != "node-1" {
		t.Fatalf("queue identity did not come from the principal: %q, %v", uuid, ok)
	}
}
