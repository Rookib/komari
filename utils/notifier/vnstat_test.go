package notifier

import (
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/models"
	"github.com/stretchr/testify/assert"
)

func TestGetClientVnstatRangeTotals(t *testing.T) {
	client := models.Client{
		VnstatAvailable: true,
		VnstatDailyJSON: `[{"date":"2026-06-21","up":100,"down":200},{"date":"2026-06-22","up":300,"down":400}]`,
	}
	start := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 6, 22, 23, 59, 59, 0, time.UTC)

	totals, ok := GetClientVnstatRangeTotals(client, start, end)

	assert.True(t, ok)
	assert.Equal(t, TrafficTotals{Up: 300, Down: 400}, totals)
}

func TestGetClientVnstatLatestTotalsCarriesAdoptionBaseline(t *testing.T) {
	client := models.Client{
		VnstatAvailable:      true,
		VnstatTotalUp:        160,
		VnstatTotalDown:      270,
		VnstatBaselineUp:     1000,
		VnstatBaselineDown:   2000,
		VnstatBaselineVnUp:   100,
		VnstatBaselineVnDown: 200,
		VnstatBaselineAt:     (time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)),
	}

	totals, ok := GetClientVnstatLatestTotals(client)

	assert.True(t, ok)
	assert.Equal(t, TrafficTotals{Up: 1060, Down: 2070}, totals)
}

func TestGetClientVnstatCycleTotalsUsesBaselineBeforeFirstReset(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	assert.NoError(t, err)
	client := models.Client{
		VnstatAvailable:      true,
		TrafficResetDay:      11,
		TrafficResetTimezone: "Asia/Shanghai",
		VnstatTotalUp:        160,
		VnstatTotalDown:      270,
		VnstatBaselineUp:     1000,
		VnstatBaselineDown:   2000,
		VnstatBaselineVnUp:   100,
		VnstatBaselineVnDown: 200,
		VnstatBaselineAt:     (time.Date(2026, 6, 22, 12, 0, 0, 0, loc)),
		VnstatDailyJSON:      `[{"date":"2026-06-22","up":60,"down":70}]`,
	}
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, loc)

	totals, ok := GetClientVnstatCycleTotals(client, now)

	assert.True(t, ok)
	assert.Equal(t, TrafficTotals{Up: 1060, Down: 2070}, totals)
}

func TestGetClientVnstatCycleTotalsUsesCumulativeWhenResetDisabled(t *testing.T) {
	client := models.Client{
		VnstatAvailable:      true,
		TrafficResetDay:      0,
		VnstatTotalUp:        600,
		VnstatTotalDown:      900,
		VnstatBaselineUp:     1000,
		VnstatBaselineDown:   2000,
		VnstatBaselineVnUp:   100,
		VnstatBaselineVnDown: 200,
		VnstatBaselineAt:     (time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)),
		VnstatDailyJSON:      `[{"date":"2026-06-01","up":10,"down":20}]`,
	}

	totals, ok := GetClientVnstatCycleTotals(client, time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC))

	assert.True(t, ok)
	assert.Equal(t, TrafficTotals{Up: 1500, Down: 2700}, totals)
}

func TestGetClientVnstatCycleTotalsUsesDailyAfterReset(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	assert.NoError(t, err)
	client := models.Client{
		VnstatAvailable:      true,
		TrafficResetDay:      11,
		TrafficResetTimezone: "Asia/Shanghai",
		VnstatTotalUp:        1600,
		VnstatTotalDown:      2700,
		VnstatBaselineUp:     1000,
		VnstatBaselineDown:   2000,
		VnstatBaselineVnUp:   100,
		VnstatBaselineVnDown: 200,
		VnstatBaselineAt:     (time.Date(2026, 6, 22, 12, 0, 0, 0, loc)),
		VnstatDailyJSON:      `[{"date":"2026-07-10","up":1,"down":2},{"date":"2026-07-11","up":300,"down":400},{"date":"2026-07-12","up":500,"down":600}]`,
	}
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, loc)

	totals, ok := GetClientVnstatCycleTotals(client, now)

	assert.True(t, ok)
	assert.Equal(t, TrafficTotals{Up: 800, Down: 1000}, totals)
}
