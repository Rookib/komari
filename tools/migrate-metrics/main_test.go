package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateOptionsRejectsUnsafeTargetsBeforeMutation(t *testing.T) {
	for _, name := range []string{"missing acknowledgement", "missing backup", "nested backup", "hardlink backup", "hardlink main metric", "hardlink snapshot databases", "cross-database backup hardlink", "outside metric", "network metric", "legacy history", "pending restore", "invalid timezone"} {
		t.Run(name, func(t *testing.T) {
			opts := createFixture(t)
			switch name {
			case "missing acknowledgement":
				opts.confirmOffline = false
			case "missing backup":
				opts.backupDir = filepath.Join(t.TempDir(), "absent")
			case "nested backup":
				opts.backupDir = filepath.Join(opts.dataDir, "nested")
				if err := os.Mkdir(opts.backupDir, 0700); err != nil {
					t.Fatal(err)
				}
			case "hardlink backup":
				path := filepath.Join(opts.backupDir, "metrics.db")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(filepath.Join(opts.dataDir, "metrics.db"), path); err != nil {
					t.Fatal(err)
				}
			case "hardlink main metric", "hardlink snapshot databases", "cross-database backup hardlink":
				source := filepath.Join(opts.dataDir, "komari.db")
				target := filepath.Join(opts.dataDir, "metrics.db")
				if name == "hardlink snapshot databases" {
					source = filepath.Join(opts.backupDir, "komari.db")
					target = filepath.Join(opts.backupDir, "metrics.db")
				} else if name == "cross-database backup hardlink" {
					source = filepath.Join(opts.dataDir, "metrics.db")
					target = filepath.Join(opts.backupDir, "komari.db")
				}
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(source, target); err != nil {
					t.Fatal(err)
				}
			case "outside metric":
				setFixtureConfig(t, opts, "metric_db_dsn", filepath.Join(opts.backupDir, "metrics.db"))
			case "network metric":
				setFixtureConfig(t, opts, "metric_db_driver", "postgresql")
				setFixtureConfig(t, opts, "metric_db_dsn", "postgresql://secret-user:secret-password@example.invalid/db")
			case "legacy history":
				db := openFixtureDB(t, filepath.Join(opts.dataDir, "komari.db"))
				mustExec(t, db, `CREATE TABLE records (id INTEGER)`)
				mustExec(t, db, `INSERT INTO records VALUES (1)`)
				db.Close()
			case "pending restore":
				if err := os.WriteFile(filepath.Join(opts.dataDir, "backup.zip"), []byte("pending"), 0600); err != nil {
					t.Fatal(err)
				}
			case "invalid timezone":
				opts.timezone = "not/a/timezone"
			}
			before := fileDigest(t, filepath.Join(opts.dataDir, "komari.db"))
			metricsBefore := fileDigest(t, filepath.Join(opts.dataDir, "metrics.db"))
			_, err := validateOptions(opts)
			if err == nil {
				t.Fatal("unsafe input was accepted")
			}
			if strings.Contains(err.Error(), "secret-password") {
				t.Fatal("error leaked credentials")
			}
			if after := fileDigest(t, filepath.Join(opts.dataDir, "komari.db")); before != after {
				t.Fatal("preflight changed main database")
			}
			if after := fileDigest(t, filepath.Join(opts.dataDir, "metrics.db")); metricsBefore != after {
				t.Fatal("preflight changed metric database")
			}
		})
	}
}

func TestResolveMetricPathRejectsEscapeAndMemory(t *testing.T) {
	opts := createFixture(t)
	opts.dataDir, _ = existingDir(opts.dataDir)
	opts.backupDir, _ = existingDir(opts.backupDir)
	for _, dsn := range []string{":memory:", "file::memory:?cache=shared", "file://remote-host/metrics.db", "../snapshot/metrics.db", "postgresql://secret@example.invalid/database"} {
		if _, err := resolveMetricPath(opts.dataDir, "sqlite", dsn); err == nil {
			t.Errorf("accepted %q", dsn)
		}
	}
	outside := filepath.Join(opts.backupDir, "metrics.db")
	if err := os.Symlink(outside, filepath.Join(opts.dataDir, "escape.db")); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveMetricPath(opts.dataDir, "sqlite", "escape.db"); err == nil {
		t.Fatal("accepted symlink escape")
	}
	for _, dsn := range []string{"", "./data/metrics.db", "metrics.db", "file:./data/metrics.db?mode=rwc"} {
		got, err := resolveMetricPath(opts.dataDir, "sqlite", dsn)
		if err != nil || got != filepath.Join(opts.dataDir, "metrics.db") {
			t.Fatalf("resolve %q: %q %v", dsn, got, err)
		}
	}
}

func TestOfflineMigrationPreservesHistoryBackupAndCanRerun(t *testing.T) {
	opts := createFixture(t)
	backupMain := fileDigest(t, filepath.Join(opts.backupDir, "komari.db"))
	backupMetrics := fileDigest(t, filepath.Join(opts.backupDir, "metrics.db"))
	for attempt := 0; attempt < 2; attempt++ {
		payload, _ := json.Marshal(optsForChild{Data: opts.dataDir, Backup: opts.backupDir})
		command := exec.Command(os.Args[0], "-test.run=^TestMigrationSubprocess$")
		command.Env = append(os.Environ(), "KOMARI_MIGRATION_TEST="+string(payload))
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("migration attempt %d: %v\n%s", attempt, err, output)
		}
		if !strings.Contains(string(output), `"status":"complete"`) || strings.Contains(string(output), "sensitive-fixture-token") {
			t.Fatalf("unexpected status output: %s", output)
		}
		db := openFixtureDB(t, filepath.Join(opts.dataDir, "metrics.db"))
		for _, value := range []float64{42, 7} {
			var count int
			if err := db.QueryRow(`SELECT count(*) FROM metric_rollups WHERE last_val = ?`, value).Scan(&count); err != nil || count == 0 {
				t.Fatalf("historical value %v missing: count=%d err=%v", value, count, err)
			}
		}
		db.Close()
		mainDB := openFixtureDB(t, filepath.Join(opts.dataDir, "komari.db"))
		var marker string
		if err := mainDB.QueryRow(`SELECT value FROM gpu_records_long_term WHERE id=1`).Scan(&marker); err != nil || marker != "preserve-this-table" {
			t.Fatal("unrelated custom history changed")
		}
		mainDB.Close()
		if fileDigest(t, filepath.Join(opts.backupDir, "komari.db")) != backupMain || fileDigest(t, filepath.Join(opts.backupDir, "metrics.db")) != backupMetrics {
			t.Fatal("independent backup was modified")
		}
		if _, err := os.Stat(filepath.Join(opts.dataDir, ".metric-migration.lock")); !os.IsNotExist(err) {
			t.Fatal("migration lock not cleaned up")
		}
	}
}

type optsForChild struct{ Data, Backup string }

func TestMigrationSubprocess(t *testing.T) {
	raw := os.Getenv("KOMARI_MIGRATION_TEST")
	if raw == "" {
		return
	}
	var paths optsForChild
	if err := json.Unmarshal([]byte(raw), &paths); err != nil {
		t.Fatal(err)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := run(context.Background(), options{dataDir: paths.Data, backupDir: paths.Backup, timezone: "Asia/Shanghai", confirmOffline: true}); err != nil {
		t.Fatal(err)
	}
}

func createFixture(t *testing.T) options {
	t.Helper()
	root := t.TempDir()
	opts := options{dataDir: filepath.Join(root, "data"), backupDir: filepath.Join(root, "snapshot"), timezone: "Asia/Shanghai", confirmOffline: true}
	for _, dir := range []string{opts.dataDir, opts.backupDir} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	main := openFixtureDB(t, filepath.Join(opts.dataDir, "komari.db"))
	mustExec(t, main, `CREATE TABLE configs (key TEXT PRIMARY KEY,value TEXT)`)
	mustExec(t, main, `INSERT INTO configs VALUES ('metric_db_driver','"sqlite"'),('metric_db_dsn','"./data/metrics.db"'),('api_key','"sensitive-fixture-token"')`)
	mustExec(t, main, `CREATE TABLE gpu_records_long_term (id INTEGER PRIMARY KEY,value TEXT)`)
	mustExec(t, main, `INSERT INTO gpu_records_long_term VALUES (1,'preserve-this-table')`)
	main.Close()
	metrics := openFixtureDB(t, filepath.Join(opts.dataDir, "metrics.db"))
	for _, stmt := range []string{
		`CREATE TABLE metric_definitions (name TEXT PRIMARY KEY,type TEXT NOT NULL,unit TEXT NOT NULL,description TEXT NOT NULL,retention_days INTEGER NOT NULL,metadata TEXT NOT NULL,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL)`,
		`CREATE TABLE metric_points (id INTEGER PRIMARY KEY,metric_name TEXT NOT NULL,entity_id TEXT NOT NULL,tags_hash TEXT NOT NULL,ts_nano INTEGER NOT NULL,value REAL NOT NULL,tags TEXT NOT NULL,labels TEXT NOT NULL,created_at INTEGER NOT NULL)`,
		`CREATE TABLE metric_rollups (id INTEGER PRIMARY KEY,metric_name TEXT NOT NULL,entity_id TEXT NOT NULL,tags_hash TEXT NOT NULL,tags TEXT NOT NULL,resolution_nano INTEGER NOT NULL,bucket_nano INTEGER NOT NULL,count INTEGER NOT NULL,sum REAL NOT NULL,sum_sq REAL NOT NULL,min_val REAL NOT NULL,max_val REAL NOT NULL,first_val REAL NOT NULL,first_ts INTEGER NOT NULL,last_val REAL NOT NULL,last_ts INTEGER NOT NULL,digest BLOB,created_at INTEGER NOT NULL)`,
		`CREATE TABLE metric_compaction_watermarks (metric_name TEXT PRIMARY KEY,watermark_nano INTEGER NOT NULL,updated_at INTEGER NOT NULL)`,
	} {
		mustExec(t, metrics, stmt)
	}
	base := time.Now().UTC().Truncate(time.Minute).Add(-2 * time.Minute)
	old := base.Add(-20 * 24 * time.Hour).Truncate(24 * time.Hour)
	mustExec(t, metrics, `INSERT INTO metric_definitions VALUES (?,?,?,?,?,?,?,?)`, "cpu", "gauge", "", "", 30, "{}", base.UnixNano(), base.UnixNano())
	mustExec(t, metrics, `INSERT INTO metric_points VALUES (?,?,?,?,?,?,?,?,?)`, 1, "cpu", "node-a", "hash", base.UnixNano(), 42.0, `{"host":"a"}`, `{"source":"test"}`, base.UnixNano())
	mustExec(t, metrics, `INSERT INTO metric_rollups VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, 1, "cpu", "node-a", "hash", `{"host":"a"}`, (24 * time.Hour).Nanoseconds(), old.UnixNano(), 1, 7.0, 49.0, 7.0, 7.0, 7.0, old.UnixNano(), 7.0, old.UnixNano(), nil, base.UnixNano())
	metrics.Close()
	for _, name := range []string{"komari.db", "metrics.db"} {
		data, err := os.ReadFile(filepath.Join(opts.dataDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(opts.backupDir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return opts
}

func openFixtureDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	return db
}
func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}
func setFixtureConfig(t *testing.T, opts options, key, value string) {
	t.Helper()
	db := openFixtureDB(t, filepath.Join(opts.dataDir, "komari.db"))
	defer db.Close()
	raw, _ := json.Marshal(value)
	mustExec(t, db, "UPDATE configs SET value=? WHERE key=?", string(raw), key)
}
func fileDigest(t *testing.T, path string) [32]byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(content)
}
