package officialtrafficapi

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/utils/messageSender"
	"github.com/komari-monitor/komari/utils/officialtraffic"
)

var verificationNotice sync.Map

const verificationNoticeCooldown = 6 * time.Hour

func UploadCache(c *gin.Context) {
	var payload officialtraffic.CollectorUpdate
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "message": "invalid json payload"})
		return
	}

	snapshot, hasSnapshot, err := officialtraffic.UpdateCollectorSnapshot(payload, bearerToken(c.GetHeader("Authorization")))
	if err != nil {
		auditlog.Log(c.ClientIP(), "", "official traffic collector update rejected: "+err.Error(), "warning")
		c.JSON(http.StatusUnauthorized, gin.H{"status": "error", "message": err.Error()})
		return
	}

	if payload.NeedsVerification {
		auditlog.EventLog("warning", fmt.Sprintf("official traffic collector needs verification: client=%s reason=%s", payload.ClientUUID, strings.TrimSpace(payload.Error)))
		maybeNotifyVerification(payload)
	} else {
		auditlog.EventLog("info", fmt.Sprintf("official traffic collector cache updated: client=%s used=%d limit=%d", payload.ClientUUID, snapshot.UsedBytes, snapshot.LimitBytes))
	}

	c.JSON(http.StatusOK, gin.H{
		"status":       "success",
		"has_snapshot": hasSnapshot,
		"client_uuid":  payload.ClientUUID,
		"updated_at":   time.Now().UTC().Format(time.RFC3339),
	})
}

func bearerToken(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return strings.TrimSpace(header[7:])
	}
	return header
}

func maybeNotifyVerification(payload officialtraffic.CollectorUpdate) {
	uuid := strings.TrimSpace(payload.ClientUUID)
	if uuid == "" {
		return
	}
	now := time.Now()
	if raw, ok := verificationNotice.Load(uuid); ok {
		if last, ok := raw.(time.Time); ok && now.Sub(last) < verificationNoticeCooldown {
			return
		}
	}
	verificationNotice.Store(uuid, now)

	client, err := clients.GetClientByUUID(uuid)
	name := uuid
	if err == nil && strings.TrimSpace(client.Name) != "" {
		name = strings.TrimSpace(client.Name)
	}
	reason := strings.TrimSpace(payload.Error)
	if reason == "" {
		reason = "Cloudflare 验证或登录状态失效"
	}
	msg := fmt.Sprintf("⚠️ 官方流量采集需要人工验证\n\n节点：%s\n来源：%s\n原因：%s\n\n请在 Windows 采集机对应浏览器 Profile 中打开服务商后台，手动完成登录/Cloudflare 验证。验证完成后，采集器会自动恢复上报。",
		name,
		displaySource(payload.SourceName),
		reason,
	)
	if err := messageSender.SendTextMessage(msg, "官方流量采集需要验证"); err != nil {
		auditlog.EventLog("warning", "official traffic verification notice failed: "+err.Error())
	}
}

func displaySource(sourceName string) string {
	sourceName = strings.TrimSpace(sourceName)
	if sourceName == "" {
		return "官方缓存采集器"
	}
	return sourceName
}
