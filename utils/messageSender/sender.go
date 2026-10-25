package messageSender

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/models"
	messageevent "github.com/komari-monitor/komari/database/models/messageEvent"
	"github.com/komari-monitor/komari/internal/config"
)

// SendNotification 是通知发送的统一实现：解析事件中的客户端 UUID（外部传入可只含
// UUID 字段）后委托 SendEvent。内部调用与 admin:sendNotification RPC 共用此实现。
func SendNotification(event models.EventMessage) error {
	if len(event.Clients) > 0 {
		eventClients := make([]models.Client, 0, len(event.Clients))
		for _, c := range event.Clients {
			if c.UUID == "" {
				continue
			}
			if full, err := clients.GetClientByUUID(c.UUID); err == nil {
				eventClients = append(eventClients, full)
			}
		}
		if len(eventClients) == 0 {
			return fmt.Errorf("none of the specified clients exist")
		}
		event.Clients = eventClients
	}
	return SendEvent(event)
}

const defaultTemplate = "{{emoji}}{{emoji}}{{emoji}}\nEvent: {{event}}\nClients: {{client}}\nMessage: {{message}}\nTime: {{time}}"

func SendEvent(event models.EventMessage) error {
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	} else {
		event.Time = event.Time.UTC()
	}
	cfg, err := config.GetMany(map[string]any{
		config.NotificationEnabledKey:    false,
		config.NotificationTemplateKey:   defaultTemplate,
		config.SSHAuthGuardSilentModeKey: true,
	})
	if err != nil {
		return err
	}
	if !cfg[config.NotificationEnabledKey].(bool) {
		return nil
	}
	if shouldSilenceSSHAuthGuardEvent(event, cfg[config.SSHAuthGuardSilentModeKey]) {
		auditlog.EventLog("info", "SSH auth guard alert notification suppressed: reason=silent_mode")
		return nil
	}
	return sendToChannels(context.Background(), ConfiguredProviderNames(), Notification{
		Event: event, Title: eventDisplayTitle(event), Message: formatEventMessage(cfg[config.NotificationTemplateKey].(string), event),
	}, CallNotificationChannel, auditlog.Log)
}

// SendTextMessage keeps compact traffic cards available through the same channel
// registry used by built-in and plugin event notifications.
func SendTextMessage(message, title string) error {
	enabled, err := config.GetAs[bool](config.NotificationEnabledKey, false)
	if err != nil || !enabled {
		return err
	}
	return sendToChannels(context.Background(), ConfiguredProviderNames(), Notification{
		Event: models.EventMessage{Event: title, Message: message, Time: time.Now().UTC()}, Title: title, Message: message,
	}, CallNotificationChannel, auditlog.Log)
}

type channelCall func(context.Context, string, Notification) error
type auditLogger func(ip, uuid, message, msgType string)

// A failing channel must not prevent another configured channel being called.
// Delivery is attempted once: retries of non-idempotent plugin handlers can
// duplicate notifications or other plugin side effects.
func sendToChannels(ctx context.Context, names []string, notification Notification, call channelCall, audit auditLogger) error {
	var errs []error
	success := false
	for _, name := range names {
		if err := call(ctx, name, notification); err != nil {
			// Channel failures may include endpoint credentials. Do not persist their
			// raw errors in audit logs or return them through the admin RPC.
			errs = append(errs, fmt.Errorf("notification channel %q failed", name))
			if audit != nil {
				audit("", "", "Failed to send notification via "+name, "error")
			}
		} else {
			success = true
			if audit != nil {
				audit("", "", "Notification sent via "+name, "info")
			}
		}
	}
	if success {
		return nil
	}
	return errors.Join(errs...)
}

func ConfiguredProviderNames() []string {
	raw, err := config.GetAs[any](config.NotificationMethodsKey)
	if err == nil && raw != nil {
		return providerNamesFromValue(raw)
	}
	method, _ := config.GetAs[string](config.NotificationMethodKey, "none")
	return providerNamesFromValue(method)
}

func IsProviderConfigured(name string) bool {
	for _, id := range ConfiguredProviderNames() {
		if id == name {
			return true
		}
	}
	return false
}

// Preserve case because plugin channel identifiers and the built-in Javascript
// identifier are case-sensitive. Unknown configured IDs are reported on send,
// rather than silently removed from the user's selection.
func providerNamesFromValue(raw any) []string {
	var values []string
	switch v := raw.(type) {
	case []string:
		values = append(values, v...)
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				values = append(values, s)
			}
		}
	case string:
		text := strings.TrimSpace(v)
		if strings.HasPrefix(text, "[") {
			if json.Unmarshal([]byte(text), &values) != nil {
				return nil
			}
		} else {
			values = strings.Split(text, ",")
		}
	}
	seen := map[string]bool{}
	result := []string{}
	for _, name := range values {
		name = strings.TrimSpace(name)
		if name == "" || name == "none" || seen[name] {
			continue
		}
		seen[name] = true
		result = append(result, name)
	}
	return result
}

func shouldSilenceSSHAuthGuardEvent(event models.EventMessage, silentMode any) bool {
	enabled, ok := silentMode.(bool)
	return ok && enabled && event.Event == "SSH 爆破告警"
}

// parseTemplate 通过反射解析 event 持有的结构体的所有字段，将模板中的 {{fieldName}}
// 占位符替换为对应值（字段名小写，如 {{event}}、{{time}}、{{message}}、{{emoji}}）。
// 为兼容旧模板，Clients 字段同时支持 {{client}}（旧）与 {{clients}}（新）两种占位符。
// event 为非结构体（如 map 等）时原样返回模板。
func parseTemplate(messageTemplate string, event any) string {
	eventValue := reflect.ValueOf(event)
	if eventValue.Kind() == reflect.Pointer {
		eventValue = eventValue.Elem()
	}
	if eventValue.Kind() != reflect.Struct {
		return messageTemplate
	}
	eventType := eventValue.Type()
	result := messageTemplate
	for i := 0; i < eventType.NumField(); i++ {
		field := eventType.Field(i)
		placeholder := "{{" + strings.ToLower(field.Name) + "}}"
		value := formatTemplateField(field.Name, eventValue.Field(i))
		result = strings.ReplaceAll(result, placeholder, value)
		if field.Name == "Clients" {
			result = strings.ReplaceAll(result, "{{client}}", value)
		}
	}
	return result
}

// formatTemplateField 将事件字段格式化为模板占位符的值：
// 字符串字段直接取值；any 字段取其实际值格式化；Clients 字段拼接客户端名称
// （无名称时回退为 UUID）；Time 字段按服务器本地时区格式化为 RFC3339；其余类型返回空串。
func formatTemplateField(fieldName string, v reflect.Value) string {
	if v.Kind() == reflect.Interface {
		if v.IsNil() {
			return ""
		}
		return fmt.Sprint(v.Interface())
	}
	switch v.Kind() {
	case reflect.String:
		return v.String()
	case reflect.Slice:
		if fieldName == "Clients" {
			clientNames := make([]string, 0, v.Len())
			for i := 0; i < v.Len(); i++ {
				clientValue := v.Index(i)
				name := clientValue.FieldByName("Name").String()
				if strings.TrimSpace(name) == "" {
					name = clientValue.FieldByName("UUID").String()
				}
				clientNames = append(clientNames, name)
			}
			return strings.Join(clientNames, ", ")
		}
	case reflect.Struct:
		if t, ok := v.Interface().(time.Time); ok {
			return t.In(time.Local).Format("2006-01-02 15:04:05")
		}
	}
	return ""
}

func formatEventMessage(messageTemplate string, event models.EventMessage) string {
	if isolated, ok := isolatedEventTemplate(event); ok {
		return isolated
	}
	return parseTemplate(messageTemplate, event)
}

func isolatedEventTemplate(event models.EventMessage) (string, bool) {
	clientText := eventClientText(event)
	timeText := formatEventTime(event.Time)

	switch eventText(event.Event) {
	case "SSH 登录成功":
		return strings.Join([]string{
			"🔐 SSH 安全登录提醒",
			"━━━━━━━━━━━━━━",
			"🖥️ 节点名称：" + clientText,
			"📌 登录事件：SSH 登录成功",
			"",
			eventText(event.Message),
			"",
			"━━━━━━━━━━━━━━",
			"✅ SSH 会话已成功建立",
			"⚠️ 若非本人操作，请立即检查 SSH 密钥、用户权限与登录日志。",
		}, "\n"), true
	case "SSH 爆破告警":
		return strings.Join([]string{
			"🚨 SSH 爆破告警",
			"━━━━━━━━━━━━━━",
			eventText(event.Message),
		}, "\n"), true
	case "Offline":
		return strings.Join([]string{
			"🔴 机器离线告警",
			"━━━━━━━━━━━━━━",
			"🖥️ 节点名称：" + clientText,
			"📌 事件类型：Offline",
			"📝 详细说明：" + eventText(event.Message),
			"🕒 时间：" + timeText,
		}, "\n"), true
	case "Online":
		return strings.Join([]string{
			"🟢 机器恢复通知",
			"━━━━━━━━━━━━━━",
			"🖥️ 节点名称：" + clientText,
			"📌 事件类型：Online",
			"📝 详细说明：" + eventText(event.Message),
			"🕒 时间：" + timeText,
		}, "\n"), true
	case messageevent.Expire:
		return strings.Join([]string{
			"⏳ 到期提醒",
			"━━━━━━━━━━━━━━",
			"📌 事件类型：Expire",
			"📝 即将到期：",
			strings.TrimSpace(eventText(event.Message)),
			"🕒 时间：" + timeText,
		}, "\n"), true
	case messageevent.Renew:
		return strings.Join([]string{
			"🔄 自动续费通知",
			"━━━━━━━━━━━━━━",
			"🖥️ 节点名称：" + clientText,
			"📌 事件类型：Renew",
			"📝 详细说明：",
			strings.TrimSpace(eventText(event.Message)),
			"🕒 时间：" + timeText,
		}, "\n"), true
	case messageevent.Login:
		return strings.Join([]string{
			"🔑 面板登录提醒",
			"━━━━━━━━━━━━━━",
			"📌 事件类型：Login",
			"📝 详细说明：" + eventText(event.Message),
			"🕒 时间：" + timeText,
		}, "\n"), true
	case messageevent.Alert:
		return strings.Join([]string{
			"⚠️ 负载告警",
			"━━━━━━━━━━━━━━",
			"🖥️ 节点名称：" + clientText,
			"📌 事件类型：Alert",
			"📝 详细说明：" + eventText(event.Message),
			"🕒 时间：" + timeText,
		}, "\n"), true
	case messageevent.Traffic:
		return strings.Join([]string{
			"⚠️ 流量阈值提醒",
			"━━━━━━━━━━━━━━",
			"🖥️ 节点名称：" + clientText,
			"📌 事件类型：Traffic",
			"📝 详细说明：" + eventText(event.Message),
			"🕒 时间：" + timeText,
		}, "\n"), true
	case messageevent.DReport, messageevent.WReport, messageevent.MReport:
		return strings.Join([]string{
			eventText(event.Emoji) + " " + trafficReportTitle(eventText(event.Event)),
			"━━━━━━━━━━━━━━",
			"📌 事件类型：" + eventText(event.Event),
			"📝 详细说明：",
			strings.TrimSpace(eventText(event.Message)),
			"🕒 时间：" + timeText,
		}, "\n"), true
	case "Test":
		return strings.Join([]string{
			"🧪 Test",
			"━━━━━━━━━━━━━━",
			"📌 事件类型：Test",
			"📝 详细说明：" + eventText(event.Message),
			"🕒 时间：" + timeText,
		}, "\n"), true
	default:
		return "", false
	}
}

func eventDisplayTitle(event models.EventMessage) string {
	switch eventText(event.Event) {
	case messageevent.Expire:
		return "到期提醒"
	case messageevent.Renew:
		return "自动续费通知"
	case messageevent.Login:
		return "面板登录提醒"
	case messageevent.Alert:
		return "负载告警"
	case messageevent.Traffic:
		return "流量阈值提醒"
	case messageevent.DReport, messageevent.WReport, messageevent.MReport:
		return trafficReportTitle(eventText(event.Event))
	default:
		return eventText(event.Event)
	}
}

func trafficReportTitle(event string) string {
	switch event {
	case messageevent.DReport:
		return "每日流量报告"
	case messageevent.WReport:
		return "每周流量报告"
	case messageevent.MReport:
		return "每月流量报告"
	default:
		return "流量报告"
	}
}

func eventClientText(event models.EventMessage) string {
	clientNames := make([]string, 0, len(event.Clients))
	for _, c := range event.Clients {
		name := c.Name
		if strings.TrimSpace(name) == "" {
			name = c.UUID
		}
		if strings.TrimSpace(name) != "" {
			clientNames = append(clientNames, name)
		}
	}
	if len(clientNames) == 0 {
		return "-"
	}
	return strings.Join(clientNames, ", ")
}

func formatEventTime(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return t.Format(time.RFC3339)
}

// eventText accepts new plugin payloads while preserving text notification formatting.
func eventText(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
