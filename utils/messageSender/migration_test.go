package messageSender

import (
	"context"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/internal/config"
	"reflect"
	"testing"
)

type captureNotificationChannel struct{ notifications []Notification }

func (c *captureNotificationChannel) Send(_ context.Context, n Notification, _ map[string]any) error {
	c.notifications = append(c.notifications, n)
	return nil
}
func (c *captureNotificationChannel) Unload() error { return nil }

func TestMigratedNotificationsPreservePluginPayloadAndSilentMode(t *testing.T) {
	clearNotificationChannels()
	t.Cleanup(clearNotificationChannels)
	db := dbcore.GetDBInstance()
	if err := db.AutoMigrate(&models.MessageSenderProvider{}, &models.Log{}); err != nil {
		t.Fatal(err)
	}
	config.SetDb(db)
	if err := config.SetMany(map[string]any{config.NotificationEnabledKey: true, config.NotificationMethodsKey: []string{"CustomPlugin"}, config.NotificationTemplateKey: "{{message}}", config.SSHAuthGuardSilentModeKey: true}); err != nil {
		t.Fatal(err)
	}
	channel := &captureNotificationChannel{}
	if err := RegisterNotificationChannel("CustomPlugin", models.Configuration{}, channel); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"nested": []any{"a", float64(7)}}
	if err := SendEvent(models.EventMessage{Event: "custom", Message: payload}); err != nil {
		t.Fatal(err)
	}
	if len(channel.notifications) != 1 || !reflect.DeepEqual(channel.notifications[0].Event.Message, payload) {
		t.Fatalf("plugin payload lost: %#v", channel.notifications)
	}
	if channel.notifications[0].Event.Time.IsZero() {
		t.Fatal("missing event timestamp")
	}
	if err := SendEvent(models.EventMessage{Event: "SSH 爆破告警", Message: "suppressed"}); err != nil {
		t.Fatal(err)
	}
	if len(channel.notifications) != 1 {
		t.Fatal("silent SSH alert reached channel")
	}
	if err := SendEventWithProvider("CustomPlugin", models.EventMessage{Event: "SSH 爆破告警"}); err != nil {
		t.Fatal(err)
	}
	if len(channel.notifications) != 1 {
		t.Fatal("explicit provider bypassed silent mode")
	}
	if err := config.Set(config.NotificationEnabledKey, false); err != nil {
		t.Fatal(err)
	}
	if err := SendTextMessage("disabled", ""); err != nil {
		t.Fatal(err)
	}
	if len(channel.notifications) != 1 {
		t.Fatal("global notification disable was bypassed")
	}
}

func TestConfiguredChannelsKeepExplicitEmptyAndLegacySelection(t *testing.T) {
	db := dbcore.GetDBInstance()
	config.SetDb(db)
	if err := config.SetMany(map[string]any{config.NotificationMethodsKey: []string{}, config.NotificationMethodKey: "telegram"}); err != nil {
		t.Fatal(err)
	}
	if len(ConfiguredProviderNames()) != 0 {
		t.Fatal("explicitly empty channels must disable delivery")
	}
	if err := config.Set(config.NotificationMethodsKey, nil); err != nil {
		t.Fatal(err)
	}
	if got := ConfiguredProviderNames(); len(got) != 1 || got[0] != "telegram" {
		t.Fatalf("legacy selection lost: %v", got)
	}
}
