package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
)

//go:embed migration_v27.sql
var migration27SQL string

// migratePlanSurfaceV27 renames the durable planning and annotation tables to
// the plan surface names. Existing plans, items, relations, selections, and
// annotations are copied row-for-row; the old tables are dropped only after
// the copy succeeds.
func migratePlanSurfaceV27(ctx context.Context, conn *sql.Conn) error {
	var newTables, oldTables int
	if err := conn.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='plans') +
		EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='plan_items') +
		EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='plan_prerequisites') +
		EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='plan_report_inputs') +
		EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='annotations'),
		EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='task_lists') +
		EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='task_list_items') +
		EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='task_list_prerequisites') +
		EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='task_list_inputs') +
		EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='driver_decisions')`).Scan(&newTables, &oldTables); err != nil {
		return err
	}
	switch {
	case newTables == 0 && oldTables == 5:
		_, err := conn.ExecContext(ctx, migration27SQL)
		return err
	case newTables == 5 && oldTables == 0:
		var triggers, indexes int
		if err := conn.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM sqlite_schema WHERE type='trigger' AND name IN (
				'plan_items_list_immutable', 'plan_items_dispatch_immutable', 'plan_items_dispatched_scope_immutable',
				'plan_items_dispatched_delete', 'plan_items_title_required_insert', 'plan_items_title_required_update',
				'plan_prerequisites_validate_insert', 'plan_prerequisites_validate_update', 'plan_prerequisites_validate_delete',
				'plan_report_inputs_validate_insert', 'plan_report_inputs_validate_update', 'plan_report_inputs_validate_delete',
				'annotations_item_scope_insert', 'annotations_revision_insert', 'annotations_immutable_update', 'annotations_immutable_delete')),
			(SELECT COUNT(*) FROM sqlite_schema WHERE type='index' AND name IN (
				'plans_driver_idx', 'plan_items_plan_idx', 'plan_prerequisites_prerequisite_idx',
				'plan_report_inputs_item_idx', 'plan_report_inputs_artifact_idx',
				'annotations_task_rev', 'annotations_item_rev'))`).Scan(&triggers, &indexes); err != nil {
			return err
		}
		if triggers != 16 || indexes != 7 {
			return fmt.Errorf("plan surface schema exists without its complete constraint set")
		}
		return nil
	case newTables > 0 && oldTables > 0:
		return fmt.Errorf("partial plan surface schema exists with both old and new table names; restore a backup before retrying the migration")
	case newTables > 0 && oldTables == 0:
		return fmt.Errorf("partial plan surface schema is missing %d of its tables; restore a backup before retrying the migration", 5-newTables)
	default:
		return fmt.Errorf("plan surface migration cannot run: the planning tables are missing (%d of 5 old tables present)", oldTables)
	}
}
