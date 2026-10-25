package messageSender

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/models"
	messageevent "github.com/komari-monitor/komari/database/models/messageEvent"
)

const sshSuccessGlobalTemplate = `🔐 SSH 安全登录提醒
━━━━━━━━━━━━━━
{{message}}

✅ SSH 会话已成功建立
⚠️ 若非本人操作，请立即检查 SSH 密钥、用户权限与登录日志。`

func TestIsolatedEventTemplatesDoNotUseSSHSuccessText(t *testing.T) {
	events := []models.EventMessage{
		{
			Event:   "SSH 爆破告警",
			Clients: []models.Client{{Name: "node-a"}},
			Time:    time.Date(2026, 6, 24, 8, 0, 0, 0, time.UTC),
			Message: "服务器：node-a\n来源 IP：203.0.113.1\n目标用户：root\n认证方式：密码\n失败次数：5\n统计窗口：60 秒\n时间：2026-06-24 16:00:00 北京时间\n\n说明：该告警只代表检测到 SSH 登录失败 / 爆破行为，未执行封禁命令。",
		},
		{
			Event:   "Offline",
			Clients: []models.Client{{Name: "node-b"}},
			Time:    time.Date(2026, 6, 24, 8, 1, 0, 0, time.UTC),
			Message: "机器已离线，超过宽限期 300 秒。",
		},
		{
			Event:   "Online",
			Clients: []models.Client{{Name: "node-c"}},
			Time:    time.Date(2026, 6, 24, 8, 2, 0, 0, time.UTC),
			Message: "机器连接已恢复，当前状态为在线。",
		},
		{
			Event:   "Test",
			Time:    time.Date(2026, 6, 24, 8, 3, 0, 0, time.UTC),
			Message: "This is a test message from Komari.",
		},
		{
			Event:   messageevent.Expire,
			Time:    time.Date(2026, 6, 24, 8, 4, 0, 0, time.UTC),
			Message: "• WEPC美国家宽直播 (7d)",
		},
		{
			Event:   messageevent.Renew,
			Clients: []models.Client{{Name: "node-renew"}},
			Time:    time.Date(2026, 6, 24, 8, 5, 0, 0, time.UTC),
			Message: "• node-renew until 2026-07-24",
		},
		{
			Event:   messageevent.Login,
			Time:    time.Date(2026, 6, 24, 8, 6, 0, 0, time.UTC),
			Message: "password: 203.0.113.10 (US)\nSafari",
		},
		{
			Event:   messageevent.Alert,
			Clients: []models.Client{{Name: "node-load"}},
			Time:    time.Date(2026, 6, 24, 8, 7, 0, 0, time.UTC),
			Message: "CPU high",
		},
		{
			Event:   messageevent.Traffic,
			Clients: []models.Client{{Name: "node-traffic"}},
			Time:    time.Date(2026, 6, 24, 8, 8, 0, 0, time.UTC),
			Message: "used 80%",
		},
		{
			Event:   messageevent.DReport,
			Time:    time.Date(2026, 6, 24, 8, 9, 0, 0, time.UTC),
			Emoji:   "📊",
			Message: "node-a：1 GB",
		},
	}

	for _, event := range events {
		got := formatEventMessage(sshSuccessGlobalTemplate, event)
		assertNotContains(t, got, "SSH 会话已成功建立")
		assertNotContains(t, got, "若非本人操作，请立即检查 SSH 密钥、用户权限与登录日志")
	}
}

func TestSSHSuccessEventUsesSSHSuccessTemplate(t *testing.T) {
	got := formatEventMessage("ignored {{message}}", models.EventMessage{
		Event:   "SSH 登录成功",
		Clients: []models.Client{{Name: "node-a"}},
		Time:    time.Date(2026, 6, 24, 8, 0, 0, 0, time.UTC),
		Message: "👤 登录账户：root\n🌐 来源地址：203.0.113.1\n💻 登录终端：ssh\n🔑 认证方式：密钥\n\n🕒 登录时间：2026-06-24 16:00:00 北京时间",
	})

	assertContains(t, got, "🔐 SSH 安全登录提醒")
	assertContains(t, got, "✅ SSH 会话已成功建立")
	assertContains(t, got, "若非本人操作，请立即检查 SSH 密钥、用户权限与登录日志")
	assertContains(t, got, "节点名称：node-a")
}

func TestCustomNonIsolatedEventsStillUseGlobalTemplate(t *testing.T) {
	got := formatEventMessage("global {{event}} {{client}} {{message}}", models.EventMessage{
		Event:   "CustomEvent",
		Clients: []models.Client{{Name: "node-a"}},
		Message: "used 80%",
	})

	assertContains(t, got, "global CustomEvent node-a used 80%")
}

func TestEventDisplayTitleUsesChineseForBuiltInEvents(t *testing.T) {
	cases := map[string]string{
		messageevent.Expire:  "到期提醒",
		messageevent.Renew:   "自动续费通知",
		messageevent.Login:   "面板登录提醒",
		messageevent.Alert:   "负载告警",
		messageevent.Traffic: "流量阈值提醒",
		messageevent.DReport: "每日流量报告",
		messageevent.WReport: "每周流量报告",
		messageevent.MReport: "每月流量报告",
		"CustomEvent":        "CustomEvent",
	}
	for event, want := range cases {
		if got := eventDisplayTitle(models.EventMessage{Event: event}); got != want {
			t.Fatalf("eventDisplayTitle(%q) = %q, want %q", event, got, want)
		}
	}
}

func TestProviderNamesFromValueDedupAndFilter(t *testing.T) {
	got := providerNamesFromValue([]any{"telegram", "email", "telegram", "none", "", "missing-provider"})
	want := []string{"telegram", "email", "missing-provider"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("providerNamesFromValue = %#v, want %#v", got, want)
	}
}

func TestProviderNamesFromJSONListString(t *testing.T) {
	got := providerNamesFromValue(`["telegram","email","telegram"]`)
	want := []string{"telegram", "email"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("providerNamesFromValue = %#v, want %#v", got, want)
	}
}

func TestChannelFanoutContinuesAfterFailureWithoutRetry(t *testing.T) {
	var calls []string
	var logs []string
	err := sendToChannels(context.Background(), []string{"telegram", "plugin:custom"}, Notification{Message: "hello"},
		func(_ context.Context, id string, n Notification) error {
			calls = append(calls, id)
			if n.Message != "hello" {
				t.Fatalf("message changed: %#v", n)
			}
			if id == "telegram" {
				return errors.New("sensitive URL and token")
			}
			return nil
		}, func(_, _, message, level string) { logs = append(logs, level+":"+message) })
	if err != nil {
		t.Fatalf("partial delivery failed: %v", err)
	}
	if strings.Join(calls, ",") != "telegram,plugin:custom" {
		t.Fatalf("unexpected calls: %v", calls)
	}
	assertJoinedContains(t, logs, "Failed to send notification via telegram")
	assertNotContains(t, strings.Join(logs, " "), "sensitive URL and token")
}

func TestChannelFanoutReportsAllFailuresWithoutCredentials(t *testing.T) {
	err := sendToChannels(context.Background(), []string{"telegram", "email"}, Notification{},
		func(context.Context, string, Notification) error { return errors.New("secret-token") }, nil)
	if err == nil {
		t.Fatal("all failures must report an error")
	}
	assertContains(t, err.Error(), "telegram")
	assertContains(t, err.Error(), "email")
	assertNotContains(t, err.Error(), "secret-token")
}

func TestChannelNamesPreservePluginCase(t *testing.T) {
	got := providerNamesFromValue([]any{"Javascript", "MyPlugin/channel", "Javascript", "none"})
	if strings.Join(got, ",") != "Javascript,MyPlugin/channel" {
		t.Fatalf("IDs were changed: %v", got)
	}
}

func TestShouldSilenceSSHAuthGuardEventOnlyAffectsAuthGuardAlerts(t *testing.T) {
	if !shouldSilenceSSHAuthGuardEvent(models.EventMessage{Event: "SSH 爆破告警"}, true) {
		t.Fatal("expected SSH auth guard alert to be silenced when silent mode is enabled")
	}
	if shouldSilenceSSHAuthGuardEvent(models.EventMessage{Event: "SSH 登录成功"}, true) {
		t.Fatal("SSH login success notifications must not be silenced by auth guard silent mode")
	}
	if shouldSilenceSSHAuthGuardEvent(models.EventMessage{Event: "Offline"}, true) {
		t.Fatal("offline notifications must not be silenced by auth guard silent mode")
	}
	if shouldSilenceSSHAuthGuardEvent(models.EventMessage{Event: "SSH 爆破告警"}, false) {
		t.Fatal("auth guard alert must not be silenced when silent mode is disabled")
	}
}

func assertContains(t *testing.T, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("expected %q to contain %q", text, want)
	}
}

func assertNotContains(t *testing.T, text, want string) {
	t.Helper()
	if strings.Contains(text, want) {
		t.Fatalf("expected %q not to contain %q", text, want)
	}
}

func assertJoinedContains(t *testing.T, lines []string, want string) {
	t.Helper()
	assertContains(t, strings.Join(lines, "\n"), want)
}
