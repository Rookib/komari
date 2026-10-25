package notifier

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/internal/config"
	corn "github.com/komari-monitor/komari/internal/scheduler"
	"github.com/komari-monitor/komari/utils/messageSender"
	"github.com/komari-monitor/komari/utils/officialtraffic"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// InitTrafficResetReminderSchedule registers the hourly reset-day reminder
// scanner. It runs immediately on startup and then every hour, so reminders are
// not missed when clients use different reset timezones or the panel restarts
// after midnight. A database dedupe row keeps each client/date to one card.
func InitTrafficResetReminderSchedule() {
	if err := corn.AddContextFunc("traffic-reset-reminder", "0 5 * * * *", true, func(ctx context.Context) {
		CheckTrafficResetReminderScheduledWork(ctx, time.Now())
	}); err != nil {
		log.Println("Failed to register traffic reset reminder scheduled task:", err)
	}
}

func CheckTrafficResetReminderScheduledWork(ctx context.Context, now time.Time) {
	enabled, err := config.GetAs[bool](config.NotificationEnabledKey, false)
	if err != nil || !enabled {
		return
	}
	if !messageSender.IsProviderConfigured("telegram") {
		return
	}

	db := dbcore.GetDBInstance()
	clients, err := dueTrafficResetReminderClients(db, now)
	if err != nil {
		log.Printf("Failed to query traffic reset reminder clients: %v", err)
		return
	}

	for _, client := range clients {
		select {
		case <-ctx.Done():
			return
		default:
		}

		resetDate := trafficResetReminderDateKey(client, now)
		claimed, err := claimTrafficResetReminder(db, client.UUID, resetDate, now)
		if err != nil {
			log.Printf("Failed to claim traffic reset reminder for client %s: %v", client.UUID, err)
			continue
		}
		if !claimed {
			continue
		}

		if err := messageSender.SendTextMessage(FormatTrafficResetReminderCard(client, now), ""); err != nil {
			log.Printf("Failed to send traffic reset reminder for client %s: %v", client.UUID, err)
			_ = unclaimTrafficResetReminder(db, client.UUID, resetDate)
		}
	}
}

func dueTrafficResetReminderClients(db *gorm.DB, now time.Time) ([]models.Client, error) {
	var clients []models.Client
	if err := db.Where("traffic_reset_reported = ? AND traffic_reset_day > ?", true, 0).
		Order("weight DESC, name ASC").
		Find(&clients).Error; err != nil {
		return nil, err
	}

	due := make([]models.Client, 0, len(clients))
	for _, client := range clients {
		if isClientTrafficResetDay(client, now) {
			due = append(due, client)
		}
	}
	return due, nil
}

func isClientTrafficResetDay(client models.Client, now time.Time) bool {
	if !client.TrafficResetReported || client.TrafficResetDay <= 0 {
		return false
	}
	loc := clientTrafficLocation(client)
	localNow := now.In(loc)
	return localNow.Day() == clampedResetDay(localNow.Year(), localNow.Month(), client.TrafficResetDay)
}

func trafficResetReminderDateKey(client models.Client, now time.Time) string {
	return dateKey(now.In(clientTrafficLocation(client)))
}

func claimTrafficResetReminder(db *gorm.DB, clientUUID, resetDate string, now time.Time) (bool, error) {
	reminder := models.TrafficResetReminder{
		Client:    clientUUID,
		ResetDate: resetDate,
		SentAt:    (now),
	}
	tx := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&reminder)
	if tx.Error != nil {
		return false, tx.Error
	}
	return tx.RowsAffected > 0, nil
}

func unclaimTrafficResetReminder(db *gorm.DB, clientUUID, resetDate string) error {
	return db.Where("client = ? AND reset_date = ?", clientUUID, resetDate).Delete(&models.TrafficResetReminder{}).Error
}

// FormatTrafficResetReminderCard renders the Telegram reset-day card in the
// same compact style as the rest of the traffic bot messages.
func FormatTrafficResetReminderCard(client models.Client, now time.Time) string {
	loc := clientTrafficLocation(client)
	localNow := now.In(loc)
	resetText := fmt.Sprintf("每月 %d 日", client.TrafficResetDay)
	if client.TrafficResetDay > clampedResetDay(localNow.Year(), localNow.Month(), client.TrafficResetDay) {
		resetText += "（短月顺延）"
	}

	totals, ok := GetClientVnstatCycleTotals(client, localNow)
	if !ok {
		if latest, err := GetLatestClientTrafficTotals(client.UUID); err == nil {
			totals = latest
			ok = true
		}
	}

	lines := []string{
		fmt.Sprintf("🖥️ 机器: %s", trafficReminderClientName(client)),
		"━━━━━━━━━━━━━━",
		fmt.Sprintf("🔄 今日重置: %s", resetText),
		fmt.Sprintf("🕛 时区: %s", loc.String()),
	}
	if snapshot, snapshotOK := officialtraffic.GetSnapshot(client); snapshotOK {
		lines = append(lines,
			fmt.Sprintf("📡 来源: %s", snapshot.SourceName),
			fmt.Sprintf("📊 当前周期: %s", humanBytes(snapshot.UsedBytes)),
		)
		if snapshot.LimitBytes > 0 {
			pct := math.Min(float64(snapshot.UsedBytes)/float64(snapshot.LimitBytes)*100, 999)
			status := "🟢 新周期"
			if snapshot.UsedBytes > snapshot.LimitBytes {
				status = "🔴 超额"
			}
			lines = append(lines,
				fmt.Sprintf("📦 剩余: %s / %s", humanBytes(snapshot.RemainingBytes), humanBytes(snapshot.LimitBytes)),
				fmt.Sprintf("🎯 状态: %s　%.1f%%", status, pct),
			)
		}
		return strings.Join(lines, "\n")
	}
	if ok {
		lines = append(lines,
			fmt.Sprintf("🔼 上传: %s", humanBytes(totals.Up)),
			fmt.Sprintf("🔽 下载: %s", humanBytes(totals.Down)),
			fmt.Sprintf("📊 当前周期: %s", humanBytes(totals.Up+totals.Down)),
		)
		if client.TrafficLimit > 0 {
			used := ComputeUsedByType(strings.ToLower(client.TrafficLimitType), totals.Up, totals.Down)
			remaining := client.TrafficLimit - used
			overLimit := remaining < 0
			if remaining < 0 {
				remaining = 0
			}
			pct := 0.0
			if client.TrafficLimit > 0 {
				pct = math.Min(float64(used)/float64(client.TrafficLimit)*100, 999)
			}
			status := "🟢 新周期"
			if overLimit {
				status = "🔴 超额"
			}
			lines = append(lines,
				fmt.Sprintf("📦 剩余: %s / %s", humanBytes(remaining), humanBytes(client.TrafficLimit)),
				fmt.Sprintf("🎯 状态: %s　%.1f%%", status, pct),
			)
		}
	} else {
		lines = append(lines, "📊 当前周期: 暂无流量数据")
	}
	return strings.Join(lines, "\n")
}

func trafficReminderClientName(client models.Client) string {
	if strings.TrimSpace(client.Name) != "" {
		return client.Name
	}
	return client.UUID
}

func clampedResetDay(year int, month time.Month, resetDay int) int {
	if resetDay < 1 {
		return 1
	}
	last := time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
	if resetDay > last {
		return last
	}
	return resetDay
}
