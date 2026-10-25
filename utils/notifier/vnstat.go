package notifier

import (
	"encoding/json"
	"time"

	"github.com/komari-monitor/komari/database/models"
)

type vnstatDay struct {
	Date string `json:"date"`
	Up   int64  `json:"up"`
	Down int64  `json:"down"`
}

// GetClientVnstatRangeTotals returns vnStat daily totals for a date range.
// The range is evaluated by local calendar date because vnStat stores daily
// buckets rather than exact timestamps.
func GetClientVnstatRangeTotals(client models.Client, start, end time.Time) (TrafficTotals, bool) {
	if !client.VnstatAvailable {
		return TrafficTotals{}, false
	}
	days, ok := parseClientVnstatDays(client)
	if !ok {
		return TrafficTotals{}, false
	}
	startDate := dateKey(start)
	endDate := dateKey(end)
	var totals TrafficTotals
	found := false
	for _, day := range days {
		if day.Date < startDate || day.Date > endDate {
			continue
		}
		totals.Up += day.Up
		totals.Down += day.Down
		found = true
	}
	return totals, found
}

// GetClientVnstatLatestTotals returns the latest cumulative totals for a
// client using vnStat as the live counter source. When vnStat was enabled after
// Komari had already accumulated traffic, the baseline captured at adoption is
// carried forward so Telegram "累计" and "剩余" cards use the same accounting
// basis instead of showing a smaller raw vnStat-only total.
func GetClientVnstatLatestTotals(client models.Client) (TrafficTotals, bool) {
	if !client.VnstatAvailable || (client.VnstatTotalUp == 0 && client.VnstatTotalDown == 0) {
		return TrafficTotals{}, false
	}
	if !client.VnstatBaselineAt.IsZero() {
		return TrafficTotals{
			Up:   client.VnstatBaselineUp + positiveDelta(client.VnstatTotalUp, client.VnstatBaselineVnUp),
			Down: client.VnstatBaselineDown + positiveDelta(client.VnstatTotalDown, client.VnstatBaselineVnDown),
		}, true
	}
	return TrafficTotals{Up: client.VnstatTotalUp, Down: client.VnstatTotalDown}, true
}

// GetClientVnstatCycleTotals returns the traffic used in the current reset
// cycle. During the first adoption cycle, it carries over the Komari counter
// baseline captured when vnStat first reported; after one reset boundary it
// uses pure vnStat daily buckets.
func GetClientVnstatCycleTotals(client models.Client, now time.Time) (TrafficTotals, bool) {
	if !client.VnstatAvailable {
		return TrafficTotals{}, false
	}
	if client.TrafficResetDay <= 0 {
		return GetClientVnstatLatestTotals(client)
	}

	loc := clientTrafficLocation(client)
	now = now.In(loc)
	resetStart := trafficCycleStart(now, client.TrafficResetDay, loc)

	if !client.VnstatBaselineAt.IsZero() && client.VnstatBaselineAt.In(loc).After(resetStart) {
		return TrafficTotals{
			Up:   client.VnstatBaselineUp + positiveDelta(client.VnstatTotalUp, client.VnstatBaselineVnUp),
			Down: client.VnstatBaselineDown + positiveDelta(client.VnstatTotalDown, client.VnstatBaselineVnDown),
		}, true
	}

	if totals, ok := GetClientVnstatRangeTotals(client, resetStart, now); ok {
		return totals, true
	}
	return GetClientVnstatLatestTotals(client)
}

func parseClientVnstatDays(client models.Client) ([]vnstatDay, bool) {
	if client.VnstatDailyJSON == "" {
		return nil, false
	}
	var days []vnstatDay
	if err := json.Unmarshal([]byte(client.VnstatDailyJSON), &days); err != nil {
		return nil, false
	}
	return days, len(days) > 0
}

func clientTrafficLocation(client models.Client) *time.Location {
	if client.TrafficResetTimezone != "" {
		if loc, err := time.LoadLocation(client.TrafficResetTimezone); err == nil {
			return loc
		}
	}
	return time.Local
}

func trafficCycleStart(now time.Time, resetDay int, loc *time.Location) time.Time {
	if resetDay <= 0 {
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	}
	candidate := dateWithClampedDay(now.Year(), now.Month(), resetDay, loc)
	if now.Before(candidate) {
		prev := now.AddDate(0, -1, 0)
		return dateWithClampedDay(prev.Year(), prev.Month(), resetDay, loc)
	}
	return candidate
}

func dateWithClampedDay(year int, month time.Month, day int, loc *time.Location) time.Time {
	if day < 1 {
		day = 1
	}
	last := time.Date(year, month+1, 0, 0, 0, 0, 0, loc).Day()
	if day > last {
		day = last
	}
	return time.Date(year, month, day, 0, 0, 0, 0, loc)
}

func dateKey(t time.Time) string {
	return t.Format("2006-01-02")
}

func positiveDelta(current, previous int64) int64 {
	if current > previous {
		return current - previous
	}
	return 0
}
