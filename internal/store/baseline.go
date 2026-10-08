package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"shephrd/internal/model"
)

//go:embed testdata/golden-schema/*.txt
var bridgeShapeGoldenFS embed.FS

// bridgeShapeGoldens holds the frozen schema-object sets captured for every
// supported bridge version. A ledger that passes the frozen identity check
// but diverges from its golden shape was hand-edited or partially migrated.
var bridgeShapeGoldens map[int]map[string]bool

// baselineExportClose is the compiled close of the 30-calendar-day legacy
// read/export window, fixed exactly 30 days after the baseline release.
var baselineExportClose = time.Date(2026, 9, 17, 15, 35, 0, 0, time.UTC)

// baselineClock is the wall clock the window refusal reads; tests pin it to
// exercise both sides of the compiled close.
var baselineClock = time.Now

// baselineStageFault is a test seam: when set, the named stage fails inside
// its write transaction before commit so tests can prove the rollback
// boundary leaves the previous schema and ledger intact.
var baselineStageFault func(stage string) error

func baselineFault(stage string) error {
	if baselineStageFault != nil {
		return baselineStageFault(stage)
	}
	return nil
}

// BaselineOptions carries the explicit acceptance inputs for the one-shot
// baseline migrator. LegacyExportDigest is the content-addressed digest of an
// accepted legacy task-list export; it is the only credential that permits
// dropping nonempty legacy_task_* tables.
type BaselineOptions struct {
	LegacyExportDigest string
}

// Open opens the state database at path, applying the bounded compatibility
// contract: fresh databases are created directly on the baseline, databases
// at the supported bridge versions are upgraded one-shot, and anything
// outside that window is refused with recovery instructions before any
// database change.
func Open(path string) (*Store, error) {
	return OpenWithOptions(path, BaselineOptions{LegacyExportDigest: legacyExportDigestFromEnvironment()})
}

func legacyExportDigestFromEnvironment() string {
	return strings.ToLower(strings.TrimSpace(os.Getenv("SHEPHRD_LEGACY_EXPORT_DIGEST")))
}

type classificationKind int

const (
	classificationFresh classificationKind = iota
	classificationBelowFloor
	classificationUpgrade
	classificationBaseline
	classificationFuture
)

type databaseClassification struct {
	kind     classificationKind
	version  int
	recorded map[int]recordedMigration
}

// classifyDatabase opens the database read-only and determines which
// compatibility path applies. It never creates directories, sets WAL mode,
// creates schema objects, or writes anything: below-floor and future
// databases are refused before a read-write handle exists.
func classifyDatabase(path string) (databaseClassification, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return databaseClassification{kind: classificationFresh}, nil
	}
	if err != nil {
		return databaseClassification{}, fmt.Errorf("stat database %s: %w", path, err)
	}
	if info.Size() == 0 {
		return databaseClassification{kind: classificationFresh}, nil
	}
	// The read-only handle still carries the busy timeout: while a concurrent
	// fresh bootstrap holds its write lock, classification waits rather than
	// failing the open with a spurious lock error.
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro&_busy_timeout=%d", path, sqliteBusyTimeout/time.Millisecond))
	if err != nil {
		return databaseClassification{}, fmt.Errorf("open database read-only: %w", err)
	}
	defer db.Close()
	var tableCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&tableCount); err != nil {
		return databaseClassification{}, fmt.Errorf("read database %s: %w", path, err)
	}
	if tableCount == 0 {
		return databaseClassification{kind: classificationFresh}, nil
	}
	identityColumns := false
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('schema_migrations') WHERE name IN ('name', 'checksum')`).Scan(&count); err == nil {
		identityColumns = count == 2
	}
	query := `SELECT version, '', '' FROM schema_migrations ORDER BY version`
	if identityColumns {
		query = `SELECT version, name, checksum FROM schema_migrations ORDER BY version`
	}
	rows, err := db.Query(query)
	if err != nil {
		return databaseClassification{}, fmt.Errorf("read schema history of %s: %w", path, err)
	}
	recorded := make(map[int]recordedMigration)
	version := 0
	for rows.Next() {
		var v int
		var record recordedMigration
		if err := rows.Scan(&v, &record.name, &record.checksum); err != nil {
			rows.Close()
			return databaseClassification{}, fmt.Errorf("read schema history of %s: %w", path, err)
		}
		recorded[v] = record
		if v > version {
			version = v
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return databaseClassification{}, fmt.Errorf("read schema history of %s: %w", path, err)
	}
	classification := databaseClassification{kind: classificationUpgrade, version: version, recorded: recorded}
	switch {
	case version == 0:
		classification.kind = classificationFresh
	case version > 30:
		classification.kind = classificationFuture
	case version < bridgeFloorVersion:
		classification.kind = classificationBelowFloor
	case recorded[baselineVersion].name == baselineName && recorded[baselineVersion].checksum == baselineChecksum():
		classification.kind = classificationBaseline
	}
	return classification, nil
}

// belowFloorRefusal is the exact recovery contract for schemas 1-14: no
// in-place upgrade, a bounded read-only export window, and a named signed
// legacy upgrader.
func belowFloorRefusal(version int, now time.Time) error {
	if !now.Before(baselineExportClose) {
		return fmt.Errorf("legacy schema support closed at %s; database schema %d cannot be opened by this binary. Restore a backup and use the archived signed legacy exporter; do not delete schema_migrations rows", baselineExportClose.Format(time.RFC3339), version)
	}
	return fmt.Errorf("database schema %d is below the minimum directly upgradable schema 15; refusing to migrate or write. Use the signed Shephrd v23 legacy upgrader or read-only exporter before %s; no database changes were made", version, baselineExportClose.Format(time.RFC3339))
}

func futureRefusal(version int) error {
	return fmt.Errorf("database records schema migration %d, which this binary does not know; refusing to open a database written by a newer or incompatible shephrd", version)
}

func OpenWithOptions(path string, opts BaselineOptions) (*Store, error) {
	classification, err := classifyDatabase(path)
	if err != nil {
		return nil, err
	}
	// Refused classifications must not touch the database read-write: no WAL
	// conversion, no sidecar files, no directory creation, byte for byte.
	switch classification.kind {
	case classificationBelowFloor:
		return nil, belowFloorRefusal(classification.version, baselineClock().UTC())
	case classificationFuture:
		return nil, futureRefusal(classification.version)
	}
	store, err := openReadWrite(path)
	if err != nil {
		return nil, err
	}
	if classification.version == 30 {
		if err := store.upgradeSubdrivers(); err != nil {
			store.Close()
			return nil, err
		}
		if err := store.validateBaselineShape(); err != nil {
			store.Close()
			return nil, err
		}
		return store, nil
	}
	switch classification.kind {
	case classificationFresh:
		if err := store.createBaseline(); err != nil {
			store.Close()
			return nil, err
		}
		if err := store.upgradeSubdrivers(); err != nil {
			store.Close()
			return nil, err
		}
		return store, nil
	case classificationBaseline:
		if err := store.validateBaselineShape(); err != nil {
			store.Close()
			return nil, err
		}
		if err := store.upgradeSubdrivers(); err != nil {
			store.Close()
			return nil, err
		}
		return store, nil
	default:
		if err := store.upgradeToBaseline(classification, opts); err != nil {
			store.Close()
			return nil, err
		}
		if err := store.upgradeSubdrivers(); err != nil {
			store.Close()
			return nil, err
		}
		return store, nil
	}
}

func openReadWrite(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	dsn := fmt.Sprintf("%s%s_busy_timeout=%d&_foreign_keys=on&_txlock=immediate", path, separator, sqliteBusyTimeout/time.Millisecond)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(8)
	var journalMode string
	if err := retrySQLiteSetup(func(ctx context.Context) error {
		return db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&journalMode)
	}); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure database: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		db.Close()
		return nil, fmt.Errorf("configure database: journal mode is %q, want WAL", journalMode)
	}
	return &Store{db: db, path: path}, nil
}

// baselineProvenanceDDL is the one schema object the bridge chain does not
// create; the finalization transaction adds it so upgraded databases carry
// the identical stored DDL as fresh ones.
const baselineProvenanceDDL = `CREATE TABLE IF NOT EXISTS schema_baseline_provenance (
    baseline_version INTEGER NOT NULL,
    baseline_name TEXT NOT NULL,
    baseline_checksum TEXT NOT NULL CHECK(length(baseline_checksum) = 64 AND baseline_checksum = lower(baseline_checksum)),
    original_version INTEGER NOT NULL DEFAULT 0,
    old_ledger_digest TEXT NOT NULL DEFAULT '' CHECK(length(old_ledger_digest) IN (0, 64) AND (old_ledger_digest = '' OR old_ledger_digest = lower(old_ledger_digest))),
    baseline_released_at TEXT NOT NULL,
    legacy_export_closes_at TEXT NOT NULL,
    upgraded_at TEXT NOT NULL
);`

// createBaseline executes the consolidated body once on a fresh database and
// records the single baseline identity plus its provenance row.
func (s *Store) createBaseline() error {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	var ledgerRows int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&ledgerRows); err != nil {
		return err
	}
	if ledgerRows != 0 {
		// A concurrent open created the baseline while we waited on the write
		// lock; verify it rather than re-running the body.
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return err
		}
		committed = true
		return s.validateBaselineShape()
	}
	if _, err := conn.ExecContext(ctx, baselineBody); err != nil {
		return fmt.Errorf("create baseline schema: %w", err)
	}
	if err := insertBaselineLedger(conn, 0, ""); err != nil {
		return err
	}
	if err := validateBaselineShapeOn(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(context.Background(), "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

func insertBaselineLedger(conn *sql.Conn, originalVersion int, oldLedgerDigest string) error {
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, `DELETE FROM schema_migrations`); err != nil {
		return fmt.Errorf("rebuild baseline ledger: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO schema_migrations(version, name, checksum, applied_at) VALUES(?, ?, ?, ?)`,
		baselineVersion, baselineName, baselineChecksum(), now()); err != nil {
		return fmt.Errorf("record baseline identity: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM schema_baseline_provenance`); err != nil {
		return fmt.Errorf("clear previous baseline provenance: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO schema_baseline_provenance(baseline_version, baseline_name, baseline_checksum, original_version, old_ledger_digest, baseline_released_at, legacy_export_closes_at, upgraded_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		baselineVersion, baselineName, baselineChecksum(), originalVersion, oldLedgerDigest, baselineReleasedAt, baselineExportClose.Format(time.RFC3339), now()); err != nil {
		return fmt.Errorf("record baseline provenance: %w", err)
	}
	return nil
}

// upgradeToBaseline runs the one-shot compatibility bridge and baseline
// finalization. Every precondition is re-asserted inside the final write
// transaction, so a concurrent change between classification and the write
// is refused rather than migrated.
func (s *Store) upgradeToBaseline(classification databaseClassification, opts BaselineOptions) error {
	if err := validateBridgeLedger(classification.recorded, classification.version); err != nil {
		return err
	}
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		return err
	}
	defer conn.Close()
	compatibility, err := validateBridgeShape(conn, classification)
	if err != nil {
		return err
	}
	ledgerDigest, err := ledgerDigest(conn)
	if err != nil {
		return err
	}
	preflight, err := captureBaselinePreflight(s.db, classification.version)
	if err != nil {
		return err
	}
	if err := s.assertBaselineSourcePreconditions(classification.version, opts); err != nil {
		return err
	}
	backupPath, err := s.createBackup()
	if err != nil {
		return err
	}
	if err := s.writeBackupManifest(backupPath, classification.version, ledgerDigest, preflight); err != nil {
		return err
	}
	recorded := make(map[int]bool, len(classification.recorded))
	for version := range classification.recorded {
		recorded[version] = true
	}
	if err := s.applyBridgeMigrations(recorded); err != nil {
		return err
	}
	if err := s.finalizeBaseline(conn, classification.version, ledgerDigest, compatibility, opts); err != nil {
		return err
	}
	if err := s.validateBaselineShape(); err != nil {
		return err
	}
	postflight, err := captureBaselinePreflight(s.db, baselineVersion)
	if err != nil {
		return err
	}
	if mismatch := preflight.compare(postflight); mismatch != "" {
		return fmt.Errorf("baseline postflight retained-evidence mismatch: %s; stop all processes and restore %s with its retained-data manifest together", mismatch, backupPath)
	}
	return nil
}

func (s *Store) createBackup() (string, error) {
	path := fmt.Sprintf("%s.baseline-backup-%s", s.path, time.Now().UTC().Format("20060102T150405Z0700"))
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("clear previous baseline backup: %w", err)
	}
	if _, err := s.db.Exec(`VACUUM INTO ?`, path); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("create baseline backup: %w", err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		return "", fmt.Errorf("verify baseline backup: %v", err)
	}
	return path, nil
}

type preflightCounts struct {
	OriginalVersion int `json:"original_version"`
	Tasks           int `json:"tasks"`
	Attempts        int `json:"attempts"`
	HeldAttempts    int `json:"held_attempts"`
	UnknownAttempts int `json:"unknown_workspace_attempts"`
	Messages        int `json:"messages"`
	Checkpoints     int `json:"attempt_checkpoints"`
	Artifacts       int `json:"verified_artifacts"`
	Notifications   int `json:"driver_notifications"`
	HandlerRows     int `json:"report_lifecycle_invocations"`
	DeliveryLog     int `json:"notification_delivery_log"`
	LocalRecoveries int `json:"local_delivery_recoveries"`
	Attestations    int `json:"external_delivery_attestations"`
	RecoveryRows    int `json:"report_recovery_attestations"`
	LandedAttempts  int `json:"landed_attempts"`
	LandedTasks     int `json:"landed_tasks"`
	PlanInputs      int `json:"plan_inputs"`
	ReportInputs    int `json:"task_report_inputs"`
	Snapshots       int `json:"report_snapshots"`
}

func captureBaselinePreflight(db *sql.DB, version int) (preflightCounts, error) {
	var counts preflightCounts
	counts.OriginalVersion = version
	query := func(target *int, sqlText string) error {
		return db.QueryRow(sqlText).Scan(target)
	}
	if err := query(&counts.Tasks, `SELECT COUNT(*) FROM tasks`); err != nil {
		return counts, err
	}
	if err := query(&counts.Attempts, `SELECT COUNT(*) FROM attempts`); err != nil {
		return counts, err
	}
	if err := query(&counts.HeldAttempts, `SELECT COUNT(*) FROM attempts WHERE release_state='held'`); err != nil {
		return counts, err
	}
	if err := query(&counts.UnknownAttempts, `SELECT COUNT(*) FROM attempts WHERE release_state='held' AND workspace_state='unknown'`); err != nil {
		return counts, err
	}
	if err := query(&counts.Messages, `SELECT COUNT(*) FROM messages`); err != nil {
		return counts, err
	}
	if err := query(&counts.Checkpoints, `SELECT COUNT(*) FROM attempt_checkpoints`); err != nil {
		return counts, err
	}
	if err := query(&counts.Artifacts, `SELECT COUNT(*) FROM verified_artifacts`); err != nil {
		return counts, err
	}
	if err := query(&counts.Notifications, `SELECT COUNT(*) FROM driver_notifications`); err != nil {
		return counts, err
	}
	// The handler table only exists from the report-lifecycle bridge step on;
	// earlier sources count zero rows, matching an empty postflight table.
	var handlerTable int
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='report_lifecycle_invocations')`).Scan(&handlerTable); err != nil {
		return counts, err
	}
	if handlerTable != 0 {
		if err := query(&counts.HandlerRows, `SELECT COUNT(*) FROM report_lifecycle_invocations`); err != nil {
			return counts, err
		}
	}
	if err := query(&counts.DeliveryLog, `SELECT COUNT(*) FROM notification_delivery_log`); err != nil {
		return counts, err
	}
	if err := query(&counts.LocalRecoveries, `SELECT COUNT(*) FROM local_delivery_recoveries`); err != nil {
		return counts, err
	}
	if err := query(&counts.Attestations, `SELECT COUNT(*) FROM external_delivery_attestations`); err != nil {
		return counts, err
	}
	var recoveryTable int
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='report_recovery_attestations')`).Scan(&recoveryTable); err != nil {
		return counts, err
	}
	if recoveryTable != 0 {
		if err := query(&counts.RecoveryRows, `SELECT COUNT(*) FROM report_recovery_attestations`); err != nil {
			return counts, err
		}
	}
	if err := query(&counts.LandedAttempts, `SELECT COUNT(*) FROM attempt_landing_projections WHERE landed=1`); err != nil {
		return counts, err
	}
	if err := query(&counts.LandedTasks, `SELECT COUNT(*) FROM task_landing_projections WHERE landed=1`); err != nil {
		return counts, err
	}
	if err := query(&counts.ReportInputs, `SELECT COUNT(*) FROM task_report_inputs`); err != nil {
		return counts, err
	}
	// Plan inputs live in task_list_inputs before the plan-surface step and
	// plan_report_inputs after it; both hold the same relation rows.
	var planInputsTable int
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='task_list_inputs')`).Scan(&planInputsTable); err != nil {
		return counts, err
	}
	if planInputsTable != 0 {
		if err := query(&counts.PlanInputs, `SELECT COUNT(*) FROM task_list_inputs`); err != nil {
			return counts, err
		}
	} else {
		if err := query(&counts.PlanInputs, `SELECT COUNT(*) FROM plan_report_inputs`); err != nil {
			return counts, err
		}
	}
	rows, err := db.Query(`SELECT snapshot_path FROM verified_artifacts WHERE snapshot_path <> ''`)
	if err != nil {
		return counts, err
	}
	defer rows.Close()
	for rows.Next() {
		var snapshotPath string
		if err := rows.Scan(&snapshotPath); err != nil {
			return counts, err
		}
		absolute, err := filepath.Abs(snapshotPath)
		if err != nil {
			return counts, err
		}
		if info, statErr := os.Stat(absolute); statErr == nil && !info.IsDir() {
			counts.Snapshots++
		}
	}
	return counts, rows.Err()
}

func (left preflightCounts) compare(right preflightCounts) string {
	left.OriginalVersion, right.OriginalVersion = 0, 0
	if left == right {
		return ""
	}
	return fmt.Sprintf("before %+v after %+v", left, right)
}

func (s *Store) writeBackupManifest(backupPath string, originalVersion int, ledgerDigest string, preflight preflightCounts) error {
	backup, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("read baseline backup: %w", err)
	}
	sum := sha256.Sum256(backup)
	manifest := map[string]any{
		"backup":                  filepath.Base(backupPath),
		"backup_sha256":           hex.EncodeToString(sum[:]),
		"backup_bytes":            len(backup),
		"original_version":        originalVersion,
		"old_ledger_digest":       ledgerDigest,
		"baseline_released_at":    baselineReleasedAt,
		"legacy_export_closes_at": baselineExportClose.Format(time.RFC3339),
		"created_at":              time.Now().UTC().Format(time.RFC3339),
		"retained":                preflight,
	}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(backupPath+".manifest.json", append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("write baseline backup manifest: %w", err)
	}
	return nil
}

func ledgerDigest(conn *sql.Conn) (string, error) {
	rows, err := conn.QueryContext(context.Background(), `SELECT version, COALESCE(name,''), COALESCE(checksum,''), COALESCE(applied_at,'') FROM schema_migrations ORDER BY version`)
	if err != nil {
		return "", fmt.Errorf("hash schema history: %w", err)
	}
	defer rows.Close()
	sum := sha256.New()
	for rows.Next() {
		var version int
		var name, checksum, appliedAt string
		if err := rows.Scan(&version, &name, &checksum, &appliedAt); err != nil {
			return "", err
		}
		fmt.Fprintf(sum, "%d|%s|%s|%s\n", version, name, checksum, appliedAt)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// applyBridgeMigrations runs the released bridge steps 16-27 in version
// order, each in its own transaction, exactly as the released binary applied
// them. Every intermediate state is a released chain state the previous
// binary can open, so a fault or crash between steps leaves the database at
// the highest fully applied step rather than in a mixed state.
func (s *Store) applyBridgeMigrations(recorded map[int]bool) error {
	bridge, err := loadBridgeMigrations()
	if err != nil {
		return err
	}
	highestRecorded := 0
	for version := range recorded {
		if version > highestRecorded {
			highestRecorded = version
		}
	}
	for _, entry := range bridge {
		if recorded[entry.version] || entry.version < highestRecorded {
			continue
		}
		if err := s.applyBridgeStep(entry); err != nil {
			return fmt.Errorf("apply bridge migration %d (%s): %w", entry.version, entry.name, err)
		}
	}
	return nil
}

func (s *Store) applyBridgeStep(entry bridgeMigration) error {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := baselineFault("bridge-" + strconv.Itoa(entry.version)); err != nil {
		return err
	}
	if entry.disableForeignKeys {
		if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
			return err
		}
		defer func() {
			_, _ = conn.ExecContext(context.Background(), "PRAGMA foreign_keys=ON")
		}()
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if entry.run != nil {
		if err := entry.run(ctx, conn); err != nil {
			return err
		}
	} else if _, err := conn.ExecContext(ctx, entry.sql); err != nil {
		return err
	}
	if entry.disableForeignKeys {
		var violation string
		err := conn.QueryRowContext(ctx, `SELECT "table" FROM pragma_foreign_key_check LIMIT 1`).Scan(&violation)
		if err == nil {
			return fmt.Errorf("bridge migration %d left a foreign key violation in table %s", entry.version, violation)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO schema_migrations(version, name, checksum, applied_at) VALUES(?, ?, ?, ?)`,
		entry.version, entry.name, entry.checksum(), now()); err != nil {
		return fmt.Errorf("record bridge migration %d: %w", entry.version, err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// finalizeBaseline is the single transaction that converges the bridged
// database on the baseline: it re-asserts the complete evidence contract on
// the fully bridged shape, retires the duplicate Herdr columns and the
// exported legacy task-list tables, and rebuilds the ledger to the one
// baseline identity. The bridge backfills (notably migration 18's
// Herdr-to-terminal copy) run first in their released transactions because
// they make the endpoint-equality contract evaluable on the oldest supported
// shapes. A fault anywhere before commit leaves the bridged schema and its
// ledger intact.
func (s *Store) finalizeBaseline(conn *sql.Conn, originalVersion int, ledgerDigest string, compatibility bridgeShapeCompatibility, opts BaselineOptions) error {
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if err := baselineFault("preconditions"); err != nil {
		return err
	}
	if err := s.assertBaselinePreconditions(ctx, conn, baselineVersion, opts); err != nil {
		return err
	}
	if err := s.ensureBaseSelectionColumns(ctx, conn); err != nil {
		return err
	}
	if err := baselineFault("endpoint-retirement"); err != nil {
		return err
	}
	if err := s.retireDuplicateEndpointColumns(ctx, conn); err != nil {
		return err
	}
	if err := baselineFault("legacy-drop"); err != nil {
		return err
	}
	if err := s.dropLegacyTaskTables(ctx, conn); err != nil {
		return err
	}
	if compatibility == bridgeShapeReleasedLive {
		if _, err := conn.ExecContext(ctx, releasedLiveVerifiedArtifactTriggers); err != nil {
			return fmt.Errorf("restore verified artifact immutability: %w", err)
		}
	}
	if err := baselineFault("ledger-rebuild"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, baselineProvenanceDDL); err != nil {
		return fmt.Errorf("create baseline provenance: %w", err)
	}
	if err := insertBaselineLedger(conn, originalVersion, ledgerDigest); err != nil {
		return err
	}
	if err := validateBaselineShapeOn(conn); err != nil {
		return err
	}
	if err := baselineFault("commit"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

func (s *Store) ensureBaseSelectionColumns(ctx context.Context, conn *sql.Conn) error {
	var strategy, ref int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('attempts') WHERE name='base_strategy'`).Scan(&strategy); err != nil {
		return err
	}
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('attempts') WHERE name='base_ref'`).Scan(&ref); err != nil {
		return err
	}
	if strategy == 1 && ref == 1 {
		return nil
	}
	if strategy != 0 || ref != 0 {
		return fmt.Errorf("base-selection columns are incomplete; refusing to converge the schema")
	}
	if _, err := conn.ExecContext(ctx, baseSelectionSchema); err != nil {
		return fmt.Errorf("add base-selection columns: %w", err)
	}
	return nil
}

func (s *Store) retireDuplicateEndpointColumns(ctx context.Context, conn *sql.Conn) error {
	for _, column := range []string{"herdr_socket_path", "herdr_workspace_id", "herdr_tab_id", "herdr_pane_id"} {
		var present int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('attempts') WHERE name=?`, column).Scan(&present); err != nil {
			return fmt.Errorf("inspect duplicate endpoint column %s: %w", column, err)
		}
		if present == 0 {
			continue
		}
		if _, err := conn.ExecContext(ctx, `ALTER TABLE attempts DROP COLUMN `+column); err != nil {
			return fmt.Errorf("retire duplicate endpoint column %s: %w", column, err)
		}
	}
	return nil
}

func (s *Store) dropLegacyTaskTables(ctx context.Context, conn *sql.Conn) error {
	for _, table := range legacyTaskTables {
		if _, err := conn.ExecContext(ctx, `DROP TABLE IF EXISTS `+table); err != nil {
			return fmt.Errorf("drop exported legacy table %s: %w", table, err)
		}
	}
	return nil
}

// assertBaselineSourcePreconditions enforces the complete evidence contract
// on the source database in one read snapshot before any bridge transaction
// writes. The version guards cover columns and tables the source shape does
// not carry yet; the finalization transaction re-asserts the full contract on
// the fully bridged shape, so a concurrent change between the two checks is
// still refused.
func (s *Store) assertBaselineSourcePreconditions(version int, opts BaselineOptions) error {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	deferred := true
	defer func() {
		if deferred {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	err = s.assertBaselinePreconditions(ctx, conn, version, opts)
	if err == nil {
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return err
		}
		deferred = false
	}
	return err
}

// assertBaselinePreconditions evaluates the evidence contract on the given
// connection. With sourceVersion below the terminal-create and report-
// lifecycle steps the corresponding columns and tables do not exist yet and
// the checks cover the columns the source shape does carry; on the fully
// bridged shape (sourceVersion = baselineVersion) every check applies.
func (s *Store) assertBaselinePreconditions(ctx context.Context, conn *sql.Conn, sourceVersion int, opts BaselineOptions) error {
	activeParts := []string{
		`(SELECT COUNT(*) FROM attempts WHERE runner_pid <> 0)`,
		`(SELECT COUNT(*) FROM attempts WHERE release_state='releasing' OR release_owner_pid <> 0)`,
	}
	if sourceVersion >= 26 {
		activeParts = append(activeParts, `(SELECT COUNT(*) FROM attempts WHERE terminal_create_state='pending')`)
	}
	if sourceVersion >= 22 {
		activeParts = append(activeParts, `(SELECT COUNT(*) FROM report_lifecycle_invocations WHERE state='invoking')`)
	}
	var active int
	if err := conn.QueryRowContext(ctx, "SELECT "+strings.Join(activeParts, " + ")).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return fmt.Errorf("baseline migration refused: %d worker, terminal, release, or handler operation(s) are active; quiesce them and retry; no database changes were made", active)
	}
	var treehouse int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM attempts
		WHERE workspace_backend='treehouse'
		AND (released_at IS NULL OR release_state<>'released' OR workspace_state<>'released')`).Scan(&treehouse); err != nil {
		return err
	}
	if treehouse > 0 {
		return fmt.Errorf("baseline migration refused: %d Treehouse attempt(s) lack complete released evidence; use the previous Treehouse-capable binary to release or explicitly discard each attempt; no database changes were made", treehouse)
	}
	// The canonical terminal columns only exist from the terminal-endpoint step
	// on, and the capability array from the diagnostics step on; earlier
	// sources are checked here against their legacy columns alone and re-checked
	// against the backfilled canonical identity after the bridge runs.
	if sourceVersion >= 18 && sourceVersion < 28 {
		parts := []string{
			`(herdr_socket_path <> '' AND herdr_socket_path <> terminal_socket_path)`,
			`(herdr_workspace_id <> '' AND herdr_workspace_id <> terminal_workspace_id)`,
			`(herdr_tab_id <> '' AND herdr_tab_id <> terminal_tab_id)`,
			`(herdr_pane_id <> '' AND herdr_pane_id <> terminal_pane_id)`,
		}
		if sourceVersion >= 19 {
			parts = append(parts, `(terminal_capabilities_json <> '' AND terminal_capabilities_json <> '[]' AND (json_valid(terminal_capabilities_json) = 0 OR substr(terminal_capabilities_json, 1, 1) <> '['))`)
		}
		var conflict int
		err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM attempts WHERE "+strings.Join(parts, " OR ")).Scan(&conflict)
		if err != nil {
			return err
		}
		if conflict > 0 {
			return fmt.Errorf("baseline migration refused: %d attempt(s) have conflicting legacy Herdr and canonical terminal endpoint identity; inspect with Shephrd v23; no database changes were made", conflict)
		}
	}
	legacyRows, legacyDigest, err := legacyTableDigest(conn)
	if err != nil {
		return err
	}
	if legacyRows > 0 && strings.ToLower(strings.TrimSpace(opts.LegacyExportDigest)) != legacyDigest {
		return fmt.Errorf("baseline migration refused: retired legacy task-list tables contain %d row(s) without an accepted export digest; export them and retry with that digest; no database changes were made", legacyRows)
	}
	var unrecovered int
	err = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM attempts a
		JOIN messages m ON m.attempt_id=a.id AND m.run_generation=a.run_generation AND m.direction='worker-to-driver' AND m.type='done' AND m.stale=0
		WHERE m.artifact_ref LIKE 'branch:%' AND m.artifact_ref <> 'branch:' || a.branch
		AND NOT EXISTS(SELECT 1 FROM local_delivery_recoveries r WHERE r.attempt_id=a.id)
		AND a.release_state <> 'released'`).Scan(&unrecovered)
	if err != nil {
		return err
	}
	var unproven int
	err = conn.QueryRowContext(ctx, `SELECT COUNT(DISTINCT r.attempt_id) FROM local_delivery_recoveries r
		JOIN attempts a ON a.id=r.attempt_id
		WHERE NOT (a.landed_proven=1 AND a.landing_kind=? AND a.released_at IS NOT NULL AND a.release_state='released')`, model.LandingKindLocalAttestedAncestry).Scan(&unproven)
	if err != nil {
		return err
	}
	if unrecovered+unproven > 0 {
		return fmt.Errorf("local-recovery writer retirement refused: %d unreleased or unproven local mismatch recovery attempt(s) remain; verify and release them or retain the compatibility command", unrecovered+unproven)
	}
	snapshots, mismatches, err := verifySnapshotIntegrity(conn)
	if err != nil {
		return err
	}
	if mismatches > 0 {
		return fmt.Errorf("baseline migration refused: %d of %d report snapshot(s) do not match their stored size or hash; restore the affected snapshots from backup and retry; no database changes were made", mismatches, snapshots)
	}
	return nil
}

func verifySnapshotIntegrity(conn *sql.Conn) (int, int, error) {
	rows, err := conn.QueryContext(context.Background(), `SELECT snapshot_path, sha256, size_bytes FROM verified_artifacts WHERE snapshot_path <> ''`)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	total, mismatches := 0, 0
	for rows.Next() {
		var snapshotPath, sha string
		var size int64
		if err := rows.Scan(&snapshotPath, &sha, &size); err != nil {
			return 0, 0, err
		}
		total++
		absolute, err := filepath.Abs(snapshotPath)
		if err != nil {
			mismatches++
			continue
		}
		file, err := os.Open(absolute)
		if err != nil {
			mismatches++
			continue
		}
		hash := sha256.New()
		if _, err := io.Copy(hash, file); err != nil {
			file.Close()
			mismatches++
			continue
		}
		file.Close()
		info, err := os.Stat(absolute)
		if err != nil || info.Size() != size || hex.EncodeToString(hash.Sum(nil)) != sha {
			mismatches++
		}
	}
	return total, mismatches, rows.Err()
}

// legacyTableDigest computes the content-addressed digest the exporter
// publishes: for every legacy task-list table that exists, its name, column
// order, and raw rows in rowid order, hashed as one bundle.
func legacyTableDigest(conn *sql.Conn) (int, string, error) {
	sum := sha256.New()
	rows := 0
	for _, table := range legacyTaskTables {
		var exists int
		if err := conn.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&exists); err != nil {
			return 0, "", err
		}
		if exists == 0 {
			continue
		}
		columns, err := columnOrder(conn, table)
		if err != nil {
			return 0, "", err
		}
		fmt.Fprintf(sum, "table=%s columns=%s\n", table, strings.Join(columns, ","))
		selectList := make([]string, len(columns))
		for index, column := range columns {
			selectList[index] = `"` + column + `"`
		}
		data, err := conn.QueryContext(context.Background(), `SELECT `+strings.Join(selectList, ",")+` FROM `+table+` ORDER BY rowid`)
		if err != nil {
			return 0, "", err
		}
		values := make([]any, len(columns))
		for data.Next() {
			for index := range values {
				values[index] = new(any)
			}
			if err := data.Scan(values...); err != nil {
				data.Close()
				return 0, "", err
			}
			for _, value := range values {
				fmt.Fprintf(sum, "%v;", *value.(*any))
			}
			fmt.Fprintln(sum)
			rows++
		}
		data.Close()
		if err := data.Err(); err != nil {
			return 0, "", err
		}
	}
	return rows, hex.EncodeToString(sum.Sum(nil)), nil
}

func columnOrder(conn *sql.Conn, table string) ([]string, error) {
	rows, err := conn.QueryContext(context.Background(), `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var position, notnull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&position, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	return columns, rows.Err()
}

type schemaQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (s *Store) validateBaselineShape() error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return validateBaselineShapeOn(tx)
}

func validateBaselineShapeOn(q schemaQueryer) error {
	ctx := context.Background()
	var version int
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil {
		return err
	}
	contract := make(map[string][]string, len(baselineObjectContract))
	for name, columns := range baselineObjectContract {
		contract[name] = columns
	}
	indexes := append([]string(nil), baselineIndexContract...)
	if version == 30 {
		for name, columns := range subdriverColumns {
			contract[name] = columns
		}
		contract["driver_notifications"] = append(append([]string(nil), contract["driver_notifications"]...), "coordinator_event_id")
		indexes = append(indexes, "coordinator_requests_owner_idx", "coordinator_events_request_idx")
	}
	objects := map[string]map[string]bool{"table": {}, "index": {}, "trigger": {}, "view": {}}
	rows, err := q.QueryContext(ctx, `SELECT type, name FROM sqlite_schema WHERE type IN ('table','index','trigger','view') AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var kind, name string
		if err := rows.Scan(&kind, &name); err != nil {
			rows.Close()
			return err
		}
		objects[kind][name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	checkSet := func(kind string, want []string) error {
		for _, name := range want {
			if !objects[kind][name] {
				return fmt.Errorf("database schema is incompatible with this binary: baseline %s %s is missing; refusing to open", kind, name)
			}
		}
		for name := range objects[kind] {
			found := false
			for _, expected := range want {
				if name == expected {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("database schema is incompatible with this binary: unknown baseline %s %s is present; refusing to open", kind, name)
			}
		}
		return nil
	}
	if err := checkSet("table", sortedKeys(contract)); err != nil {
		return err
	}
	if err := checkSet("index", indexes); err != nil {
		return err
	}
	if err := checkSet("trigger", baselineTriggerContract); err != nil {
		return err
	}
	if err := checkSet("view", baselineViewContract); err != nil {
		return err
	}
	for _, table := range sortedKeys(contract) {
		var got []string
		rows, err := q.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var position, notnull, pk int
			var name, ctype string
			var dflt any
			if err := rows.Scan(&position, &name, &ctype, &notnull, &dflt, &pk); err != nil {
				rows.Close()
				return err
			}
			got = append(got, name)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		want := contract[table]
		if len(got) != len(want) {
			return fmt.Errorf("database schema is incompatible with this binary: table %s has %d columns, baseline has %d; refusing to open", table, len(got), len(want))
		}
		for index, column := range want {
			if got[index] != column {
				return fmt.Errorf("database schema is incompatible with this binary: table %s column %d is %s, baseline expects %s; refusing to open", table, index+1, got[index], column)
			}
		}
	}
	for table, forbidden := range baselineForbiddenColumns {
		for _, column := range forbidden {
			var count int
			if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, table, column).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return fmt.Errorf("database schema is incompatible with this binary: table %s still has retired column %s; refusing to open", table, column)
			}
		}
	}
	for _, table := range baselineForbiddenTables {
		var count int
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("database schema is incompatible with this binary: legacy table %s is still present under its live name; refusing to open", table)
		}
	}
	var violation string
	err = q.QueryRowContext(ctx, `SELECT "table" FROM pragma_foreign_key_check LIMIT 1`).Scan(&violation)
	if err == nil {
		return fmt.Errorf("database schema is incompatible with this binary: foreign key violation in table %s; refusing to open", violation)
	}
	if err != sql.ErrNoRows {
		return err
	}
	return nil
}

// bridgeShapeOptionalObjects are historical objects a released database may
// additionally carry: migration 9 preserved the abandoned v6/v7 task-list
// schema under legacy_* names (including the immutable event triggers) in the
// databases that had it, and the baseline migrator drops them after export.
var bridgeShapeOptionalObjects = map[string]bool{
	"table legacy_task_lists":                          true,
	"table legacy_task_list_items":                     true,
	"table legacy_task_list_prerequisites":             true,
	"table legacy_task_list_item_inputs":               true,
	"table legacy_task_list_events":                    true,
	"trigger legacy_task_list_events_immutable_update": true,
	"trigger legacy_task_list_events_immutable_delete": true,
}

type bridgeShapeCompatibility int

const (
	bridgeShapeCanonical bridgeShapeCompatibility = iota
	bridgeShapeReleasedLive
)

type bridgeSchemaObject struct {
	table string
	sql   string
}

var releasedLiveLegacyObjects = map[string]bool{
	"table legacy_task_lists":                   true,
	"table legacy_task_list_items":              true,
	"table legacy_task_list_prerequisites":      true,
	"table legacy_task_list_item_inputs":        true,
	"table legacy_task_list_events":             true,
	"trigger task_list_events_immutable_update": true,
	"trigger task_list_events_immutable_delete": true,
}

const releasedLiveLegacyDigest = "ccec472bc26266cfe67aa84f33da6df7c0ac7a3dc18ab71a7cd4c6e3f09f0031"

var releasedLiveReplacedObjects = map[string]bool{
	"trigger verified_artifacts_immutable_update": true,
	"trigger verified_artifacts_immutable_delete": true,
}

const releasedLiveVerifiedArtifactTriggers = `CREATE TRIGGER verified_artifacts_immutable_delete
BEFORE DELETE ON verified_artifacts
BEGIN
    SELECT RAISE(ABORT, 'verified artifacts are immutable');
END;

CREATE TRIGGER verified_artifacts_immutable_update
BEFORE UPDATE ON verified_artifacts
BEGIN
    SELECT RAISE(ABORT, 'verified artifacts are immutable');
END;`

func validateBridgeShape(conn *sql.Conn, classification databaseClassification) (bridgeShapeCompatibility, error) {
	version := classification.version
	golden, ok := bridgeShapeGoldens[version]
	if !ok {
		return bridgeShapeCanonical, fmt.Errorf("no frozen schema shape is recorded for bridge version %d; refusing to open", version)
	}
	got := map[string]bridgeSchemaObject{}
	rows, err := conn.QueryContext(context.Background(), `SELECT type || ' ' || name, tbl_name, COALESCE(sql, '') FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return bridgeShapeCanonical, err
	}
	for rows.Next() {
		var key string
		var object bridgeSchemaObject
		if err := rows.Scan(&key, &object.table, &object.sql); err != nil {
			rows.Close()
			return bridgeShapeCanonical, err
		}
		got[key] = object
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return bridgeShapeCanonical, err
	}
	var problems []string
	for key := range golden {
		if _, exists := got[key]; !exists {
			problems = append(problems, "missing "+key)
		}
	}
	for key := range got {
		if !golden[key] && !bridgeShapeOptionalObjects[key] {
			problems = append(problems, "unexpected "+key)
		}
	}
	if len(problems) == 0 {
		return bridgeShapeCanonical, nil
	}
	if matchesReleasedLiveBridgeShape(classification.recorded, golden, got) {
		return bridgeShapeReleasedLive, nil
	}
	sort.Strings(problems)
	return bridgeShapeCanonical, fmt.Errorf("database schema shape at version %d diverges from the released shape (%s); refusing to open", version, strings.Join(problems, ", "))
}

func matchesReleasedLiveBridgeShape(recorded map[int]recordedMigration, golden map[string]bool, got map[string]bridgeSchemaObject) bool {
	for version := 1; version <= len(recorded); version++ {
		record := recorded[version]
		if version <= 8 {
			if record.checksum != "" {
				return false
			}
			continue
		}
		identity := frozenMigrationIdentities[version]
		if record.name != identity.name || record.checksum != identity.checksum {
			return false
		}
	}
	if len(got) != len(golden)-len(releasedLiveReplacedObjects)+len(releasedLiveLegacyObjects) {
		return false
	}
	for key := range golden {
		if releasedLiveReplacedObjects[key] {
			if _, exists := got[key]; exists {
				return false
			}
			continue
		}
		if _, exists := got[key]; !exists {
			return false
		}
	}
	for key := range got {
		if !golden[key] {
			if _, exists := releasedLiveLegacyObjects[key]; !exists {
				return false
			}
		}
	}
	keys := make([]string, 0, len(releasedLiveLegacyObjects))
	for key := range releasedLiveLegacyObjects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	digest := sha256.New()
	for _, key := range keys {
		object, exists := got[key]
		if !exists {
			return false
		}
		for _, value := range []string{key, object.table, object.sql} {
			digest.Write([]byte(value))
			digest.Write([]byte{0})
		}
	}
	return hex.EncodeToString(digest.Sum(nil)) == releasedLiveLegacyDigest
}

func init() {
	bridgeShapeGoldens = map[int]map[string]bool{}
	entries, err := bridgeShapeGoldenFS.ReadDir("testdata/golden-schema")
	if err != nil {
		panic(err)
	}
	for _, entry := range entries {
		body, err := bridgeShapeGoldenFS.ReadFile("testdata/golden-schema/" + entry.Name())
		if err != nil {
			panic(err)
		}
		var version int
		if _, err := fmt.Sscanf(entry.Name(), "v%d.txt", &version); err != nil {
			panic(err)
		}
		set := map[string]bool{}
		for _, line := range strings.Split(string(body), "\n") {
			if line != "" {
				set[line] = true
			}
		}
		bridgeShapeGoldens[version] = set
	}
}
