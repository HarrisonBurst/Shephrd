package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// LegacyExport is the read-only export contract for legacy schemas: below the
// bridge floor and at the supported bridge versions. It publishes the raw
// table names, column orders, row counts, and the content-addressed digest of
// the retired legacy task-list tables so an operator can accept a nonempty
// export before the migrator drops anything; the bridge versions consume that
// digest through the baseline migration precondition.
type LegacyExport struct {
	SchemaVersion int                `json:"schema_version"`
	ExportedAt    string             `json:"exported_at"`
	DatabaseFile  string             `json:"database_file"`
	DatabaseSHA256 string            `json:"database_sha256"`
	DatabaseBytes int                `json:"database_bytes"`
	Ledger        []recordedMigration `json:"ledger"`
	Tables        []LegacyExportTable `json:"tables"`
	LegacyTables  []LegacyExportTable `json:"legacy_task_tables"`
	LegacyRows    int                `json:"legacy_task_rows"`
	Digest        string             `json:"digest"`
	Snapshots     []LegacyExportSnapshot `json:"report_snapshots"`
}

type LegacyExportTable struct {
	Name     string   `json:"name"`
	Columns  []string `json:"columns"`
	RowCount int      `json:"row_count"`
	SHA256   string   `json:"sha256"`
	Rows     [][]any  `json:"rows,omitempty"`
}

type LegacyExportSnapshot struct {
	Path     string `json:"path"`
	SHA256   string `json:"sha256,omitempty"`
	SizeBytes int   `json:"size_bytes,omitempty"`
	Present  bool   `json:"present"`
}

// ExportLegacyDatabase performs the bounded read-only export of a legacy
// database at a below-floor or supported bridge version. It never opens the
// database read-write and never invokes ordinary bootstrap. The compiled
// export window closes the below-floor exports only; bridge-version exports
// stay available for this binary's lifetime because the baseline migration's
// accepted-digest precondition depends on them.
func ExportLegacyDatabase(path, outDir string, now time.Time) (*LegacyExport, error) {
	classification, err := classifyDatabase(path)
	if err != nil {
		return nil, err
	}
	if classification.kind == classificationFresh {
		return nil, fmt.Errorf("database %s has no schema history to export", path)
	}
	if classification.version >= baselineVersion {
		return nil, fmt.Errorf("database schema %d is not a legacy schema; the ordinary inspect commands apply", classification.version)
	}
	if classification.version < bridgeFloorVersion && !now.Before(baselineExportClose) {
		return nil, fmt.Errorf("legacy schema support closed at %s; the read-only export window is over for database schema %d. Restore a backup and use the archived signed legacy exporter", baselineExportClose.Format(time.RFC3339), classification.version)
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return nil, fmt.Errorf("create export directory: %w", err)
	}
	if err := copyDatabaseBundle(path, outDir); err != nil {
		return nil, err
	}
	export, err := exportLegacyContents(path, outDir)
	if err != nil {
		return nil, err
	}
	if err := copySnapshotBundle(export.Snapshots, outDir); err != nil {
		return nil, err
	}
	export.SchemaVersion = classification.version
	export.ExportedAt = now.UTC().Format(time.RFC3339)
	body, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(outDir, "export-manifest.json"), append(body, '\n'), 0o600); err != nil {
		return nil, fmt.Errorf("write export manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "export-digest.txt"), []byte(export.Digest+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write export digest: %w", err)
	}
	upgrade := "To upgrade in place, use the signed Shephrd v23 legacy upgrader. To migrate\nwith the baseline binary, retry the migration with:"
	if classification.version >= bridgeFloorVersion {
		upgrade = "To upgrade in place with this baseline binary, retry the migration with\nthe accepted export digest:"
	}
	instructions := fmt.Sprintf(`Legacy database export for schema %d

This bundle contains the original database file, its export manifest, and the
content-addressed digest of the retired legacy task-list tables.

%s

  SHEPHRD_LEGACY_EXPORT_DIGEST=%s <any shephrd command>

Do not delete schema_migrations rows from the original database.
`, classification.version, upgrade, export.Digest)
	if err := os.WriteFile(filepath.Join(outDir, "recovery-instructions.txt"), []byte(instructions), 0o600); err != nil {
		return nil, fmt.Errorf("write recovery instructions: %w", err)
	}
	return export, nil
}

func copyDatabaseBundle(path, outDir string) error {
	files := []string{path, path + "-wal", path + "-shm"}
	for _, file := range files {
		info, err := os.Stat(file)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("stat %s: %w", file, err)
		}
		if err := copyFile(file, filepath.Join(outDir, filepath.Base(file)), info.Mode()); err != nil {
			return err
		}
	}
	return nil
}

// copySnapshotBundle retains the report snapshot files the exported database
// references inside the export bundle, so a below-floor installation can be
// archived without losing durable evidence.
func copySnapshotBundle(snapshots []LegacyExportSnapshot, outDir string) error {
	dir := filepath.Join(outDir, "snapshots")
	used := map[string]bool{}
	for index, snapshot := range snapshots {
		if !snapshot.Present {
			continue
		}
		absolute, err := filepath.Abs(snapshot.Path)
		if err != nil {
			continue
		}
		name := filepath.Base(absolute)
		for collision := 2; used[name]; collision++ {
			name = fmt.Sprintf("%d-%s", collision, filepath.Base(absolute))
		}
		if !used[name] {
			used[name] = true
		}
		info, err := os.Stat(absolute)
		if err != nil {
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create snapshot bundle directory: %w", err)
		}
		if err := copyFile(absolute, filepath.Join(dir, name), info.Mode()); err != nil {
			return fmt.Errorf("copy report snapshot %d: %w", index+1, err)
		}
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return nil
}

func exportLegacyContents(path, outDir string) (*LegacyExport, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open database read-only: %w", err)
	}
	defer db.Close()
	ctx := context.Background()

	original, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read database bundle: %w", err)
	}
	sum := sha256.Sum256(original)

	ledger, err := exportLedger(db, ctx)
	if err != nil {
		return nil, err
	}
	tables, err := exportTableSummaries(db, ctx)
	if err != nil {
		return nil, err
	}
	legacy := make([]LegacyExportTable, 0, len(legacyTaskTables))
	legacyRows := 0
	digestSum := sha256.New()
	for _, name := range legacyTaskTables {
		table, ok, err := exportFullTable(db, ctx, name)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		legacy = append(legacy, table)
		legacyRows += table.RowCount
		fmt.Fprintf(digestSum, "table=%s columns=%s\n", name, strings.Join(table.Columns, ","))
		for _, row := range table.Rows {
			for _, value := range row {
				fmt.Fprintf(digestSum, "%v;", value)
			}
			fmt.Fprintln(digestSum)
		}
	}
	snapshots, err := exportSnapshots(db, ctx)
	if err != nil {
		return nil, err
	}
	export := &LegacyExport{
		DatabaseFile:   filepath.Base(path),
		DatabaseSHA256: hex.EncodeToString(sum[:]),
		DatabaseBytes:  len(original),
		Ledger:         ledger,
		Tables:         tables,
		LegacyTables:   legacy,
		LegacyRows:     legacyRows,
		Digest:         hex.EncodeToString(digestSum.Sum(nil)),
		Snapshots:      snapshots,
	}
	return export, nil
}

func exportLedger(db *sql.DB, ctx context.Context) ([]recordedMigration, error) {
	rows, err := db.QueryContext(ctx, `SELECT version, COALESCE(name,''), COALESCE(checksum,''), COALESCE(applied_at,'') FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("read export ledger: %w", err)
	}
	defer rows.Close()
	var ledger []recordedMigration
	for rows.Next() {
		var version int
		var record recordedMigration
		var appliedAt string
		if err := rows.Scan(&version, &record.name, &record.checksum, &appliedAt); err != nil {
			return nil, err
		}
		ledger = append(ledger, record)
	}
	return ledger, rows.Err()
}

func exportTableSummaries(db *sql.DB, ctx context.Context) ([]LegacyExportTable, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	legacy := map[string]bool{}
	for _, name := range legacyTaskTables {
		legacy[name] = true
	}
	summary := func(name string) (LegacyExportTable, error) {
		columns, err := columnOrderDB(db, ctx, name)
		if err != nil {
			return LegacyExportTable{}, err
		}
		var count int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+name).Scan(&count); err != nil {
			return LegacyExportTable{}, err
		}
		return LegacyExportTable{Name: name, Columns: columns, RowCount: count}, nil
	}
	var tables []LegacyExportTable
	for _, name := range names {
		if legacy[name] {
			continue
		}
		table, err := summary(name)
		if err != nil {
			return nil, err
		}
		tables = append(tables, table)
	}
	return tables, nil
}

func exportFullTable(db *sql.DB, ctx context.Context, name string) (LegacyExportTable, bool, error) {
	var exists int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name=?`, name).Scan(&exists); err != nil {
		return LegacyExportTable{}, false, err
	}
	if exists == 0 {
		return LegacyExportTable{}, false, nil
	}
	columns, err := columnOrderDB(db, ctx, name)
	if err != nil {
		return LegacyExportTable{}, false, err
	}
	selectList := make([]string, len(columns))
	for index, column := range columns {
		selectList[index] = `"` + column + `"`
	}
	data, err := db.QueryContext(ctx, `SELECT `+strings.Join(selectList, ",")+` FROM `+name+` ORDER BY rowid`)
	if err != nil {
		return LegacyExportTable{}, false, err
	}
	defer data.Close()
	table := LegacyExportTable{Name: name, Columns: columns}
	for data.Next() {
		values := make([]any, len(columns))
		for index := range values {
			values[index] = new(any)
		}
		if err := data.Scan(values...); err != nil {
			return LegacyExportTable{}, false, err
		}
		row := make([]any, len(values))
		for index, value := range values {
			row[index] = *value.(*any)
		}
		table.Rows = append(table.Rows, row)
		table.RowCount++
	}
	if err := data.Err(); err != nil {
		return LegacyExportTable{}, false, err
	}
	sum := sha256.New()
	for _, row := range table.Rows {
		for _, value := range row {
			fmt.Fprintf(sum, "%v;", value)
		}
		fmt.Fprintln(sum)
	}
	table.SHA256 = hex.EncodeToString(sum.Sum(nil))
	return table, true, nil
}

func columnOrderDB(db *sql.DB, ctx context.Context, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
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


func exportSnapshots(db *sql.DB, ctx context.Context) ([]LegacyExportSnapshot, error) {
	rows, err := db.QueryContext(ctx, `SELECT snapshot_path, sha256, size_bytes FROM verified_artifacts WHERE snapshot_path <> '' ORDER BY snapshot_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var snapshots []LegacyExportSnapshot
	for rows.Next() {
		var snapshotPath, sha string
		var size int64
		if err := rows.Scan(&snapshotPath, &sha, &size); err != nil {
			return nil, err
		}
		snapshot := LegacyExportSnapshot{Path: snapshotPath}
		absolute, err := filepath.Abs(snapshotPath)
		if err == nil {
			file, openErr := os.Open(absolute)
			if openErr == nil {
				hash := sha256.New()
				written, copyErr := io.Copy(hash, file)
				file.Close()
				if copyErr == nil {
					info, statErr := os.Stat(absolute)
					if statErr == nil {
						snapshot.Present = true
						snapshot.SHA256 = hex.EncodeToString(hash.Sum(nil))
						snapshot.SizeBytes = int(info.Size())
						_ = written
					}
				}
			}
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, rows.Err()
}
