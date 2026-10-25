package metric

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Capture the plan for the real maintenance query, rather than a simplified
// duplicate whose projection or ordering could hide a planner regression.
type restructurePlanQuery struct {
	db   *sql.DB
	plan []string
}

func (q *restructurePlanQuery) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	rows, err := q.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			_ = rows.Close()
			return nil, err
		}
		q.plan = append(q.plan, detail)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return q.db.QueryContext(ctx, query, args...)
}

func TestRestructureStatisticsTargetsMetricAndPreservesRollups(t *testing.T) {
	ctx := context.Background()
	base := time.Now().UTC().Truncate(24 * time.Hour).Add(-60 * 24 * time.Hour)
	baseline := seedRestructureStatisticsStore(t, base)
	optimized := seedRestructureStatisticsStore(t, base)
	beforeQuery := &restructurePlanQuery{db: optimized.db}
	before, err := optimized.scanRollupRowsBetweenWith(ctx, beforeQuery, "metric-00", "", nil, time.Hour, -1<<62, 1<<62, true)
	if err != nil {
		t.Fatalf("query before statistics: %v", err)
	}
	if err := optimized.prepareRestructureStatistics(ctx); err != nil {
		t.Fatalf("prepare statistics: %v", err)
	}
	for _, table := range []string{optimized.tables.series, optimized.tables.labels, optimized.tables.resolutions, optimized.tables.rollups} {
		var count int
		if err := optimized.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_stat1 WHERE tbl = ?", table).Scan(&count); err != nil || count == 0 {
			t.Fatalf("statistics for %s: count=%d err=%v", table, count, err)
		}
	}
	afterQuery := &restructurePlanQuery{db: optimized.db}
	after, err := optimized.scanRollupRowsBetweenWith(ctx, afterQuery, "metric-00", "", nil, time.Hour, -1<<62, 1<<62, true)
	if err != nil {
		t.Fatalf("query after statistics: %v", err)
	}
	// Equal-time rows from different series have no SQL ordering guarantee.
	// Compare canonical stored rows below, and check this query's population.
	if len(before) != 18*30 || len(after) != len(before) {
		t.Fatalf("hourly row counts before=%d after=%d", len(before), len(after))
	}
	seriesStep, rollupStep := -1, -1
	for i, detail := range afterQuery.plan {
		if strings.HasPrefix(detail, "SEARCH s ") && strings.Contains(detail, "metric_name=?") {
			seriesStep = i
		}
		if strings.HasPrefix(detail, "SEARCH r ") && strings.Contains(detail, "series_id=?") {
			rollupStep = i
		}
	}
	if seriesStep < 0 || rollupStep <= seriesStep {
		t.Fatalf("expected metric lookup before targeted rollup lookup; before=%q after=%q", beforeQuery.plan, afterQuery.plan)
	}
	t.Logf("before statistics: %q; after statistics: %q", beforeQuery.plan, afterQuery.plan)
	if !reflect.DeepEqual(snapshotStatisticsRollups(t, baseline), snapshotStatisticsRollups(t, optimized)) {
		t.Fatal("statistics changed stored rollups")
	}
	for _, store := range []*Store{baseline, optimized} {
		if err := store.rebuildDailyRollups(ctx); err != nil {
			t.Fatalf("rebuild daily rollups: %v", err)
		}
		if _, err := store.Compact(ctx, base.Add(61*24*time.Hour)); err != nil {
			t.Fatalf("compact: %v", err)
		}
		if err := store.validateNormalizedRestructure(ctx, 30); err != nil {
			t.Fatalf("validate rebuilt rollups: %v", err)
		}
	}
	// Compare every aggregate, first/last timestamp, dictionary value and digest
	// after the unchanged daily rebuild, retention and complete validation.
	if !reflect.DeepEqual(snapshotStatisticsRollups(t, baseline), snapshotStatisticsRollups(t, optimized)) {
		t.Fatal("statistics changed rebuilt rollups or digests")
	}
}

func TestRestructureStatisticsSkipsNonSQLite(t *testing.T) {
	for _, driver := range []Driver{DriverMySQL, DriverPostgreSQL} {
		t.Run(string(driver), func(t *testing.T) {
			// A nil database also ensures the skipped driver executes no SQL.
			store := &Store{cfg: Config{Driver: driver}}
			if err := store.prepareRestructureStatistics(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func seedRestructureStatisticsStore(t *testing.T, base time.Time) *Store {
	t.Helper()
	ctx := context.Background()
	intervals := []time.Duration{time.Minute, 5 * time.Minute, time.Hour, 24 * time.Hour}
	policy := RollupPolicy{RawRetention: time.Minute, Compression: 30}
	for _, interval := range intervals {
		policy.Tiers = append(policy.Tiers, RollupTier{Interval: interval, Retention: 90 * 24 * time.Hour})
	}
	s, err := Open(ctx, SQLite(":memory:", WithTablePrefix("metric_rebuild_"), WithRollupPolicy(policy)))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for metric := 0; metric < 30; metric++ {
		if err := s.UpsertMetric(ctx, Definition{Name: fmt.Sprintf("metric-%02d", metric), Type: TypeGauge}); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "INSERT INTO "+s.tables.labels+" (id, labels_hash, labels) VALUES (1, 'labels', '{}')"); err != nil {
		t.Fatal(err)
	}
	for i, interval := range intervals {
		if _, err := tx.ExecContext(ctx, "INSERT INTO "+s.tables.resolutions+" (id, resolution_milli) VALUES (?, ?)", i+1, interval.Milliseconds()); err != nil {
			t.Fatal(err)
		}
	}
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO "+s.tables.rollups+" "+normalizedRollupColumns+" VALUES (?, ?, 1, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)")
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	for metric := 0; metric < 30; metric++ {
		for entity := 0; entity < 18; entity++ {
			seriesID := metric*18 + entity + 1
			if _, err := tx.ExecContext(ctx, "INSERT INTO "+s.tables.series+" (id, metric_name, entity_id, tags_hash, tags) VALUES (?, ?, ?, 'tags', '{}')", seriesID, fmt.Sprintf("metric-%02d", metric), fmt.Sprintf("node-%02d", entity)); err != nil {
				t.Fatal(err)
			}
			for resolution, interval := range intervals {
				for bucket := 0; bucket < 30; bucket++ {
					at := base.Add(time.Duration(bucket) * interval).UnixMilli()
					value := float64((metric*10 + entity + bucket) % 100)
					if _, err := stmt.ExecContext(ctx, seriesID, resolution+1, at, value, value*value, value, value, value, at, value, at, at); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return s
}

func snapshotStatisticsRollups(t *testing.T, s *Store) [][]any {
	t.Helper()
	rows, err := s.db.Query(fmt.Sprintf(`SELECT s.metric_name, s.entity_id, s.tags_hash, s.tags,
		l.labels_hash, l.labels, d.resolution_milli, r.bucket_milli, r.count,
		r.sum, r.sum_sq, r.min_val, r.max_val, r.first_val, r.first_ts_milli,
		r.last_val, r.last_ts_milli, r.digest
		FROM %s r JOIN %s s ON s.id = r.series_id
		JOIN %s d ON d.id = r.resolution_id JOIN %s l ON l.id = r.label_id
		ORDER BY s.metric_name, s.entity_id, s.tags_hash, l.labels_hash, d.resolution_milli, r.bucket_milli`,
		s.tables.rollups, s.tables.series, s.tables.resolutions, s.tables.labels))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result [][]any
	for rows.Next() {
		values := make([]any, 18)
		dest := make([]any, len(values))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		result = append(result, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
