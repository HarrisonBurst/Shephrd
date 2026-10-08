// Package store test fixtures: frozen historical migration code.
//
// These constants and functions are the released bodies of migrations 9 and
// 15. They are no longer executable production code: databases below the
// bridge floor are refused for in-place upgrade, and these fixtures exist
// only so tests can reconstruct historical schema shapes byte-for-byte.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// Migration 9 repairs lifecycle and schema integrity for databases whose
// recorded history no longer matches the committed source contract:
//
//  1. schema_migrations gains mandatory identity columns (name, checksum) so a
//     recorded version can never again mask incompatible SQL.
//  2. Databases whose recorded versions 6/7 actually created the abandoned
//     legacy task-list schema (task_list_items.list_id, task_list_item_inputs,
//     task_list_events) are rebuilt to the committed contract. Every legacy row
//     is preserved: transformed rows are copied into the current tables and the
//     original tables are retained under legacy_* names, including the
//     immutable task_list_events audit history.
//  3. The attempts table is rebuilt with lifecycle coherence invariants:
//     release_state and released_at can no longer contradict, acquisition
//     failures that never had a worktree are represented as no_workspace
//     instead of held, and landed_proven can no longer be recorded without a
//     complete proof (landing kind plus verification timestamp).
//  4. Incomplete legacy landing flags are repaired conservatively: a
//     report proof is reconstructed only from a complete immutable
//     verified-artifact row bound to the attempt; every other bare
//     landed_proven boolean is quarantined (cleared with a recorded reason)
//     without inventing landing evidence and without touching messages,
//     artifacts, notifications, archives, or task history.
func migrateLifecycleIntegrityV9(ctx context.Context, conn *sql.Conn) error {
	if err := rebuildSchemaMigrationsV9(ctx, conn); err != nil {
		return err
	}
	if err := rebuildTaskListsV9(ctx, conn); err != nil {
		return err
	}
	if err := repairLandingProofsV9(ctx, conn); err != nil {
		return err
	}
	return rebuildAttemptsV9(ctx, conn)
}

func rebuildSchemaMigrationsV9(ctx context.Context, conn *sql.Conn) error {
	statements := []string{
		`CREATE TABLE schema_migrations_v9 (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			checksum TEXT NOT NULL DEFAULT '',
			applied_at TEXT NOT NULL
		)`,
		`INSERT INTO schema_migrations_v9(version, applied_at) SELECT version, applied_at FROM schema_migrations`,
		`DROP TABLE schema_migrations`,
		`ALTER TABLE schema_migrations_v9 RENAME TO schema_migrations`,
	}
	for _, statement := range statements {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("rebuild schema_migrations: %w", err)
		}
	}
	for version, name := range map[int]string{
		1: "001_current", 2: "002_verified_report_inputs", 3: "003_local_landing", 4: "004_task_archive",
		5: "005_runtime_executable", 6: "006_task_lists", 7: "007_report_input_position_default", 8: "008_external_delivery_attestations",
	} {
		if _, err := conn.ExecContext(ctx, `UPDATE schema_migrations SET name=? WHERE version=? AND name=''`, name, version); err != nil {
			return err
		}
	}
	return nil
}

const (
	taskListShapeCurrent      = "current"
	taskListShapeLegacy       = "legacy"
	taskListShapeMissing      = "missing"
	taskListShapeUnrecognized = "unrecognized"
)

func detectTaskListShapeV9(ctx context.Context, conn *sql.Conn) (string, error) {
	tableExists := func(name string) (bool, error) {
		var count int
		err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&count)
		return count != 0, err
	}
	lists, err := tableExists("task_lists")
	if err != nil {
		return "", err
	}
	items, err := tableExists("task_list_items")
	if err != nil {
		return "", err
	}
	if !lists && !items {
		return taskListShapeMissing, nil
	}
	if !lists || !items {
		return taskListShapeUnrecognized, nil
	}
	columns := make(map[string]bool)
	rows, err := conn.QueryContext(ctx, `SELECT name FROM pragma_table_info('task_list_items')`)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return "", err
		}
		columns[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	inputs, err := tableExists("task_list_inputs")
	if err != nil {
		return "", err
	}
	itemInputs, err := tableExists("task_list_item_inputs")
	if err != nil {
		return "", err
	}
	events, err := tableExists("task_list_events")
	if err != nil {
		return "", err
	}
	prerequisites, err := tableExists("task_list_prerequisites")
	if err != nil {
		return "", err
	}
	if columns["task_list_id"] && columns["description"] && inputs && !itemInputs && !events {
		return taskListShapeCurrent, nil
	}
	if columns["list_id"] && !columns["task_list_id"] && columns["dispatched_at"] && itemInputs && events && prerequisites && !inputs {
		return taskListShapeLegacy, nil
	}
	return taskListShapeUnrecognized, nil
}

func rebuildTaskListsV9(ctx context.Context, conn *sql.Conn) error {
	shape, err := detectTaskListShapeV9(ctx, conn)
	if err != nil {
		return err
	}
	switch shape {
	case taskListShapeCurrent:
		return nil
	case taskListShapeMissing:
		return execAllV9(ctx, conn, migration9TaskListTables, migration9TaskListTriggers, migration9TaskListIndexes)
	case taskListShapeLegacy:
	default:
		return fmt.Errorf("task-list tables do not match any recognized historical shape; refusing to migrate an unrecognized schema (recorded migration versions 6/7 mask incompatible SQL)")
	}
	rename := []string{
		`DROP INDEX IF EXISTS task_list_events_list_idx`,
		`DROP INDEX IF EXISTS task_list_item_inputs_artifact_idx`,
		`DROP INDEX IF EXISTS task_list_item_inputs_order_idx`,
		`DROP INDEX IF EXISTS task_list_items_order_idx`,
		`DROP INDEX IF EXISTS task_list_prerequisites_source_idx`,
		`ALTER TABLE task_lists RENAME TO legacy_task_lists`,
		`ALTER TABLE task_list_items RENAME TO legacy_task_list_items`,
		`ALTER TABLE task_list_prerequisites RENAME TO legacy_task_list_prerequisites`,
		`ALTER TABLE task_list_item_inputs RENAME TO legacy_task_list_item_inputs`,
		`ALTER TABLE task_list_events RENAME TO legacy_task_list_events`,
	}
	for _, statement := range rename {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("preserve legacy task-list tables: %w", err)
		}
	}
	if err := execAllV9(ctx, conn, migration9TaskListTables); err != nil {
		return err
	}
	// Rows are copied before the validation triggers exist so historical data
	// (including dispatched items) transfers verbatim; future edits are
	// validated by the recreated current-contract triggers.
	transform := []string{
		`INSERT INTO task_lists(id, name, driver_id, created_at, updated_at)
			SELECT id, name, driver_id, created_at, updated_at FROM legacy_task_lists`,
		`INSERT INTO task_list_items(id, task_list_id, position, feature_key, objective, description, acceptance_criteria, repo_id, deliverable, dispatched_task_id, created_at, updated_at)
			SELECT id, list_id, position, feature_key, objective, '', acceptance_criteria, repo_id, deliverable, dispatched_task_id, created_at, updated_at FROM legacy_task_list_items`,
		`INSERT INTO task_list_prerequisites(item_id, prerequisite_item_id, created_at)
			SELECT item_id, prerequisite_item_id, added_at FROM legacy_task_list_prerequisites`,
		`INSERT INTO task_list_prerequisites(item_id, prerequisite_item_id, created_at)
			SELECT i.item_id, i.producer_item_id, i.selected_at FROM legacy_task_list_item_inputs i
			WHERE NOT EXISTS(SELECT 1 FROM task_list_prerequisites p WHERE p.item_id=i.item_id AND p.prerequisite_item_id=i.producer_item_id)`,
		`INSERT INTO task_list_inputs(item_id, prerequisite_item_id, position, artifact_id, selected_by_driver_id, selected_at, created_at, updated_at)
			SELECT item_id, producer_item_id, position, artifact_id, selected_by_driver_id, selected_at, selected_at, selected_at FROM legacy_task_list_item_inputs`,
	}
	for _, statement := range transform {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("transform legacy task-list rows: %w", err)
		}
	}
	return execAllV9(ctx, conn, migration9TaskListTriggers, migration9TaskListIndexes)
}

func repairLandingProofsV9(ctx context.Context, conn *sql.Conn) error {
	// Reconstruct a report proof only where a complete immutable
	// verified-artifact row is bound to the attempt and its task.
	if _, err := conn.ExecContext(ctx, `UPDATE attempts SET
		landing_kind='report_artifact',
		landed_verified_at=(SELECT MIN(va.verified_at) FROM verified_artifacts va
			WHERE va.producer_attempt_id=attempts.id AND va.producer_task_id=attempts.task_id AND va.kind='report')
		WHERE landed_proven=1 AND (landing_kind='' OR landing_kind='report_artifact') AND landed_verified_at IS NULL
		AND EXISTS(SELECT 1 FROM verified_artifacts va
			WHERE va.producer_attempt_id=attempts.id AND va.producer_task_id=attempts.task_id AND va.kind='report')`); err != nil {
		return fmt.Errorf("reconstruct legacy report proofs: %w", err)
	}
	return nil
}

func rebuildAttemptsV9(ctx context.Context, conn *sql.Conn) error {
	statements := []string{
		migration9AttemptsTable,
		`INSERT INTO attempts_v9(id, task_id, number, harness, model, runtime_backend, runtime_generation, run_generation,
			resume_source_attempt_id, resume_source_revision, herdr_socket_path, herdr_workspace_id, herdr_tab_id, herdr_pane_id,
			session_id, worktree_path, lease_id, branch, status, runner_pid, cursor, exit_code, failure_reason,
			landed_proven, discard_authorized, released_at, created_at, updated_at, ended_at, base_commit, landing_kind,
			landed_source_commit, landed_target_ref, landed_target_commit, landed_checkpoint_revision, landed_verified_at,
			release_state, release_claimed_at, release_owner_pid, release_reason, runtime_executable, landing_quarantine_reason)
		SELECT id, task_id, number, harness, model, runtime_backend, runtime_generation, run_generation,
			resume_source_attempt_id, resume_source_revision, herdr_socket_path, herdr_workspace_id, herdr_tab_id, herdr_pane_id,
			session_id, worktree_path, lease_id, branch, status, runner_pid, cursor, exit_code, failure_reason,
			CASE WHEN landed_proven=1 AND (landing_kind='' OR landed_verified_at IS NULL) THEN 0 ELSE landed_proven END,
			discard_authorized, released_at, created_at, updated_at, ended_at, base_commit, landing_kind,
			landed_source_commit, landed_target_ref, landed_target_commit, landed_checkpoint_revision, landed_verified_at,
			CASE
				WHEN released_at IS NOT NULL THEN 'released'
				WHEN release_state='held' AND worktree_path='' AND lease_id='' AND status IN ('blocked', 'failed', 'stopped', 'superseded') THEN 'no_workspace'
				ELSE release_state
			END,
			CASE WHEN released_at IS NOT NULL THEN NULL ELSE release_claimed_at END,
			CASE WHEN released_at IS NOT NULL THEN 0 ELSE release_owner_pid END,
			CASE
				WHEN release_reason<>'' THEN release_reason
				WHEN released_at IS NOT NULL AND release_state<>'released' THEN 'release_state backfilled to released to match the legacy released_at timestamp (migration 9)'
				WHEN release_state='held' AND worktree_path='' AND lease_id='' AND status IN ('blocked', 'failed', 'stopped', 'superseded') THEN 'workspace acquisition failed before any worktree existed; reclassified from held to no_workspace (migration 9)'
				ELSE release_reason
			END,
			runtime_executable,
			CASE WHEN landed_proven=1 AND (landing_kind='' OR landed_verified_at IS NULL)
				THEN 'legacy landed_proven flag lacked a complete landing proof (kind ' || quote(landing_kind) || ', verified_at ' || COALESCE(quote(landed_verified_at), 'NULL') || '); proof projection quarantined by migration 9 without inventing landing evidence'
				ELSE '' END
		FROM attempts`,
		`DROP TABLE attempts`,
		`ALTER TABLE attempts_v9 RENAME TO attempts`,
		`CREATE INDEX attempts_task_idx ON attempts(task_id, number)`,
	}
	for _, statement := range statements {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("rebuild attempts with lifecycle invariants: %w", err)
		}
	}
	return nil
}

func execAllV9(ctx context.Context, conn *sql.Conn, scripts ...string) error {
	for _, script := range scripts {
		if _, err := conn.ExecContext(ctx, script); err != nil {
			return fmt.Errorf("apply current task-list contract: %w", err)
		}
	}
	return nil
}

// migration9Identity is the declared identity of the Go migration: it covers
// every DDL constant the migration produces plus a semantic revision marker.
// Changing any of it changes the recorded checksum, which the open-time fence
// would then reject, so the migration must never be edited after release; add
// a new version instead.
var migration9Identity = "009_lifecycle_integrity_repair revision 1\n" +
	migration9TaskListTables + "\n" + migration9TaskListTriggers + "\n" + migration9TaskListIndexes + "\n" + migration9AttemptsTable

// migration9TaskListTables matches the task-list portion of committed
// migration 006 exactly so fresh and rebuilt databases converge on one shape.
const migration9TaskListTables = `
CREATE TABLE task_lists (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    driver_id TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(driver_id, name)
);

CREATE TABLE task_list_items (
    id TEXT PRIMARY KEY,
    task_list_id TEXT NOT NULL REFERENCES task_lists(id),
    position INTEGER NOT NULL CHECK(position > 0),
    feature_key TEXT NOT NULL DEFAULT '',
    objective TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    acceptance_criteria TEXT NOT NULL DEFAULT '',
    repo_id TEXT REFERENCES repos(id),
    deliverable TEXT NOT NULL DEFAULT 'code' CHECK(deliverable IN ('code', 'report')),
    dispatched_task_id TEXT UNIQUE REFERENCES tasks(id),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(task_list_id, position)
);

CREATE TABLE task_list_prerequisites (
    item_id TEXT NOT NULL REFERENCES task_list_items(id),
    prerequisite_item_id TEXT NOT NULL REFERENCES task_list_items(id),
    created_at TEXT NOT NULL,
    PRIMARY KEY(item_id, prerequisite_item_id),
    CHECK(item_id <> prerequisite_item_id)
);

CREATE TABLE task_list_inputs (
    item_id TEXT NOT NULL REFERENCES task_list_items(id),
    prerequisite_item_id TEXT NOT NULL REFERENCES task_list_items(id),
    position INTEGER NOT NULL CHECK(position > 0),
    artifact_id TEXT REFERENCES verified_artifacts(id),
    selected_by_driver_id TEXT NOT NULL DEFAULT '',
    selected_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY(item_id, prerequisite_item_id),
    UNIQUE(item_id, position)
);
`

const migration9TaskListTriggers = `
CREATE TRIGGER task_list_items_list_immutable
BEFORE UPDATE OF task_list_id ON task_list_items
BEGIN
    SELECT RAISE(ABORT, 'task-list item list identity is immutable');
END;

CREATE TRIGGER task_list_items_dispatch_immutable
BEFORE UPDATE OF dispatched_task_id ON task_list_items
WHEN OLD.dispatched_task_id IS NOT NULL OR NEW.dispatched_task_id IS NULL
BEGIN
    SELECT RAISE(ABORT, 'task-list item dispatch identity is immutable');
END;

CREATE TRIGGER task_list_items_dispatched_scope_immutable
BEFORE UPDATE OF feature_key, objective, description, acceptance_criteria, repo_id, deliverable ON task_list_items
WHEN OLD.dispatched_task_id IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'dispatched task-list item scope is immutable');
END;

CREATE TRIGGER task_list_items_dispatched_delete
BEFORE DELETE ON task_list_items
WHEN OLD.dispatched_task_id IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'dispatched task-list items cannot be deleted');
END;

CREATE TRIGGER task_list_prerequisites_validate_insert
BEFORE INSERT ON task_list_prerequisites
BEGIN
    SELECT CASE WHEN
        (SELECT task_list_id FROM task_list_items WHERE id=NEW.item_id) IS NULL OR
        (SELECT task_list_id FROM task_list_items WHERE id=NEW.prerequisite_item_id) IS NULL OR
        (SELECT task_list_id FROM task_list_items WHERE id=NEW.item_id) <> (SELECT task_list_id FROM task_list_items WHERE id=NEW.prerequisite_item_id)
    THEN RAISE(ABORT, 'task-list prerequisites must belong to the same list') END;
    SELECT CASE WHEN (SELECT dispatched_task_id FROM task_list_items WHERE id=NEW.item_id) IS NOT NULL
    THEN RAISE(ABORT, 'dispatched task-list item prerequisites are immutable') END;
    SELECT CASE WHEN EXISTS(
        WITH RECURSIVE dependencies(id) AS (
            SELECT NEW.prerequisite_item_id
            UNION
            SELECT p.prerequisite_item_id
            FROM task_list_prerequisites p
            JOIN dependencies d ON p.item_id=d.id
        )
        SELECT 1 FROM dependencies WHERE id=NEW.item_id
    ) THEN RAISE(ABORT, 'task-list prerequisite cycle') END;
END;

CREATE TRIGGER task_list_prerequisites_validate_update
BEFORE UPDATE ON task_list_prerequisites
BEGIN
    SELECT RAISE(ABORT, 'task-list prerequisite identity is immutable');
END;

CREATE TRIGGER task_list_prerequisites_validate_delete
BEFORE DELETE ON task_list_prerequisites
BEGIN
    SELECT CASE WHEN (SELECT dispatched_task_id FROM task_list_items WHERE id=OLD.item_id) IS NOT NULL
    THEN RAISE(ABORT, 'dispatched task-list item prerequisites are immutable') END;
    SELECT CASE WHEN EXISTS(
        SELECT 1 FROM task_list_inputs WHERE item_id=OLD.item_id AND prerequisite_item_id=OLD.prerequisite_item_id
    ) THEN RAISE(ABORT, 'remove the task-list input before its prerequisite') END;
END;

CREATE TRIGGER task_list_inputs_validate_insert
BEFORE INSERT ON task_list_inputs
BEGIN
    SELECT CASE WHEN NOT EXISTS(
        SELECT 1 FROM task_list_prerequisites WHERE item_id=NEW.item_id AND prerequisite_item_id=NEW.prerequisite_item_id
    ) THEN RAISE(ABORT, 'task-list input must reference a prerequisite') END;
    SELECT CASE WHEN (SELECT dispatched_task_id FROM task_list_items WHERE id=NEW.item_id) IS NOT NULL
    THEN RAISE(ABORT, 'dispatched task-list item inputs are immutable') END;
    SELECT CASE WHEN NEW.artifact_id IS NOT NULL AND NOT EXISTS(
        SELECT 1
        FROM verified_artifacts va
        JOIN task_list_items prerequisite ON prerequisite.id=NEW.prerequisite_item_id
        WHERE va.id=NEW.artifact_id AND va.producer_task_id=prerequisite.dispatched_task_id
    ) THEN RAISE(ABORT, 'task-list input artifact does not belong to its prerequisite task') END;
END;

CREATE TRIGGER task_list_inputs_validate_update
BEFORE UPDATE ON task_list_inputs
BEGIN
    SELECT CASE WHEN (SELECT dispatched_task_id FROM task_list_items WHERE id=OLD.item_id) IS NOT NULL
    THEN RAISE(ABORT, 'dispatched task-list item inputs are immutable') END;
    SELECT CASE WHEN NEW.item_id<>OLD.item_id OR NEW.prerequisite_item_id<>OLD.prerequisite_item_id
    THEN RAISE(ABORT, 'task-list input identity is immutable') END;
    SELECT CASE WHEN NEW.artifact_id IS NOT NULL AND NOT EXISTS(
        SELECT 1
        FROM verified_artifacts va
        JOIN task_list_items prerequisite ON prerequisite.id=NEW.prerequisite_item_id
        WHERE va.id=NEW.artifact_id AND va.producer_task_id=prerequisite.dispatched_task_id
    ) THEN RAISE(ABORT, 'task-list input artifact does not belong to its prerequisite task') END;
END;

CREATE TRIGGER task_list_inputs_validate_delete
BEFORE DELETE ON task_list_inputs
BEGIN
    SELECT CASE WHEN (SELECT dispatched_task_id FROM task_list_items WHERE id=OLD.item_id) IS NOT NULL
    THEN RAISE(ABORT, 'dispatched task-list item inputs are immutable') END;
END;
`

const migration9TaskListIndexes = `
CREATE INDEX task_lists_driver_idx ON task_lists(driver_id, created_at);
CREATE INDEX task_list_items_list_idx ON task_list_items(task_list_id, position);
CREATE INDEX task_list_prerequisites_prerequisite_idx ON task_list_prerequisites(prerequisite_item_id, item_id);
CREATE INDEX task_list_inputs_item_idx ON task_list_inputs(item_id, position);
CREATE INDEX task_list_inputs_artifact_idx ON task_list_inputs(artifact_id);
`

// migration9AttemptsTable preserves the exact historical column order and adds
// the lifecycle coherence invariants:
//   - release_state may also be no_workspace (an acquisition failure that
//     never owned a worktree);
//   - release_state is released exactly when released_at is set;
//   - landed_proven requires a complete proof projection.
const migration9AttemptsTable = `
CREATE TABLE attempts_v9 (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    number INTEGER NOT NULL,
    harness TEXT NOT NULL,
    model TEXT NOT NULL DEFAULT '',
    runtime_backend TEXT NOT NULL DEFAULT 'headless',
    runtime_generation INTEGER NOT NULL DEFAULT 0,
    run_generation INTEGER NOT NULL DEFAULT 0,
    resume_source_attempt_id TEXT REFERENCES attempts(id),
    resume_source_revision INTEGER NOT NULL DEFAULT 0,
    herdr_socket_path TEXT NOT NULL DEFAULT '',
    herdr_workspace_id TEXT NOT NULL DEFAULT '',
    herdr_tab_id TEXT NOT NULL DEFAULT '',
    herdr_pane_id TEXT NOT NULL DEFAULT '',
    session_id TEXT NOT NULL DEFAULT '',
    worktree_path TEXT NOT NULL DEFAULT '',
    lease_id TEXT NOT NULL DEFAULT '',
    branch TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    runner_pid INTEGER NOT NULL DEFAULT 0,
    cursor INTEGER NOT NULL DEFAULT 0,
    exit_code INTEGER,
    failure_reason TEXT NOT NULL DEFAULT '',
    landed_proven INTEGER NOT NULL DEFAULT 0,
    discard_authorized INTEGER NOT NULL DEFAULT 0,
    released_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    ended_at TEXT,
    base_commit TEXT NOT NULL DEFAULT '',
    landing_kind TEXT NOT NULL DEFAULT '',
    landed_source_commit TEXT NOT NULL DEFAULT '',
    landed_target_ref TEXT NOT NULL DEFAULT '',
    landed_target_commit TEXT NOT NULL DEFAULT '',
    landed_checkpoint_revision INTEGER NOT NULL DEFAULT 0,
    landed_verified_at TEXT,
    release_state TEXT NOT NULL DEFAULT 'held' CHECK(release_state IN ('held', 'releasing', 'released', 'no_workspace')),
    release_claimed_at TEXT,
    release_owner_pid INTEGER NOT NULL DEFAULT 0,
    release_reason TEXT NOT NULL DEFAULT '',
    runtime_executable TEXT NOT NULL DEFAULT '',
    landing_quarantine_reason TEXT NOT NULL DEFAULT '',
    UNIQUE(task_id, number),
    CHECK((release_state = 'released') = (released_at IS NOT NULL)),
    CHECK(landed_proven = 0 OR (landing_kind <> '' AND landed_verified_at IS NOT NULL))
);
`

type unreleasedTreehouseAttempt struct {
	attemptID      string
	taskID         string
	workspaceState string
	releaseState   string
	path           string
	leaseID        string
}

func migrateTreehouseRetirementV15(ctx context.Context, conn *sql.Conn) error {
	rows, err := conn.QueryContext(ctx, `SELECT id, task_id, workspace_state, release_state, worktree_path, lease_id
		FROM attempts
		WHERE workspace_backend='treehouse'
		AND (released_at IS NULL OR release_state<>'released' OR workspace_state<>'released')
		ORDER BY created_at, id`)
	if err != nil {
		return fmt.Errorf("inspect legacy Treehouse attempts: %w", err)
	}
	defer rows.Close()
	var attempts []unreleasedTreehouseAttempt
	for rows.Next() {
		var attempt unreleasedTreehouseAttempt
		if err := rows.Scan(&attempt.attemptID, &attempt.taskID, &attempt.workspaceState, &attempt.releaseState, &attempt.path, &attempt.leaseID); err != nil {
			return fmt.Errorf("read legacy Treehouse attempt: %w", err)
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read legacy Treehouse attempts: %w", err)
	}
	if len(attempts) == 0 {
		return nil
	}
	evidence := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		evidence = append(evidence, fmt.Sprintf("attempt_id=%q task_id=%q workspace_state=%q release_state=%q path=%q lease_id=%q",
			attempt.attemptID, attempt.taskID, attempt.workspaceState, attempt.releaseState, attempt.path, attempt.leaseID))
	}
	return fmt.Errorf("Treehouse retirement blocked by %d unreleased legacy attempt(s): %s; use the previous Treehouse-capable Shephrd binary to release or explicitly discard every listed attempt, then retry",
		len(attempts), strings.Join(evidence, "; "))
}

const migration15Identity = "015_treehouse_retirement_gate revision 1\n" +
	"SELECT id, task_id, workspace_state, release_state, worktree_path, lease_id FROM attempts WHERE workspace_backend='treehouse' AND (released_at IS NULL OR release_state<>'released' OR workspace_state<>'released') ORDER BY created_at, id"

//go:embed testdata/legacy-migrations/*.sql
var legacyFixtureSQL embed.FS

// loadFrozenLegacyChain assembles the complete released 1-27 chain from the frozen
// pre-floor fixtures and the temporary bridge embed.
func loadFrozenLegacyChain(t *testing.T) []bridgeMigration {
	t.Helper()
	registry := []bridgeMigration{
		{version: 9, name: "009_lifecycle_integrity_repair", run: migrateLifecycleIntegrityV9, identity: migration9Identity, disableForeignKeys: true},
		{version: 15, name: "015_treehouse_retirement_gate", run: migrateTreehouseRetirementV15, identity: migration15Identity},
		{version: 21, name: "021_report_recovery", run: migrateReportRecoveryV21, identity: migration21SQL},
		{version: 27, name: "027_plan_surface", run: migratePlanSurfaceV27, identity: migration27SQL, disableForeignKeys: true},
	}
	add := func(fs interface {
		ReadDir(string) ([]os.DirEntry, error)
		ReadFile(string) ([]byte, error)
	}, dir string) {
		entries, err := fs.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			version, err := strconv.Atoi(strings.SplitN(entry.Name(), "_", 2)[0])
			if err != nil {
				t.Fatal(err)
			}
			body, err := fs.ReadFile(dir + "/" + entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			registry = append(registry, bridgeMigration{version: version, name: strings.TrimSuffix(entry.Name(), ".sql"), sql: string(body)})
		}
	}
	add(legacyFixtureSQL, "testdata/legacy-migrations")
	add(migrations, "migrations")
	sort.Slice(registry, func(i, j int) bool { return registry[i].version < registry[j].version })
	if len(registry) != 27 {
		t.Fatalf("registry length = %d", len(registry))
	}
	for i, entry := range registry {
		if entry.version != i+1 {
			t.Fatalf("not contiguous at %d: %d", i+1, entry.version)
		}
	}
	return registry
}

// buildLegacyDB reconstructs a historical database at the given schema
// version using only frozen migration bodies.
func buildLegacyDB(t *testing.T, target int) *sql.DB {
	t.Helper()
	return buildLegacyDBAt(t, filepath.Join(t.TempDir(), "state.db"), target)
}

func buildLegacyDBAt(t *testing.T, path string, target int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	registry := loadFrozenLegacyChain(t)
	for i, entry := range registry {
		if i >= target {
			break
		}
		ctx := context.Background()
		if entry.version == 1 {
			if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
				t.Fatal(err)
			}
		}
		if entry.disableForeignKeys {
			if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.Exec(`BEGIN IMMEDIATE`); err != nil {
			t.Fatal(err)
		}
		var err2 error
		if entry.run != nil {
			conn, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			err2 = entry.run(ctx, conn)
			conn.Close()
		} else if _, err2 = db.Exec(entry.sql); err2 != nil {
		}
		if entry.disableForeignKeys {
			if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
				t.Fatal(err)
			}
		}
		if err2 == nil && entry.version < 9 {
			_, err2 = db.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(?, '2026-01-01T00:00:00Z')`, entry.version)
		} else if err2 == nil {
			_, err2 = db.Exec(`INSERT INTO schema_migrations(version, name, checksum, applied_at) VALUES(?, ?, ?, '2026-01-01T00:00:00Z')`, entry.version, entry.name, entry.checksum())
		}
		if err2 != nil {
			db.Exec(`ROLLBACK`)
			t.Fatalf("apply %d: %v", entry.version, err2)
		}
		if _, err := db.Exec(`COMMIT`); err != nil {
			t.Fatal(err)
		}
	}
	if target >= 9 {
		for i, entry := range registry {
			if i >= 8 {
				break
			}
			if _, err := db.Exec(`UPDATE schema_migrations SET name=?, checksum=? WHERE version=? AND checksum=''`, entry.name, entry.checksum(), entry.version); err != nil {
				t.Fatal(err)
			}
		}
	}
	return db
}
