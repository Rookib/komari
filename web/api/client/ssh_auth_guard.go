package client

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/utils/notifier"
)

// UploadSSHAuthGuardAlert accepts an already-aggregated SSH failed-login alert
// from an authenticated safe agent. The server does not parse raw SSH logs.
func UploadSSHAuthGuardAlert(c *gin.Context) {
	uuid, ok := securityClientUUIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"status": "error", "error": "Invalid token"})
		return
	}

	var params notifier.SSHAuthGuardAlertParams
	if err := c.ShouldBindJSON(&params); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Invalid request body"})
		return
	}
	if err := notifier.NotifySSHAuthGuardAlert(uuid, params); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "success"})
}
