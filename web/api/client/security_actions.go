package client

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/database/securityactions"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/api"
)

type securityActionAckRequest struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

func ListSecurityActions(c *gin.Context) {
	clientUUID, ok := securityClientUUIDFromContext(c)
	if !ok {
		api.RespondError(c, http.StatusUnauthorized, "client token required")
		return
	}
	actions, err := securityactions.PendingForClient(clientUUID)
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	api.RespondSuccess(c, gin.H{"actions": actions})
}

func AckSecurityAction(c *gin.Context) {
	clientUUID, ok := securityClientUUIDFromContext(c)
	if !ok {
		api.RespondError(c, http.StatusUnauthorized, "client token required")
		return
	}
	var req securityActionAckRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.RespondError(c, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	action, err := securityactions.Ack(clientUUID, req.ID, req.Status, req.Message)
	if err != nil {
		api.RespondError(c, http.StatusBadRequest, err.Error())
		return
	}
	auditlog.Log(
		c.ClientIP(),
		clientUUID,
		fmt.Sprintf("security action ack: client=%s ip=%s action=%s status=%s message=%s", clientUUID, action.IP, action.Action, action.Status, action.Message),
		"info",
	)
	api.RespondSuccess(c, gin.H{"action": action})
}

// Firewall queues are bound to the authenticated agent principal. A user or API
// key must not turn into an agent by adding a client token or UUID to a request.
func securityClientUUIDFromContext(c *gin.Context) (string, bool) {
	principal := api.GetPrincipal(c)
	if principal == nil || principal.Type != rpc.PrincipalAgent || principal.ClientUUID == "" {
		return "", false
	}
	return principal.ClientUUID, true
}
