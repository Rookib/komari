package notifier

import (
	"testing"
	"time"

	v2 "github.com/komari-monitor/komari/protocol/v2"
	"github.com/stretchr/testify/assert"
)

func TestNotifySSHLoginRejectsUnsafeInputBeforeLookup(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tests := []v2.SSHLoginParams{
		{User: "root<script>", RemoteIP: "203.0.113.5", RemotePort: 22, AuthMethod: "publickey", OccurredAt: now},
		{User: "root", RemoteIP: "not-an-ip", RemotePort: 22, AuthMethod: "publickey", OccurredAt: now},
		{User: "root", RemoteIP: "203.0.113.5", RemotePort: 0, AuthMethod: "publickey", OccurredAt: now},
		{User: "root", RemoteIP: "203.0.113.5", RemotePort: 22, AuthMethod: "unknown", OccurredAt: now},
		{User: "root", RemoteIP: "203.0.113.5", RemotePort: 22, AuthMethod: "publickey", OccurredAt: "invalid"},
	}
	for _, params := range tests {
		assert.Error(t, NotifySSHLogin("missing-client", params))
	}
}

func TestAllowSSHLoginEventRateLimit(t *testing.T) {
	clientUUID := "rate-limit-test"
	now := time.Now()
	for i := 0; i < 10; i++ {
		assert.True(t, allowSSHLoginEvent(clientUUID, now.Add(time.Duration(i)*time.Millisecond)))
	}
	assert.False(t, allowSSHLoginEvent(clientUUID, now.Add(time.Second)))
	assert.True(t, allowSSHLoginEvent(clientUUID, now.Add(2*time.Minute)))
}

func TestFormatSSHLoginMessageUsesChineseCardFields(t *testing.T) {
	occurredAt := time.Date(2026, 6, 23, 14, 52, 26, 0, time.UTC)
	message := formatSSHLoginMessage(v2.SSHLoginParams{
		User:       "root",
		RemoteIP:   "216.167.123.206",
		RemotePort: 22,
		AuthMethod: "publickey",
	}, occurredAt)

	assert.Contains(t, message, "👤 登录账户：root")
	assert.Contains(t, message, "🌐 来源地址：216.167.123.206")
	assert.Contains(t, message, "💻 登录终端：ssh")
	assert.Contains(t, message, "🔑 认证方式：密钥")
	assert.Contains(t, message, "🕒 登录时间：2026-06-23 22:52:26 北京时间")
}
