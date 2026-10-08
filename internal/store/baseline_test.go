package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"shephrd/internal/model"
)

const baselineWindowOpen = "2026-09-17T15:35:00Z"

var baselineWindowClose = time.Date(2026, 9, 17, 15, 35, 0, 0, time.UTC)

// populateBaselineEvidence inserts the durable evidence the baseline contract
// must preserve: a fully released Treehouse attempt with a complete attested
// local landing proof and its immutable recovery row, held and unknown native
// work, a no-workspace acquisition failure, a report artifact with a real
// snapshot, report inputs, and a notification. The column sets are valid for
// every supported bridge version.
func populateBaselineEvidence(t *testing.T, db *sql.DB, version int, snapshotDir string) {
	t.Helper()
	timestamp := "2026-01-01T00:00:00Z"
	snapshotPath := filepath.Join(snapshotDir, "report.md")
	body := []byte("durable report evidence\n")
	if err := os.WriteFile(snapshotPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	snapshotSHA := hex.EncodeToString(sum[:])
	sealed := strings.Repeat("e", 40)
	head := strings.Repeat("f", 40)

	taskColumns := "id, repo_id, feature_key, driver_id, objective, status, current_attempt_id, artifact_ref, claimed_done"
	if version >= 20 {
		taskColumns += ", title"
	}
	taskColumns += ", created_at, updated_at"
	taskValues := "'task_ev_done', 'repo_ev', 'ev-done', 'driver:test', 'released evidence', 'done', 'attempt_ev_done', 'branch:shephrd/task_ev_done', 1"
	if version >= 20 {
		taskValues += ", 'Ev done'"
	}
	taskValues += ", '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'"
	// Workspace identity columns exist from the native-worktree step on; the
	// canonical terminal columns exist from the terminal-endpoint step on, and
	// a released database at that step already carries backfilled values equal
	// to the legacy Herdr columns.
	heldWorkspaceColumns := ""
	heldWorkspaceValues := ""
	if version >= 12 {
		heldWorkspaceColumns = ", workspace_backend, workspace_state"
		heldWorkspaceValues = ", 'native_git_worktree', 'held'"
	}
	unknownWorkspaceColumns := ""
	unknownWorkspaceValues := ""
	if version >= 12 {
		unknownWorkspaceColumns = ", workspace_backend, workspace_state"
		unknownWorkspaceValues = ", 'native_git_worktree', 'unknown'"
	}
	doneWorkspaceColumns := ""
	doneWorkspaceValues := ""
	if version >= 12 {
		doneWorkspaceColumns = ", workspace_backend, workspace_state"
		doneWorkspaceValues = ", 'treehouse', 'released'"
	}
	terminalColumns := ""
	if version >= 18 {
		terminalColumns = ", terminal_socket_path, terminal_workspace_id, terminal_tab_id, terminal_pane_id"
	}
	terminalValues := ""
	if version >= 18 {
		terminalValues = ", '/socket', 'ws1', 'tab1', 'pane1'"
	}
	// The accepted-event identity columns exist from the report-lifecycle step
	// on; a current artifact carries the identity its invocations must match.
	artifactEventColumns := ""
	artifactEventValues := ""
	if version >= 22 {
		artifactEventColumns = ", accepted_event_id, accepted_event_name, accepted_event_version"
		artifactEventValues = ", 'event-1', 'report.accepted', 1"
	}
	legacyTerminalColumns := ", herdr_socket_path, herdr_workspace_id, herdr_tab_id, herdr_pane_id"
	legacyTerminalValues := ", '/socket', 'ws1', 'tab1', 'pane1'"
	if version >= 29 {
		legacyTerminalColumns, legacyTerminalValues = "", ""
	}
	statements := []string{
		fmt.Sprintf(`INSERT INTO repos(id, name, path, default_branch, created_at, updated_at)
			VALUES('repo_ev', 'ev', '/tmp/ev', 'main', '%s', '%s')`, timestamp, timestamp),
		fmt.Sprintf(`INSERT INTO tasks(%s)
			VALUES(%s),
			('task_ev_work', 'repo_ev', 'ev-work', 'driver:test', 'held and unknown work', 'working', 'attempt_ev_held', '', 0%s, '%s', '%s')`,
			taskColumns, taskValues, conditionalTitle(version, ", 'Ev work'"), timestamp, timestamp),
		fmt.Sprintf(`INSERT INTO attempts(id, task_id, number, harness, runtime_backend, run_generation`+legacyTerminalColumns+`,
			session_id, worktree_path, lease_id, branch, status, base_commit, landed_proven, landing_kind, landed_source_commit, landed_target_ref,
			landed_target_commit, landed_checkpoint_revision, landed_verified_at, release_state, released_at`+doneWorkspaceColumns+terminalColumns+`, created_at, updated_at)
			VALUES('attempt_ev_done', 'task_ev_done', 1, 'pi', 'herdr', 1`+legacyTerminalValues+`,
			'session-done', '/trees/done', 'lease-done', 'shephrd/task_ev_done', 'done', 'base', 1, 'local_default_branch_attested_ancestry', '%[1]s', 'refs/heads/main',
			'%[2]s', 2, '2026-01-02T00:00:00Z', 'released', '2026-01-02T00:00:00Z'`+doneWorkspaceValues+terminalValues+`, '%[3]s', '%[3]s')`, sealed, head, timestamp),
		fmt.Sprintf(`INSERT INTO attempts(id, task_id, number, harness, session_id, worktree_path, lease_id, branch, status, release_state`+heldWorkspaceColumns+`, created_at, updated_at)
			VALUES('attempt_ev_held', 'task_ev_work', 1, 'pi', 'session-held', '/trees/held', 'lease-held', 'shephrd/task_ev_work-1', 'working', 'held'`+heldWorkspaceValues+`, '%s', '%s')`, timestamp, timestamp),
		fmt.Sprintf(`INSERT INTO attempts(id, task_id, number, harness, session_id, worktree_path, lease_id, branch, status, release_state, failure_reason`+unknownWorkspaceColumns+`, created_at, updated_at)
			VALUES('attempt_ev_unknown', 'task_ev_work', 2, 'pi', 'session-unknown', '/trees/unknown', 'lease-unknown', 'shephrd/task_ev_work-2', 'unknown', 'held', 'lease identity cannot be verified'`+unknownWorkspaceValues+`, '%s', '%s')`, timestamp, timestamp),
		fmt.Sprintf(`INSERT INTO attempts(id, task_id, number, harness, status, release_state, release_reason, failure_reason, created_at, updated_at)
			VALUES('attempt_ev_nowork', 'task_ev_work', 3, 'pi', 'blocked', 'no_workspace', 'treehouse acquire failed: authentication failed', 'treehouse acquire failed: authentication failed', '%s', '%s')`, timestamp, timestamp),
		fmt.Sprintf(`INSERT INTO messages(task_id, attempt_id, direction, type, payload, artifact_ref, source_cursor, run_generation, created_at)
			VALUES('task_ev_done', 'attempt_ev_done', 'worker-to-driver', 'done', 'done', 'branch:shephrd/task_ev_done', 1, 1, '%s')`, timestamp),
		fmt.Sprintf(`INSERT INTO verified_artifacts(id, producer_task_id, producer_attempt_id, done_message_id, kind, original_ref, sha256, size_bytes, snapshot_path, verified_at`+artifactEventColumns+`)
			VALUES('artifact_ev', 'task_ev_done', 'attempt_ev_done', 1, 'report', 'report:/tmp/report.md', '%s', %d, '%s', '%s'`+artifactEventValues+`)`, snapshotSHA, len(body), snapshotPath, timestamp),
		fmt.Sprintf(`INSERT INTO task_report_inputs(target_task_id, artifact_id, attached_by_driver_id, attached_at)
			VALUES('task_ev_work', 'artifact_ev', 'driver:test', '%s')`, timestamp),
		fmt.Sprintf(`INSERT INTO driver_notifications(notification_id, message_id, task_id, attempt_id, target_driver_id, worker_run_generation, source_cursor, kind, state, created_at, updated_at)
			VALUES('notif_ev', 1, 'task_ev_done', 'attempt_ev_done', 'driver:test', 1, 1, 'done', 'acknowledged', '%s', '%s')`, timestamp, timestamp),
	}
	// The immutable recovery table only exists from its released migration on;
	// below-floor exports carry whatever rows the source actually had.
	if version >= 11 {
		statements = append(statements, fmt.Sprintf(`INSERT INTO local_delivery_recoveries(id, schema_version, task_id, attempt_id, repo_id, run_generation, done_message_id, checkpoint_revision,
			original_artifact_ref, attempt_branch, sealed_commit, registered_common_git_dir, registered_default_branch, default_head_at_validation,
			ancestry_validation, attested_by_driver_id, evidence_validated_at, created_at)
			VALUES('recovery_ev', 1, 'task_ev_done', 'attempt_ev_done', 'repo_ev', 1, 1, 2, 'branch:shephrd/task_ev_don', 'shephrd/task_ev_done', '%[1]s', '/repo/.git', 'main', '%[2]s',
			'local-default-ancestry+shared-common-dir-v1', 'driver:test', '%[3]s', '%[3]s')`, sealed, head, timestamp))
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("fixture statement failed: %v\n%s", err, statement)
		}
	}
}

func conditionalTitle(version int, title string) string {
	if version >= 20 {
		return title
	}
	return ""
}

// addLegacyTaskTables reproduces the live database's migration-9 preserved
// tables: the abandoned v6/v7 task-list schema retained under legacy_* names.
func addLegacyTaskTables(t *testing.T, db *sql.DB, populated bool) {
	t.Helper()
	statements := []string{
		`CREATE TABLE legacy_task_lists (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			driver_id TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE legacy_task_list_items (
			id TEXT PRIMARY KEY,
			list_id TEXT NOT NULL,
			position INTEGER NOT NULL,
			repo_id TEXT NOT NULL,
			feature_key TEXT NOT NULL,
			objective TEXT NOT NULL,
			acceptance_criteria TEXT NOT NULL DEFAULT '',
			deliverable TEXT NOT NULL DEFAULT 'code',
			dispatched_task_id TEXT,
			dispatched_at TEXT,
			dispatch_override_reason TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE legacy_task_list_prerequisites (
			item_id TEXT NOT NULL,
			prerequisite_item_id TEXT NOT NULL,
			added_at TEXT NOT NULL,
			PRIMARY KEY(item_id, prerequisite_item_id)
		)`,
		`CREATE TABLE legacy_task_list_item_inputs (
			item_id TEXT NOT NULL,
			producer_item_id TEXT NOT NULL,
			artifact_id TEXT NOT NULL,
			position INTEGER NOT NULL,
			selected_by_driver_id TEXT NOT NULL,
			selected_at TEXT NOT NULL,
			PRIMARY KEY(item_id, artifact_id)
		)`,
		`CREATE TABLE legacy_task_list_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			list_id TEXT NOT NULL,
			item_id TEXT,
			driver_id TEXT NOT NULL,
			operation TEXT NOT NULL,
			detail_json TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TRIGGER legacy_task_list_events_immutable_update
			BEFORE UPDATE ON legacy_task_list_events
			BEGIN
				SELECT RAISE(ABORT, 'task list events are immutable');
			END`,
		`CREATE TRIGGER legacy_task_list_events_immutable_delete
			BEFORE DELETE ON legacy_task_list_events
			BEGIN
				SELECT RAISE(ABORT, 'task list events are immutable');
			END`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("legacy table statement failed: %v\n%s", err, statement)
		}
	}
	if !populated {
		return
	}
	inserts := []string{
		`INSERT INTO legacy_task_lists(id, name, driver_id, created_at, updated_at)
			VALUES('list_legacy', 'rebuild', 'driver:test', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		`INSERT INTO legacy_task_list_items(id, list_id, position, repo_id, feature_key, objective, created_at, updated_at)
			VALUES('item_legacy', 'list_legacy', 1, 'repo_ev', 'legacy-item', 'historical item', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		`INSERT INTO legacy_task_list_events(list_id, item_id, driver_id, operation, detail_json, created_at)
			VALUES('list_legacy', 'item_legacy', 'driver:test', 'dispatch', '{}', '2026-01-01T00:00:00Z')`,
	}
	for _, statement := range inserts {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("legacy table insert failed: %v\n%s", err, statement)
		}
	}
}

func addReleasedLiveBridgeShape(t *testing.T, db *sql.DB) {
	t.Helper()
	statements := []string{
		`CREATE TABLE "legacy_task_lists" (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    driver_id TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
)`,
		`CREATE TABLE "legacy_task_list_items" (
    id TEXT PRIMARY KEY,
    list_id TEXT NOT NULL REFERENCES "legacy_task_lists"(id),
    position INTEGER NOT NULL CHECK(position > 0),
    repo_id TEXT NOT NULL REFERENCES repos(id),
    feature_key TEXT NOT NULL,
    objective TEXT NOT NULL,
    acceptance_criteria TEXT NOT NULL DEFAULT '',
    deliverable TEXT NOT NULL DEFAULT 'code' CHECK(deliverable IN ('code', 'report')),
    dispatched_task_id TEXT UNIQUE REFERENCES tasks(id),
    dispatched_at TEXT,
    dispatch_override_reason TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(list_id, feature_key)
)`,
		`CREATE TABLE "legacy_task_list_prerequisites" (
    item_id TEXT NOT NULL REFERENCES "legacy_task_list_items"(id),
    prerequisite_item_id TEXT NOT NULL REFERENCES "legacy_task_list_items"(id),
    added_at TEXT NOT NULL,
    PRIMARY KEY(item_id, prerequisite_item_id),
    CHECK(item_id <> prerequisite_item_id)
)`,
		`CREATE TABLE "legacy_task_list_item_inputs" (
    item_id TEXT NOT NULL REFERENCES "legacy_task_list_items"(id),
    producer_item_id TEXT NOT NULL REFERENCES "legacy_task_list_items"(id),
    artifact_id TEXT NOT NULL REFERENCES verified_artifacts(id),
    position INTEGER NOT NULL CHECK(position > 0),
    selected_by_driver_id TEXT NOT NULL,
    selected_at TEXT NOT NULL,
    PRIMARY KEY(item_id, artifact_id),
    UNIQUE(item_id, producer_item_id),
    UNIQUE(item_id, position),
    CHECK(item_id <> producer_item_id)
)`,
		`CREATE TABLE "legacy_task_list_events" (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    list_id TEXT NOT NULL REFERENCES "legacy_task_lists"(id),
    item_id TEXT REFERENCES "legacy_task_list_items"(id),
    driver_id TEXT NOT NULL,
    operation TEXT NOT NULL,
    detail_json TEXT NOT NULL,
    created_at TEXT NOT NULL
)`,
		`CREATE TRIGGER task_list_events_immutable_update
BEFORE UPDATE ON "legacy_task_list_events"
BEGIN
    SELECT RAISE(ABORT, 'task list events are immutable');
END`,
		`CREATE TRIGGER task_list_events_immutable_delete
BEFORE DELETE ON "legacy_task_list_events"
BEGIN
    SELECT RAISE(ABORT, 'task list events are immutable');
END`,
		`DROP TRIGGER verified_artifacts_immutable_update`,
		`DROP TRIGGER verified_artifacts_immutable_delete`,
		`UPDATE schema_migrations SET checksum='' WHERE version BETWEEN 1 AND 8`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("released live shape statement failed: %v\n%s", err, statement)
		}
	}
}

// assertBaselineEvidence verifies the retained-evidence contract on an
// upgraded or fresh database: every count, key relation, landing projection,
// and artifact hash survives the convergence.
func assertBaselineEvidence(t *testing.T, state *Store, version int) {
	t.Helper()
	var count int
	if err := state.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("ledger rows = %d, err = %v", count, err)
	}
	var recordedVersion int
	var recordedName, checksum string
	if err := state.db.QueryRow(`SELECT version, name, checksum FROM schema_migrations`).Scan(&recordedVersion, &recordedName, &checksum); err != nil {
		t.Fatal(err)
	}
	if recordedVersion != baselineVersion || recordedName != baselineName || checksum != baselineChecksum() {
		t.Fatalf("baseline identity = %d %q %s", recordedVersion, recordedName, checksum)
	}
	var originalVersion int
	var oldLedgerDigest, releasedAt, exportClose string
	if err := state.db.QueryRow(`SELECT original_version, old_ledger_digest, baseline_released_at, legacy_export_closes_at FROM schema_baseline_provenance`).Scan(&originalVersion, &oldLedgerDigest, &releasedAt, &exportClose); err != nil {
		t.Fatal(err)
	}
	if originalVersion != version {
		t.Fatalf("provenance original version = %d, want %d", originalVersion, version)
	}
	if version != 0 && oldLedgerDigest == "" {
		t.Fatal("upgraded provenance lost the old ledger digest")
	}
	if releasedAt != baselineReleasedAt || exportClose != baselineWindowOpen {
		t.Fatalf("provenance window = %s %s", releasedAt, exportClose)
	}

	counts := map[string]int{}
	for table, want := range map[string]int{"tasks": 2, "attempts": 4, "messages": 1, "verified_artifacts": 1, "driver_notifications": 1, "local_delivery_recoveries": 1, "task_report_inputs": 1, "plan_report_inputs": 0} {
		if err := state.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != want {
			t.Fatalf("%s rows = %d, want %d", table, count, want)
		}
		counts[table] = count
	}
	if err := state.db.QueryRow(`SELECT COUNT(*) FROM attempts WHERE release_state='held'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("held attempts = %d, err = %v", count, err)
	}
	if err := state.db.QueryRow(`SELECT COUNT(*) FROM attempts WHERE workspace_state='unknown'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("unknown workspaces = %d, err = %v", count, err)
	}
	if err := state.db.QueryRow(`SELECT COUNT(*) FROM attempt_landing_projections WHERE landed=1`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("landed attempt projections = %d, err = %v", count, err)
	}
	if err := state.db.QueryRow(`SELECT COUNT(*) FROM task_landing_projections WHERE landed=1`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("landed task projections = %d, err = %v", count, err)
	}
	var landed int
	var landedReason string
	if err := state.db.QueryRow(`SELECT landed, COALESCE(landed_reason,'') FROM task_landing_projections WHERE task_id='task_ev_done'`).Scan(&landed, &landedReason); err != nil {
		t.Fatal(err)
	}
	if landed != 1 {
		t.Fatalf("released evidence task projection = %d, reason %q", landed, landedReason)
	}

	done, err := state.Attempt("attempt_ev_done")
	if err != nil {
		t.Fatal(err)
	}
	if done.WorkspaceBackend != model.WorkspaceBackendTreehouse || done.WorkspaceState != model.WorkspaceStateReleased || done.ReleaseState != "released" || done.ReleasedAt == nil {
		t.Fatalf("treehouse evidence = %+v", done)
	}
	if !done.LandedProven || done.LandingKind != model.LandingKindLocalAttestedAncestry || done.LandedSourceCommit != strings.Repeat("e", 40) || done.LandedTargetCommit != strings.Repeat("f", 40) {
		t.Fatalf("landing proof = %+v", done)
	}
	if done.TerminalEndpoint == nil || done.TerminalEndpoint.Backend != "herdr" || done.TerminalEndpoint.SocketPath != "/socket" || done.TerminalEndpoint.WorkspaceID != "ws1" || done.TerminalEndpoint.TabID != "tab1" || done.TerminalEndpoint.PaneID != "pane1" {
		t.Fatalf("retired endpoint columns lost the canonical identity: %+v", done.TerminalEndpoint)
	}
	held, err := state.Attempt("attempt_ev_held")
	if err != nil {
		t.Fatal(err)
	}
	if held.WorkspaceBackend != model.WorkspaceBackendNative || held.WorkspaceState != model.WorkspaceStateHeld || held.ReleaseState != "held" || held.LeaseID != "lease-held" {
		t.Fatalf("held native evidence = %+v", held)
	}
	unknown, err := state.Attempt("attempt_ev_unknown")
	if err != nil {
		t.Fatal(err)
	}
	if unknown.WorkspaceState != model.WorkspaceStateUnknown || unknown.Status != "unknown" || unknown.ReleaseState != "held" {
		t.Fatalf("unknown workspace evidence = %+v", unknown)
	}
	nowork, err := state.Attempt("attempt_ev_nowork")
	if err != nil {
		t.Fatal(err)
	}
	if nowork.ReleaseState != "no_workspace" || nowork.FailureReason == "" {
		t.Fatalf("no-workspace evidence = %+v", nowork)
	}
	recovery, err := state.LocalDeliveryRecoveryForAttempt("attempt_ev_done")
	if err != nil || recovery == nil || recovery.OriginalArtifactRef != "branch:shephrd/task_ev_don" || recovery.SealedCommit != strings.Repeat("e", 40) {
		t.Fatalf("immutable recovery = %+v, err = %v", recovery, err)
	}
	var sha, size string
	var sizeBytes int64
	if err := state.db.QueryRow(`SELECT sha256, size_bytes, snapshot_path FROM verified_artifacts WHERE id='artifact_ev'`).Scan(&sha, &sizeBytes, &size); err != nil {
		t.Fatal(err)
	}
	if sha == "" || sizeBytes != 24 || size == "" {
		t.Fatalf("artifact evidence = %q %d %q", sha, sizeBytes, size)
	}
	var violations int
	if err := state.db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
		t.Fatalf("foreign key violations = %d, err = %v", violations, err)
	}
	for _, column := range []string{"herdr_socket_path", "herdr_workspace_id", "herdr_tab_id", "herdr_pane_id"} {
		if err := state.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('attempts') WHERE name=?`, column).Scan(&count); err != nil || count != 0 {
			t.Fatalf("retired column %s still present = %d", column, count)
		}
	}
	for _, table := range legacyTaskTables {
		if err := state.db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name=?`, table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("legacy table %s still present = %d", table, count)
		}
	}
}

// schemaObjects returns the complete schema-object level of a database:
// type, name, and the stored DDL text for every non-internal object.
func schemaObjects(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	objects := map[string]string{}
	rows, err := db.Query(`SELECT type, name, COALESCE(sql, '') FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var kind, name, sqlText string
		if err := rows.Scan(&kind, &name, &sqlText); err != nil {
			t.Fatal(err)
		}
		objects[kind+" "+name] = sqlText
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return objects
}

func TestBaselineFreshInstallMatchesUpgradedBaseline(t *testing.T) {
	freshPath := filepath.Join(t.TempDir(), "fresh.db")
	fresh, err := Open(freshPath)
	if err != nil {
		t.Fatal(err)
	}
	freshObjects := schemaObjects(t, fresh.db)
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}

	upgradePath := filepath.Join(t.TempDir(), "upgraded.db")
	legacy := buildLegacyDBAt(t, upgradePath, 15)
	populateBaselineEvidence(t, legacy, 15, t.TempDir())
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(upgradePath)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	assertBaselineEvidence(t, upgraded, 15)

	upgradeObjects := schemaObjects(t, upgraded.db)
	if len(freshObjects) != len(upgradeObjects) {
		t.Fatalf("fresh objects = %d, upgraded objects = %d", len(freshObjects), len(upgradeObjects))
	}
	var mismatches []string
	for key, sqlText := range freshObjects {
		if got, ok := upgradeObjects[key]; !ok {
			mismatches = append(mismatches, "missing "+key)
		} else if got != sqlText {
			mismatches = append(mismatches, "diverged "+key)
		}
	}
	for key := range upgradeObjects {
		if _, ok := freshObjects[key]; !ok {
			mismatches = append(mismatches, "unexpected "+key)
		}
	}
	if len(mismatches) != 0 {
		sort.Strings(mismatches)
		t.Fatalf("fresh and upgraded baselines diverge: %s", strings.Join(mismatches, ", "))
	}
}

func TestBaselineUpgradesEverySupportedVersionWithPopulatedEvidence(t *testing.T) {
	for version := 15; version <= 27; version++ {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			snapshotDir := t.TempDir()
			path := filepath.Join(t.TempDir(), "state.db")
			legacy := buildLegacyDBAt(t, path, version)
			populateBaselineEvidence(t, legacy, version, snapshotDir)
			if err := legacy.Close(); err != nil {
				t.Fatal(err)
			}
			state, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer state.Close()
			assertBaselineEvidence(t, state, version)
			if err := state.Close(); err != nil {
				t.Fatal(err)
			}
			state, err = Open(path)
			if err != nil {
				t.Fatalf("second open of the upgraded database: %v", err)
			}
			defer state.Close()
			var count int
			if err := state.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil || count != 2 {
				t.Fatalf("reopened ledger rows = %d, err = %v", count, err)
			}
		})
	}
}

func fileMetadata(t *testing.T, dir string) map[string]os.FileInfo {
	t.Helper()
	metadata := map[string]os.FileInfo{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		info, err := os.Stat(filepath.Join(dir, entry.Name()))
		if err == nil {
			metadata[entry.Name()] = info
		}
	}
	return metadata
}

func TestBaselineUpgradesReleasedLiveShapeAtEveryBridgeVersion(t *testing.T) {
	for version := 15; version <= 27; version++ {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			legacy := buildLegacyDBAt(t, path, version)
			addReleasedLiveBridgeShape(t, legacy)
			if err := legacy.Close(); err != nil {
				t.Fatal(err)
			}
			state, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer state.Close()
			var originalVersion int
			if err := state.db.QueryRow(`SELECT original_version FROM schema_baseline_provenance`).Scan(&originalVersion); err != nil || originalVersion != version {
				t.Fatalf("baseline provenance version = %d, err = %v", originalVersion, err)
			}
			for _, name := range []string{"verified_artifacts_immutable_delete", "verified_artifacts_immutable_update"} {
				var count int
				if err := state.db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type='trigger' AND name=? AND tbl_name='verified_artifacts'`, name).Scan(&count); err != nil || count != 1 {
					t.Fatalf("restored trigger %s count = %d, err = %v", name, count, err)
				}
			}
		})
	}
}

func TestBaselineUpgradesReleasedLiveV23ShapeWithEvidence(t *testing.T) {
	snapshotDir := t.TempDir()
	path := filepath.Join(t.TempDir(), "state.db")
	legacy := buildLegacyDBAt(t, path, 23)
	populateBaselineEvidence(t, legacy, 23, snapshotDir)
	addReleasedLiveBridgeShape(t, legacy)
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	state, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	assertBaselineEvidence(t, state, 23)
	for _, statement := range []string{
		`UPDATE verified_artifacts SET size_bytes=size_bytes WHERE id='artifact_ev'`,
		`DELETE FROM verified_artifacts WHERE id='artifact_ev'`,
	} {
		if _, err := state.db.Exec(statement); err == nil || !strings.Contains(err.Error(), "verified artifacts are immutable") {
			t.Fatalf("restored artifact immutability did not reject %q: %v", statement, err)
		}
	}
}

func TestBaselineRefusesReleasedLiveShapeNearMisses(t *testing.T) {
	tests := []struct {
		name      string
		released  bool
		mutateSQL string
	}{
		{"missing verified triggers without legacy lineage", false, `DROP TRIGGER verified_artifacts_immutable_update; DROP TRIGGER verified_artifacts_immutable_delete`},
		{"unexpected legacy triggers with verified immutability", true, releasedLiveVerifiedArtifactTriggers},
		{"missing legacy trigger", true, `DROP TRIGGER task_list_events_immutable_update`},
		{"weakened legacy trigger", true, `DROP TRIGGER task_list_events_immutable_update; CREATE TRIGGER task_list_events_immutable_update BEFORE UPDATE ON legacy_task_list_events BEGIN SELECT 1; END`},
		{"altered legacy table", true, `ALTER TABLE legacy_task_list_events ADD COLUMN forged TEXT`},
		{"weakened legacy table constraint", true, `PRAGMA writable_schema=ON; UPDATE sqlite_schema SET sql=replace(sql, 'position INTEGER NOT NULL CHECK(position > 0)', 'position INTEGER NOT NULL CHECK(position >= 0)') WHERE name='legacy_task_list_items'; PRAGMA writable_schema=OFF`},
		{"canonicalized preidentity checksum", true, `UPDATE schema_migrations SET checksum='62d6bd2991767aa0c0dc9f903f4d1e92eda2d9a385ef5fcd872d9c2feef7cc52' WHERE version=1`},
		{"missing postidentity checksum", true, `UPDATE schema_migrations SET checksum='' WHERE version=9`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			legacy := buildLegacyDBAt(t, path, 23)
			if test.released {
				addReleasedLiveBridgeShape(t, legacy)
			}
			if _, err := legacy.Exec(test.mutateSQL); err != nil {
				t.Fatal(err)
			}
			if err := legacy.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "database schema shape at version 23 diverges from the released shape") {
				t.Fatalf("near-miss shape opened, err = %v", err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var version int
			if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil || version != 23 {
				t.Fatalf("refusal changed schema version to %d, err = %v", version, err)
			}
		})
	}
}

func TestBaselineBelowFloorVersionsAreRefusedReadOnly(t *testing.T) {
	previousClock := baselineClock
	baselineClock = func() time.Time { return baselineExportClose.Add(-time.Hour) }
	t.Cleanup(func() { baselineClock = previousClock })
	for version := 1; version <= 14; version++ {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state.db")
			legacy := buildLegacyDBAt(t, path, version)
			if err := legacy.Close(); err != nil {
				t.Fatal(err)
			}
			before := fileMetadata(t, dir)
			_, err := Open(path)
			want := fmt.Sprintf("database schema %d is below the minimum directly upgradable schema 15; refusing to migrate or write. Use the signed Shephrd v23 legacy upgrader or read-only exporter before %s; no database changes were made", version, baselineWindowOpen)
			if err == nil || err.Error() != want {
				t.Fatalf("refusal = %q, want %q", err, want)
			}
			after := fileMetadata(t, dir)
			if len(before) != len(after) {
				t.Fatalf("refusal changed the database directory: %v -> %v", names(before), names(after))
			}
			for name, info := range before {
				got := after[name]
				if got == nil || got.Size() != info.Size() || !got.ModTime().Equal(info.ModTime()) {
					t.Fatalf("refusal mutated %s: %v -> %v", name, info, got)
				}
			}
		})
	}
}

func names(metadata map[string]os.FileInfo) []string {
	out := make([]string, 0, len(metadata))
	for name := range metadata {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func TestBaselineBelowFloorRefusalClosesWithTheWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	legacy := buildLegacyDBAt(t, path, 8)
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	previous := baselineClock
	baselineClock = func() time.Time { return baselineWindowClose.Add(time.Second) }
	defer func() { baselineClock = previous }()
	_, err := Open(path)
	want := fmt.Sprintf("legacy schema support closed at %s; database schema 8 cannot be opened by this binary. Restore a backup and use the archived signed legacy exporter; do not delete schema_migrations rows", baselineWindowOpen)
	if err == nil || err.Error() != want {
		t.Fatalf("refusal = %q, want %q", err, want)
	}
}

func TestBaselineRefusesActiveOperations(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		mutate string
	}{
		{"live worker", `UPDATE attempts SET runner_pid=4242 WHERE id='attempt_ev_held'`},
		{"pending terminal create", `UPDATE attempts SET terminal_create_state='pending' WHERE id='attempt_ev_held'`},
		{"active release owner", `UPDATE attempts SET release_state='releasing', release_claimed_at='2026-01-01T00:00:00Z', release_owner_pid=4242 WHERE id='attempt_ev_held'`},
		{"invoking handler", `INSERT INTO report_lifecycle_invocations(id, event_id, event_name, event_version, artifact_id, task_id, attempt_id, handler_name, extension_id, configuration_hash, position, state, created_at, updated_at)
			VALUES('invocation-1', 'event-1', 'report.accepted', 1, 'artifact_ev', 'task_ev_done', 'attempt_ev_done', 'handler-1', 'extension-1', '` + strings.Repeat("a", 64) + `', 1, 'invoking', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			legacy := buildLegacyDBAt(t, path, 26)
			populateBaselineEvidence(t, legacy, 26, t.TempDir())
			if _, err := legacy.Exec(scenario.mutate); err != nil {
				t.Fatal(err)
			}
			if err := legacy.Close(); err != nil {
				t.Fatal(err)
			}
			_, err := Open(path)
			want := "baseline migration refused: 1 worker, terminal, release, or handler operation(s) are active; quiesce them and retry; no database changes were made"
			if err == nil || err.Error() != want {
				t.Fatalf("refusal = %q, want %q", err, want)
			}
		})
	}
}

func TestBaselineRefusesUnreleasedTreehouseAndPreservesReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	legacy := buildLegacyDBAt(t, path, 22)
	populateBaselineEvidence(t, legacy, 22, t.TempDir())
	if _, err := legacy.Exec(`INSERT INTO attempts(id, task_id, number, harness, status, worktree_path, lease_id, branch, release_state,
		workspace_backend, workspace_state, created_at, updated_at)
		VALUES('attempt_ev_treehouse', 'task_ev_work', 4, 'pi', 'working', '/trees/th', 'lease-th', 'shephrd/treehouse', 'held',
		'treehouse', 'held', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := Open(path)
	want := "baseline migration refused: 1 Treehouse attempt(s) lack complete released evidence; use the previous Treehouse-capable binary to release or explicitly discard each attempt; no database changes were made"
	if err == nil || err.Error() != want {
		t.Fatalf("refusal = %q, want %q", err, want)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil || count != 22 {
		t.Fatalf("refusal changed the ledger: %d rows, err = %v", count, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM attempts WHERE id='attempt_ev_done' AND release_state='released'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("released treehouse evidence lost: %d, err = %v", count, err)
	}
	db.Close()
}

func TestBaselineRefusesHerdrEndpointConflictAndMalformedCapabilities(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		mutate string
	}{
		{"conflicting herdr identity", `UPDATE attempts SET herdr_socket_path='/other' WHERE id='attempt_ev_done'`},
		{"malformed capability array", `UPDATE attempts SET terminal_capabilities_json='{"not":"array"}' WHERE id='attempt_ev_done'`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			legacy := buildLegacyDBAt(t, path, 22)
			populateBaselineEvidence(t, legacy, 22, t.TempDir())
			if _, err := legacy.Exec(scenario.mutate); err != nil {
				t.Fatal(err)
			}
			if err := legacy.Close(); err != nil {
				t.Fatal(err)
			}
			_, err := Open(path)
			want := "baseline migration refused: 1 attempt(s) have conflicting legacy Herdr and canonical terminal endpoint identity; inspect with Shephrd v23; no database changes were made"
			if err == nil || err.Error() != want {
				t.Fatalf("refusal = %q, want %q", err, want)
			}
		})
	}
}

func TestBaselineRetiresHerdrColumnsWhenEqual(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	legacy := buildLegacyDBAt(t, path, 17)
	populateBaselineEvidence(t, legacy, 17, t.TempDir())
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	state, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	assertBaselineEvidence(t, state, 17)
}

func TestBaselineRequiresAcceptedLegacyExportDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	legacy := buildLegacyDBAt(t, path, 15)
	populateBaselineEvidence(t, legacy, 15, t.TempDir())
	addLegacyTaskTables(t, legacy, true)
	conn, err := legacy.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, digest, err := legacyTableDigest(conn)
	conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = Open(path)
	want := "baseline migration refused: retired legacy task-list tables contain 3 row(s) without an accepted export digest; export them and retry with that digest; no database changes were made"
	if err == nil || err.Error() != want {
		t.Fatalf("refusal = %q, want %q", err, want)
	}

	state, err := OpenWithOptions(path, BaselineOptions{LegacyExportDigest: digest})
	if err != nil {
		t.Fatalf("accepted digest did not migrate: %v", err)
	}
	defer state.Close()
	assertBaselineEvidence(t, state, 15)
}

func TestBaselineRefusesUnresolvedLocalRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	legacy := buildLegacyDBAt(t, path, 22)
	populateBaselineEvidence(t, legacy, 22, t.TempDir())
	// The completed recovery row stays, but its attempt loses its proof: the
	// recovery is now unproven and the writer retirement must refuse.
	if _, err := legacy.Exec(`UPDATE attempts SET landed_proven=0, landing_kind='', landed_verified_at=NULL, landed_source_commit='', landed_target_ref='', landed_target_commit='', landed_checkpoint_revision=0
		WHERE id='attempt_ev_done'`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := Open(path)
	want := "local-recovery writer retirement refused: 1 unreleased or unproven local mismatch recovery attempt(s) remain; verify and release them or retain the compatibility command"
	if err == nil || err.Error() != want {
		t.Fatalf("refusal = %q, want %q", err, want)
	}
}

func TestBaselineRefusesSnapshotMismatch(t *testing.T) {
	snapshotDir := t.TempDir()
	path := filepath.Join(t.TempDir(), "state.db")
	legacy := buildLegacyDBAt(t, path, 22)
	populateBaselineEvidence(t, legacy, 22, snapshotDir)
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshotDir, "report.md"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(path)
	want := "baseline migration refused: 1 of 1 report snapshot(s) do not match their stored size or hash; restore the affected snapshots from backup and retry; no database changes were made"
	if err == nil || err.Error() != want {
		t.Fatalf("refusal = %q, want %q", err, want)
	}
}

func TestBaselineFaultsBeforeCommitLeaveDatabaseIntact(t *testing.T) {
	stages := []string{"preconditions", "endpoint-retirement", "legacy-drop", "ledger-rebuild", "commit"}
	for version := 16; version <= 27; version++ {
		stages = append(stages, fmt.Sprintf("bridge-%d", version))
	}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			expected := 27
			if strings.HasPrefix(stage, "bridge-") {
				step, err := strconv.Atoi(strings.TrimPrefix(stage, "bridge-"))
				if err != nil {
					t.Fatal(err)
				}
				expected = step - 1
			}
			path := filepath.Join(t.TempDir(), "state.db")
			legacy := buildLegacyDBAt(t, path, 15)
			populateBaselineEvidence(t, legacy, 15, t.TempDir())
			if err := legacy.Close(); err != nil {
				t.Fatal(err)
			}
			previous := baselineStageFault
			baselineStageFault = func(faulted string) error {
				if faulted == stage {
					return fmt.Errorf("injected %s fault", stage)
				}
				return nil
			}
			defer func() { baselineStageFault = previous }()
			_, err := Open(path)
			if err == nil || !strings.Contains(err.Error(), "injected "+stage+" fault") {
				t.Fatalf("fault did not surface: %v", err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var maxVersion int
			if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&maxVersion); err != nil || maxVersion != expected {
				t.Fatalf("fault left the database at version %d, want the released step %d, err = %v", maxVersion, expected, err)
			}
			var herdrColumns int
			if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('attempts') WHERE name LIKE 'herdr_%'`).Scan(&herdrColumns); err != nil || herdrColumns != 4 {
				t.Fatalf("fault retired the endpoint columns: %d, err = %v", herdrColumns, err)
			}
			var evidence int
			if err := db.QueryRow(`SELECT COUNT(*) FROM attempts WHERE id='attempt_ev_done'`).Scan(&evidence); err != nil || evidence != 1 {
				t.Fatalf("fault lost durable evidence: %d, err = %v", evidence, err)
			}
		})
	}
}

func TestBaselineBackupAndManifest(t *testing.T) {
	snapshotDir := t.TempDir()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	legacy := buildLegacyDBAt(t, path, 15)
	populateBaselineEvidence(t, legacy, 15, snapshotDir)
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	state, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()

	backups, manifests := 0, 0
	var manifestPath string
	for _, entry := range names(fileMetadata(t, dir)) {
		if strings.Contains(entry, ".manifest.json") {
			manifests++
			manifestPath = filepath.Join(dir, entry)
			continue
		}
		if strings.Contains(entry, ".baseline-backup-") {
			backups++
		}
	}
	if backups != 1 || manifests != 1 {
		t.Fatalf("backups=%d manifests=%d in %v", backups, manifests, names(fileMetadata(t, dir)))
	}
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Backup       string `json:"backup"`
		BackupSHA256 string `json:"backup_sha256"`
		BackupBytes  int    `json:"backup_bytes"`
		Original     int    `json:"original_version"`
		LedgerDigest string `json:"old_ledger_digest"`
		ReleasedAt   string `json:"baseline_released_at"`
		ExportCloses string `json:"legacy_export_closes_at"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Original != 15 || manifest.LedgerDigest == "" || manifest.BackupSHA256 == "" || manifest.ReleasedAt != baselineReleasedAt || manifest.ExportCloses != baselineWindowOpen {
		t.Fatalf("manifest = %+v", manifest)
	}
	backupBytes, err := os.ReadFile(filepath.Join(dir, manifest.Backup))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(backupBytes)
	if hex.EncodeToString(sum[:]) != manifest.BackupSHA256 || len(backupBytes) != manifest.BackupBytes {
		t.Fatalf("backup hash mismatch: %x vs %s", sum, manifest.BackupSHA256)
	}
}

// previousBinaryRefusal replays the released previous binary's migration
// fence: it knew versions 1-27 from the released chain. Any ledger row it does
// not recognize by identity is refused before writes.
func previousBinaryRefusal(t *testing.T, db *sql.DB) string {
	t.Helper()
	known := map[int]string{}
	for version, frozen := range frozenMigrationIdentities {
		known[version] = frozen.name
	}
	rows, err := db.Query(`SELECT version, COALESCE(name, '') FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var version int
		var name string
		if err := rows.Scan(&version, &name); err != nil {
			t.Fatal(err)
		}
		expected, knownVersion := known[version]
		if !knownVersion {
			return fmt.Sprintf("database records schema migration %d, which this binary does not know; refusing to open a database written by a newer or incompatible shephrd", version)
		}
		if name != "" && name != expected {
			return fmt.Sprintf("database migration %d identity is %q but this binary expects %q; a recorded version number cannot substitute for compatible SQL, refusing to open", version, name, expected)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ""
}

func TestPreviousBinaryRefusesUpgradedDatabase(t *testing.T) {
	for _, version := range []int{15, 22, 27} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			legacy := buildLegacyDBAt(t, path, version)
			populateBaselineEvidence(t, legacy, version, t.TempDir())
			if err := legacy.Close(); err != nil {
				t.Fatal(err)
			}
			upgraded, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer upgraded.Close()
			refusal := previousBinaryRefusal(t, upgraded.db)
			if refusal == "" {
				t.Fatal("the previous binary would accept the upgraded database; old and new binaries must mutually refuse")
			}
			if !strings.Contains(refusal, "does not know") && !strings.Contains(refusal, "027_plan_surface") {
				t.Fatalf("refusal = %q", refusal)
			}
		})
	}
}

func TestLegacyExportRoundTrip(t *testing.T) {
	snapshotDir := t.TempDir()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	legacy := buildLegacyDBAt(t, path, 9)
	populateBaselineEvidence(t, legacy, 9, snapshotDir)
	addLegacyTaskTables(t, legacy, true)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	before := fileMetadata(t, dir)
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(t.TempDir(), "export")
	export, err := ExportLegacyDatabase(path, outDir, baselineExportClose.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if export.SchemaVersion != 9 {
		t.Fatalf("export schema = %d", export.SchemaVersion)
	}
	copied, err := os.ReadFile(filepath.Join(outDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytesEqual(original, copied) {
		t.Fatal("exported database is not byte-identical to the original")
	}
	after := fileMetadata(t, dir)
	if len(before) != len(after) {
		t.Fatalf("export changed the source directory: %v -> %v", names(before), names(after))
	}
	for name, info := range before {
		if got := after[name]; got == nil || got.Size() != info.Size() || !got.ModTime().Equal(info.ModTime()) {
			t.Fatalf("export mutated %s", name)
		}
	}
	manifestBody, err := os.ReadFile(filepath.Join(outDir, "export-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Tables []struct {
			Name     string   `json:"name"`
			Columns  []string `json:"columns"`
			RowCount int      `json:"row_count"`
		} `json:"tables"`
		LegacyTables []struct {
			Name     string   `json:"name"`
			Columns  []string `json:"columns"`
			RowCount int      `json:"row_count"`
			SHA256   string   `json:"sha256"`
		} `json:"legacy_task_tables"`
		LegacyRows int    `json:"legacy_task_rows"`
		Digest     string `json:"digest"`
	}
	if err := json.Unmarshal(manifestBody, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.LegacyRows != 3 {
		t.Fatalf("legacy rows = %d, manifest = %+v", manifest.LegacyRows, manifest.LegacyTables)
	}
	legacyNames := map[string]bool{}
	for _, table := range manifest.LegacyTables {
		legacyNames[table.Name] = true
		if len(table.Columns) == 0 || table.SHA256 == "" {
			t.Fatalf("legacy table %s incomplete: %+v", table.Name, table)
		}
	}
	if len(legacyNames) != len(legacyTaskTables) {
		t.Fatalf("legacy table set = %v", legacyNames)
	}
	regularNames := map[string]bool{}
	for _, table := range manifest.Tables {
		regularNames[table.Name] = true
		if len(table.Columns) == 0 {
			t.Fatalf("table %s has no columns", table.Name)
		}
	}
	if !regularNames["tasks"] || !regularNames["attempts"] || regularNames["legacy_task_lists"] {
		t.Fatalf("table set = %v", regularNames)
	}
	digestBody, err := os.ReadFile(filepath.Join(outDir, "export-digest.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(digestBody) != export.Digest+"\n" || manifest.Digest != export.Digest {
		t.Fatalf("digest files disagree: %q %q %q", digestBody, manifest.Digest, export.Digest)
	}
	instructions, err := os.ReadFile(filepath.Join(outDir, "recovery-instructions.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(instructions), export.Digest) || !strings.Contains(string(instructions), "SHEPHRD_LEGACY_EXPORT_DIGEST=") {
		t.Fatalf("recovery instructions = %s", instructions)
	}
	snapshotCopy, err := os.ReadFile(filepath.Join(outDir, "snapshots", "report.md"))
	if err != nil {
		t.Fatalf("report snapshot not retained in the bundle: %v", err)
	}
	if !bytesEqual(snapshotCopy, []byte("durable report evidence\n")) {
		t.Fatal("retained report snapshot diverges")
	}
}

func bytesEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func TestLegacyExportRefusesNonLegacyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	state, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ExportLegacyDatabase(path, t.TempDir(), time.Now().UTC()); err == nil || !strings.Contains(err.Error(), "not a legacy schema") {
		t.Fatalf("baseline database exported, err = %v", err)
	}
	if _, err := ExportLegacyDatabase(filepath.Join(t.TempDir(), "missing.db"), t.TempDir(), time.Now().UTC()); err == nil || !strings.Contains(err.Error(), "no schema history") {
		t.Fatalf("fresh database exported, err = %v", err)
	}
}

func TestLegacyExportRefusesAfterWindowClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	legacy := buildLegacyDBAt(t, path, 8)
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ExportLegacyDatabase(path, t.TempDir(), baselineWindowClose); err == nil || !strings.Contains(err.Error(), "legacy schema support closed") {
		t.Fatalf("closed-window export succeeded, err = %v", err)
	}
}

func TestLegacyExportUnblocksBridgeUpgrade(t *testing.T) {
	for _, version := range []int{15, 23} {
		t.Run(fmt.Sprintf("schema %d", version), func(t *testing.T) {
			snapshotDir := t.TempDir()
			path := filepath.Join(t.TempDir(), "state.db")
			legacy := buildLegacyDBAt(t, path, version)
			populateBaselineEvidence(t, legacy, version, snapshotDir)
			addLegacyTaskTables(t, legacy, true)
			if err := legacy.Close(); err != nil {
				t.Fatal(err)
			}
			_, err := Open(path)
			want := "baseline migration refused: retired legacy task-list tables contain 3 row(s) without an accepted export digest; export them and retry with that digest; no database changes were made"
			if err == nil || err.Error() != want {
				t.Fatalf("refusal = %q, want %q", err, want)
			}
			outDir := filepath.Join(t.TempDir(), "export")
			export, err := ExportLegacyDatabase(path, outDir, time.Now().UTC())
			if err != nil {
				t.Fatalf("bridge-version export refused: %v", err)
			}
			if export.SchemaVersion != version || export.LegacyRows != 3 || export.Digest == "" {
				t.Fatalf("export = %+v", export)
			}
			instructions, err := os.ReadFile(filepath.Join(outDir, "recovery-instructions.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(instructions), export.Digest) || !strings.Contains(string(instructions), "SHEPHRD_LEGACY_EXPORT_DIGEST=") {
				t.Fatalf("recovery instructions = %s", instructions)
			}
			if strings.Contains(string(instructions), "v23 legacy upgrader") {
				t.Fatalf("bridge-version instructions point at the below-floor upgrader: %s", instructions)
			}
			state, err := OpenWithOptions(path, BaselineOptions{LegacyExportDigest: export.Digest})
			if err != nil {
				t.Fatalf("export digest did not unblock the upgrade: %v", err)
			}
			defer state.Close()
			assertBaselineEvidence(t, state, version)
		})
	}
}

func TestLegacyExportBridgeStaysOpenAfterWindowClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	legacy := buildLegacyDBAt(t, path, 27)
	addLegacyTaskTables(t, legacy, true)
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	export, err := ExportLegacyDatabase(path, filepath.Join(t.TempDir(), "export"), baselineWindowClose)
	if err != nil {
		t.Fatalf("bridge-version export after window close: %v", err)
	}
	if export.SchemaVersion != 27 || export.LegacyRows != 3 || export.Digest == "" {
		t.Fatalf("export = %+v", export)
	}
}
