// migrate-metrics upgrades an existing local metric store while Komari is
// stopped. It never starts providers, notification jobs, agents, or an HTTP
// listener. Run in a container with --network none and a read-only backup mount.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/komari-monitor/komari/cmd/flags"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/internal/config"
	"github.com/komari-monitor/komari/internal/metricstore"
	"github.com/komari-monitor/komari/internal/migrations"
)

type options struct {
	dataDir, backupDir, timezone string
	confirmOffline               bool
}

type paths struct{ data, backup, mainDB, metricsDB string }

type event struct {
	Status      string `json:"status"`
	Phase       string `json:"phase"`
	Error       string `json:"error,omitempty"`
	Timezone    string `json:"legacy_timezone,omitempty"`
	RowsDone    int64  `json:"rows_done,omitempty"`
	RowsTotal   int64  `json:"rows_total,omitempty"`
	Metrics     int    `json:"metrics,omitempty"`
	BeforeBytes int64  `json:"before_bytes,omitempty"`
	AfterBytes  int64  `json:"after_bytes,omitempty"`
}

func main() {
	var opts options
	flag.StringVar(&opts.dataDir, "data-dir", "", "Existing stopped-service data directory (required)")
	flag.StringVar(&opts.backupDir, "backup-dir", "", "Existing independent snapshot directory containing the database files (required)")
	flag.StringVar(&opts.timezone, "legacy-timezone", os.Getenv("TZ"), "Original service TZ; empty means UTC")
	flag.BoolVar(&opts.confirmOffline, "confirm-offline", false, "Confirm all processes using this data directory are stopped")
	flag.Parse()
	if flag.NArg() != 0 {
		emit(event{Status: "failed", Phase: "arguments", Error: "unexpected positional arguments"})
		os.Exit(2)
	}
	// SQL errors can include complete queries. The CLI emits only bounded,
	// non-sensitive status fields; lower-level SQL/config logging is disabled.
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, opts); err != nil {
		emit(event{Status: "failed", Phase: "migration", Error: err.Error()})
		os.Exit(1)
	}
}

func emit(value event) { _ = json.NewEncoder(os.Stdout).Encode(value) }

func run(ctx context.Context, opts options) error {
	p, err := validateOptions(opts)
	if err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(p.data, ".metric-migration.lock"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("migration lock exists or cannot be created")
	}
	_ = lock.Close()
	defer os.Remove(filepath.Join(p.data, ".metric-migration.lock"))
	if opts.timezone == "" {
		opts.timezone = "UTC"
	}
	if err := os.Setenv("TZ", opts.timezone); err != nil {
		return errors.New("cannot set original timezone")
	}
	if err := os.Chdir(p.data); err != nil {
		return errors.New("cannot enter data directory")
	}
	flags.DatabaseType = flags.DatabaseTypeSQLite
	flags.DatabaseFile = p.mainDB
	// The independent pre-migration snapshot was validated before any writes.
	// Do not update system_version or invoke automatic version backup here.
	dbcore.SetVersionID("")
	emit(event{Status: "running", Phase: "main_database", Timezone: opts.timezone})
	if err := dbcore.Initialize(); err != nil {
		return errors.New("main database migration failed; restore snapshot before retrying")
	}
	defer dbcore.Close()
	mainDB := dbcore.GetDBInstance()
	requiredLegacy, _, err := migrations.LegacyMonitoringMigrationRequired(mainDB)
	if err != nil {
		return errors.New("cannot inspect legacy monitoring history")
	}
	if requiredLegacy {
		return errors.New("legacy SQL monitoring history requires a separately reviewed migration; no history was discarded")
	}
	// Resolve to the already validated file, never create an empty replacement
	// or open a host/database outside the explicitly selected data directory.
	dsn := (&url.URL{Scheme: "file", Path: p.metricsDB}).String() + "?mode=rw&_txlock=immediate"
	if err := config.SetMany(map[string]any{metricstore.MetricDBDriverKey: "sqlite", metricstore.MetricDBDSNKey: dsn}); err != nil {
		return errors.New("cannot bind validated local metric store")
	}
	required, err := metricstore.StructureUpgradeRequired(ctx)
	if err != nil {
		return errors.New("cannot inspect metric store structure")
	}
	if required {
		result, err := metricstore.RestructureConfiguredStore(ctx, func(progress metricstore.RestructureProgress) {
			emit(event{Status: "running", Phase: progress.Phase, RowsDone: progress.RowsDone, RowsTotal: progress.RowsTotal, Metrics: progress.MetricsDone})
		})
		if err != nil {
			return errors.New("metric structure migration failed; retain snapshot for rollback")
		}
		emit(event{Status: "running", Phase: "copied", RowsDone: result.RowsCopied, Metrics: result.Metrics, BeforeBytes: result.BeforeBytes, AfterBytes: result.AfterBytes})
	}
	required, err = metricstore.StructureUpgradeRequired(ctx)
	if err != nil || required {
		return errors.New("metric structure post-check failed")
	}
	if err := mainDB.Exec("PRAGMA wal_checkpoint(TRUNCATE)").Error; err != nil {
		return errors.New("main database checkpoint failed")
	}
	if err := dbcore.Close(); err != nil {
		return errors.New("main database close failed")
	}
	emit(event{Status: "complete", Phase: "verified", Timezone: opts.timezone})
	return nil
}

func validateOptions(opts options) (paths, error) {
	var p paths
	if !opts.confirmOffline {
		return p, errors.New("--confirm-offline is required for a stopped service")
	}
	if opts.dataDir == "" || opts.backupDir == "" {
		return p, errors.New("--data-dir and --backup-dir are required")
	}
	if opts.timezone != "" {
		if _, err := time.LoadLocation(opts.timezone); err != nil {
			return p, errors.New("legacy timezone is not recognized")
		}
	}
	var err error
	if p.data, err = existingDir(opts.dataDir); err != nil {
		return p, errors.New("data directory must already exist")
	}
	if p.backup, err = existingDir(opts.backupDir); err != nil {
		return p, errors.New("backup directory must already exist")
	}
	if inside(p.data, p.backup) || inside(p.backup, p.data) {
		return p, errors.New("backup and data directories must be independent")
	}
	for _, name := range []string{"backup.zip", "data/backup.zip"} {
		if _, err := os.Lstat(filepath.Join(p.data, name)); err == nil || !os.IsNotExist(err) {
			return p, errors.New("pending restore archive must be resolved before migration")
		}
	}
	p.mainDB = filepath.Join(p.data, "komari.db")
	if err := validateSnapshotFile(p.data, p.backup, "komari.db"); err != nil {
		return p, err
	}
	driver, dsn, legacyRows, err := inspectMainReadOnly(p.mainDB)
	if err != nil {
		return p, errors.New("main SQLite preflight failed")
	}
	if legacyRows > 0 {
		return p, errors.New("nonempty legacy monitoring tables require a separate migration; no changes made")
	}
	if p.metricsDB, err = resolveMetricPath(p.data, driver, dsn); err != nil {
		return p, err
	}
	rel, err := filepath.Rel(p.data, p.metricsDB)
	if err != nil {
		return p, errors.New("metric path cannot be resolved")
	}
	if rel == "komari.db" {
		return p, errors.New("main and metric databases must be separate files")
	}
	if err := validateSnapshotFile(p.data, p.backup, rel); err != nil {
		return p, err
	}
	return p, nil
}

func existingDir(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", errors.New("not a directory")
	}
	return canonical, nil
}

func inside(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel)
}

func validateSnapshotFile(data, backup, rel string) error {
	var infos []os.FileInfo
	for _, root := range []string{data, backup} {
		path := filepath.Join(root, rel)
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil || !inside(root, canonical) {
			return errors.New("database or snapshot file is missing or outside its directory")
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("database and snapshot must be regular files")
		}
		file, err := os.Open(path)
		if err != nil {
			return errors.New("database or snapshot cannot be read")
		}
		header := make([]byte, 16)
		_, err = io.ReadFull(file, header)
		_ = file.Close()
		if err != nil || string(header) != "SQLite format 3\x00" {
			return errors.New("database or snapshot is not an existing SQLite database")
		}
		infos = append(infos, info)
	}
	if os.SameFile(infos[0], infos[1]) {
		return errors.New("snapshot is a hard link to live data, not an independent copy")
	}
	return nil
}

func resolveMetricPath(data, driver, dsn string) (string, error) {
	canonicalData, err := existingDir(data)
	if err != nil {
		return "", errors.New("data directory must already exist")
	}
	data = canonicalData
	if driver != "" && strings.ToLower(driver) != "sqlite" {
		return "", errors.New("offline migration supports only a local SQLite metric store")
	}
	raw := strings.TrimSpace(dsn)
	if raw == "" {
		raw = "./data/metrics.db"
	}
	if strings.HasPrefix(raw, "file:") {
		u, err := url.Parse(raw)
		if err != nil || u.Host != "" {
			return "", errors.New("unsupported metric file URI")
		}
		raw = u.Path
		if raw == "" {
			raw = u.Opaque
		}
	} else if strings.Contains(raw, "://") || strings.Contains(raw, "@") || strings.Contains(raw, "=") {
		return "", errors.New("metric store DSN must name a local SQLite file")
	}
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		raw = raw[:i]
	}
	if raw == ":memory:" || strings.ContainsRune(raw, '\x00') {
		return "", errors.New("metric store must be a persistent SQLite file")
	}
	raw = strings.TrimPrefix(raw, "./")
	if strings.HasPrefix(raw, "data/") {
		raw = strings.TrimPrefix(raw, "data/")
	}
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(data, raw)
	}
	canonical, err := filepath.EvalSymlinks(raw)
	if err != nil || !inside(data, canonical) {
		return "", errors.New("configured metric file must exist inside --data-dir")
	}
	return canonical, nil
}

func inspectMainReadOnly(path string) (driver, dsn string, legacyRows int64, err error) {
	uri := (&url.URL{Scheme: "file", Path: path}).String() + "?mode=ro"
	db, err := sql.Open("sqlite3", uri)
	if err != nil {
		return "", "", 0, err
	}
	defer db.Close()
	var hasConfigs int
	if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='configs'").Scan(&hasConfigs); err != nil {
		return
	}
	if hasConfigs != 1 {
		err = errors.New("configuration table missing")
		return
	}
	for _, key := range []string{metricstore.MetricDBDriverKey, metricstore.MetricDBDSNKey} {
		var value string
		scanErr := db.QueryRow("SELECT value FROM configs WHERE key = ?", key).Scan(&value)
		if errors.Is(scanErr, sql.ErrNoRows) {
			continue
		}
		if scanErr != nil {
			err = scanErr
			return
		}
		var decoded string
		if err = json.Unmarshal([]byte(value), &decoded); err != nil {
			return
		}
		if key == metricstore.MetricDBDriverKey {
			driver = decoded
		} else {
			dsn = decoded
		}
	}
	for _, table := range []string{"records", "records_long_term", "gpu_records", "ping_records"} {
		var count int64
		if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&count); err != nil {
			return
		}
		if count == 0 {
			continue
		}
		if err = db.QueryRow(fmt.Sprintf("SELECT count(*) FROM %q", table)).Scan(&count); err != nil {
			return
		}
		legacyRows += count
	}
	return
}
