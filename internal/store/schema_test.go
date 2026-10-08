package store

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"shephrd/internal/model"
)

func TestFreshSchemaMatchesCurrentContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	state, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	var name, checksum string
	if err := state.db.QueryRow(`SELECT version, name, checksum FROM schema_migrations`).Scan(&version, &name, &checksum); err != nil {
		t.Fatal(err)
	}
	if version != baselineVersion || name != baselineName || checksum != baselineChecksum() {
		t.Fatalf("baseline identity = %d %q %s", version, name, checksum)
	}
	var originalVersion int
	var oldLedgerDigest, baselineReleasedAt, exportClose string
	if err := state.db.QueryRow(`SELECT original_version, old_ledger_digest, baseline_released_at, legacy_export_closes_at FROM schema_baseline_provenance`).Scan(&originalVersion, &oldLedgerDigest, &baselineReleasedAt, &exportClose); err != nil {
		t.Fatal(err)
	}
	if originalVersion != 0 || oldLedgerDigest != "" || baselineReleasedAt != baselineReleasedAt || exportClose != legacyExportClosesAt {
		t.Fatalf("fresh provenance = %+v", []string{oldLedgerDigest, baselineReleasedAt, exportClose})
	}

	objects := map[string][]string{"table": {}, "index": {}, "trigger": {}, "view": {}}
	rows, err := state.db.Query(`SELECT type, name FROM sqlite_schema WHERE type IN ('table','index','trigger','view') AND name NOT LIKE 'sqlite_%' ORDER BY type,name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var kind, objectName string
		if err := rows.Scan(&kind, &objectName); err != nil {
			t.Fatal(err)
		}
		objects[kind] = append(objects[kind], objectName)
	}
	rows.Close()
	wantTables := []string{"annotations", "attempt_checkpoints", "attempts", "coordinator_events", "coordinator_requests", "coordinator_workers", "coordinators", "driver_notifications", "external_delivery_attestations", "local_delivery_recoveries", "messages", "notification_delivery_log", "plan_items", "plan_prerequisites", "plan_report_inputs", "plans", "report_lifecycle_invocations", "report_recovery_attestations", "repos", "schema_baseline_provenance", "schema_migrations", "task_report_inputs", "tasks", "verified_artifacts"}
	for _, value := range [][]string{objects["table"], objects["index"], objects["trigger"], objects["view"]} {
		sort.Strings(value)
	}
	if !reflect.DeepEqual(objects["table"], wantTables) {
		t.Fatalf("tables = %v", objects["table"])
	}
	if err := validateBaselineShapeOn(state.db); err != nil {
		t.Fatalf("fresh schema diverges from the baseline contract: %v", err)
	}

	var settledIndexSQL string
	if err := state.db.QueryRow(`SELECT sql FROM sqlite_schema WHERE type='index' AND name='driver_notifications_settled_run_idx'`).Scan(&settledIndexSQL); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(settledIndexSQL, "CREATE UNIQUE INDEX") || !strings.Contains(settledIndexSQL, "(attempt_id, worker_run_generation)") || !strings.Contains(settledIndexSQL, "WHERE kind='settled'") {
		t.Fatalf("settled notification index = %q", settledIndexSQL)
	}

	var defaultPosition sql.NullString
	if err := state.db.QueryRow(`SELECT dflt_value FROM pragma_table_info('task_report_inputs') WHERE name='position'`).Scan(&defaultPosition); err != nil {
		t.Fatal(err)
	}
	if defaultPosition.String != "1" {
		t.Fatalf("task report input position default = %q", defaultPosition.String)
	}

	rows, err = state.db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Fatal("fresh schema has a foreign key violation")
	}
	rows.Close()
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	state, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	var count int
	if err := state.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("schema migration count = %d, err = %v", count, err)
	}
}

// TestBaselineUpgradeBackfillsTitlesFromFeatureKeys proves the released title
// backfill converges an older source on the baseline: pre-title tasks and
// plan items gain their feature keys (or objectives) as titles, and the
// baseline triggers enforce nonempty titles on new writes.
func TestBaselineUpgradeBackfillsTitlesFromFeatureKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	legacy := buildLegacyDBAt(t, path, 19)
	timestamp := "2026-01-01T00:00:00Z"
	if _, err := legacy.Exec(`INSERT INTO repos(id, name, path, default_branch, created_at, updated_at)
		VALUES('repo_titles', 'titles', '/tmp/titles', 'main', ?, ?)`, timestamp, timestamp); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`INSERT INTO tasks(id, repo_id, feature_key, driver_id, objective, status, created_at, updated_at)
		VALUES('task_titles', 'repo_titles', 'stable-title-key', 'driver:test', 'Complete the full task outcome', 'queued', ?, ?)`, timestamp, timestamp); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`INSERT INTO task_lists(id, name, driver_id, created_at, updated_at)
		VALUES('list_titles', 'titles', 'driver:test', ?, ?)`, timestamp, timestamp); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`INSERT INTO task_list_items(id, task_list_id, position, feature_key, objective, created_at, updated_at)
		VALUES('item_keyed', 'list_titles', 1, 'planned-title-key', 'Plan the keyed outcome', ?, ?),
		('item_incomplete', 'list_titles', 2, '', 'Plan an incomplete legacy outcome', ?, ?)`, timestamp, timestamp, timestamp, timestamp); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	state, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	task, err := state.Task("task_titles")
	if err != nil {
		t.Fatal(err)
	}
	if task.Title != "stable-title-key" {
		t.Fatalf("task title = %q", task.Title)
	}
	list, err := state.Plan("list_titles", "driver:test")
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 || list.Items[0].Title != "planned-title-key" || list.Items[1].Title != "Plan an incomplete legacy outcome" {
		t.Fatalf("migrated plan titles = %+v", list.Items)
	}
	if _, err := state.db.Exec(`INSERT INTO tasks(id, repo_id, feature_key, driver_id, objective, status, created_at, updated_at)
		VALUES('task_blank_title', 'repo_titles', 'blank-title', 'driver:test', 'Blank title', 'queued', ?, ?)`, timestamp, timestamp); err == nil || !strings.Contains(err.Error(), "task title must not be empty") {
		t.Fatalf("blank task title insert = %v", err)
	}
	if _, err := state.db.Exec(`INSERT INTO plan_items(id, plan_id, position, feature_key, objective, created_at, updated_at)
		VALUES('item_blank_title', 'list_titles', 3, 'blank-item-title', 'Blank item title', ?, ?)`, timestamp, timestamp); err == nil || !strings.Contains(err.Error(), "plan item title must not be empty") {
		t.Fatalf("blank plan item title insert = %v", err)
	}
	if _, err := state.db.Exec(`UPDATE tasks SET title='' WHERE id='task_titles'`); err == nil || !strings.Contains(err.Error(), "task title must not be empty") {
		t.Fatalf("blank task title update = %v", err)
	}
	if _, err := state.db.Exec(`UPDATE plan_items SET title='  ' WHERE id='item_keyed'`); err == nil || !strings.Contains(err.Error(), "plan item title must not be empty") {
		t.Fatalf("blank plan item title update = %v", err)
	}
	var count int
	if err := state.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE id='task_titles'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("task preservation count = %d, err = %v", count, err)
	}
}

// TestBaselineUpgradePreservesAnnotations proves the plan-surface bridge
// carries historical decision rows into the annotation surface on an upgrade
// from below the plan rename, and that the baseline annotation contract is
// live immediately after convergence.
func TestBaselineUpgradePreservesAnnotations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	legacy := buildLegacyDBAt(t, path, 17)
	timestamp := "2026-01-01T00:00:00Z"
	if _, err := legacy.Exec(`INSERT INTO repos(id, name, path, default_branch, created_at, updated_at)
		VALUES('repo_annotations', 'annotations', '/tmp/annotations', 'main', ?, ?)`, timestamp, timestamp); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`INSERT INTO tasks(id, repo_id, feature_key, driver_id, objective, status, created_at, updated_at)
		VALUES('task_annotations', 'repo_annotations', 'annotations', 'driver:existing', 'existing', 'queued', ?, ?)`, timestamp, timestamp); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`INSERT INTO driver_decisions(id, driver_id, task_id, revision, decision, reason, next_action, created_at)
		VALUES('decision-1', 'driver:existing', 'task_annotations', 1, 'resume', 'historical driver decision', 'continue', ?)`, timestamp); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	state, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	annotations, err := state.Annotations(model.AnnotationScope{TaskID: "task_annotations"})
	if err != nil {
		t.Fatal(err)
	}
	if len(annotations) != 1 || annotations[0].Judgment != "resume" || annotations[0].Reason != "historical driver decision" {
		t.Fatalf("migrated annotations = %+v", annotations)
	}
	if _, err := state.db.Exec(`SELECT COUNT(*) FROM driver_decisions`); err == nil {
		t.Fatal("retired driver decision table still exists after the baseline")
	}
	if _, err := state.AddAnnotation(model.AnnotationScope{TaskID: "task_annotations"}, "driver:existing", 1, model.AnnotationInput{Judgment: "resume"}); err != nil {
		t.Fatal(err)
	}
}
