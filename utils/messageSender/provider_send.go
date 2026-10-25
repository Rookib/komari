package messageSender

import (
	"context"
	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/internal/config"
	"time"
)

// Compatibility entry point for custom notifier call sites; routing is shared.
func SendEventAndEmailCopy(event models.EventMessage) error { return SendEvent(event) }
func SendEventWithProvider(name string, event models.EventMessage) error {
	enabled, err := config.GetAs[bool](config.NotificationEnabledKey, false)
	if err != nil || !enabled {
		return err
	}
	silent, err := config.GetAs[bool](config.SSHAuthGuardSilentModeKey, true)
	if err != nil {
		return err
	}
	if shouldSilenceSSHAuthGuardEvent(event, silent) {
		return nil
	}
	template, err := config.GetAs[string](config.NotificationTemplateKey, defaultTemplate)
	if err != nil {
		return err
	}
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	return sendToChannels(context.Background(), []string{name}, Notification{Event: event, Title: eventDisplayTitle(event), Message: formatEventMessage(template, event)}, CallNotificationChannel, auditlog.Log)
}
