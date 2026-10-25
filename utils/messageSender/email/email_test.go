package email

import (
	"strings"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/models"
)

func TestBuildSenderAddressUsesFromName(t *testing.T) {
	addr, header := buildSenderAddress("noreply@example.com", "Komari Monitor")

	if addr != "noreply@example.com" {
		t.Fatalf("sender address = %q, want noreply@example.com", addr)
	}
	if header != `"Komari Monitor" <noreply@example.com>` {
		t.Fatalf("sender header = %q", header)
	}
}

func TestBuildSenderAddressEncodesNonASCIIFromName(t *testing.T) {
	addr, header := buildSenderAddress("noreply@example.com", "Komari 监控")

	if addr != "noreply@example.com" {
		t.Fatalf("sender address = %q, want noreply@example.com", addr)
	}
	if header != "=?utf-8?q?Komari_=E7=9B=91=E6=8E=A7?= <noreply@example.com>" {
		t.Fatalf("sender header = %q", header)
	}
}

func TestBuildSenderAddressPreservesExistingDisplayNameWhenFromNameEmpty(t *testing.T) {
	addr, header := buildSenderAddress(`"Existing Name" <noreply@example.com>`, "")

	if addr != "noreply@example.com" {
		t.Fatalf("sender address = %q, want noreply@example.com", addr)
	}
	if header != `"Existing Name" <noreply@example.com>` {
		t.Fatalf("sender header = %q", header)
	}
}

func TestBuildMIMEMessageUsesMultipartAlternativeForHTML(t *testing.T) {
	msg, err := buildMIMEMessage([]string{
		"To: user@example.com",
		"From: Komari <noreply@example.com>",
		"Subject: Test",
		"MIME-Version: 1.0",
	}, "plain fallback", "<html><body><div>card</div></body></html>")
	if err != nil {
		t.Fatalf("buildMIMEMessage returned error: %v", err)
	}
	text := string(msg)
	assertEmailContains(t, text, "Content-Type: multipart/alternative")
	assertEmailContains(t, text, "Content-Type: text/plain; charset=UTF-8")
	assertEmailContains(t, text, "Content-Type: text/html; charset=UTF-8")
}

func TestFormatHTMLEventEmailSSHAuthGuardCard(t *testing.T) {
	htmlBody := formatHTMLEventEmail(models.EventMessage{
		Event:   "SSH 爆破告警",
		Clients: []models.Client{{Name: "node-a"}},
		Time:    time.Date(2026, 6, 24, 8, 0, 0, 0, time.UTC),
		Message: "服务器：node-a\n来源 IP：203.0.113.1\n目标用户：root, ubuntu\n认证方式：密码\n失败次数：5\n统计窗口：60 秒\n时间：2026-06-24 16:00:00 北京时间\n封禁状态：未启用自动封禁\n\n说明：该告警只代表检测到 SSH 登录失败 / 爆破行为。",
	})

	assertEmailContains(t, htmlBody, "🚨 SSH 爆破告警")
	assertEmailContains(t, htmlBody, "检测到 SSH 登录失败 / 爆破行为")
	assertEmailContains(t, htmlBody, "node-a")
	assertEmailContains(t, htmlBody, "203.0.113.1")
	assertEmailContains(t, htmlBody, "root, ubuntu")
	assertEmailContains(t, htmlBody, "未启用自动封禁")
	if strings.Contains(htmlBody, "SSH 会话已成功建立") {
		t.Fatalf("auth guard email must not contain SSH success wording")
	}
}

func TestFormatHTMLEventEmailSSHSuccessCard(t *testing.T) {
	htmlBody := formatHTMLEventEmail(models.EventMessage{
		Event:   "SSH 登录成功",
		Clients: []models.Client{{Name: "node-b"}},
		Time:    time.Date(2026, 6, 24, 8, 0, 0, 0, time.UTC),
		Message: "👤 登录账户：root\n🌐 来源地址：203.0.113.2\n💻 登录终端：ssh\n🔑 认证方式：密钥\n\n🕒 登录时间：2026-06-24 16:00:00 北京时间",
	})

	assertEmailContains(t, htmlBody, "🔐 SSH 安全登录提醒")
	assertEmailContains(t, htmlBody, "✅ SSH 会话已成功建立")
	assertEmailContains(t, htmlBody, "203.0.113.2")
}

func assertEmailContains(t *testing.T, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("expected %q to contain %q", text, want)
	}
}
