package migrations

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestLegacyTimestampMigrationDefaultsToUTC(t *testing.T) {
	t.Setenv("TZ", "")
	db := openTimestampMigrationDB(t)
	insertLegacyLogTime(t, db, "2026-07-17 09:30:00.1234567")

	if err := migrateLegacyTimestampColumns(db); err != nil {
		t.Fatalf("migrate timestamps: %v", err)
	}
	want := time.Date(2026, 7, 17, 9, 30, 0, 123456700, time.UTC)
	assertMigratedLogTime(t, db, want)
}

func TestLegacyTimestampMigrationUsesLegacyTZ(t *testing.T) {
	t.Setenv("TZ", "Asia/Shanghai")
	db := openTimestampMigrationDB(t)
	insertLegacyLogTime(t, db, "2026-07-17 09:30:00.123456789")

	if err := migrateLegacyTimestampColumns(db); err != nil {
		t.Fatalf("migrate timestamps: %v", err)
	}
	want := time.Date(2026, 7, 17, 1, 30, 0, 123456789, time.UTC)
	assertMigratedLogTime(t, db, want)
}

func TestParseLegacyTimestampAcceptsHistoricalEpochUnits(t *testing.T) {
	stamp := time.Date(2026, 7, 17, 1, 30, 0, 123456789, time.UTC)
	tests := map[string]struct {
		value string
		want  time.Time
	}{
		"seconds":      {value: fmt.Sprint(stamp.Unix()), want: stamp.Truncate(time.Second)},
		"milliseconds": {value: fmt.Sprint(stamp.UnixMilli()), want: stamp.Truncate(time.Millisecond)},
		"microseconds": {value: fmt.Sprint(stamp.UnixMicro()), want: stamp.Truncate(time.Microsecond)},
		"nanoseconds":  {value: fmt.Sprint(stamp.UnixNano()), want: stamp},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := parseLegacyTimestamp(test.value, time.UTC)
			if err != nil {
				t.Fatalf("parse legacy timestamp: %v", err)
			}
			if !got.Equal(test.want) {
				t.Fatalf("parsed timestamp = %s, want %s", got, test.want)
			}
		})
	}
}

func openTimestampMigrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if err := db.Exec(`CREATE TABLE logs (id INTEGER PRIMARY KEY, time TIMESTAMP)`).Error; err != nil {
		t.Fatalf("create logs table: %v", err)
	}
	return db
}

func insertLegacyLogTime(t *testing.T, db *gorm.DB, value string) {
	t.Helper()
	if err := db.Exec(`INSERT INTO logs (time) VALUES (?)`, value).Error; err != nil {
		t.Fatalf("insert legacy timestamp: %v", err)
	}
}

func assertMigratedLogTime(t *testing.T, db *gorm.DB, want time.Time) {
	t.Helper()
	var got time.Time
	if err := db.Raw(`SELECT time FROM logs LIMIT 1`).Scan(&got).Error; err != nil {
		t.Fatalf("read migrated timestamp: %v", err)
	}
	if !got.Equal(want) || got.Nanosecond() != want.Nanosecond() {
		t.Fatalf("migrated time = %s, want %s", got, want)
	}

	var raw string
	if err := db.Raw(`SELECT CAST(time AS TEXT) FROM logs LIMIT 1`).Scan(&raw).Error; err != nil {
		t.Fatalf("read raw migrated timestamp: %v", err)
	}
	if !strings.HasSuffix(raw, "+00:00") {
		t.Fatalf("stored timestamp %q has no explicit UTC offset", raw)
	}
}

func TestCustomTimestampColumnsMigrateLegacyUTC8AndZeroValues(t *testing.T) {
	t.Setenv("TZ", "Asia/Shanghai")
	db := openTimestampMigrationDB(t)
	createCustomTimestampTables(t, db)
	for _, target := range customLegacyTimestampColumns {
		column := quoteSQLiteIdentifier(target.column)
		table := quoteSQLiteIdentifier(target.table)
		for _, value := range []any{"2026-07-17 09:30:00.123456789", "0001-01-01 00:00:00", nil, ""} {
			if err := db.Exec("INSERT INTO "+table+" ("+column+") VALUES (?)", value).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := migrateLegacyTimestampColumns(db); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 7, 17, 1, 30, 0, 123456789, time.UTC)
	for _, target := range customLegacyTimestampColumns {
		var values []string
		query := "SELECT CAST(" + quoteSQLiteIdentifier(target.column) + " AS TEXT) FROM " + quoteSQLiteIdentifier(target.table) + " WHERE " + quoteSQLiteIdentifier(target.column) + " IS NOT NULL ORDER BY rowid"
		if err := db.Raw(query).Scan(&values).Error; err != nil {
			t.Fatal(err)
		}
		if len(values) != 3 {
			t.Fatalf("%s.%s got %d non-null values", target.table, target.column, len(values))
		}
		stamp, err := parseLegacyTimestamp(values[0], time.UTC)
		if err != nil || !stamp.Equal(want) || !strings.HasSuffix(values[0], "+00:00") {
			t.Fatalf("%s.%s timestamp = %q, err=%v", target.table, target.column, values[0], err)
		}
		zero, err := parseLegacyTimestamp(values[1], time.UTC)
		if err != nil || !zero.IsZero() {
			t.Fatalf("%s.%s zero changed to %q, err=%v", target.table, target.column, values[1], err)
		}
		if values[2] != "" {
			t.Fatalf("%s.%s empty timestamp changed", target.table, target.column)
		}
	}
}

func TestCustomTimestampMigrationAfterOfficialMarkerIsIdempotent(t *testing.T) {
	t.Setenv("TZ", "Asia/Shanghai")
	db := openTimestampMigrationDB(t)
	createCustomTimestampTables(t, db)
	if err := db.Exec(`CREATE TABLE configs (key TEXT PRIMARY KEY, value TEXT)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO configs (key, value) VALUES (?, 'true')`, timestampUTCMigrationKey).Error; err != nil {
		t.Fatal(err)
	}
	insertLegacyLogTime(t, db, "2026-07-17 01:30:00+00:00")
	if err := db.Exec(`INSERT INTO clients (vnstat_baseline_at) VALUES ('2026-07-17 09:30:00')`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateLegacyTimestampColumns(db); err != nil {
		t.Fatal(err)
	}
	if err := markTimestampMigrationDone(db); err != nil {
		t.Fatal(err)
	}
	if !timestampMigrationKeyDone(db, customTimestampUTCMigrationKey) {
		t.Fatal("custom timestamp completion marker not recorded")
	}
	if err := migrateLegacyTimestampColumns(db); err != nil {
		t.Fatal(err)
	}
	var baseline time.Time
	if err := db.Raw(`SELECT vnstat_baseline_at FROM clients LIMIT 1`).Scan(&baseline).Error; err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 7, 17, 1, 30, 0, 0, time.UTC)
	if !baseline.Equal(want) {
		t.Fatalf("baseline = %s, want %s", baseline, want)
	}
	assertMigratedLogTime(t, db, want)
}

func TestLegacyZeroTimestampDoesNotAcquireTimezoneOffset(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"0001-01-01", "0001-01-01 00:00:00", "0001-01-01T00:00:00", "0001-01-01 00:00:00.000000000", "0001-01-01T00:00:00Z"} {
		got, err := parseLegacyTimestamp(raw, location)
		if err != nil || !got.IsZero() {
			t.Errorf("parse %q = %s, %v; want zero", raw, got, err)
		}
	}
	got, err := parseLegacyTimestamp("0", location)
	if err != nil || !got.Equal(time.Unix(0, 0)) {
		t.Fatalf("epoch zero must remain 1970: %s, %v", got, err)
	}
}

func createCustomTimestampTables(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, stmt := range []string{
		`CREATE TABLE clients (id INTEGER PRIMARY KEY, vnstat_baseline_at TIMESTAMP, vnstat_updated_at TIMESTAMP)`,
		`CREATE TABLE ssh_login_events (id INTEGER PRIMARY KEY, occurred_at TIMESTAMP, created_at TIMESTAMP)`,
		`CREATE TABLE traffic_reset_reminders (id INTEGER PRIMARY KEY, sent_at TIMESTAMP)`,
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatal(err)
		}
	}
}
