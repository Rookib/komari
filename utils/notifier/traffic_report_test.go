package notifier

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestSumTrafficDeltasUsesStoredDeltas(t *testing.T) {
	records := []trafficDeltaRecord{
		{TrafficUp: 30, TrafficDown: 40, NetTotalUp: 130, NetTotalDown: 240},
		{TrafficUp: 25, TrafficDown: 35, NetTotalUp: 155, NetTotalDown: 275},
	}

	up, down := sumTrafficDeltas(records, nil)
	assert.Equal(t, int64(55), up)
	assert.Equal(t, int64(75), down)
}

func TestSumTrafficDeltasFallsBackToCumulativeTotals(t *testing.T) {
	previous := &trafficDeltaRecord{NetTotalUp: 100, NetTotalDown: 200}
	records := []trafficDeltaRecord{
		{NetTotalUp: 130, NetTotalDown: 250},
		{NetTotalUp: 160, NetTotalDown: 310},
	}

	up, down := sumTrafficDeltas(records, previous)
	assert.Equal(t, int64(60), up)
	assert.Equal(t, int64(110), down)
}

func TestSumTrafficDeltasDoesNotCountUnknownCounterResetAsUsage(t *testing.T) {
	previous := &trafficDeltaRecord{NetTotalUp: 400, NetTotalDown: 550}
	records := []trafficDeltaRecord{
		{NetTotalUp: 500, NetTotalDown: 600},
		{NetTotalUp: 100, NetTotalDown: 150},
	}

	up, down := sumTrafficDeltas(records, previous)
	assert.Equal(t, int64(100), up)
	assert.Equal(t, int64(50), down)
}

func TestSumTrafficDeltasKeepsStoredResetDeltas(t *testing.T) {
	previous := &trafficDeltaRecord{NetTotalUp: 1000, NetTotalDown: 2000}
	records := []trafficDeltaRecord{
		{TrafficUp: 20, TrafficDown: 30, NetTotalUp: 20, NetTotalDown: 30},
		{TrafficUp: 15, TrafficDown: 25, NetTotalUp: 35, NetTotalDown: 55},
	}

	up, down := sumTrafficDeltas(records, previous)
	assert.Equal(t, int64(35), up)
	assert.Equal(t, int64(55), down)
}

func TestSumTrafficDeltasIgnoresInterleavedCounterRollbacks(t *testing.T) {
	previous := &trafficDeltaRecord{NetTotalUp: 1000, NetTotalDown: 2000}
	records := []trafficDeltaRecord{
		{NetTotalUp: 1030, NetTotalDown: 2050},
		{NetTotalUp: 650, NetTotalDown: 900},
		{NetTotalUp: 1040, NetTotalDown: 2060},
		{NetTotalUp: 660, NetTotalDown: 910},
	}

	up, down := sumTrafficDeltas(records, previous)
	assert.Equal(t, int64(40), up)
	assert.Equal(t, int64(60), down)
}

func TestSumTrafficDeltasCapsStoredSpikesDuringRollback(t *testing.T) {
	previous := &trafficDeltaRecord{NetTotalUp: 1000, NetTotalDown: 2000}
	records := []trafficDeltaRecord{
		{TrafficUp: 30, TrafficDown: 50, NetTotalUp: 1030, NetTotalDown: 2050},
		{NetTotalUp: 650, NetTotalDown: 900},
		{TrafficUp: 390, TrafficDown: 1160, NetTotalUp: 1040, NetTotalDown: 2060},
		{NetTotalUp: 660, NetTotalDown: 910},
	}

	up, down := sumTrafficDeltas(records, previous)
	assert.Equal(t, int64(40), up)
	assert.Equal(t, int64(60), down)
}

func TestSumTrafficDeltasCountsOnlyAboveRollbackHighWater(t *testing.T) {
	previous := &trafficDeltaRecord{NetTotalUp: 1000, NetTotalDown: 2000}
	records := []trafficDeltaRecord{
		{NetTotalUp: 650, NetTotalDown: 900},
		{NetTotalUp: 700, NetTotalDown: 950},
		{NetTotalUp: 1030, NetTotalDown: 2040},
	}

	up, down := sumTrafficDeltas(records, previous)
	assert.Equal(t, int64(80), up)
	assert.Equal(t, int64(90), down)
}

func TestSumTrafficDeltasEmpty(t *testing.T) {
	up, down := sumTrafficDeltas(nil, nil)
	assert.Zero(t, up)
	assert.Zero(t, down)
}

func TestTrafficDeltaOrFallback(t *testing.T) {
	delta, previous, rollback, hasPrevious, hasRollback := trafficDeltaOrFallback(42, 142, 100, true, 0, false)
	assert.Equal(t, int64(42), delta)
	assert.Equal(t, int64(142), previous)
	assert.Equal(t, int64(0), rollback)
	assert.True(t, hasPrevious)
	assert.False(t, hasRollback)

	delta, _, _, _, _ = trafficDeltaOrFallback(0, 500, 100, true, 0, false)
	assert.Equal(t, int64(400), delta)

	delta, _, rollback, _, hasRollback = trafficDeltaOrFallback(0, 50, 500, true, 0, false)
	assert.Zero(t, delta)
	assert.Equal(t, int64(500), rollback)
	assert.True(t, hasRollback)
}

func TestLatestTrafficResetFindsMostRecentCounterDrop(t *testing.T) {
	start := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	records := []trafficDeltaRecord{
		{Time: (start), NetTotalUp: 100, NetTotalDown: 200},
		{Time: (start.Add(time.Hour)), NetTotalUp: 130, NetTotalDown: 260},
		{Time: (start.AddDate(0, 1, 0)), NetTotalUp: 5, NetTotalDown: 8},
		{Time: (start.AddDate(0, 1, 0).Add(time.Hour)), NetTotalUp: 20, NetTotalDown: 30},
		{Time: (start.AddDate(0, 2, 0)), NetTotalUp: 2, NetTotalDown: 4},
	}

	resetAt, found := latestTrafficReset(records)
	assert.True(t, found)
	assert.Equal(t, start.AddDate(0, 2, 0), resetAt)
}

func TestLatestTrafficResetReturnsNotFound(t *testing.T) {
	start := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	records := []trafficDeltaRecord{
		{Time: (start), NetTotalUp: 100, NetTotalDown: 200},
		{Time: (start.Add(time.Hour)), NetTotalUp: 130, NetTotalDown: 260},
	}

	resetAt, found := latestTrafficReset(records)
	assert.False(t, found)
	assert.True(t, resetAt.IsZero())
}

func TestFormatCompactTrafficCard(t *testing.T) {
	message := FormatCompactTrafficCard("VPS <01>", "上周", TrafficTotals{
		Up:   12 * 1024 * 1024,
		Down: 608 * 1024 * 1024,
	})
	assert.Equal(t, "🖥️ 机器: <b>VPS &lt;01&gt;</b>\n🔼 上传: 12.00 MB\n🔽 下载: 608.00 MB\n📊 上周: <b>620.00 MB</b>", message)
}

func TestFormatCompactTrafficTextCard(t *testing.T) {
	message := FormatCompactTrafficTextCard("VPS <01>", "上周", TrafficTotals{
		Up:   12 * 1024 * 1024,
		Down: 608 * 1024 * 1024,
	})
	assert.Equal(t, "🖥️ 机器: VPS <01>\n🔼 上传: 12.00 MB\n🔽 下载: 608.00 MB\n📊 上周: 620.00 MB", message)
}

func TestComputeUsedByTypeForTrafficReports(t *testing.T) {
	assert.Equal(t, int64(30), ComputeUsedByType("up", 30, 70))
	assert.Equal(t, int64(70), ComputeUsedByType("down", 30, 70))
	assert.Equal(t, int64(100), ComputeUsedByType("sum", 30, 70))
	assert.Equal(t, int64(30), ComputeUsedByType("min", 30, 70))
	assert.Equal(t, int64(70), ComputeUsedByType("max", 30, 70))
	assert.Equal(t, int64(70), ComputeUsedByType("unknown", 30, 70))
}
