package notifier

import (
	"strings"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/models"
	"github.com/stretchr/testify/assert"
)

func TestFormatSSHAuthGuardAlertMessage(t *testing.T) {
	occurredAt := time.Date(2026, 6, 23, 14, 52, 26, 0, time.UTC)
	message := formatSSHAuthGuardAlertMessage(models.Client{Name: "NOSLA"}, SSHAuthGuardAlertParams{
		SourceIP:      "216.167.123.206",
		User:          "root",
		Method:        "password",
		FailedCount:   5,
		WindowSeconds: 60,
	}, occurredAt)

	assert.Contains(t, message, "服务器：NOSLA")
	assert.Contains(t, message, "来源 IP：216.167.123.206")
	assert.Contains(t, message, "目标用户：root")
	assert.Contains(t, message, "认证方式：密码")
	assert.Contains(t, message, "失败次数：5")
	assert.Contains(t, message, "统计窗口：60 秒")
	assert.Contains(t, message, "时间：2026-06-23 22:52:26 北京时间")
	assert.Contains(t, message, "封禁状态：未启用自动封禁")
	assert.Contains(t, message, "该告警只代表检测到 SSH 登录失败 / 爆破行为")
}

func TestSanitizeSSHAuthGuardSample(t *testing.T) {
	sample := sanitizeSSHAuthGuardSample("Failed password\nTOKEN=secret\r\x00 from 203.0.113.5")
	assert.NotContains(t, sample, "\n")
	assert.NotContains(t, sample, "\r")
	assert.NotContains(t, sample, "\x00")
	assert.Contains(t, sample, "Failed password")

	long := sanitizeSSHAuthGuardSample(strings.Repeat("a", 400))
	assert.LessOrEqual(t, len(long), 240)
}

func TestFormatSSHAuthGuardMethodList(t *testing.T) {
	assert.Equal(t, "密码, 无效用户, PAM", formatSSHAuthGuardMethod("password, invalid-user, pam"))
}

func TestShouldSendSSHAuthGuardNotification(t *testing.T) {
	assert.True(t, shouldSendSSHAuthGuardNotification(models.SSHLoginNotification{
		Enable:      true,
		IPWhitelist: models.StringArray{"198.51.100.1"},
	}, "203.0.113.5"))

	assert.False(t, shouldSendSSHAuthGuardNotification(models.SSHLoginNotification{
		Enable: true,
		IPWhitelist: models.StringArray{
			"203.0.113.0/24",
		},
	}, "203.0.113.5"))

	assert.False(t, shouldSendSSHAuthGuardNotification(models.SSHLoginNotification{
		Enable:      false,
		IPWhitelist: models.StringArray{},
	}, "203.0.113.5"))
}

func TestAllowSSHAuthGuardPanelNotificationCooldown(t *testing.T) {
	sshAuthGuardNotifyCooldown.Lock()
	sshAuthGuardNotifyCooldown.last = make(map[string]time.Time)
	sshAuthGuardNotifyCooldown.Unlock()

	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	assert.True(t, allowSSHAuthGuardPanelNotification("client-a", "203.0.113.5", now))
	assert.False(t, allowSSHAuthGuardPanelNotification("client-a", "203.0.113.5", now.Add(10*time.Minute)))
	assert.True(t, allowSSHAuthGuardPanelNotification("client-a", "203.0.113.6", now.Add(10*time.Minute)))
	assert.True(t, allowSSHAuthGuardPanelNotification("client-a", "203.0.113.5", now.Add(31*time.Minute)))
}
