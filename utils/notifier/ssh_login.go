package notifier

import (
	"fmt"
	"log"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/database/sshlogin"
	v2 "github.com/komari-monitor/komari/protocol/v2"
	"github.com/komari-monitor/komari/utils/messageSender"
	cache "github.com/patrickmn/go-cache"
)

var (
	sshLoginDedup = cache.New(10*time.Minute, time.Minute)
	sshSafeUser   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	sshLoginRate  = struct {
		sync.Mutex
		byClient map[string][]time.Time
	}{byClient: make(map[string][]time.Time)}
)

// NotifySSHLogin validates an outbound-only security event from an
// authenticated agent, rate-limits it, and queues panel notifications.
func NotifySSHLogin(clientUUID string, params v2.SSHLoginParams) error {
	params.User = strings.TrimSpace(params.User)
	params.RemoteIP = strings.TrimSpace(params.RemoteIP)
	params.AuthMethod = strings.ToLower(strings.TrimSpace(params.AuthMethod))
	if !sshSafeUser.MatchString(params.User) {
		return fmt.Errorf("invalid SSH user")
	}
	if net.ParseIP(params.RemoteIP) == nil {
		return fmt.Errorf("invalid remote IP")
	}
	if params.RemotePort < 1 || params.RemotePort > 65535 {
		return fmt.Errorf("invalid remote port")
	}
	switch params.AuthMethod {
	case "publickey", "password", "keyboard-interactive", "hostbased":
	default:
		return fmt.Errorf("unsupported SSH authentication method")
	}
	occurredAt, err := time.Parse(time.RFC3339Nano, params.OccurredAt)
	if err != nil {
		return fmt.Errorf("invalid event timestamp")
	}
	now := time.Now()
	if occurredAt.Before(now.Add(-24*time.Hour)) || occurredAt.After(now.Add(5*time.Minute)) {
		return fmt.Errorf("SSH login timestamp outside allowed window")
	}
	if !allowSSHLoginEvent(clientUUID, now) {
		return fmt.Errorf("SSH login notification rate limit exceeded")
	}
	key := fmt.Sprintf("%s|%s|%s|%d|%s", clientUUID, params.User, params.RemoteIP, params.RemotePort, params.OccurredAt)
	if _, exists := sshLoginDedup.Get(key); exists {
		return nil
	}
	sshLoginDedup.SetDefault(key, true)

	client, err := clients.GetClientByUUID(clientUUID)
	if err != nil {
		return err
	}
	notificationConfig, err := sshlogin.GetNotification(clientUUID)
	if err != nil {
		return err
	}
	if !notificationConfig.Enable {
		return nil
	}
	whitelisted := notificationConfig.IsIPWhitelisted(params.RemoteIP)
	if err := sshlogin.CreateEvent(models.SSHLoginEvent{
		ID:          uuid.New().String(),
		Client:      clientUUID,
		User:        params.User,
		RemoteIP:    params.RemoteIP,
		RemotePort:  params.RemotePort,
		AuthMethod:  params.AuthMethod,
		OccurredAt:  (occurredAt),
		CreatedAt:   time.Now().UTC(),
		Whitelisted: whitelisted,
	}); err != nil {
		return err
	}
	if whitelisted {
		return nil
	}
	event := models.EventMessage{
		Event:   "SSH 登录成功",
		Clients: []models.Client{client},
		Time:    occurredAt,
		Emoji:   "🔐",
		Message: formatSSHLoginMessage(params, occurredAt),
	}
	go sendSSHLoginNotifications(event)
	return nil
}

func formatSSHLoginMessage(params v2.SSHLoginParams, occurredAt time.Time) string {
	return fmt.Sprintf(
		"👤 登录账户：%s\n🌐 来源地址：%s\n💻 登录终端：ssh\n🔑 认证方式：%s\n\n🕒 登录时间：%s",
		params.User,
		params.RemoteIP,
		formatSSHAuthMethod(params.AuthMethod),
		formatBeijingTime(occurredAt),
	)
}

func formatSSHAuthMethod(method string) string {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "publickey":
		return "密钥"
	case "password":
		return "密码"
	case "keyboard-interactive":
		return "交互式"
	case "hostbased":
		return "主机认证"
	default:
		return method
	}
}

func formatBeijingTime(t time.Time) string {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.FixedZone("CST", 8*60*60)
	}
	return t.In(loc).Format("2006-01-02 15:04:05 北京时间")
}

func allowSSHLoginEvent(clientUUID string, now time.Time) bool {
	sshLoginRate.Lock()
	defer sshLoginRate.Unlock()
	cutoff := now.Add(-time.Minute)
	hits := sshLoginRate.byClient[clientUUID][:0]
	for _, hit := range sshLoginRate.byClient[clientUUID] {
		if hit.After(cutoff) {
			hits = append(hits, hit)
		}
	}
	if len(hits) >= 10 {
		sshLoginRate.byClient[clientUUID] = hits
		return false
	}
	sshLoginRate.byClient[clientUUID] = append(hits, now)
	return true
}

func sendSSHLoginNotifications(event models.EventMessage) {
	if err := messageSender.SendEventAndEmailCopy(event); err != nil {
		log.Printf("Failed to send SSH login notification: %v", err)
	}
}
