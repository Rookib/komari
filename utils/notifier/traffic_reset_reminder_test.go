package notifier

import (
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestIsClientTrafficResetDayUsesClientTimezone(t *testing.T) {
	now := time.Date(2026, 6, 21, 16, 30, 0, 0, time.UTC) // 2026-06-22 00:30 Asia/Shanghai
	client := models.Client{
		TrafficResetReported: true,
		TrafficResetDay:      22,
		TrafficResetTimezone: "Asia/Shanghai",
	}

	assert.True(t, isClientTrafficResetDay(client, now))
	assert.Equal(t, "2026-06-22", trafficResetReminderDateKey(client, now))
}

func TestIsClientTrafficResetDayClampsShortMonth(t *testing.T) {
	now := time.Date(2026, 2, 28, 12, 0, 0, 0, time.UTC)
	client := models.Client{
		TrafficResetReported: true,
		TrafficResetDay:      31,
		TrafficResetTimezone: "UTC",
	}

	assert.True(t, isClientTrafficResetDay(client, now))
}

func TestDueTrafficResetReminderClientsFiltersAndOrders(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.Client{}, &models.TrafficResetReminder{}))

	clients := []models.Client{
		{UUID: "not-reported", Token: "token-not-reported", Name: "not reported", TrafficResetReported: false, TrafficResetDay: 22},
		{UUID: "wrong-day", Token: "token-wrong-day", Name: "wrong day", TrafficResetReported: true, TrafficResetDay: 21, TrafficResetTimezone: "Asia/Shanghai"},
		{UUID: "due-low", Token: "token-due-low", Name: "B", Weight: 1, TrafficResetReported: true, TrafficResetDay: 22, TrafficResetTimezone: "Asia/Shanghai"},
		{UUID: "due-high", Token: "token-due-high", Name: "A", Weight: 9, TrafficResetReported: true, TrafficResetDay: 22, TrafficResetTimezone: "Asia/Shanghai"},
	}
	for _, client := range clients {
		require.NoError(t, db.Create(&client).Error)
	}

	due, err := dueTrafficResetReminderClients(db, time.Date(2026, 6, 21, 16, 30, 0, 0, time.UTC))

	require.NoError(t, err)
	require.Len(t, due, 2)
	assert.Equal(t, "due-high", due[0].UUID)
	assert.Equal(t, "due-low", due[1].UUID)
}

func TestClaimTrafficResetReminderDedupesPerClientDate(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.TrafficResetReminder{}))

	now := time.Date(2026, 6, 22, 0, 5, 0, 0, time.UTC)
	first, err := claimTrafficResetReminder(db, "client-1", "2026-06-22", now)
	require.NoError(t, err)
	second, err := claimTrafficResetReminder(db, "client-1", "2026-06-22", now.Add(time.Hour))
	require.NoError(t, err)
	otherDate, err := claimTrafficResetReminder(db, "client-1", "2026-07-22", now.AddDate(0, 1, 0))
	require.NoError(t, err)

	assert.True(t, first)
	assert.False(t, second)
	assert.True(t, otherDate)
}

func TestFormatTrafficResetReminderCard(t *testing.T) {
	client := models.Client{
		UUID:                 "client-card",
		Name:                 "VPS <01>",
		TrafficLimit:         1000,
		TrafficLimitType:     "sum",
		TrafficResetReported: true,
		TrafficResetDay:      22,
		TrafficResetTimezone: "UTC",
		VnstatAvailable:      true,
		VnstatTotalUp:        100,
		VnstatTotalDown:      200,
		VnstatDailyJSON:      `[{"date":"2026-06-22","up":100,"down":200}]`,
	}

	card := FormatTrafficResetReminderCard(client, time.Date(2026, 6, 22, 0, 5, 0, 0, time.UTC))

	assert.Contains(t, card, "🖥️ 机器: VPS <01>")
	assert.Contains(t, card, "🔄 今日重置: 每月 22 日")
	assert.Contains(t, card, "📊 当前周期: 300 B")
	assert.Contains(t, card, "📦 剩余: 700 B / 1000 B")
}
