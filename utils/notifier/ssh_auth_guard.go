package notifier

import (
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/database/sshlogin"
	"github.com/komari-monitor/komari/utils/messageSender"
)

var sshAuthGuardSafeToken = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
var sshAuthGuardSafeList = regexp.MustCompile(`^[A-Za-z0-9._,\- ]{1,256}$`)

const sshAuthGuardPanelCooldown = 30 * time.Minute

var sshAuthGuardNotifyCooldown = struct {
	sync.Mutex
	last map[string]time.Time
}{
	last: make(map[string]time.Time),
}

// SSHAuthGuardAlertParams is an already-aggregated SSH authentication failure
// alert reported by a safe agent. It is not a raw SSH log line.
type SSHAuthGuardAlertParams struct {
	SourceIP      string `json:"source_ip"`
	User          string `json:"user"`
	Method        string `json:"method"`
	FailedCount   int    `json:"failed_count"`
	WindowSeconds int    `json:"window_seconds"`
	OccurredAt    string `json:"occurred_at"`
	SampleMessage string `json:"sample_message"`
	BanStatus     string `json:"ban_status"`
}

// NotifySSHAuthGuardAlert validates an aggregated, outbound-only SSH auth
// failure alert reported by an authenticated safe agent. It deliberately does
// not write to SSHLoginEvent, which is reserved for successful SSH logins.
func NotifySSHAuthGuardAlert(clientUUID string, params SSHAuthGuardAlertParams) error {
	params.SourceIP = strings.TrimSpace(params.SourceIP)
	params.User = strings.TrimSpace(params.User)
	params.Method = strings.ToLower(strings.TrimSpace(params.Method))
	params.SampleMessage = sanitizeSSHAuthGuardSample(params.SampleMessage)

	if net.ParseIP(params.SourceIP) == nil {
		return fmt.Errorf("invalid SSH auth guard source IP")
	}
	if params.User == "" || !sshAuthGuardSafeList.MatchString(params.User) {
		return fmt.Errorf("invalid SSH auth guard user")
	}
	if params.Method == "" || !sshAuthGuardSafeList.MatchString(params.Method) {
		return fmt.Errorf("invalid SSH auth guard method")
	}
	if params.FailedCount < 1 || params.FailedCount > 10000 {
		return fmt.Errorf("invalid SSH auth guard failed count")
	}
	if params.WindowSeconds < 1 || params.WindowSeconds > 86400 {
		return fmt.Errorf("invalid SSH auth guard window")
	}
	params.BanStatus = sanitizeSSHAuthGuardSample(params.BanStatus)
	occurredAt, err := time.Parse(time.RFC3339Nano, params.OccurredAt)
	if err != nil {
		return fmt.Errorf("invalid SSH auth guard timestamp")
	}
	now := time.Now()
	if occurredAt.Before(now.Add(-24*time.Hour)) || occurredAt.After(now.Add(5*time.Minute)) {
		return fmt.Errorf("SSH auth guard timestamp outside allowed window")
	}

	client, err := clients.GetClientByUUID(clientUUID)
	if err != nil {
		return err
	}
	notificationConfig, err := sshlogin.GetNotification(clientUUID)
	if err != nil {
		return err
	}
	auditlog.EventLog("warn", fmt.Sprintf(
		"SSH auth guard alert: client=%s, uuid=%s, ip=%s, user=%s, method=%s, count=%d",
		client.Name, client.UUID, params.SourceIP, params.User, params.Method, params.FailedCount,
	))
	if !shouldSendSSHAuthGuardNotification(notificationConfig, params.SourceIP) {
		return nil
	}
	if !allowSSHAuthGuardPanelNotification(client.UUID, params.SourceIP, now) {
		auditlog.EventLog("warn", fmt.Sprintf(
			"SSH auth guard alert suppressed: client=%s uuid=%s ip=%s reason=cooldown",
			client.Name, client.UUID, params.SourceIP,
		))
		return nil
	}

	event := models.EventMessage{
		Event:   "SSH 爆破告警",
		Clients: []models.Client{client},
		Time:    occurredAt,
		Emoji:   "🚨",
		Message: formatSSHAuthGuardAlertMessage(client, params, occurredAt),
	}
	go sendSSHAuthGuardAlert(event)
	return nil
}

func shouldSendSSHAuthGuardNotification(notification models.SSHLoginNotification, sourceIP string) bool {
	return notification.Enable && !notification.IsIPWhitelisted(sourceIP)
}

func formatSSHAuthGuardAlertMessage(client models.Client, params SSHAuthGuardAlertParams, occurredAt time.Time) string {
	banStatus := params.BanStatus
	if banStatus == "" {
		banStatus = "未启用自动封禁"
	}
	return fmt.Sprintf(
		"服务器：%s\n来源 IP：%s\n目标用户：%s\n认证方式：%s\n失败次数：%d\n统计窗口：%d 秒\n时间：%s\n封禁状态：%s\n\n说明：该告警只代表检测到 SSH 登录失败 / 爆破行为。",
		client.Name,
		params.SourceIP,
		params.User,
		formatSSHAuthGuardMethod(params.Method),
		params.FailedCount,
		params.WindowSeconds,
		formatBeijingTime(occurredAt),
		banStatus,
	)
}

func formatSSHAuthGuardMethod(method string) string {
	parts := strings.Split(method, ",")
	result := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || seen[part] {
			continue
		}
		seen[part] = true
		switch strings.ToLower(part) {
		case "password":
			result = append(result, "密码")
		case "publickey":
			result = append(result, "密钥")
		case "invalid-user":
			result = append(result, "无效用户")
		case "pam":
			result = append(result, "PAM")
		default:
			result = append(result, part)
		}
	}
	if len(result) == 0 {
		return method
	}
	return strings.Join(result, ", ")
}

func sanitizeSSHAuthGuardSample(sample string) string {
	sample = strings.TrimSpace(sample)
	if sample == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range sample {
		if r == '\n' || r == '\r' || r == '\t' {
			b.WriteByte(' ')
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= 240 {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

func sendSSHAuthGuardAlert(event models.EventMessage) {
	if err := messageSender.SendEvent(event); err != nil {
		auditlog.EventLog("error", "Failed to send SSH auth guard alert: "+err.Error())
	}
}

func allowSSHAuthGuardPanelNotification(clientUUID, sourceIP string, now time.Time) bool {
	key := clientUUID + "|" + sourceIP + "|SSHAuthGuard"
	sshAuthGuardNotifyCooldown.Lock()
	defer sshAuthGuardNotifyCooldown.Unlock()
	if last, ok := sshAuthGuardNotifyCooldown.last[key]; ok && now.Sub(last) < sshAuthGuardPanelCooldown {
		return false
	}
	sshAuthGuardNotifyCooldown.last[key] = now
	cutoff := now.Add(-2 * sshAuthGuardPanelCooldown)
	for k, seen := range sshAuthGuardNotifyCooldown.last {
		if seen.Before(cutoff) {
			delete(sshAuthGuardNotifyCooldown.last, k)
		}
	}
	return true
}
