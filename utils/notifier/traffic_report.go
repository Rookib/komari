package notifier

import (
	"context"
	"fmt"
	"html"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	messageevent "github.com/komari-monitor/komari/database/models/messageEvent"
	"github.com/komari-monitor/komari/internal/config"
	"github.com/komari-monitor/komari/internal/metricstore"
	corn "github.com/komari-monitor/komari/internal/scheduler"
	"github.com/komari-monitor/komari/utils/messageSender"
)

// InitTrafficReportSchedule 注册三个定时任务：日报、周报、月报。
func InitTrafficReportSchedule() {
	if err := corn.AddFunc("traffic-report-daily", "0 0 0 * * *", func() {
		sendTrafficReport(true, false, false)
	}); err != nil {
		log.Println("Failed to register daily traffic report job:", err)
	}

	if err := corn.AddFunc("traffic-report-weekly", "0 0 0 * * 1", func() {
		sendTrafficReport(false, true, false)
	}); err != nil {
		log.Println("Failed to register weekly traffic report job:", err)
	}

	if err := corn.AddFunc("traffic-report-monthly", "0 0 0 1 * *", func() {
		sendTrafficReport(false, false, true)
	}); err != nil {
		log.Println("Failed to register monthly traffic report job:", err)
	}

	log.Println("Traffic report schedules registered: daily, weekly, monthly")
}

// sendTrafficReport 汇聚所有启用了指定报告类型的服务器流量并发送通知。
func sendTrafficReport(daily, weekly, monthly bool) {
	enabled, err := config.GetAs[bool](config.NotificationEnabledKey, false)
	if err != nil || !enabled {
		return
	}

	db := dbcore.GetDBInstance()
	now := time.Now()

	var start, end time.Time
	var eventType, label, suffix, compactLabel string
	switch {
	case daily:
		yesterday := now.AddDate(0, 0, -1)
		start = time.Date(yesterday.Year(), yesterday.Month(), yesterday.Day(), 0, 0, 0, 0, yesterday.Location())
		end = time.Date(yesterday.Year(), yesterday.Month(), yesterday.Day(), 23, 59, 59, 0, yesterday.Location())
		eventType = messageevent.DReport
		label = "daily"
		suffix = "昨日流量"
		compactLabel = "昨日"
	case weekly:
		weekday := int(now.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		lastMonday := now.AddDate(0, 0, -(weekday-1)-7)
		lastSunday := lastMonday.AddDate(0, 0, 6)
		start = time.Date(lastMonday.Year(), lastMonday.Month(), lastMonday.Day(), 0, 0, 0, 0, lastMonday.Location())
		end = time.Date(lastSunday.Year(), lastSunday.Month(), lastSunday.Day(), 23, 59, 59, 0, lastSunday.Location())
		eventType = messageevent.WReport
		label = "weekly"
		suffix = "上周流量"
		compactLabel = "上周"
	case monthly:
		firstOfThisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		start = firstOfThisMonth.AddDate(0, -1, 0)
		end = firstOfThisMonth.Add(-time.Second)
		eventType = messageevent.MReport
		label = "monthly"
		suffix = "上个月流量"
		compactLabel = "上月"
	default:
		return
	}

	var notifications []models.TrafficReportNotification
	query := db.Model(&models.TrafficReportNotification{}).Where("enable = ?", true)
	if daily {
		query = query.Where("daily = ?", true)
	} else if weekly {
		query = query.Where("weekly = ?", true)
	} else {
		query = query.Where("monthly = ?", true)
	}
	if err := query.Find(&notifications).Error; err != nil {
		log.Printf("Failed to query traffic report notifications (%s): %v", label, err)
		return
	}
	if len(notifications) == 0 {
		return
	}

	clientUUIDs := make([]string, 0, len(notifications))
	for _, notification := range notifications {
		clientUUIDs = append(clientUUIDs, notification.Client)
	}
	var clientList []models.Client
	if err := db.Where("uuid IN ?", clientUUIDs).Find(&clientList).Error; err != nil {
		log.Printf("Failed to query clients for traffic report (%s): %v", label, err)
		return
	}
	clientMap := make(map[string]models.Client, len(clientList))
	for _, client := range clientList {
		clientMap[client.UUID] = client
	}

	compactTelegram := messageSender.IsProviderConfigured("telegram")
	var lines []string
	eventClients := make([]models.Client, 0, len(notifications))
	for _, notification := range notifications {
		client, ok := clientMap[notification.Client]
		if !ok {
			continue
		}

		totals, err := GetClientTrafficTotalsInRange(notification.Client, start, end)
		if err != nil {
			log.Printf("Failed to compute traffic for client %s (%s): %v", notification.Client, label, err)
			continue
		}
		if vnstatTotals, ok := GetClientVnstatRangeTotals(client, start, end); ok {
			totals = vnstatTotals
		}

		if compactTelegram {
			name := strings.TrimSpace(client.Name)
			if name == "" {
				name = client.UUID
			}
			if err := messageSender.SendTextMessage(FormatCompactTrafficTextCard(name, compactLabel, totals), ""); err != nil {
				log.Printf("Failed to send compact %s traffic report for client %s: %v", label, notification.Client, err)
			}
			continue
		}

		used := ComputeUsedByType(strings.ToLower(client.TrafficLimitType), totals.Up, totals.Down)
		lines = append(lines, fmt.Sprintf("%s%s：%s", client.Name, suffix, humanBytes(used)))
		eventClients = append(eventClients, client)
	}
	if compactTelegram || len(lines) == 0 {
		return
	}

	emoji := "📊"
	if weekly {
		emoji = "📈"
	} else if monthly {
		emoji = "📅"
	}
	if err := messageSender.SendEvent(models.EventMessage{
		Event:   eventType,
		Clients: eventClients,
		Time:    now,
		Emoji:   emoji,
		Message: strings.Join(lines, "\n"),
	}); err != nil {
		log.Printf("Failed to send %s traffic report: %v", label, err)
	}
}

// TrafficTotals contains exact upload/download deltas for a client and range.
type TrafficTotals struct {
	Up   int64
	Down int64
}

// FormatCompactTrafficCard returns the Telegram HTML traffic layout.
func FormatCompactTrafficCard(name, totalLabel string, totals TrafficTotals) string {
	return fmt.Sprintf("🖥️ 机器: <b>%s</b>\n🔼 上传: %s\n🔽 下载: %s\n📊 %s: <b>%s</b>", html.EscapeString(name), humanBytes(totals.Up), humanBytes(totals.Down), html.EscapeString(totalLabel), humanBytes(totals.Up+totals.Down))
}

// FormatCompactTrafficTextCard is the plain-text variant used by SendTextMessage.
func FormatCompactTrafficTextCard(name, totalLabel string, totals TrafficTotals) string {
	return fmt.Sprintf("🖥️ 机器: %s\n🔼 上传: %s\n🔽 下载: %s\n📊 %s: %s", name, humanBytes(totals.Up), humanBytes(totals.Down), totalLabel, humanBytes(totals.Up+totals.Down))
}

// getClientTrafficInRange returns usage according to the configured accounting mode.
func getClientTrafficInRange(clientUUID, trafficType string, start, end time.Time) (int64, error) {
	totals, err := GetClientTrafficTotalsInRange(clientUUID, start, end)
	if err != nil {
		return 0, err
	}
	return ComputeUsedByType(strings.ToLower(trafficType), totals.Up, totals.Down), nil
}

// GetClientTrafficTotalsInRange reads 1.2.6's metric store and applies the
// reset/rollback-safe accounting used by the custom traffic notifications.
func GetClientTrafficTotalsInRange(clientUUID string, start, end time.Time) (TrafficTotals, error) {
	ctx := context.Background()
	recs, err := metricstore.GetRecordsByClientAndTime(ctx, clientUUID, start, end)
	if err != nil {
		return TrafficTotals{}, err
	}

	records := make([]trafficDeltaRecord, 0, len(recs))
	for _, record := range recs {
		records = append(records, trafficDeltaRecord{
			Time:         record.Time,
			NetTotalUp:   record.NetTotalUp,
			NetTotalDown: record.NetTotalDown,
			TrafficUp:    record.TrafficUp,
			TrafficDown:  record.TrafficDown,
		})
	}
	sortTrafficDeltaRecords(records)

	var previous *trafficDeltaRecord
	baseline, err := metricstore.GetLatestTrafficBefore(ctx, []string{clientUUID}, start)
	if err != nil {
		return TrafficTotals{}, err
	}
	if record, ok := baseline[clientUUID]; ok {
		previous = &trafficDeltaRecord{
			Time:         record.Time,
			NetTotalUp:   record.NetTotalUp,
			NetTotalDown: record.NetTotalDown,
		}
	}

	up, down := sumTrafficDeltas(records, previous)
	return TrafficTotals{Up: up, Down: down}, nil
}

// GetLatestClientTrafficTotals returns the latest cumulative agent counters.
func GetLatestClientTrafficTotals(clientUUID string) (TrafficTotals, error) {
	latest, err := metricstore.GetLatestTrafficBefore(context.Background(), []string{clientUUID}, time.Now().Add(time.Nanosecond))
	if err != nil {
		return TrafficTotals{}, err
	}
	record, ok := latest[clientUUID]
	if !ok {
		return TrafficTotals{}, nil
	}
	return TrafficTotals{Up: record.NetTotalUp, Down: record.NetTotalDown}, nil
}

// GetLatestClientTrafficReset finds the latest counter reset in the last 62 days.
func GetLatestClientTrafficReset(clientUUID string, now time.Time) (time.Time, bool, error) {
	recs, err := metricstore.GetRecordsByClientAndTime(context.Background(), clientUUID, now.AddDate(0, 0, -62), now)
	if err != nil {
		return time.Time{}, false, err
	}
	records := make([]trafficDeltaRecord, 0, len(recs))
	for _, record := range recs {
		records = append(records, trafficDeltaRecord{
			Time:         record.Time,
			NetTotalUp:   record.NetTotalUp,
			NetTotalDown: record.NetTotalDown,
		})
	}
	sortTrafficDeltaRecords(records)
	resetAt, found := latestTrafficReset(records)
	return resetAt, found, nil
}

type trafficDeltaRecord struct {
	Time         time.Time
	NetTotalUp   int64
	NetTotalDown int64
	TrafficUp    int64
	TrafficDown  int64
}

func sortTrafficDeltaRecords(records []trafficDeltaRecord) {
	sort.Slice(records, func(i, j int) bool {
		return records[i].Time.Before(records[j].Time)
	})
}

func latestTrafficReset(records []trafficDeltaRecord) (time.Time, bool) {
	var latest time.Time
	for index := 1; index < len(records); index++ {
		if records[index].NetTotalUp < records[index-1].NetTotalUp || records[index].NetTotalDown < records[index-1].NetTotalDown {
			latest = records[index].Time
		}
	}
	return latest, !latest.IsZero()
}

func sumTrafficDeltas(records []trafficDeltaRecord, previous *trafficDeltaRecord) (int64, int64) {
	var totalUp, totalDown int64
	var previousUp, previousDown int64
	hasPreviousUp := previous != nil
	hasPreviousDown := previous != nil
	if previous != nil {
		previousUp = previous.NetTotalUp
		previousDown = previous.NetTotalDown
	}
	var rollbackBaseUp, rollbackBaseDown int64
	var hasRollbackBaseUp, hasRollbackBaseDown bool

	for _, record := range records {
		var up, down int64
		up, previousUp, rollbackBaseUp, hasPreviousUp, hasRollbackBaseUp = trafficDeltaOrFallback(
			record.TrafficUp, record.NetTotalUp, previousUp, hasPreviousUp, rollbackBaseUp, hasRollbackBaseUp,
		)
		down, previousDown, rollbackBaseDown, hasPreviousDown, hasRollbackBaseDown = trafficDeltaOrFallback(
			record.TrafficDown, record.NetTotalDown, previousDown, hasPreviousDown, rollbackBaseDown, hasRollbackBaseDown,
		)
		totalUp += up
		totalDown += down
	}

	return totalUp, totalDown
}

func trafficDeltaOrFallback(storedDelta, currentTotal, previousTotal int64, hasPrevious bool, rollbackBase int64, hasRollbackBase bool) (int64, int64, int64, bool, bool) {
	if !hasPrevious {
		if storedDelta > 0 {
			return storedDelta, currentTotal, 0, true, false
		}
		return 0, currentTotal, rollbackBase, true, hasRollbackBase
	}

	if storedDelta > 0 {
		if currentTotal == 0 && previousTotal == 0 {
			return storedDelta, currentTotal, 0, true, false
		}
		if hasRollbackBase {
			if currentTotal >= rollbackBase {
				return minTrafficDelta(storedDelta, currentTotal-rollbackBase), currentTotal, 0, true, false
			}
			if currentTotal >= previousTotal {
				return minTrafficDelta(storedDelta, currentTotal-previousTotal), currentTotal, rollbackBase, true, true
			}
			return 0, currentTotal, rollbackBase, true, true
		}
		if currentTotal >= previousTotal {
			return minTrafficDelta(storedDelta, currentTotal-previousTotal), currentTotal, 0, true, false
		}
		return storedDelta, currentTotal, 0, true, false
	}

	if hasRollbackBase {
		if currentTotal >= rollbackBase {
			return currentTotal - rollbackBase, currentTotal, 0, true, false
		}
		if currentTotal >= previousTotal {
			return currentTotal - previousTotal, currentTotal, rollbackBase, true, true
		}
		return 0, currentTotal, rollbackBase, true, true
	}
	if currentTotal < previousTotal {
		return 0, currentTotal, previousTotal, true, true
	}
	return currentTotal - previousTotal, currentTotal, 0, true, false
}

func minTrafficDelta(storedDelta, computedDelta int64) int64 {
	if computedDelta < 0 {
		return 0
	}
	if storedDelta < computedDelta {
		return storedDelta
	}
	return computedDelta
}
